package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestLoadAuthorityForkUsesPinnedFileAndReplacementAuthority(t *testing.T) {
	oldKey, oldPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	replacementKey, replacementPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	fork := &core.ValidatorAuthorityFork{
		Version:        1,
		Genesis:        crypto.HashBytes([]byte("genesis")),
		Height:         5,
		Parent:         crypto.HashBytes([]byte("activation-parent")),
		Identity:       oldPub,
		Replacement:    replacementPub,
		RegistrySHA256: crypto.HashBytes([]byte("registry")),
	}
	fork.Possession, err = replacementKey.Sign(fork.Message())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(fork)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := os.MkdirTemp("/tmp", "finality-audit-authority-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "authority.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	loaded, err := loadAuthorityFork(path, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("load pinned authority fork: %v", err)
	}
	if loaded.Height != fork.Height || !loaded.Identity.Equals(oldPub) || !loaded.Replacement.Equals(replacementPub) {
		t.Fatal("loaded authority fork differs from the signed rule")
	}

	header := core.BlockHeader{
		Height:       fork.Height,
		PrevHash:     fork.Parent,
		MerkleRoot:   core.MerkleRoot(nil),
		Timestamp:    1,
		ValidatorPub: oldPub,
	}
	header.Signature, err = oldKey.Sign(header.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if header.VerifySignature(authorityFor(loaded, header.ValidatorPub, header.Height)) {
		t.Fatal("old identity key remained authorized after the fork")
	}
	header.Signature, err = replacementKey.Sign(header.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if header.VerifySignature() {
		t.Fatal("replacement signature unexpectedly verifies as the old identity key")
	}
	if !header.VerifySignature(authorityFor(loaded, header.ValidatorPub, header.Height)) {
		t.Fatal("replacement signature did not verify under the pinned authority rule")
	}
	if !authorityFor(nil, oldPub, header.Height).Equals(oldPub) {
		t.Fatal("no-fork behavior changed the validator identity")
	}
}

func TestLoadAuthorityForkRejectsUnpinnedBytes(t *testing.T) {
	raw := []byte(`{"version":1}`)
	dir, err := os.MkdirTemp("/tmp", "finality-audit-authority-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "authority.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAuthorityFork(path, hex.EncodeToString(make([]byte, sha256.Size))); err == nil {
		t.Fatal("accepted authority bytes that do not match the configured pin")
	}
}

func TestLoadAuthorityForkRequiresPathAndHashTogether(t *testing.T) {
	if _, err := loadAuthorityFork("/tmp/authority.json", ""); err == nil {
		t.Fatal("accepted authority file without its configured SHA-256")
	}
	if _, err := loadAuthorityFork("", "abcd"); err == nil {
		t.Fatal("accepted authority SHA-256 without its file")
	}
}
