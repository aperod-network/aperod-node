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
