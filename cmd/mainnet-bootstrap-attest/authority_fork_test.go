package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestReadSuccessorPrivateKeyRequiresMode0600RegularFile(t *testing.T) {
	private, public, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "successor.seed")
	if err := os.WriteFile(path, private.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := readSuccessorPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Public().Equals(public) {
		t.Fatal("loaded successor key has the wrong public key")
	}
	crypto.ZeroBytes(loaded.Bytes())

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readSuccessorPrivateKey(path); err == nil {
		t.Fatal("group-readable successor key accepted")
	}
	link := filepath.Join(t.TempDir(), "successor-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSuccessorPrivateKey(link); err == nil {
		t.Fatal("successor-key symlink accepted")
	}
}
