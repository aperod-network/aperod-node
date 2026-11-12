package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/aperod/aperod/store"
)

const stagingRoot = "/opt/aperod/data/testnet/.backup-staging"

var allowedCheckpoint = regexp.MustCompile(
	`^` + regexp.QuoteMeta(stagingRoot) + `/checkpoint-(1189583511|1235802059|147415538)/chain\.db$`,
)

type tipReport struct {
	Height             uint64 `json:"height"`
	Hash               string `json:"hash"`
	HeightIndexMatches bool   `json:"height_index_matches"`
}

func allowedDatabasePath(path string) bool {
	return allowedCheckpoint.MatchString(path)
}

func allowedDatabasePathUnder(path, root string) bool {
	pattern := regexp.MustCompile(
		`^` + regexp.QuoteMeta(filepath.Clean(root)) +
			`/checkpoint-(1189583511|1235802059|147415538)/chain\.db$`,
	)
	return pattern.MatchString(path)
}

func requireOwnedRegular(path string, expectedUID uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != expectedUID {
		if expectedUID == 0 {
			return fmt.Errorf("%s is not root-owned", path)
		}
		return fmt.Errorf("%s is not owned by uid %d", path, expectedUID)
	}
	return nil
}

func inspectCheckpoint(input, root string, expectedUID uint32) (tipReport, error) {
	input = filepath.Clean(input)
	resolved, err := filepath.EvalSymlinks(input)
	if err != nil || resolved != input || !allowedDatabasePathUnder(resolved, root) {
		return tipReport{}, fmt.Errorf("refusing any path outside the three approved staging checkpoints")
	}
	database, err := os.Lstat(resolved)
	if err != nil || !database.IsDir() || database.Mode()&os.ModeSymlink != 0 {
		return tipReport{}, fmt.Errorf("existing non-symlink LevelDB directory required")
	}
	if err := requireOwnedRegular(filepath.Join(resolved, "CURRENT"), expectedUID); err != nil {
		return tipReport{}, err
	}
	if err := requireOwnedRegular(filepath.Join(resolved, "LOCK"), expectedUID); err != nil {
		return tipReport{}, err
	}

	hash, height, err := store.ReadTipOnly(resolved)
	if err != nil {
		return tipReport{}, err
	}
	return tipReport{
		Height: height, Hash: fmt.Sprintf("%x", hash[:]), HeightIndexMatches: true,
	}, nil
}

func main() {
	if len(os.Args) != 2 {
		panic("expected one approved staging checkpoint database")
	}
	report, err := inspectCheckpoint(os.Args[1], stagingRoot, 0)
	if err != nil {
		panic(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		panic(err)
	}
}
