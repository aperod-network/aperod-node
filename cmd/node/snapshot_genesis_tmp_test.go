package main

// Tests for cleanStaleSnapshotTmpFiles coverage of the genesis startup path.
//
// Background
// ----------
// cleanStaleSnapshotTmpFiles is called in step 3 of run(), BEFORE the
// tipHash == zero check that branches into genesis initialisation.  This
// placement ensures that a snapshot .tmp left behind by a crash during a
// first-run (genesis) initialisation is cleaned up on the next startup, not
// accumulated indefinitely across crash-restart cycles.
//
// The tests below simulate that exact failure mode and confirm the function
// removes the orphaned file before any genesis or resume work begins.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanStaleSnapshotTmpFiles_GenesisCrashRemoved is the primary regression
// test for the genesis-path crash case.
//
// Scenario:
//  1. The node starts for the very first time (no chain.db tip yet).
//  2. Partway through genesis initialisation it crashes, leaving a stale
//     snapshot-v2-0.json.gz.tmp in the data directory.
//  3. On the next startup, cleanStaleSnapshotTmpFiles (called before the
//     genesis branch) finds and removes the orphaned file.
//  4. The genesis branch then proceeds with a clean data directory.
func TestCleanStaleSnapshotTmpFiles_GenesisCrashRemoved(t *testing.T) {
	dir := t.TempDir()

	// Plant a stale .tmp at height 0 — the height used during genesis init.
	stalePath := filepath.Join(dir, "snapshot-v2-0.json.gz.tmp")
	if err := os.WriteFile(stalePath, []byte("partial-genesis-snapshot"), 0o600); err != nil {
		t.Fatalf("plant genesis stale tmp: %v", err)
	}
	staleTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(stalePath, staleTime, staleTime); err != nil {
		t.Fatalf("backdate genesis stale tmp: %v", err)
	}

	// Confirm the file is present before the call — simulates the state right
	// after the crash, before the next node restart processes it.
	if _, err := os.Stat(stalePath); err != nil {
		t.Fatalf("stale genesis tmp not present before cleanup: %v", err)
	}

	// Call the function that run() invokes before entering the genesis branch.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	cleanStaleSnapshotTmpFiles(dir, log)

	// The stale .tmp must be gone so the genesis branch runs on a clean dir.
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("genesis stale .tmp still present after cleanStaleSnapshotTmpFiles (err=%v)", err)
	}

	// The cleanup log line must appear so operators can audit crash history.
	const wantMsg = "snapshot: removed stale tmp file from previous crash"
	if !logContainsMsg(&logBuf, wantMsg) {
		t.Errorf("cleanup log %q missing — genesis-path stale .tmp not logged:\n%s",
			wantMsg, logBuf.String())
	}
}

// TestCleanStaleSnapshotTmpFiles_GenesisCrashMultipleHeights verifies that the
// cleanup handles multiple stale .tmp files left by several crash cycles during
// genesis (each restart may write a new .tmp at the same or different height).
func TestCleanStaleSnapshotTmpFiles_GenesisCrashMultipleHeights(t *testing.T) {
	dir := t.TempDir()

	// Simulate two crash cycles during genesis — both leave stale .tmp files.
	heights := []uint64{0, 1}
	stalePaths := make([]string, 0, len(heights))
	for _, h := range heights {
		name := snapshotTmpName(h) // helper defined in snapshot_tmp_cleanup_test.go
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatalf("plant stale tmp height %d: %v", h, err)
		}
		staleTime := time.Now().Add(-15 * time.Minute)
		if err := os.Chtimes(p, staleTime, staleTime); err != nil {
			t.Fatalf("backdate stale tmp height %d: %v", h, err)
		}
		stalePaths = append(stalePaths, p)
	}

	// A valid (non-.tmp) file must not be touched.
	goodFile := filepath.Join(dir, "snapshot-v2-0.json.gz")
	if err := os.WriteFile(goodFile, []byte("valid-snapshot"), 0o600); err != nil {
		t.Fatalf("write good snapshot: %v", err)
	}

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	cleanStaleSnapshotTmpFiles(dir, log)

	// All stale .tmp files must be removed.
	for _, p := range stalePaths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale .tmp %s still present after cleanup (err=%v)", p, err)
		}
	}

	// The good snapshot must remain untouched.
	if _, err := os.Stat(goodFile); err != nil {
		t.Errorf("valid snapshot file was incorrectly removed: %v", err)
	}
}

// TestCleanStaleSnapshotTmpFiles_GenesisPathNoTmpIsNoop confirms that when the
// node crashes before writing any .tmp (e.g. crash before the snapshot write
// even starts), cleanStaleSnapshotTmpFiles is a silent no-op and the empty
// data directory is left unchanged.
func TestCleanStaleSnapshotTmpFiles_GenesisPathNoTmpIsNoop(t *testing.T) {
	dir := t.TempDir()

	// No .tmp files — the data dir is completely empty (first ever start).

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	cleanStaleSnapshotTmpFiles(dir, log)

	// Must not panic and must not log a removal message.
	const unwantedMsg = "snapshot: removed stale tmp file from previous crash"
	if logContainsMsg(&logBuf, unwantedMsg) {
		t.Errorf("cleanup message logged when data dir had no .tmp files:\n%s", logBuf.String())
	}

	// The directory must still be empty.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir after noop: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("cleanStaleSnapshotTmpFiles created unexpected files in empty dir: %v", entries)
	}
}

// TestCleanStaleSnapshotTmpFiles_GenesisFreshTmpPreserved verifies that a fresh
// .tmp written in a concurrent genesis attempt (e.g. two node processes racing)
// is NOT removed, preserving an in-progress write.
func TestCleanStaleSnapshotTmpFiles_GenesisFreshTmpPreserved(t *testing.T) {
	dir := t.TempDir()

	// Fresh .tmp (< snapshotStaleTmpMaxAge) at genesis height 0.
	freshPath := filepath.Join(dir, "snapshot-v2-0.json.gz.tmp")
	if err := os.WriteFile(freshPath, []byte("in-progress"), 0o600); err != nil {
		t.Fatalf("plant fresh genesis tmp: %v", err)
	}
	// Leave mtime at now (default) — it is younger than the threshold.

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	cleanStaleSnapshotTmpFiles(dir, log)

	// Fresh .tmp must still exist.
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("fresh genesis .tmp was incorrectly removed by cleanStaleSnapshotTmpFiles: %v", err)
	}

	// No removal message must appear.
	const unwantedMsg = "snapshot: removed stale tmp file from previous crash"
	if logContainsMsg(&logBuf, unwantedMsg) {
		t.Errorf("removal log emitted for a fresh .tmp — age guard broken:\n%s", logBuf.String())
	}
}
