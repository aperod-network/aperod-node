package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckStore_MissingDatabaseFailsWithoutCreatingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing-data")
	old := os.Args
	os.Args = []string{"aperod-node", "--check-store", "--data-dir=" + dir}
	defer func() { os.Args = old }()
	if err := runCheckStore(); err == nil {
		t.Fatal("missing store was reported as a healthy empty chain")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification created a data directory: %v", err)
	}
}
