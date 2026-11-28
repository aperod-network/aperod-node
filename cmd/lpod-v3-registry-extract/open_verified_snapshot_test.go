package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestVerifiedSnapshot_PathReplacementCannotChangeVerifiedDescriptor(t *testing.T) {
	path, _ := makeSnapshotFiles(t, `{"v":2,"fixture":"original"}`, crypto.Hash32{})
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := openVerifiedSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := verified.Close(); err != nil {
			t.Errorf("close verified fixture: %v", err)
		}
	}()
	replacement := filepath.Join(filepath.Dir(path), "replacement.tmp")
	if err := os.WriteFile(replacement, []byte("unverified replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(verified)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("verified descriptor read replaced path: equal=%v err=%v", bytes.Equal(actual, original), err)
	}
	if unverified, err := openVerifiedSnapshot(path); err == nil {
		if closeErr := unverified.Close(); closeErr != nil {
			t.Errorf("close unexpected fixture: %v", closeErr)
		}
		t.Fatal("replacement file passed the original checksum")
	}
}
