package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestAuthorityForkLoaderPinsRuleAndHistoricalWitness(t *testing.T) {
	old, _, _ := crypto.GenerateValidatorKey()
	next, _, _ := crypto.GenerateValidatorKey()
	next2, _, _ := crypto.GenerateValidatorKey()
	witness := []byte("existing immutable witness")
	first := &core.ValidatorAuthorityFork{Version: 1, Height: 10,
		Identity: old.Public(), Replacement: next.Public(), Genesis: crypto.HashBytes([]byte("genesis")),
		Parent: crypto.HashBytes([]byte("parent")), RegistrySHA256: crypto.HashBytes([]byte("registry")),
		LegacyWitnessSHA256: crypto.Hash32(sha256.Sum256(witness))}
	first.Possession, _ = next.Sign(first.Message())
	f := &core.ValidatorAuthorityFork{Version: 2, Height: 11,
		Identity: old.Public(), Replacement: next2.Public(), Genesis: first.Genesis,
		Parent: crypto.HashBytes([]byte("rotation parent")), RegistrySHA256: crypto.HashBytes([]byte("rotation registry")),
		LegacyWitnessSHA256: first.LegacyWitnessSHA256, Previous: first}
	f.Possession, _ = next2.Sign(f.Message())
	raw, _ := json.Marshal(f)
	dir := t.TempDir()
	rulePath, witnessPath := filepath.Join(dir, "rule.json"), filepath.Join(dir, "witness.json")
	if err := os.WriteFile(rulePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(witnessPath, witness, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	cfg := config.ConsensusConfig{ValidatorAuthorityForkFile: rulePath,
		ValidatorAuthorityForkSHA256: hex.EncodeToString(sum[:]), LPoDMigrationFile: witnessPath}
	loaded, err := loadValidatorAuthorityFork(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Authority(old.Public(), 10).Equals(next.Public()) ||
		!loaded.Authority(old.Public(), 11).Equals(next2.Public()) {
		t.Fatal("loaded schedule did not preserve both signing eras")
	}
	cfg.ValidatorAuthorityForkSHA256 = ""
	if _, err := loadValidatorAuthorityFork(cfg); err == nil {
		t.Fatal("unpinned rule accepted")
	}
	cfg.ValidatorAuthorityForkSHA256 = hex.EncodeToString(sum[:])
	if err := os.WriteFile(witnessPath, []byte("new forged witness"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadValidatorAuthorityFork(cfg); err == nil {
		t.Fatal("substituted witness accepted")
	}
}
