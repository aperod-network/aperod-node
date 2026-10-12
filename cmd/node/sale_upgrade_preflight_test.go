package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

func saleUpgradeStage(t *testing.T, root string) (string, checkpointManifest) {
	t.Helper()
	stage, err := os.MkdirTemp(root, "checkpoint-")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(stage, "chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	block := core.Block{}
	block.Header.MerkleRoot = core.MerkleRoot(nil)
	raw, _ := json.Marshal(block)
	hash := block.Hash()
	if err := db.PutRawBlock(hash, 0, raw); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(hash, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := checkpointManifest{Success: true, TipHash: fmt.Sprintf("%x", hash[:]), TipHeight: 0}
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return stage, manifest
}

func TestSaleUpgradeStageManifest(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	stage, manifest := saleUpgradeStage(t, t.TempDir())
	var output bytes.Buffer
	if err := verifySaleUpgradeStage(cfg, stage, nil, &output, false); err != nil {
		t.Fatal(err)
	}
	var report store.SaleUpgradeReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || report.Status != "ok" || report.Mode != "inactive" {
		t.Fatal(output.String(), err)
	}
	reply := checkpointResponse{Path: stage, TipHeight: 1, TipHash: manifest.TipHash}
	if err := verifySaleUpgradeStage(cfg, stage, &reply, io.Discard, false); err == nil {
		t.Fatal("receipt/manifest disagreement accepted")
	}
	manifest.Success = false
	raw, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(stage, "manifest.json"), raw, 0600)
	if err := verifySaleUpgradeStage(cfg, stage, nil, io.Discard, false); err == nil {
		t.Fatal("incomplete copy accepted")
	}
}

func TestSaleUpgradeFreshSocketCopy(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(dataDir, ".backup-staging")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	stage, manifest := saleUpgradeStage(t, root)
	path := filepath.Join(dataDir, backupSocketName)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/checkpoint" || r.ContentLength != 0 {
			t.Error("unexpected backup request")
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(checkpointResponse{Path: stage, TipHeight: manifest.TipHeight, TipHash: manifest.TipHash})
	})}
	go server.Serve(listener)
	defer server.Close()
	cfg := &config.Config{DataDir: dataDir}
	if err := runSaleUpgradePreflight(cfg, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("successful owned temporary copy not cleaned")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("cleanup removed parent")
	}
}

func TestSaleUpgradeCannotDispatchMutatingModes(t *testing.T) {
	before := os.Args
	defer func() { os.Args = before }()
	for _, mode := range []string{"--compact-db", "--repair-height-index", "--check-store", "--check-snapshot"} {
		os.Args = []string{"node", "--sale-upgrade-preflight", mode}
		if err := run(); err == nil {
			t.Fatalf("accepted incompatible mode %s", mode)
		}
	}
}

func TestSaleUpgradeCapabilityDoesNotLoadConfig(t *testing.T) {
	beforeArgs, beforeStdout := os.Args, os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer func() { os.Args, os.Stdout = beforeArgs, beforeStdout; writer.Close() }()
	os.Args = []string{"node", "--sale-upgrade-preflight-capability", "--validate-config", "--config", "/definitely-missing-config"}
	os.Stdout = writer
	if err := run(); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	result, err := io.ReadAll(reader)
	if err != nil || string(result) != "{\"schema\":1,\"command\":\"sale-upgrade-preflight\"}\n" {
		t.Fatalf("capability receipt %s: %v", result, err)
	}
}
