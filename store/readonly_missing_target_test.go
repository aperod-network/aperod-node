package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenReadOnly_MissingEvidenceIsNeverCreated(t *testing.T) {
	for _, mode := range []string{"missing_parent", "missing_leaf", "empty_directory"} {
		t.Run(mode, func(t *testing.T) {
			parent := t.TempDir()
			path := filepath.Join(parent, "chain.db")
			if mode == "missing_parent" {
				path = filepath.Join(parent, "absent", "chain.db")
			}
			if mode == "empty_directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			db, err := OpenReadOnly(path)
			if err == nil {
				if closeErr := db.Close(); closeErr != nil {
					t.Errorf("close unexpected database: %v", closeErr)
				}
				t.Fatal("missing database evidence was accepted")
			}
			if mode == "empty_directory" {
				entries, readErr := os.ReadDir(path)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("read-only probe created database files: %v / %v", entries, readErr)
				}
			} else if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("read-only probe created a missing target: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(parent, "absent")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("read-only probe created a missing parent: %v", statErr)
			}
		})
	}
}
