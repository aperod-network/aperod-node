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

	saveShutdownSnapshot(db, utxos, registry, dir, log, nil)

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

	saveShutdownSnapshot(db, utxos, registry, dir, log, nil)

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

// TestSaveShutdownSnapshot_RatioPctFiresAfterRealWrite is an end-to-end test that
// exercises the full path from saveShutdownSnapshotWithPaths → real snapshot
// serialisation → warnIfSnapshotSlowRelativeToTimeout → ratio_pct log field.
//
// It uses a synthetic drop-in with a short TimeoutStopSec (100 ms) together with
// a saveDurOverride (500 ms) so that the ratio is deterministically 500 %, well
// above the 80 % Error threshold.  The test also populates a configurable number
// of UTXOs (utxoCount) to exercise realistic snapshot serialisation — a future
// format change that inflates the snapshot schema will still exercise the real
// write path even though the ratio check uses the override.
func TestSaveShutdownSnapshot_RatioPctFiresAfterRealWrite(t *testing.T) {
	const utxoCount = 200 // realistic but fast; increase if format grows significantly

	dir := t.TempDir()
	db, utxos, registry := buildMinimalChainForShutdown(t, dir)

	// Populate synthetic UTXOs so the snapshot serialisation covers a
	// realistic payload.  The fields are zero-valued except for BlockHeight and
	// OutputIndex to keep each UTXO key unique.
	for i := 0; i < utxoCount; i++ {
		var txHash crypto.Hash32
		txHash[0] = byte(i)
		txHash[1] = byte(i >> 8)
		utxos.Add(&core.UTXO{
			TxHash:      txHash,
			OutputIndex: uint32(i),
			BlockHeight: 1,
		})
	}

	// Write a synthetic drop-in with a very short TimeoutStopSec so the ratio
	// check is triggered without depending on wall-clock write speed.
	dropinDir := t.TempDir()
	const timeoutConf = "[Service]\nTimeoutStopSec=100ms\n"
	if err := os.WriteFile(filepath.Join(dropinDir, "timeout.conf"), []byte(timeoutConf), 0o644); err != nil {
		t.Fatalf("write synthetic timeout.conf: %v", err)
	}
	servicePath := filepath.Join(t.TempDir(), "aperod-node.service") // intentionally absent

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	// saveDurOverride = 500 ms → ratio = 500 ms / 100 ms = 5.0 → 500 % > 80 %.
	// The Error-level message must be emitted with a ratio_pct field.
	const syntheticDur = 500 * time.Millisecond
	saveShutdownSnapshotWithPaths(db, utxos, registry, dir, log, nil, dropinDir, servicePath, syntheticDur)

	// The snapshot itself must have been written successfully.
	const wantSavedMsg = "shutdown: snapshot saved"
	if !logContainsMsg(&buf, wantSavedMsg) {
		t.Fatalf("expected log %q not found; full output:\n%s", wantSavedMsg, buf.String())
	}

	// The ratio_pct field must appear on the Error-level warning message.
	// 500 ms / 100 ms = 5.0 → ratio_pct must be exactly "500%".
	const wantWarnMsg = "snapshot save time is dangerously close to TimeoutStopSec — increase it immediately to avoid losing the snapshot on next shutdown"
	if !logContainsMsg(&buf, wantWarnMsg) {
		t.Fatalf("expected Error-level ratio warning %q not found; full output:\n%s", wantWarnMsg, buf.String())
	}

	// Verify the record is logged at ERROR level (not Warn or Info).
	level, hasLevel := logFieldValue(&buf, wantWarnMsg, "level")
	if !hasLevel {
		t.Fatalf("level field missing from ratio warning record; full output:\n%s", buf.String())
	}
	if level != "ERROR" {
		t.Errorf("expected log level ERROR for ratio warning, got %q", level)
	}

	// Verify ratio_pct is exactly "500%" — deterministic given 500 ms / 100 ms = 5.0×.
	pct, ok := logFieldValue(&buf, wantWarnMsg, "ratio_pct")
	if !ok || pct == "" {
		t.Fatalf("ratio_pct field missing from ratio warning; full output:\n%s", buf.String())
	}
	if pct != "500%" {
		t.Errorf("ratio_pct = %q, want \"500%%\" (500 ms / 100 ms = 5.0×)", pct)
	}
}

