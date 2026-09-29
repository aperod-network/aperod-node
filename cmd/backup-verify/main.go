// Command backup-verify validates the manifest and canonical tip of an
// extracted backup using read-only database access.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aperod/aperod/store"
)

type manifest struct {
	success   bool
	tipHash   string
	tipHeight uint64
}

func main() {
	stage := flag.String("stage", "", "directory containing extracted chain.db and manifest.json")
	legacyStage := flag.String("legacy-stage", "", "legacy extracted root containing testnet/chain.db and explorer_db.dump (read-only DB check only)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "backup-verify: unexpected positional arguments")
		os.Exit(2)
	}

	mode, err := selectMode(*stage, *legacyStage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup-verify: %v\n", err)
		os.Exit(2)
	}
	var hash string
	var height uint64
	switch mode {
	case "stage":
		hash, height, err = verifyStage(*stage)
	case "legacy":
		hash, height, err = verifyLegacyStage(*legacyStage)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup-verify: %v\n", err)
		os.Exit(1)
	}
	if mode == "legacy" {
		fmt.Fprintln(os.Stderr, "backup-verify: legacy mode is an operational read-only DB check only; historical completeness is not verified")
		fmt.Printf("{\"success\":true,\"legacy\":true,\"tip_hash\":%q,\"tip_height\":%d}\n", hash, height)
		return
	}
	fmt.Printf("{\"success\":true,\"tip_hash\":%q,\"tip_height\":%d}\n", hash, height)
}

func selectMode(stage, legacyStage string) (string, error) {
	if stage != "" && legacyStage != "" {
		return "", errors.New("--stage and --legacy-stage are mutually exclusive")
	}
	if stage != "" {
		return "stage", nil
	}
	if legacyStage != "" {
		return "legacy", nil
	}
	return "", errors.New("one of --stage or --legacy-stage is required")
}

func verifyStage(stage string) (string, uint64, error) {
	stageInfo, err := os.Lstat(stage)
	if err != nil {
		return "", 0, fmt.Errorf("inspect stage: %w", err)
	}
	if stageInfo.Mode()&os.ModeSymlink != 0 || !stageInfo.IsDir() {
		return "", 0, errors.New("stage must be a real directory")
	}

	manifestPath := filepath.Join(stage, "manifest.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return "", 0, fmt.Errorf("inspect manifest.json: %w", err)
	}
	if !manifestInfo.Mode().IsRegular() {
		return "", 0, errors.New("manifest.json must be a regular file")
	}
	m, err := readManifest(manifestPath)
	if err != nil {
		return "", 0, err
	}
	if !m.success {
		return "", 0, errors.New("manifest success must be true")
	}
	if len(m.tipHash) != 64 {
		return "", 0, errors.New("manifest tip_hash must be 64 lowercase hexadecimal characters")
	}
	for _, c := range m.tipHash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", 0, errors.New("manifest tip_hash must be 64 lowercase hexadecimal characters")
		}
	}

	dbPath := filepath.Join(stage, "chain.db")
	if err := validateDBTree(dbPath); err != nil {
		return "", 0, err
	}

	tipHash, tipHeight, err := store.ReadTipOnly(dbPath)
	if err != nil {
		return "", 0, fmt.Errorf("validate read-only chain.db: %w", err)
	}
	actualHash := fmt.Sprintf("%x", tipHash[:])
	if actualHash != m.tipHash || tipHeight != m.tipHeight {
		return "", 0, fmt.Errorf(
			"manifest tip mismatch: database has %s/%d, manifest has %s/%d",
			actualHash, tipHeight, m.tipHash, m.tipHeight)
	}
	return actualHash, tipHeight, nil
}

func verifyLegacyStage(root string) (string, uint64, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", 0, fmt.Errorf("inspect legacy root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", 0, errors.New("legacy root must be a real directory")
	}

	testnetPath := filepath.Join(root, "testnet")
	testnetInfo, err := os.Lstat(testnetPath)
	if err != nil {
		return "", 0, fmt.Errorf("inspect legacy testnet directory: %w", err)
	}
	if testnetInfo.Mode()&os.ModeSymlink != 0 || !testnetInfo.IsDir() {
		return "", 0, errors.New("legacy testnet must be a real directory")
	}

	dbPath := filepath.Join(testnetPath, "chain.db")
	if err := validateDBTree(dbPath); err != nil {
		return "", 0, fmt.Errorf("legacy chain.db: %w", err)
	}

	dumpPath := filepath.Join(root, "explorer_db.dump")
	dumpInfo, err := os.Lstat(dumpPath)
	if err != nil {
		return "", 0, fmt.Errorf("inspect legacy explorer_db.dump: %w", err)
	}
	if !dumpInfo.Mode().IsRegular() {
		return "", 0, errors.New("legacy explorer_db.dump must be a regular file")
	}

	tipHash, tipHeight, err := store.ReadTipOnly(dbPath)
	if err != nil {
		return "", 0, fmt.Errorf("validate read-only legacy chain.db: %w", err)
	}
	return fmt.Sprintf("%x", tipHash[:]), tipHeight, nil
}

func readManifest(path string) (manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return manifest{}, fmt.Errorf("open manifest.json: %w", err)
	}
	defer f.Close()

	decoder := json.NewDecoder(f)
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return manifest{}, fmt.Errorf("decode manifest.json: %w", err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return manifest{}, errors.New("manifest.json must be a JSON object")
	}

	var result manifest
	seen := make(map[string]bool, 3)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return manifest{}, fmt.Errorf("decode manifest.json field: %w", err)
		}
		key, ok := token.(string)
		if !ok {
			return manifest{}, errors.New("manifest.json field names must be strings")
		}
		if seen[key] {
			return manifest{}, fmt.Errorf("manifest.json has duplicate field %q", key)
		}
		seen[key] = true
		switch key {
		case "success":
			if err := decoder.Decode(&result.success); err != nil {
				return manifest{}, errors.New("manifest success must be a boolean")
			}
		case "tip_hash":
			if err := decoder.Decode(&result.tipHash); err != nil {
				return manifest{}, errors.New("manifest tip_hash must be a string")
			}
		case "tip_height":
			var number json.Number
			if err := decoder.Decode(&number); err != nil {
				return manifest{}, errors.New("manifest tip_height must be a nonnegative integer")
			}
			height, err := strconv.ParseUint(number.String(), 10, 64)
			if err != nil {
				return manifest{}, errors.New("manifest tip_height must be a nonnegative integer")
			}
			result.tipHeight = height
		default:
			return manifest{}, fmt.Errorf("manifest.json has unknown field %q", key)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return manifest{}, fmt.Errorf("decode manifest.json object: %w", err)
	}
	if !seen["success"] || !seen["tip_hash"] || !seen["tip_height"] {
		return manifest{}, errors.New("manifest.json must contain success, tip_hash, and tip_height")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return manifest{}, errors.New("manifest.json contains trailing JSON")
		}
		return manifest{}, fmt.Errorf("decode manifest.json trailing data: %w", err)
	}
	return result, nil
}

func validateDBTree(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect chain.db: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("chain.db must be a real directory")
	}
	err = filepath.WalkDir(path, func(entryPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("inspect chain.db entry: %w", walkErr)
		}
		info, err := os.Lstat(entryPath)
		if err != nil {
			return fmt.Errorf("inspect chain.db entry: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("chain.db contains symlink %q", strings.TrimPrefix(entryPath, path+string(os.PathSeparator)))
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("chain.db contains special file %q", strings.TrimPrefix(entryPath, path+string(os.PathSeparator)))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}
