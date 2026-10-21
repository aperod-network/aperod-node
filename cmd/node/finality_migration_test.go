package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func finalityMigrationFixture(t *testing.T) ([]byte, []string) {
	t.Helper()
	_, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	migration := store.FinalityCommitteeMigration{
		Version:              store.FinalityCommitteeMigrationVersion,
		Genesis:              crypto.Hash32{1},
		CertificateHeight:    10,
		CertificateBlockHash: crypto.Hash32{2},
		ApprovalHeight:       11,
		ApprovalBlockHash:    crypto.Hash32{3},
		BFTThresholdPPM:      667_000,
		HistoricalCommittee:  []crypto.ValidatorPubKey{pub},
		Approvals: []store.FinalityMigrationApproval{{
			Validator: pub,
			Signature: make([]byte, 64),
		}},
	}
	raw, err := json.Marshal(migration)
	if err != nil {
		t.Fatal(err)
	}
	return raw, []string{pub.Hex()}
}

func TestLoadFinalityCommitteeMigrationRequiresFileAndTrustPinsTogether(t *testing.T) {
	if migration, authorities, err := loadFinalityCommitteeMigration("", nil); err != nil ||
		migration != nil || authorities != nil {
		t.Fatalf("disabled migration returned migration=%v authorities=%v err=%v",
			migration, authorities, err)
	}
	if _, _, err := loadFinalityCommitteeMigration("", []string{"01"}); err == nil {
		t.Fatal("trust authorities without a migration file were accepted")
	}
	if _, _, err := loadFinalityCommitteeMigration(filepath.Join(t.TempDir(), "missing.json"), nil); err == nil {
		t.Fatal("migration file without trust authorities was accepted")
	}
}

func TestLoadFinalityCommitteeMigrationValidatesAndLoads(t *testing.T) {
	raw, trust := finalityMigrationFixture(t)
	path := filepath.Join(t.TempDir(), "migration.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	migration, authorities, err := loadFinalityCommitteeMigration(path, trust)
	if err != nil {
		t.Fatal(err)
	}
	if migration == nil || len(migration.Approvals) != 1 || len(authorities) != 1 ||
		!authorities[0].Equals(migration.Approvals[0].Validator) {
		t.Fatalf("unexpected loaded migration or trust authorities: migration=%+v authorities=%v",
			migration, authorities)
	}
}

func TestLoadFinalityCommitteeMigrationRejectsMalformedAndOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.json")
	raw, trust := finalityMigrationFixture(t)
	for name, contents := range map[string][]byte{
		"unknown field":  []byte(`{"version":1,"unexpected":true}`),
		"trailing value": append(append([]byte(nil), raw...), []byte(` {}`)...),
		"oversized":      make([]byte, maxFinalityMigrationFileBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadFinalityCommitteeMigration(path, trust); err == nil {
				t.Fatal("malformed migration file was accepted")
			}
		})
	}
}

func TestLoadFinalityCommitteeMigrationRejectsDuplicateTrustPins(t *testing.T) {
	raw, trust := finalityMigrationFixture(t)
	path := filepath.Join(t.TempDir(), "migration.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadFinalityCommitteeMigration(path, []string{trust[0], trust[0]}); err == nil {
		t.Fatal("duplicate trust authority was accepted")
	}
}