func TestSaveShutdownSnapshot_NilRegistryNoOp(t *testing.T) {
	dir := t.TempDir()
	db, utxos, _ := buildMinimalChainForShutdown(t, dir)

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	// Passing nil registry must be a no-op (no panic, no file written).
	saveShutdownSnapshot(db, utxos, nil, dir, log, nil)

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

// ─── parseSystemdDuration ────────────────────────────────────────────────────

func TestParseSystemdDuration_Plain(t *testing.T) {
	tests := []struct {
		input   string
		wantSec float64
		wantOK  bool
	}{
		{"900", 900, true},
		{"240", 240, true},
		{"0", 0, true},
		{"infinity", 1e18, true},
		{"INFINITY", 1e18, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseSystemdDuration(tc.input)
		if ok != tc.wantOK {
			t.Errorf("parseSystemdDuration(%q) ok=%v, want %v", tc.input, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.wantSec {
			t.Errorf("parseSystemdDuration(%q) = %v, want %v", tc.input, got, tc.wantSec)
		}
	}
}

func TestParseSystemdDuration_Suffixes(t *testing.T) {
	tests := []struct {
		input   string
		wantSec float64
	}{
		{"900s", 900},
		{"15min", 900},
		{"1h", 3600},
		{"1d", 86400},
		{"1w", 604800},
		{"500ms", 0.5},
	}
	for _, tc := range tests {
		got, ok := parseSystemdDuration(tc.input)
		if !ok {
			t.Errorf("parseSystemdDuration(%q): expected ok=true", tc.input)
			continue
		}
		if got != tc.wantSec {
			t.Errorf("parseSystemdDuration(%q) = %v, want %v", tc.input, got, tc.wantSec)
		}
	}
}

// ─── readEffectiveTimeoutStopSec ─────────────────────────────────────────────
//
// NOTE: readEffectiveTimeoutStopSec now accepts a DIRECTORY path (dropinDir)
// and scans all *.conf files in lex order.  Tests write the conf file into a
// temp dir and pass the directory, not the individual file path.

func TestReadEffectiveTimeoutStopSec_DropinWins(t *testing.T) {
	// Two .conf files in the same dir; later lex name takes precedence.
	dropinDir := t.TempDir()
	service := filepath.Join(t.TempDir(), "aperod-node.service")
	// "a.conf" is scanned first, then "z.conf" — last value wins.
	if err := os.WriteFile(filepath.Join(dropinDir, "a.conf"), []byte("[Service]\nTimeoutStopSec=60\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropinDir, "z.conf"), []byte("[Service]\nTimeoutStopSec=300\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service, []byte("[Service]\nTimeoutStopSec=999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found := readEffectiveTimeoutStopSec(dropinDir, service)
	if !found {
		t.Fatal("expected found=true")
	}
	// z.conf (300) wins over a.conf (60); service (999) never consulted.
	if got != 300 {
		t.Errorf("got %v, want 300 (last lex .conf should win)", got)
	}
}

func TestReadEffectiveTimeoutStopSec_FallsBackToService(t *testing.T) {
	dropinDir := t.TempDir() // empty — no .conf files
	serviceDir := t.TempDir()
	service := filepath.Join(serviceDir, "aperod-node.service")
	if err := os.WriteFile(service, []byte("[Service]\nTimeoutStopSec=180\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found := readEffectiveTimeoutStopSec(dropinDir, service)
	if !found {
		t.Fatal("expected found=true")
	}
	if got != 180 {
		t.Errorf("got %v, want 180", got)
	}
}

func TestReadEffectiveTimeoutStopSec_NeitherPresent(t *testing.T) {
	dropinDir := t.TempDir() // empty
	service := filepath.Join(t.TempDir(), "aperod-node.service") // absent

	got, found := readEffectiveTimeoutStopSec(dropinDir, service)
	if found {
		t.Errorf("expected found=false when neither dir nor service has TimeoutStopSec, got secs=%v", got)
	}
}

func TestReadEffectiveTimeoutStopSec_Infinity(t *testing.T) {
	dropinDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dropinDir, "timeout.conf"), []byte("[Service]\nTimeoutStopSec=infinity\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found := readEffectiveTimeoutStopSec(dropinDir, "")
	if !found {
		t.Fatal("expected found=true for infinity")
	}
	if got < 1e15 {
		t.Errorf("infinity should map to a very large value, got %v", got)
	}
}

func TestReadEffectiveTimeoutStopSec_SuffixedValue(t *testing.T) {
	// Verify that systemd duration suffixes (900s, 15min) are parsed correctly.
	dropinDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dropinDir, "timeout.conf"), []byte("[Service]\nTimeoutStopSec=15min\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found := readEffectiveTimeoutStopSec(dropinDir, "")
	if !found {
		t.Fatal("expected found=true")
	}
	if got != 900 {
		t.Errorf("15min should parse to 900 s, got %v", got)
	}
}

func TestReadEffectiveTimeoutStopSec_NoTimeoutLine(t *testing.T) {
	dropinDir := t.TempDir()
	serviceDir := t.TempDir()
	service := filepath.Join(serviceDir, "aperod-node.service")
	// Files exist but contain no TimeoutStopSec= line.
	if err := os.WriteFile(filepath.Join(dropinDir, "gomemlimit.conf"), []byte("[Service]\nEnvironment=GOMEMLIMIT=5000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service, []byte("[Service]\nRestartSec=10\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found := readEffectiveTimeoutStopSec(dropinDir, service)
	if found {
		t.Errorf("expected found=false when no TimeoutStopSec line is present, got secs=%v", got)
	}
}

// TestReadEffectiveTimeoutStopSec_LastAssignmentWinsInFile verifies that when
// a single drop-in .conf contains TimeoutStopSec more than once the LAST
// assignment is returned (matching systemd's reassignment semantics).
func TestReadEffectiveTimeoutStopSec_LastAssignmentWinsInFile(t *testing.T) {
	dropinDir := t.TempDir()
	// First assignment: 60 s; second (override) assignment: 900 s.
	content := "[Service]\nTimeoutStopSec=60\nRestartSec=5\nTimeoutStopSec=900\n"
	if err := os.WriteFile(filepath.Join(dropinDir, "timeout.conf"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found := readEffectiveTimeoutStopSec(dropinDir, "")
	if !found {
		t.Fatal("expected found=true")
	}
	if got != 900 {
		t.Errorf("got %v, want 900 (last assignment in file must win)", got)
	}
}

// TestReadEffectiveTimeoutStopSec_LastAssignmentWinsInServiceFile verifies the
// same last-assignment semantics for the fallback service file path.
func TestReadEffectiveTimeoutStopSec_LastAssignmentWinsInServiceFile(t *testing.T) {
	dropinDir := t.TempDir() // empty — no .conf files
	serviceDir := t.TempDir()
	service := filepath.Join(serviceDir, "aperod-node.service")
	// Earlier line says 60 s; later line overrides to 300 s.
	content := "[Service]\nTimeoutStopSec=60\nTimeoutStopSec=300\n"
	if err := os.WriteFile(service, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found := readEffectiveTimeoutStopSec(dropinDir, service)
	if !found {
		t.Fatal("expected found=true")
	}
	if got != 300 {
		t.Errorf("got %v, want 300 (last assignment in service file must win)", got)
	}
}

// TestReadEffectiveTimeoutStopSec_WhitespaceAroundEquals verifies that
// whitespace around the '=' is accepted (systemd's parser is lenient).
func TestReadEffectiveTimeoutStopSec_WhitespaceAroundEquals(t *testing.T) {
	dropinDir := t.TempDir()
	content := "[Service]\nTimeoutStopSec = 450\n"
	if err := os.WriteFile(filepath.Join(dropinDir, "timeout.conf"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found := readEffectiveTimeoutStopSec(dropinDir, "")
	if !found {
		t.Fatal("expected found=true with whitespace around =")
	}
	if got != 450 {
		t.Errorf("got %v, want 450", got)
	}
}

// TestReadEffectiveTimeoutStopSec_MultipleConfs verifies that when both
// gomemlimit.conf and timeout.conf exist (the production layout), the value
// from timeout.conf (lexically last) is returned.
func TestReadEffectiveTimeoutStopSec_MultipleConfs(t *testing.T) {
	dropinDir := t.TempDir()
	// gomemlimit.conf has no TimeoutStopSec (matches real production layout).
	if err := os.WriteFile(filepath.Join(dropinDir, "gomemlimit.conf"), []byte("[Service]\nEnvironment=GOMEMLIMIT=5905580032\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// timeout.conf has TimeoutStopSec=900 (lexically after gomemlimit.conf).
	if err := os.WriteFile(filepath.Join(dropinDir, "timeout.conf"), []byte("[Service]\nTimeoutStopSec=900\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, found := readEffectiveTimeoutStopSec(dropinDir, "")
	if !found {
		t.Fatal("expected found=true")
	}
	if got != 900 {
		t.Errorf("got %v, want 900", got)
	}
}

// ─── warnIfSnapshotSlowRelativeToTimeout ─────────────────────────────────────

// writeTimeoutConfInDir writes a minimal systemd drop-in file with the given
// TimeoutStopSec value to dir/timeout.conf and returns (dropinDir, servicePath).
// The returned dropinDir is the directory (not the file path) — matching the
// new readEffectiveTimeoutStopSec interface.
func writeTimeoutConfInDir(t *testing.T, secs int) (dropinDir, service string) {
	t.Helper()
	dropinDir = t.TempDir()
	content := fmt.Sprintf("[Service]\nTimeoutStopSec=%d\n", secs)
	if err := os.WriteFile(filepath.Join(dropinDir, "timeout.conf"), []byte(content), 0o644); err != nil {
		t.Fatalf("write timeout.conf: %v", err)
	}
	service = filepath.Join(t.TempDir(), "aperod-node.service") // absent
	return dropinDir, service
}

func TestWarnIfSnapshotSlow_BelowThreshold(t *testing.T) {
	dropinDir, service := writeTimeoutConfInDir(t, 300) // 300 s timeout

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	// 30 s save out of 300 s = 10 % — well below 50 %; no message expected.
	warnIfSnapshotSlowRelativeToTimeout(30*time.Second, dropinDir, service, log)

	if buf.Len() != 0 {
		t.Errorf("expected no log output for 10%% ratio, got:\n%s", buf.String())
	}
}

func TestWarnIfSnapshotSlow_WarnThreshold(t *testing.T) {
	dropinDir, service := writeTimeoutConfInDir(t, 200) // 200 s timeout

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	// 110 s save out of 200 s = 55 % — above 50 %; Warn expected.
	warnIfSnapshotSlowRelativeToTimeout(110*time.Second, dropinDir, service, log)

	const wantMsg = "snapshot save time is approaching TimeoutStopSec — consider increasing it before it causes a missed snapshot"
	if !logContainsMsg(&buf, wantMsg) {
		t.Errorf("expected Warn log at 55%% ratio, got:\n%s", buf.String())
	}
	// Must NOT be the Error-level message.
	const errMsg = "snapshot save time is dangerously close to TimeoutStopSec"
	if logContainsMsg(&buf, errMsg) {
		t.Errorf("unexpected Error log at 55%% ratio:\n%s", buf.String())
	}
	// ratio_pct field must be present.
	pct, ok := logFieldValue(&buf, wantMsg, "ratio_pct")
	if !ok || pct == "" {
		t.Errorf("ratio_pct field missing; log:\n%s", buf.String())
	}
}

func TestWarnIfSnapshotSlow_ErrorThreshold(t *testing.T) {
	dropinDir, service := writeTimeoutConfInDir(t, 100) // 100 s timeout

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	// 85 s save out of 100 s = 85 % — above 80 %; Error expected.
	warnIfSnapshotSlowRelativeToTimeout(85*time.Second, dropinDir, service, log)

	const wantMsg = "snapshot save time is dangerously close to TimeoutStopSec — increase it immediately to avoid losing the snapshot on next shutdown"
	if !logContainsMsg(&buf, wantMsg) {
		t.Errorf("expected Error log at 85%% ratio, got:\n%s", buf.String())
	}
	pct, ok := logFieldValue(&buf, wantMsg, "ratio_pct")
	if !ok || pct == "" {
		t.Errorf("ratio_pct field missing; log:\n%s", buf.String())
	}
}

func TestWarnIfSnapshotSlow_SuffixedTimeout(t *testing.T) {
	// 15min = 900 s; a 600 s save = 67 % → Warn.
	dropinDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dropinDir, "timeout.conf"), []byte("[Service]\nTimeoutStopSec=15min\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := filepath.Join(t.TempDir(), "aperod-node.service") // absent

	var buf bytes.Buffer
	warnIfSnapshotSlowRelativeToTimeout(600*time.Second, dropinDir, service, newCaptureLogger(&buf))

	const wantMsg = "snapshot save time is approaching TimeoutStopSec — consider increasing it before it causes a missed snapshot"
	if !logContainsMsg(&buf, wantMsg) {
		t.Errorf("expected Warn for 600s/15min (67%%), got:\n%s", buf.String())
	}
}

func TestWarnIfSnapshotSlow_NoConfigNoOp(t *testing.T) {
	dropinDir := t.TempDir() // empty — no .conf files
	service := filepath.Join(t.TempDir(), "aperod-node.service") // absent

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	// Even if the save takes a very long time, without a timeout config the
	// function must be a no-op (non-systemd host).
	warnIfSnapshotSlowRelativeToTimeout(10000*time.Second, dropinDir, service, log)

	if buf.Len() != 0 {
		t.Errorf("expected no log output when no config files exist, got:\n%s", buf.String())
	}
}

func TestWarnIfSnapshotSlow_InfinityNoOp(t *testing.T) {
	dropinDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dropinDir, "timeout.conf"), []byte("[Service]\nTimeoutStopSec=infinity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := filepath.Join(t.TempDir(), "aperod-node.service") // absent

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	// infinity timeout → ratio is effectively 0 → no log.
	warnIfSnapshotSlowRelativeToTimeout(999*time.Second, dropinDir, service, log)

	if buf.Len() != 0 {
		t.Errorf("expected no log output for infinity timeout, got:\n%s", buf.String())
	}
}

// ─── run() wiring tests ───────────────────────────────────────────────────────
//
// The tests below exercise tryLoadStartupSnapshot — the production helper that
// run() calls (main.go ~609):
//
//	if snap, snapIsRelaxed, serr := tryLoadStartupSnapshot(cfg.DataDir, tipHeight, tipHashHex, log); serr == nil {
//
// tryLoadStartupSnapshot (snapshot.go) is the single place that combines
// loadStartupSnapshotWithFallback + logSnapshotStartupReason.  Calling it
// directly in tests means that if logSnapshotStartupReason is ever removed
// from tryLoadStartupSnapshot the tests fail — the log line will be absent.
// The tests use buildChainInStore so tipHeight is a real DB-backed value,
// matching the conditions present when run() reaches the snapshot fast-path.

// TestRunWiring_CorruptPrimarySnapshot verifies that when the primary v2 snapshot
// file exists but is corrupt (truncated gzip — the exact state left by a SIGKILL
// arriving mid-write), tryLoadStartupSnapshot emits startup_reason=corrupt_snapshot.
// run() delegates to tryLoadStartupSnapshot, so this test covers the production
// wiring end-to-end.
func TestRunWiring_CorruptPrimarySnapshot(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 3) // tip at height 3

	tip := blocks[len(blocks)-1]
	tipHash := tip.Hash()
	tipHeight := tip.Header.Height
	tipHashHex := fmt.Sprintf("%x", tipHash[:])

	// Write a truncated gzip at the primary snapshot path — simulates a SIGKILL
	// arriving mid-write during saveStartupSnapshot (task #1464 scenario).
	primaryPath := filepath.Join(dir, fmt.Sprintf("snapshot-v%d-%d.json.gz", snapVersion, tipHeight))
	writeTruncatedGzip(t, primaryPath)

	// ── Call the production helper that run() calls. ──────────────────────────
	// tryLoadStartupSnapshot wraps loadStartupSnapshotWithFallback and calls
	// logSnapshotStartupReason internally on error — this is the seam under test.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	_, _, serr := tryLoadStartupSnapshot(dir, tipHeight, tipHashHex, log)
	if serr == nil {
		t.Fatal("expected error for corrupt primary snapshot, got nil")
	}

	// ── Assertions. ──────────────────────────────────────────────────────────
	const wantMsg = "snapshot corrupt or unreadable — falling back to full block scan"
	if !logContainsMsg(&logBuf, wantMsg) {
		t.Fatalf("expected log %q not found;\nlog:\n%s", wantMsg, logBuf.String())
	}
	reason, ok := logFieldValue(&logBuf, wantMsg, "startup_reason")
	if !ok {
		t.Fatalf("startup_reason field missing in log; full log:\n%s", logBuf.String())
	}
	if reason != "corrupt_snapshot" {
		t.Errorf("startup_reason = %q, want %q", reason, "corrupt_snapshot")
	}
}

// TestRunWiring_NoSnapshot verifies that when no snapshot file exists at all
// (first start, or after a clean data-dir wipe), tryLoadStartupSnapshot emits
// startup_reason=no_snapshot.  run() delegates to tryLoadStartupSnapshot, so
// this test covers the production wiring end-to-end.
func TestRunWiring_NoSnapshot(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 2) // tip at height 2, no snapshot written

	tip := blocks[len(blocks)-1]
	tipHash := tip.Hash()
	tipHeight := tip.Header.Height
	tipHashHex := fmt.Sprintf("%x", tipHash[:])

	// ── Call the production helper that run() calls. ──────────────────────────
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	_, _, serr := tryLoadStartupSnapshot(dir, tipHeight, tipHashHex, log)
	if serr == nil {
		t.Fatal("expected error when no snapshot exists, got nil")
	}

	// ── Assertions. ──────────────────────────────────────────────────────────
	const wantMsg = "no snapshot found — full block scan required"
	if !logContainsMsg(&logBuf, wantMsg) {
		t.Fatalf("expected log %q not found;\nlog:\n%s", wantMsg, logBuf.String())
	}
	reason, ok := logFieldValue(&logBuf, wantMsg, "startup_reason")
	if !ok {
		t.Fatalf("startup_reason field missing in log; full log:\n%s", logBuf.String())
	}
	if reason != "no_snapshot" {
		t.Errorf("startup_reason = %q, want %q", reason, "no_snapshot")
	}
}

// ─── TestFallbackClassification_PrimaryAbsentCorruptLegacy ───────────────────

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

// ─── Silent on-disk snapshot replacement (task: stale/foreign snapshot) ───────
//
// A snapshot file can be silently replaced on disk by an older backup or a
// partial rsync.  Two distinct modes must both be rejected and classified via
// logSnapshotStartupReason using the same wiring as the corrupt-primary tests:
//
//   A. The file at the CURRENT tip's path contains a structurally valid
//      gzip+JSON snapshot from a DIFFERENT tip height (file + checksum sidecar
//      copied together, so the checksum passes) → TipHeight validation fails
//      → non-NotExist error → startup_reason=corrupt_snapshot.
//
//   B. The file at the current tip's path has the right height but a WRONG
//      TipHashHex (snapshot from a different chain / pre-reorg backup)
//      → hash validation fails → startup_reason=corrupt_snapshot.
//
//   C. A valid snapshot exists only at the OLD height path after the DB tip
//      advanced → nothing found at the new height → os.ErrNotExist →
//      startup_reason=no_snapshot (the stale file is ignored, never loaded).

// saveSnapAtHeight saves a minimal but valid v2 snapshot (with checksum
// sidecar) at the given height/hash via the production save path.
func saveSnapAtHeight(t *testing.T, dir string, height uint64, hashHex string) {
	t.Helper()
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  height,
		TipHashHex: hashHex,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot(h=%d): %v", height, err)
	}
}

// assertStartupReasonInBuf asserts that the captured logger buffer — filled by
// tryLoadStartupSnapshot itself, the exact helper run() calls — contains the
// structured startup_reason entry with the wanted value ("corrupt_snapshot" or
// "no_snapshot").  Because the assertion reads the SAME buffer the production
// helper wrote to, these tests fail if tryLoadStartupSnapshot ever stops
// calling logSnapshotStartupReason on a load failure.
func assertStartupReasonInBuf(t *testing.T, buf *bytes.Buffer, want string) {
	t.Helper()
	wantMsg := "snapshot corrupt or unreadable — falling back to full block scan"
	if want == "no_snapshot" {
		wantMsg = "no snapshot found — full block scan required"
	}
	if !logContainsMsg(buf, wantMsg) {
		t.Fatalf("expected log %q emitted by tryLoadStartupSnapshot, got:\n%s", wantMsg, buf.String())
	}
	reason, ok := logFieldValue(buf, wantMsg, "startup_reason")
	if !ok || reason != want {
		t.Errorf("startup_reason = %q (ok=%v), want %q", reason, ok, want)
	}
}

// TestSilentReplacement_WrongHeightContentAtTipPath covers mode A: the primary
// at the current tip's path is a fully valid gzip+JSON snapshot — checksum
// sidecar and all — but its content is from an older tip height (e.g. an old
// backup rsync'd over the current file, sidecar included).  The height check
// must reject it with a non-NotExist error so the operator sees
// startup_reason=corrupt_snapshot, not a silent wrong-state fast path.
func TestSilentReplacement_WrongHeightContentAtTipPath(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	const oldHeight, newHeight = uint64(5), uint64(6)
	const newHash = "beefbeef"

	// A valid old snapshot exists at height 5.
	saveSnapAtHeight(t, dir, oldHeight, "cafecafe")

	// Simulate the partial-rsync replacement: the height-5 file AND its
	// checksum sidecar are copied over the height-6 primary path, so the
	// checksum verification passes and only the height check can catch it.
	oldPath := snapshotPath(dir, oldHeight)
	newPath := snapshotPath(dir, newHeight)
	if err := copyFile(oldPath, newPath); err != nil {
		t.Fatalf("copy snapshot file: %v", err)
	}
	if err := copyFile(snapshotChecksumPath(oldPath), snapshotChecksumPath(newPath)); err != nil {
		t.Fatalf("copy checksum sidecar: %v", err)
	}

	// Production step: run() calls tryLoadStartupSnapshot, which both loads
	// (with fallbacks) AND emits the startup_reason entry on failure.
	_, _, err := tryLoadStartupSnapshot(dir, newHeight, newHash, log)
	if err == nil {
		t.Fatal("stale snapshot content at the current tip path was ACCEPTED — height check bypassed")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected non-NotExist error (corrupt signal), got os.ErrNotExist; full err: %v", err)
	}
	if !strings.Contains(err.Error(), "height mismatch") {
		t.Errorf("expected a height-mismatch rejection, got: %v", err)
	}

	assertStartupReasonInBuf(t, &buf, "corrupt_snapshot")
}

// TestSilentReplacement_WrongHashSameHeight covers mode B: the primary at the
// current tip path has the correct height but a different TipHashHex (backup
// from a different chain state).  The hash check must reject it and the error
// must classify as corrupt_snapshot.
func TestSilentReplacement_WrongHashSameHeight(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	const height = uint64(9)

	// The file on disk claims hash "aaaa1111", but the DB tip hash is different.
	saveSnapAtHeight(t, dir, height, "aaaa1111")

	_, _, err := tryLoadStartupSnapshot(dir, height, "bbbb2222", log)
	if err == nil {
		t.Fatal("snapshot with mismatched tip hash was ACCEPTED — hash check bypassed")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected non-NotExist error, got os.ErrNotExist; full err: %v", err)
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("expected a hash-mismatch rejection, got: %v", err)
	}

	assertStartupReasonInBuf(t, &buf, "corrupt_snapshot")
}

// TestSilentReplacement_StaleSnapshotAtOldHeightIgnored covers mode C: a valid
// snapshot was saved at height N, the DB tip then advanced to N+1 (e.g. blocks
// accepted after the last checkpoint), and startup looks for a snapshot at the
// NEW height.  The stale height-N file must be ignored entirely — never loaded
// as chain state — and the load must report os.ErrNotExist so the operator
// sees startup_reason=no_snapshot (clean full-scan, not corruption).
func TestSilentReplacement_StaleSnapshotAtOldHeightIgnored(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	const oldHeight, newHeight = uint64(41), uint64(42)

	saveSnapAtHeight(t, dir, oldHeight, "0ddba11c")

	snap, _, err := tryLoadStartupSnapshot(dir, newHeight, "5eaf00d5", log)
	if err == nil {
		t.Fatalf("expected error, got snapshot at height %d (stale file loaded as current state!)", snap.TipHeight)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist (stale file ignored), got: %v", err)
	}

	assertStartupReasonInBuf(t, &buf, "no_snapshot")
}

// ─── checkStartupSnapshotTiming ──────────────────────────────────────────────
//
// checkStartupSnapshotTiming is the proactive startup warning that compares the
// persisted last_snap_save_ms against the effective systemd TimeoutStopSec and
// emits a structured log entry when the ratio crosses 50 % (Warn) or 80 %
// (Error).  The tests below verify:
//
//  1. The full DB round-trip path: StoreSnapshotSaveDuration → LoadSnapshotSaveDuration
//     → checkStartupSnapshotTiming emits ratio_pct in the log.
//  2. Error-level warning fires when savedSnapMs > 80 % of TimeoutStopSec.
//  3. Warn-level warning fires when savedSnapMs > 50 % of TimeoutStopSec.
//  4. No log is emitted when the ratio is below both thresholds.
//  5. The function is a no-op when no TimeoutStopSec config file is found.

// openTempDB creates a real LevelDB store in a temporary directory and
// registers a Cleanup to close it.  It is used by the
// checkStartupSnapshotTiming tests so they exercise the actual DB code path
// rather than passing a synthetic int64 directly.
func openTempDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("openTempDB: store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestCheckStartupSnapshotTiming_DBRoundtrip_ErrorThreshold is the primary
// integration test for the proactive startup warning path.  It verifies the
// full pipeline:
//
//  1. db.StoreSnapshotSaveDuration persists a synthetic save duration.
//  2. db.LoadSnapshotSaveDuration retrieves it successfully (found=true).
//  3. checkStartupSnapshotTiming emits an Error-level log entry with a
//     ratio_pct field when the stored duration exceeds 80 % of the
//     TimeoutStopSec read from an injectable drop-in directory.
//
// Thresholds used: TimeoutStopSec=100 s, savedSnapMs=90 000 ms (90 s) →
// ratio = 90 % > 80 % → Error-level "SIGKILL risk" message.
func TestCheckStartupSnapshotTiming_DBRoundtrip_ErrorThreshold(t *testing.T) {
	db := openTempDB(t)

	// 90 seconds expressed in milliseconds — 90 % of a 100 s timeout.
	const saveMs = int64(90_000)
	if err := db.StoreSnapshotSaveDuration(saveMs); err != nil {
		t.Fatalf("StoreSnapshotSaveDuration(%d): %v", saveMs, err)
	}

	// Verify the round-trip: the value must be readable on the same DB handle.
	got, found, err := db.LoadSnapshotSaveDuration()
	if err != nil {
		t.Fatalf("LoadSnapshotSaveDuration: %v", err)
	}
	if !found {
		t.Fatal("LoadSnapshotSaveDuration: found=false immediately after store")
	}
	if got != saveMs {
		t.Fatalf("LoadSnapshotSaveDuration = %d, want %d", got, saveMs)
	}

	// Write a synthetic drop-in with TimeoutStopSec=100 s so ratio = 90 %.
	dropinDir, service := writeTimeoutConfInDir(t, 100)

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	checkStartupSnapshotTiming(got, dropinDir, service, log)

	const wantMsg = "startup: last snapshot save duration exceeds 80% of systemd stop timeout — SIGKILL risk on next shutdown"
	if !logContainsMsg(&buf, wantMsg) {
		t.Fatalf("expected Error-level log %q not found; full output:\n%s", wantMsg, buf.String())
	}

	// Log level must be ERROR.
	level, hasLevel := logFieldValue(&buf, wantMsg, "level")
	if !hasLevel {
		t.Fatalf("level field missing from ratio warning record; full output:\n%s", buf.String())
	}
	if level != "ERROR" {
		t.Errorf("expected log level ERROR for 90%% ratio, got %q", level)
	}

	// ratio_pct must be present.
	pct, ok := logFieldValue(&buf, wantMsg, "ratio_pct")
	if !ok || pct == "" {
		t.Fatalf("ratio_pct field missing from Error-level warning; full output:\n%s", buf.String())
	}
}

// TestCheckStartupSnapshotTiming_DBRoundtrip_WarnThreshold verifies that the
// Warn-level message fires (not Error) when the stored duration is between
// 50 % and 80 % of TimeoutStopSec.
//
// Thresholds: TimeoutStopSec=200 s, savedSnapMs=120 000 ms (120 s) →
// ratio = 60 % → Warn, not Error.
func TestCheckStartupSnapshotTiming_DBRoundtrip_WarnThreshold(t *testing.T) {
	db := openTempDB(t)

	const saveMs = int64(120_000) // 120 s = 60 % of 200 s
	if err := db.StoreSnapshotSaveDuration(saveMs); err != nil {
		t.Fatalf("StoreSnapshotSaveDuration(%d): %v", saveMs, err)
	}
	got, found, err := db.LoadSnapshotSaveDuration()
	if err != nil || !found {
		t.Fatalf("LoadSnapshotSaveDuration: err=%v found=%v", err, found)
	}

	dropinDir, service := writeTimeoutConfInDir(t, 200)

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	checkStartupSnapshotTiming(got, dropinDir, service, log)

	const wantWarn = "startup: last snapshot save duration exceeds 50% of systemd stop timeout — consider increasing TimeoutStopSec"
	if !logContainsMsg(&buf, wantWarn) {
		t.Fatalf("expected Warn-level log %q not found; full output:\n%s", wantWarn, buf.String())
	}

	// Must NOT escalate to Error.
	const errMsg = "SIGKILL risk"
	if logContainsMsg(&buf, errMsg) {
		t.Errorf("unexpected Error-level message at 60%% ratio; full output:\n%s", buf.String())
	}

	// ratio_pct must be present.
	pct, ok := logFieldValue(&buf, wantWarn, "ratio_pct")
	if !ok || pct == "" {
		t.Fatalf("ratio_pct field missing from Warn-level warning; full output:\n%s", buf.String())
	}
}

// TestCheckStartupSnapshotTiming_BelowThreshold verifies that no log entry is
// emitted when the stored duration is below the 50 % warn threshold.
//
// Thresholds: TimeoutStopSec=300 s, savedSnapMs=30 000 ms (30 s) →
// ratio = 10 % → no log expected.
func TestCheckStartupSnapshotTiming_BelowThreshold(t *testing.T) {
	db := openTempDB(t)

	const saveMs = int64(30_000) // 30 s = 10 % of 300 s
	if err := db.StoreSnapshotSaveDuration(saveMs); err != nil {
		t.Fatalf("StoreSnapshotSaveDuration(%d): %v", saveMs, err)
	}
	got, found, err := db.LoadSnapshotSaveDuration()
	if err != nil || !found {
		t.Fatalf("LoadSnapshotSaveDuration: err=%v found=%v", err, found)
	}

	dropinDir, service := writeTimeoutConfInDir(t, 300)

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	checkStartupSnapshotTiming(got, dropinDir, service, log)

	if buf.Len() != 0 {
		t.Errorf("expected no log at 10%% ratio, got:\n%s", buf.String())
	}
}

// TestCheckStartupSnapshotTiming_NoTimeoutConfig verifies that
// checkStartupSnapshotTiming is a no-op when no drop-in directory or service
// file contains a TimeoutStopSec line (non-systemd host, or default config).
// Even a very long save duration must produce no log output.
func TestCheckStartupSnapshotTiming_NoTimeoutConfig(t *testing.T) {
	dropinDir := t.TempDir() // empty — no .conf files
	service := filepath.Join(t.TempDir(), "aperod-node.service") // absent

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	// 9999 s would be 100× any reasonable timeout — but without a config
	// the function must be silent.
	checkStartupSnapshotTiming(9_999_000, dropinDir, service, log)

	if buf.Len() != 0 {
		t.Errorf("expected no log when no TimeoutStopSec config exists, got:\n%s", buf.String())
	}
}

// TestCheckStartupSnapshotTiming_DBRoundtrip_PersistAcrossReopen verifies that
// the stored snapshot save duration survives a DB close/reopen — the scenario
// that occurs between two node restarts.  This ensures the proactive warning
// path (which reads from the previous run's DB) actually sees the persisted
// value and is not silently reading zero.
func TestCheckStartupSnapshotTiming_DBRoundtrip_PersistAcrossReopen(t *testing.T) {
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "chain.db")

	// First "run": store the save duration.
	const saveMs = int64(85_000) // 85 s
	db1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open (first): %v", err)
	}
	if err := db1.StoreSnapshotSaveDuration(saveMs); err != nil {
		db1.Close()
		t.Fatalf("StoreSnapshotSaveDuration: %v", err)
	}
	db1.Close()

	// Second "run": reopen and load; must find the stored value.
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open (second): %v", err)
	}
	defer db2.Close()

	got, found, err := db2.LoadSnapshotSaveDuration()
	if err != nil {
		t.Fatalf("LoadSnapshotSaveDuration after reopen: %v", err)
	}
	if !found {
		t.Fatal("LoadSnapshotSaveDuration: found=false after close+reopen — value did not survive restart")
	}
	if got != saveMs {
		t.Fatalf("LoadSnapshotSaveDuration after reopen = %d, want %d", got, saveMs)
	}

	// With TimeoutStopSec=100 s the ratio is 85 % → Error-level warning must fire.
	dropinDir, service := writeTimeoutConfInDir(t, 100)

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	checkStartupSnapshotTiming(got, dropinDir, service, log)

	const wantMsg = "startup: last snapshot save duration exceeds 80% of systemd stop timeout — SIGKILL risk on next shutdown"
	if !logContainsMsg(&buf, wantMsg) {
		t.Fatalf("expected Error log %q after DB reopen, got:\n%s", wantMsg, buf.String())
	}

	pct, ok := logFieldValue(&buf, wantMsg, "ratio_pct")
	if !ok || pct == "" {
		t.Fatalf("ratio_pct missing after DB reopen; full output:\n%s", buf.String())
	}
}
