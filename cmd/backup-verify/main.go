// Command backup-verify validates the manifest and canonical tip of an
// extracted backup using read-only database access.
package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type manifest struct {
	success   bool
	tipHash   string
	tipHeight uint64
}

type anchor struct {
	ID     string `json:"id"`
	Height uint64 `json:"height"`
	Hash   string `json:"hash"`
}

type anchorsFile struct {
	Schema      int      `json:"schema,omitempty"`
	GenesisHash string   `json:"genesis_hash"`
	Anchors     []anchor `json:"anchors"`
}

type proof struct {
	Schema      int      `json:"schema"`
	GenesisHash string   `json:"genesis_hash"`
	TipHeight   uint64   `json:"tip_height"`
	TipHash     string   `json:"tip_hash"`
	Anchors     []anchor `json:"anchors"`
}

func main() {
	stage := flag.String("stage", "", "directory containing extracted chain.db and manifest.json")
	legacyStage := flag.String("legacy-stage", "", "legacy extracted root containing testnet/chain.db and explorer_db.dump (read-only DB check only)")
	anchorsPath := flag.String("anchors-file", "", "completed live-chain anchors captured before checkpoint")
	proofPath := flag.String("proof-output", "", "atomically write a strict genesis/anchor proof")
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
	if (*anchorsPath == "") != (*proofPath == "") {
		fmt.Fprintln(os.Stderr, "backup-verify: --anchors-file and --proof-output must be supplied together")
		os.Exit(2)
	}
	if *anchorsPath != "" && mode != "stage" {
		fmt.Fprintln(os.Stderr, "backup-verify: proof extraction is supported only for a new-format --stage")
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
	if *anchorsPath != "" {
		if err := writeChainProof(filepath.Join(*stage, "chain.db"), *anchorsPath, *proofPath, hash, height); err != nil {
			fmt.Fprintf(os.Stderr, "backup-verify: extract chain proof: %v\n", err)
			os.Exit(1)
		}
	}
	if mode == "legacy" {
		fmt.Fprintln(os.Stderr, "backup-verify: legacy mode is an operational read-only DB check only; historical completeness is not verified")
		fmt.Printf("{\"success\":true,\"legacy\":true,\"tip_hash\":%q,\"tip_height\":%d}\n", hash, height)
		return
	}
	fmt.Printf("{\"success\":true,\"tip_hash\":%q,\"tip_height\":%d}\n", hash, height)
}

func writeChainProof(dbPath, anchorsPath, outputPath, tipHash string, tipHeight uint64) error {
	if !validHash(tipHash) {
		return errors.New("verified tip hash must be 64 lowercase hexadecimal characters")
	}
	expected, err := readAnchors(anchorsPath)
	if err != nil {
		return err
	}
	if !validHash(expected.GenesisHash) {
		return errors.New("anchors genesis_hash must be 64 lowercase hexadecimal characters")
	}
	seenIDs := make(map[string]bool, len(expected.Anchors))
	anchorHashes := make(map[string]crypto.Hash32, len(expected.Anchors))
	for _, item := range expected.Anchors {
		if item.ID == "" || strings.TrimSpace(item.ID) != item.ID {
			return errors.New("anchor id must be a nonempty string without surrounding whitespace")
		}
		if seenIDs[item.ID] {
			return fmt.Errorf("duplicate anchor id %q", item.ID)
		}
		seenIDs[item.ID] = true
		if !validHash(item.Hash) {
			return fmt.Errorf("anchor %q hash must be 64 lowercase hexadecimal characters", item.ID)
		}
		anchorHash, _ := hexHash(item.Hash)
		anchorHashes[item.ID] = anchorHash
		if item.Height > tipHeight {
			return fmt.Errorf("anchor %q height %d is above closed backup tip %d", item.ID, item.Height, tipHeight)
		}
	}

	db, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open closed chain.db read-only: %w", err)
	}
	defer db.Close()

	genesisHash, found, err := db.GetCanonicalHash(0)
	if err != nil {
		return fmt.Errorf("read canonical genesis height index: %w", err)
	}
	if !found || genesisHash == (crypto.Hash32{}) {
		return errors.New("canonical genesis height index is missing or zero")
	}
	if fmt.Sprintf("%x", genesisHash[:]) != expected.GenesisHash {
		return errors.New("captured genesis_hash does not match canonical stored height 0")
	}
	if err := verifyStoredBlock(db, genesisHash, 0, true); err != nil {
		return fmt.Errorf("verify canonical genesis block: %w", err)
	}

	tipHashBytes, err := hexHash(tipHash)
	if err != nil {
		return err
	}
	indexedTip, found, err := db.GetCanonicalHash(tipHeight)
	if err != nil {
		return fmt.Errorf("read canonical checkpoint tip index: %w", err)
	}
	if !found || indexedTip != tipHashBytes {
		return errors.New("verified checkpoint tip does not match canonical stored height index")
	}
	tipBlock, err := readStoredBlock(db, indexedTip, tipHeight, false)
	if err != nil {
		return fmt.Errorf("verify checkpoint tip block: %w", err)
	}

	proofAnchors := make([]anchor, 0, len(expected.Anchors))
	if len(expected.Anchors) == 0 {
		// With no captured rollout anchors there is no deletion authorization
		// range to prove; still require the stored genesis identity and exact
		// checkpoint tip body/index, without demanding pruned historical bodies.
	} else {
		anchorsAtHeight := make(map[uint64][]anchor, len(expected.Anchors))
		oldestHeight := tipHeight
		for _, item := range expected.Anchors {
			anchorsAtHeight[item.Height] = append(anchorsAtHeight[item.Height], item)
			if item.Height < oldestHeight {
				oldestHeight = item.Height
			}
		}
		expectedHash := tipHashBytes
		currentBlock := tipBlock
		for height := tipHeight; ; height-- {
			indexedHash, exists, err := db.GetCanonicalHash(height)
			if err != nil {
				return fmt.Errorf("read canonical ancestry index at height %d: %w", height, err)
			}
			if !exists {
				return fmt.Errorf("canonical ancestry index is missing at required height %d", height)
			}
			if indexedHash != expectedHash {
				return fmt.Errorf("canonical ancestry is disconnected at height %d", height)
			}
			if height != tipHeight {
				currentBlock, err = readStoredBlock(db, indexedHash, height, false)
				if err != nil {
					return fmt.Errorf("verify required ancestry body at height %d: %w", height, err)
				}
			}
			for _, item := range anchorsAtHeight[height] {
				if indexedHash != anchorHashes[item.ID] {
					return fmt.Errorf("canonical hash mismatch for anchor %q at height %d", item.ID, item.Height)
				}
			}
			if height == oldestHeight {
				break
			}
			expectedHash = currentBlock.Header.PrevHash
			if expectedHash == (crypto.Hash32{}) {
				return fmt.Errorf("canonical ancestry has a zero PrevHash above required height %d", height-1)
			}
		}
		proofAnchors = append(proofAnchors, expected.Anchors...)
	}

	document := proof{
		Schema: 1, GenesisHash: fmt.Sprintf("%x", genesisHash[:]),
		TipHeight: tipHeight, TipHash: tipHash, Anchors: proofAnchors,
	}
	return writeProofAtomic(outputPath, document)
}

