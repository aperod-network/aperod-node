package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func makeSnapshotFiles(t *testing.T, jsonSnapshot string, hash crypto.Hash32) (string, string) {
	t.Helper()
	dir := t.TempDir()
	snapshotPath := filepath.Join(dir, "snapshot.json.gz")
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(jsonSnapshot)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath, compressed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(compressed.Bytes())
	if err := os.WriteFile(snapshotPath+".sha256", []byte(hex.EncodeToString(digest[:])), 0600); err != nil {
		t.Fatal(err)
	}
	return snapshotPath, filepath.Join(dir, "registry.json")
}

func snapshotJSON(hash crypto.Hash32, suffix string) string {
	return `{"v":2,"tip_height":42,"tip_hash":"` + hex.EncodeToString(hash[:]) +
		`","tx_total":7,"saved_at":"2026-01-02T03:04:05Z","utxos":{"active_utxos":[],"staked_utxos":[],"spent_decoys":[],"key_images":[]},"registry":{"validators":{},"dynamic_min_napr":9}` + suffix
}

func TestExtractRegistrySuccess(t *testing.T) {
	var hash crypto.Hash32
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	snapshotPath, outputPath := makeSnapshotFiles(t, snapshotJSON(hash, "}\n"), hash)
	if err := extract(snapshotPath, 42, hash, outputPath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var got anchoredRegistry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Height != 42 || got.Hash != hash || got.Registry.DynamicMinNAPR != 9 ||
		got.Registry.Validators == nil || len(got.Registry.Validators) != 0 {
		t.Fatalf("unexpected extracted registry: %+v", got)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("output permissions = %04o, want 0600", info.Mode().Perm())
	}
}

func TestExtractWrongAnchorHash(t *testing.T) {
	var sourceHash crypto.Hash32
	sourceHash[0] = 1
	snapshotPath, outputPath := makeSnapshotFiles(t, snapshotJSON(sourceHash, "}\n"), sourceHash)
	wrongHash := sourceHash
	wrongHash[0] = 2
	if err := extract(snapshotPath, 42, wrongHash, outputPath); err == nil {
		t.Fatal("expected mismatched snapshot hash to fail")
	}
}

func TestExtractWrongSidecar(t *testing.T) {
	var hash crypto.Hash32
	hash[0] = 1
	snapshotPath, outputPath := makeSnapshotFiles(t, snapshotJSON(hash, "}\n"), hash)
	if err := os.WriteFile(snapshotPath+".sha256", []byte(strings.Repeat("0", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := extract(snapshotPath, 42, hash, outputPath); err == nil {
		t.Fatal("expected checksum mismatch to fail")
	}
}

func TestVerifySidecarLineEndings(t *testing.T) {
	var hash crypto.Hash32
	hash[0] = 1
	snapshotPath, _ := makeSnapshotFiles(t, snapshotJSON(hash, "}\n"), hash)
	digest := sha256.Sum256(mustReadFile(t, snapshotPath))
	checksum := []byte(hex.EncodeToString(digest[:]))

	for _, suffix := range [][]byte{nil, []byte("\n"), []byte("\r\n")} {
		if err := os.WriteFile(snapshotPath+".sha256", append(append([]byte(nil), checksum...), suffix...), 0600); err != nil {
			t.Fatal(err)
		}
		if err := verifySidecar(snapshotPath); err != nil {
			t.Errorf("sidecar suffix %q rejected: %v", suffix, err)
		}
	}
	for _, suffix := range [][]byte{[]byte(" "), []byte("\r"), []byte("\n\n"), []byte("\r\n "), []byte("\nextra")} {
		if err := os.WriteFile(snapshotPath+".sha256", append(append([]byte(nil), checksum...), suffix...), 0600); err != nil {
			t.Fatal(err)
		}
		if err := verifySidecar(snapshotPath); err == nil {
			t.Errorf("sidecar suffix %q unexpectedly accepted", suffix)
		}
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestExtractIncompleteJSON(t *testing.T) {
	var hash crypto.Hash32
	hash[0] = 1
	snapshotPath, outputPath := makeSnapshotFiles(t, snapshotJSON(hash, ""), hash)
	if err := extract(snapshotPath, 42, hash, outputPath); err == nil {
		t.Fatal("expected incomplete snapshot JSON to fail")
	}
}
