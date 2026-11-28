package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveEviction_StaysInsideCopiedRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "table.ldb")
	want := []byte("synthetic offline table")
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	if err := evictArchiveCopy(dir); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("preserve outside"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("close test root: %v", err)
		}
	}()
	// Model a regular entry observed by WalkDir, then replaced before open.
	if info, err := root.Stat("table.ldb"); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("missing positive regular-file control: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := evictArchiveCopyFile(root, "table.ldb"); err == nil {
		t.Fatal("post-enumeration symlink escaped the copied root")
	}
	if err := evictArchiveCopy(dir); err == nil {
		t.Fatal("walker accepted a symlink")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "preserve outside" {
		t.Fatalf("outside content changed: %q %v", data, err)
	}
}