func hexHash(value string) (crypto.Hash32, error) {
	var hash crypto.Hash32
	if !validHash(value) {
		return hash, errors.New("hash must be 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return hash, err
	}
	copy(hash[:], decoded)
	return hash, nil
}

func verifyStoredBlock(db *store.DB, hash crypto.Hash32, height uint64, genesis bool) error {
	_, err := readStoredBlock(db, hash, height, genesis)
	return err
}

func readStoredBlock(db *store.DB, hash crypto.Hash32, height uint64, genesis bool) (*core.Block, error) {
	raw, err := db.GetRawBlock(hash)
	if err != nil {
		return nil, fmt.Errorf("read stored block body: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("stored block body is missing at height %d", height)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, fmt.Errorf("decode stored block at height %d: %w", height, err)
	}
	if block.Header.Height != height || block.Hash() != hash {
		return nil, fmt.Errorf("stored block identity does not match canonical height %d", height)
	}
	if genesis && !block.IsGenesis() {
		return nil, errors.New("height 0 block is not a genesis block")
	}
	return &block, nil
}

func readAnchors(path string) (anchorsFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return anchorsFile{}, fmt.Errorf("open anchors file: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var result anchorsFile
	if err := decoder.Decode(&result); err != nil {
		return anchorsFile{}, fmt.Errorf("decode anchors file: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return anchorsFile{}, errors.New("anchors file contains trailing JSON")
		}
		return anchorsFile{}, fmt.Errorf("decode anchors file trailing data: %w", err)
	}
	if result.Anchors == nil {
		return anchorsFile{}, errors.New("anchors file must contain an anchors array")
	}
	if result.Schema != 0 && result.Schema != 1 {
		return anchorsFile{}, errors.New("anchors file schema, when present, must be 1")
	}
	return result, nil
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func writeProofAtomic(path string, document proof) error {
	if path == "" {
		return errors.New("proof output path is required")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("proof output must be a regular file if it already exists")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect proof output: %w", err)
	}
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".backup-proof-*")
	if err != nil {
		return fmt.Errorf("create proof temporary file: %w", err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure proof temporary file: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode proof: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync proof: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close proof: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish proof atomically: %w", err)
	}
	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open proof directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync proof directory: %w", err)
	}
	return nil
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
