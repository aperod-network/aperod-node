package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/mainnetbootstrap"
)

func TestVerifyPublishedMainnetEvidence(t *testing.T) {
	deploy := filepath.Join("..", "..", "deploy")
	manifest := filepath.Join(deploy, "mainnet-bootstrap-4076315.json")
	checkpoint := filepath.Join(deploy, "mainnet-avm-checkpoint-4076315.json")

	file, err := os.Open(manifest)
	if err != nil {
		t.Fatal(err)
	}
	signed, decodeErr := mainnetbootstrap.ReadStrict(file)
	closeErr := file.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err := signed.VerifyHistorical(); err != nil {
		t.Fatalf("verify historical Mainnet statement: %v", err)
	}
	checkpointData, found, err := avm.ReadCheckpointFile(checkpoint)
	if err != nil || !found {
		t.Fatalf("read historical AVM checkpoint: found=%t err=%v", found, err)
	}
	if err := verifyBootstrapAVM(
		signed.Statement, checkpointData, 1_850_000,
		mainnetbootstrap.HistoricalBootstrapSignerPublicKeyHex,
	); err != nil {
		t.Fatalf("verify historical AVM checkpoint: %v", err)
	}
	if signed.Statement.Network != "mainnet" ||
		signed.Statement.ChainID != "aperod-mainnet-1" ||
		signed.Statement.TrustModel != "operator_1_of_1" ||
		signed.Statement.CheckpointHeight != 4_076_315 {
		t.Fatalf("unexpected historical Mainnet identity: %+v", signed.Statement)
	}
}

func TestVerifyPublishedMainnetEvidenceRejectsTamperedSignature(t *testing.T) {
	deploy := filepath.Join("..", "..", "deploy")
	raw, err := os.ReadFile(filepath.Join(deploy, "mainnet-bootstrap-4076315.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tampered.json")
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["signature"] = strings.Repeat("0", 128)
	tampered, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tamperedStatement, decodeErr := mainnetbootstrap.ReadStrict(file)
	closeErr := file.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err := tamperedStatement.VerifyHistorical(); err == nil {
		t.Fatal("tampered bootstrap signature unexpectedly verified")
	}
}
