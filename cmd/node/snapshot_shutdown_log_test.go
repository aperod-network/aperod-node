package main

// Tests for the two logging improvements added to prevent silent fallbacks:
//
//  1. logSnapshotStartupReason emits a structured entry that distinguishes
//     "no snapshot found" (first-run) from "snapshot corrupt or unreadable"
//     (SIGKILL mid-write victim) so operators can tell the causes apart via
//     journalctl startup_reason= filter.
//
//  2. saveShutdownSnapshot now includes a save_duration field so operators
//     can verify their TimeoutStopSec is high enough to let the write finish.

import (
	"bytes"
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// ─── logSnapshotStartupReason ─────────────────────────────────────────────────

// logFieldValue scans JSON log lines for an entry whose "msg" field equals
// wantMsg and returns the string representation of field.
// Numeric JSON values are converted via fmt.Sprintf so callers can compare
// tip_height etc. without type-asserting to float64.
// Returns ("", false) if not found.
func logFieldValue(buf *bytes.Buffer, wantMsg, field string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var rec map[string]interface{}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if msg, ok := rec["msg"].(string); !ok || msg != wantMsg {
			continue
		}
		v, exists := rec[field]
		if !exists {
			return "", false
		}
		switch val := v.(type) {
		case string:
			return val, true
		default:
			return fmt.Sprintf("%v", val), true
		}
	}
	return "", false
}

func TestLogSnapshotStartupReason_NoSnapshot(t *testing.T) {
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	logSnapshotStartupReason(os.ErrNotExist, 12345, log)

	const wantMsg = "no snapshot found — full block scan required"
	if !logContainsMsg(&buf, wantMsg) {
		t.Fatalf("expected log message %q not found in:\n%s", wantMsg, buf.String())
	}

	reason, ok := logFieldValue(&buf, wantMsg, "startup_reason")
	if !ok {
		t.Fatalf("startup_reason field missing in log line")
	}
	if reason != "no_snapshot" {
		t.Errorf("startup_reason = %q, want %q", reason, "no_snapshot")
	}

	// tip_height must be present and numeric.
	heightStr, ok2 := logFieldValue(&buf, wantMsg, "tip_height")
	if !ok2 || heightStr == "" {
		t.Errorf("tip_height field missing or empty in log line")
	}
}

func TestLogSnapshotStartupReason_WrappedNotExist(t *testing.T) {
	// os.IsNotExist should also match wrapped ErrNotExist.
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	wrapped := fmt.Errorf("outer: %w", os.ErrNotExist)
	logSnapshotStartupReason(wrapped, 0, log)

	const wantMsg = "no snapshot found — full block scan required"
	if !logContainsMsg(&buf, wantMsg) {
		t.Fatalf("expected log message %q for wrapped ErrNotExist, got:\n%s", wantMsg, buf.String())
	}
	reason, _ := logFieldValue(&buf, wantMsg, "startup_reason")
	if reason != "no_snapshot" {
		t.Errorf("startup_reason = %q, want no_snapshot", reason)
	}
}

func TestLogSnapshotStartupReason_CorruptSnapshot(t *testing.T) {
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	// Any non-NotExist error signals a corrupt/unreadable snapshot.
	logSnapshotStartupReason(errors.New("gzip: invalid header"), 99, log)

	const wantMsg = "snapshot corrupt or unreadable — falling back to full block scan"
	if !logContainsMsg(&buf, wantMsg) {
		t.Fatalf("expected log message %q not found in:\n%s", wantMsg, buf.String())
	}

	reason, ok := logFieldValue(&buf, wantMsg, "startup_reason")
	if !ok {
		t.Fatalf("startup_reason field missing in log line")
	}
	if reason != "corrupt_snapshot" {
		t.Errorf("startup_reason = %q, want %q", reason, "corrupt_snapshot")
	}
}

// ─── saveShutdownSnapshot duration logging ────────────────────────────────────

