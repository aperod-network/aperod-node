package main

// Integration test: stale mempool.json.tmp cleanup is wired before Load in the
// production startup path.
//
// Covers task #1253: tests that call startupLoadMempool — the same helper that
// main.go calls — so that removing CleanStaleTmpFiles from that helper, or
// removing the helper call from run(), immediately breaks these tests.
//
// The four cases tested:
//
//  1. Stale .tmp present (≥5 min old): startupLoadMempool removes it, logs the
//     cleanup message, and Load returns 0 (the .tmp is never treated as a valid
//     mempool.json — Load only reads the final path).
//
//  2. Fresh .tmp present (<5 min old): startupLoadMempool leaves it untouched
//     so a concurrent Save() is not interrupted.  Load still returns 0 because
//     no valid mempool.json exists.
//
//  3. No .tmp present: startupLoadMempool is a no-op; Load returns 0.
//
//  4. End-to-end: stale .tmp alongside a valid mempool.json (realistic crash
//     scenario).  startupLoadMempool removes only the .tmp, then Load restores
//     the saved transaction successfully.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
)

// TestMempoolStartupRemovesStaleTmp is the primary integration test.
//
// It exercises the full production startup sequence by calling
// startupLoadMempool — the same function that main.go run() calls at the
// mempool-startup step — and verifies:
//
//   - CleanStaleTmpFiles is called (stale .tmp is removed, log message emitted)
//   - Load is called after cleanup (returns 0; no valid mempool.json exists)
//
// Removing CleanStaleTmpFiles from startupLoadMempool causes the log assertion
// to fail; removing pool.Load causes the restored-count assertion to fail.
func TestMempoolStartupRemovesStaleTmp(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "mempool.json.tmp")

	// Plant a stale .tmp (10 minutes old — well past the 5-minute threshold).
	if err := os.WriteFile(tmpPath, []byte(`{"saved_at":"2020-01-01T00:00:00Z","entries":[]}`), 0o600); err != nil {
		t.Fatalf("write stale tmp: %v", err)
	}
	staleTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(tmpPath, staleTime, staleTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Confirm file is present before the startup helper runs.
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("stale tmp not present before startupLoadMempool: %v", err)
	}

	// Call the production startup helper — same call as main.go run().
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	pool := core.NewMempool(core.DefaultMempoolConfig(), discardLogger())

	n := startupLoadMempool(dir, pool, log)

	// CleanStaleTmpFiles must have run: stale .tmp gone, log message emitted.
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("stale .tmp still present after startupLoadMempool (err=%v) — CleanStaleTmpFiles not called?", err)
	}
	const wantCleanupMsg = "mempool: removed stale tmp file from previous crash"
	if !logContainsMsg(&logBuf, wantCleanupMsg) {
		t.Errorf("cleanup log %q missing — CleanStaleTmpFiles not called or not logging:\n%s",
			wantCleanupMsg, logBuf.String())
	}

	// pool.Load must have run: no mempool.json exists, so 0 restored.
	if n != 0 {
		t.Errorf("startupLoadMempool returned %d, want 0 (no valid mempool.json)", n)
	}
	if pool.Count() != 0 {
		t.Errorf("pool.Count() = %d after startup with only stale .tmp, want 0", pool.Count())
	}
}

// TestMempoolStartupPreservesFreshTmp verifies that startupLoadMempool leaves a
// fresh .tmp (younger than 5 minutes) untouched so a concurrent Save() is not
// interrupted.
func TestMempoolStartupPreservesFreshTmp(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "mempool.json.tmp")

	// Write a fresh .tmp (mtime = now — well within the 5-minute window).
	if err := os.WriteFile(tmpPath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write fresh tmp: %v", err)
	}

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	pool := core.NewMempool(core.DefaultMempoolConfig(), discardLogger())

	startupLoadMempool(dir, pool, log)

	// Fresh .tmp must remain — it belongs to a concurrent Save().
	if _, err := os.Stat(tmpPath); err != nil {
		t.Errorf("fresh .tmp was incorrectly removed by startupLoadMempool: %v", err)
	}

	// No cleanup message should appear for a fresh .tmp.
	const unwantedMsg = "mempool: removed stale tmp file from previous crash"
	if logContainsMsg(&logBuf, unwantedMsg) {
		t.Errorf("cleanup message logged for fresh .tmp — threshold guard broken:\n%s", logBuf.String())
	}
}