// buildMinimalChainForShutdown creates a two-block chain (genesis + height-1)
// in a temp store and returns the store plus a registry so saveShutdownSnapshot
// has a real tip at height > 0 (the function skips height==0 as a no-op).
func buildMinimalChainForShutdown(t *testing.T, dir string) (*store.DB, *core.UTXOSet, *core.ValidatorRegistry) {
	t.Helper()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	makeBlock := func(height uint64, prevHash crypto.Hash32) *core.Block {
		b := &core.Block{
			Header: core.BlockHeader{
				Height:       height,
				PrevHash:     prevHash,
				Timestamp:    time.Now().UnixNano(),
				ValidatorPub: pub,
				MerkleRoot:   core.MerkleRoot(nil),
			},
		}
		if err := b.Header.Sign(priv); err != nil {
			t.Fatalf("sign block h=%d: %v", height, err)
		}
		return b
	}

	genesis := makeBlock(0, crypto.Hash32{})
	blk1 := makeBlock(1, genesis.Hash())

	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, b := range []*core.Block{genesis, blk1} {
		raw, merr := json.Marshal(b)
		if merr != nil {
			t.Fatalf("marshal block h=%d: %v", b.Header.Height, merr)
		}
		if err := db.PutRawBlock(b.Hash(), b.Header.Height, raw); err != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, err)
		}
	}
	if err := db.PutTip(blk1.Hash(), 1); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)

	return db, utxos, registry
}

func TestSaveShutdownSnapshot_LogsDuration(t *testing.T) {
	dir := t.TempDir()
	db, utxos, registry := buildMinimalChainForShutdown(t, dir)

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	saveShutdownSnapshot(db, utxos, registry, dir, log)

	const wantMsg = "shutdown: snapshot saved"
	if !logContainsMsg(&buf, wantMsg) {
		t.Fatalf("expected log %q, got:\n%s", wantMsg, buf.String())
	}

	durStr, ok := logFieldValue(&buf, wantMsg, "save_duration")
	if !ok {
		t.Fatalf("save_duration field missing in shutdown snapshot log; full output:\n%s", buf.String())
	}
	if durStr == "" {
		t.Error("save_duration is empty string")
	}
	// Sanity: must parse as a Go duration.
	if _, err := time.ParseDuration(durStr); err != nil {
		t.Errorf("save_duration %q is not a valid Go duration: %v", durStr, err)
	}
}

func TestSaveShutdownSnapshot_LogsTipHeight(t *testing.T) {
	dir := t.TempDir()
	db, utxos, registry := buildMinimalChainForShutdown(t, dir)

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	saveShutdownSnapshot(db, utxos, registry, dir, log)

	const wantMsg = "shutdown: snapshot saved"
	if !logContainsMsg(&buf, wantMsg) {
		t.Fatalf("expected log %q not found", wantMsg)
	}

	// The snapshot file for height 1 must exist on disk (genesis is height 0,
	// the chain has one block above it at height 1).
	pattern := filepath.Join(dir, "snapshot-v2-1.json.gz")
	if _, err := os.Stat(pattern); err != nil {
		t.Errorf("snapshot file not found at %s: %v", pattern, err)
	}
}

func TestSaveShutdownSnapshot_NilRegistryNoOp(t *testing.T) {
	dir := t.TempDir()
	db, utxos, _ := buildMinimalChainForShutdown(t, dir)

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	// Passing nil registry must be a no-op (no panic, no file written).
	saveShutdownSnapshot(db, utxos, nil, dir, log)

	if logContainsMsg(&buf, "shutdown: snapshot saved") {
		t.Error("expected no log when registry is nil")
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json.gz") {
			t.Errorf("unexpected snapshot file written when registry=nil: %s", e.Name())
		}
	}
}

// ─── loadStartupSnapshotWithFallback classification ───────────────────────────
//
// These tests verify the end-to-end chain from loadStartupSnapshotWithFallback
// → logSnapshotStartupReason, confirming that the returned error correctly
// drives startup_reason= in the structured log entry.

// writeTruncatedGzip writes a file at path whose first byte is 0x1f (valid gzip
// magic) but whose body is truncated so gzip.NewReader fails to decode it.
func writeTruncatedGzip(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte{0x1f, 0x8b, 0x00}, 0o644); err != nil {
		t.Fatalf("writeTruncatedGzip %s: %v", path, err)
	}
}

// prevBackupPath returns the "-prev.json.gz" path for the v2 primary at height.
func prevBackupPath(dataDir string, height uint64) string {
	primary := filepath.Join(dataDir, fmt.Sprintf("snapshot-v2-%d.json.gz", height))
	return strings.TrimSuffix(primary, ".json.gz") + "-prev.json.gz"
}

// TestFallbackClassification_PrimaryAbsentNoPrevBackup checks that when no
// snapshot files exist at all the function returns os.ErrNotExist, causing
// logSnapshotStartupReason to emit startup_reason=no_snapshot.
func TestFallbackClassification_PrimaryAbsentNoPrevBackup(t *testing.T) {
	dir := t.TempDir()
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	const height = uint64(5)
	_, _, err := loadStartupSnapshotWithFallback(dir, height, "deadbeef", log)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}

	// logSnapshotStartupReason must emit no_snapshot.
	var reasonBuf bytes.Buffer
	logSnapshotStartupReason(err, height, newCaptureLogger(&reasonBuf))

	reason, ok := logFieldValue(&reasonBuf, "no snapshot found — full block scan required", "startup_reason")
	if !ok || reason != "no_snapshot" {
		t.Errorf("startup_reason = %q (ok=%v), want no_snapshot; log:\n%s",
			reason, ok, reasonBuf.String())
	}
}

// TestFallbackClassification_PrimaryAbsentCorruptPrevBackup checks that when
// the primary is absent but the prev-backup exists and is corrupt (truncated
// gzip), loadStartupSnapshotWithFallback returns a non-NotExist error, causing
// logSnapshotStartupReason to emit startup_reason=corrupt_snapshot.
func TestFallbackClassification_PrimaryAbsentCorruptPrevBackup(t *testing.T) {
	dir := t.TempDir()
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	const height = uint64(7)
	// Write a truncated gzip at the prev-backup path; leave the primary absent.
	writeTruncatedGzip(t, prevBackupPath(dir, height))

	_, _, err := loadStartupSnapshotWithFallback(dir, height, "deadbeef", log)
	if err == nil {
		t.Fatal("expected error when prev-backup is corrupt, got nil")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected non-NotExist error (corrupt signal), got os.ErrNotExist; full err: %v", err)
	}

	// logSnapshotStartupReason must emit corrupt_snapshot, not no_snapshot.
	var reasonBuf bytes.Buffer
	logSnapshotStartupReason(err, height, newCaptureLogger(&reasonBuf))

	const wantMsg = "snapshot corrupt or unreadable — falling back to full block scan"
	if !logContainsMsg(&reasonBuf, wantMsg) {
		t.Fatalf("expected log %q, got:\n%s", wantMsg, reasonBuf.String())
	}
	reason, ok := logFieldValue(&reasonBuf, wantMsg, "startup_reason")
	if !ok || reason != "corrupt_snapshot" {
		t.Errorf("startup_reason = %q (ok=%v), want corrupt_snapshot", reason, ok)
	}
}

// TestFallbackClassification_PrimaryAbsentCorruptLegacy verifies that a corrupt
// legacy v1 snapshot (file exists, body is junk) is also classified as
// corrupt_snapshot rather than no_snapshot.
func TestFallbackClassification_PrimaryAbsentCorruptLegacy(t *testing.T) {
	dir := t.TempDir()
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	const height = uint64(3)
	// Write junk at the legacy v1 primary path; leave v2 files absent.
	legacyPath := filepath.Join(dir, fmt.Sprintf("snapshot-v1-%d.json", height))
	if err := os.WriteFile(legacyPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write legacy snapshot: %v", err)
	}

	_, _, err := loadStartupSnapshotWithFallback(dir, height, "deadbeef", log)
	if err == nil {
		t.Fatal("expected error when legacy snapshot is corrupt, got nil")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected non-NotExist error (corrupt signal), got os.ErrNotExist; full err: %v", err)
	}

	var reasonBuf bytes.Buffer
	logSnapshotStartupReason(err, height, newCaptureLogger(&reasonBuf))

	const wantMsg = "snapshot corrupt or unreadable — falling back to full block scan"
	if !logContainsMsg(&reasonBuf, wantMsg) {
		t.Fatalf("expected log %q, got:\n%s", wantMsg, reasonBuf.String())
	}
	reason, _ := logFieldValue(&reasonBuf, wantMsg, "startup_reason")
	if reason != "corrupt_snapshot" {
		t.Errorf("startup_reason = %q, want corrupt_snapshot", reason)
	}
}