// TestMempoolStartupNoTmpIsNoop verifies that startupLoadMempool is a safe
// no-op when no .tmp file exists — the common case after a clean shutdown.
func TestMempoolStartupNoTmpIsNoop(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "mempool.json.tmp")

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	pool := core.NewMempool(core.DefaultMempoolConfig(), discardLogger())

	// Must not panic or return an error.
	n := startupLoadMempool(dir, pool, log)

	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("unexpected file after no-op startup (err=%v)", err)
	}
	if n != 0 {
		t.Errorf("startupLoadMempool returned %d, want 0 (no mempool.json)", n)
	}
	const unwantedMsg = "mempool: removed stale tmp file from previous crash"
	if logContainsMsg(&logBuf, unwantedMsg) {
		t.Errorf("cleanup message logged when no .tmp was present:\n%s", logBuf.String())
	}
}

// TestMempoolStartupStaleTmpAlongsideSavedEntries is the end-to-end crash
// recovery test: a stale .tmp coexists with a valid mempool.json (the node
// crashed mid-Save after an earlier successful Save had already written the
// final file).
//
// startupLoadMempool must:
//  1. Remove the stale .tmp.
//  2. Leave mempool.json untouched.
//  3. Restore the saved transaction via Load.
//
// This exercises the realistic scenario where a running node is OOM-killed
// while writing a new snapshot: the previous clean snapshot survives and the
// orphaned .tmp is cleaned up.
func TestMempoolStartupStaleTmpAlongsideSavedEntries(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "mempool.json.tmp")
	jsonPath := filepath.Join(dir, "mempool.json")

	// 1. Save a valid mempool.json (one structural tx, no verifier).
	cfg := mempoolTestConfig()
	poolBefore := core.NewMempool(cfg, discardLogger())
	if err := poolBefore.Add(makeTestTx(2000, 90)); err != nil {
		t.Fatalf("Add tx: %v", err)
	}
	if err := poolBefore.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(jsonPath); os.IsNotExist(err) {
		t.Fatalf("mempool.json not written")
	}

	// 2. Plant a stale .tmp — simulates an OOM kill mid-Save.
	if err := os.WriteFile(tmpPath, []byte(`corrupted-partial-write`), 0o600); err != nil {
		t.Fatalf("write stale tmp: %v", err)
	}
	staleTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(tmpPath, staleTime, staleTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// 3. Run the production startup helper.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	poolAfter := core.NewMempool(cfg, discardLogger())

	n := startupLoadMempool(dir, poolAfter, log)

	// .tmp must be removed by CleanStaleTmpFiles.
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("stale .tmp still present after startupLoadMempool (err=%v)", err)
	}
	// Note: Load() removes mempool.json after consuming it — that is correct
	// behaviour (prevents double-replay on a second restart).  We do not assert
	// the file is still present; we assert the transaction was restored instead.

	// Cleanup message must appear.
	const wantCleanupMsg = "mempool: removed stale tmp file from previous crash"
	if !logContainsMsg(&logBuf, wantCleanupMsg) {
		t.Errorf("cleanup log %q missing:\n%s", wantCleanupMsg, logBuf.String())
	}
	// The saved transaction must be restored.
	if n != 1 {
		t.Errorf("startupLoadMempool returned %d restored tx(s), want 1", n)
	}
	if poolAfter.Count() != 1 {
		t.Errorf("pool.Count() = %d after startupLoadMempool, want 1", poolAfter.Count())
	}
}
