package main

// Tests for cleanStaleTmpFile and cleanStaleSnapshotTmpFiles in snapshot.go.
//
// These tests mirror the pattern in mempool_tmp_cleanup_test.go.
//
// The four cases tested:
//
//  1. Stale .tmp present (≥5 min old): cleanStaleSnapshotTmpFiles removes it
//     and logs the cleanup message.
//
//  2. Fresh .tmp present (<5 min old): cleanStaleSnapshotTmpFiles leaves it
//     untouched so a concurrent write is not interrupted.
//
//  3. No .tmp present: cleanStaleSnapshotTmpFiles is a no-op with no log output.
//
//  4. Multiple files — stale .tmp removed, fresh .tmp preserved, and non-matching
//     files untouched.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// snapshotTmpName returns a canonical snapshot .tmp filename at the given height.
func snapshotTmpName(height uint64) string {
	return fmt.Sprintf("snapshot-v%d-%d.json.gz.tmp", snapVersion, height)
}

// plantStaleTmpFile creates a snapshot .tmp file older than snapshotStaleTmpMaxAge.
func plantStaleTmpFile(t *testing.T, dir string, height uint64) string {
	t.Helper()
	path := filepath.Join(dir, snapshotTmpName(height))
	if err := os.WriteFile(path, []byte("partial-write"), 0o600); err != nil {
		t.Fatalf("write stale tmp: %v", err)
	}
	staleTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(path, staleTime, staleTime); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}
	return path
}

// plantFreshTmpFile creates a snapshot .tmp file younger than snapshotStaleTmpMaxAge.
func plantFreshTmpFile(t *testing.T, dir string, height uint64) string {
	t.Helper()
	path := filepath.Join(dir, snapshotTmpName(height))
	if err := os.WriteFile(path, []byte("in-progress-write"), 0o600); err != nil {
		t.Fatalf("write fresh tmp: %v", err)
	}
	return path
}

// ─── Test 1: stale .tmp is removed ───────────────────────────────────────────

// TestCleanStaleSnapshotTmpFiles_RemovesStale verifies that a snapshot .tmp
// file older than snapshotStaleTmpMaxAge is deleted by cleanStaleSnapshotTmpFiles
// and the cleanup message is logged.
func TestCleanStaleSnapshotTmpFiles_RemovesStale(t *testing.T) {
	dir := t.TempDir()
	tmpPath := plantStaleTmpFile(t, dir, 1000)

	// Confirm file exists before the call.
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("stale tmp not present before cleanup: %v", err)
	}

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	cleanStaleSnapshotTmpFiles(dir, log)

	// Stale .tmp must be gone.
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("stale .tmp still present after cleanStaleSnapshotTmpFiles (err=%v)", err)
	}
	// Cleanup log message must appear.
	const wantMsg = "snapshot: removed stale tmp file from previous crash"
	if !logContainsMsg(&logBuf, wantMsg) {
		t.Errorf("cleanup log %q missing — cleanStaleTmpFile not called or not logging:\n%s",
			wantMsg, logBuf.String())
	}
}

// ─── Test 2: fresh .tmp is preserved ─────────────────────────────────────────

// TestCleanStaleSnapshotTmpFiles_PreservesFresh verifies that a .tmp file
// younger than snapshotStaleTmpMaxAge is left untouched so a concurrent
// saveStartupSnapshot is not interrupted.
func TestCleanStaleSnapshotTmpFiles_PreservesFresh(t *testing.T) {
	dir := t.TempDir()
	tmpPath := plantFreshTmpFile(t, dir, 2000)

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	cleanStaleSnapshotTmpFiles(dir, log)

	// Fresh .tmp must still exist.
	if _, err := os.Stat(tmpPath); err != nil {
		t.Errorf("fresh .tmp was incorrectly removed by cleanStaleSnapshotTmpFiles: %v", err)
	}
	// No cleanup message should appear for a fresh file.
	const unwantedMsg = "snapshot: removed stale tmp file from previous crash"
	if logContainsMsg(&logBuf, unwantedMsg) {
		t.Errorf("cleanup message logged for fresh .tmp — age threshold guard broken:\n%s",
			logBuf.String())
	}
}

// ─── Test 3: no .tmp present is a no-op ──────────────────────────────────────

// TestCleanStaleSnapshotTmpFiles_NoTmpIsNoop verifies that cleanStaleSnapshotTmpFiles
// is safe and silent when no .tmp files exist — the common case after a clean
// shutdown.
func TestCleanStaleSnapshotTmpFiles_NoTmpIsNoop(t *testing.T) {
	dir := t.TempDir()

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	// Must not panic.
	cleanStaleSnapshotTmpFiles(dir, log)

	const unwantedMsg = "snapshot: removed stale tmp file from previous crash"
	if logContainsMsg(&logBuf, unwantedMsg) {
		t.Errorf("cleanup message logged when no .tmp was present:\n%s", logBuf.String())
	}
}

// ─── Test 4: mixed files — stale removed, fresh kept, non-matching untouched ─

// TestCleanStaleSnapshotTmpFiles_MixedFiles is the realistic end-to-end case:
// two stale .tmp files (from different heights), one fresh .tmp, and an
// unrelated file that must never be touched.
//
// cleanStaleSnapshotTmpFiles must:
//   - Remove both stale .tmp files.
//   - Leave the fresh .tmp untouched.
//   - Leave the unrelated file untouched.
func TestCleanStaleSnapshotTmpFiles_MixedFiles(t *testing.T) {
	dir := t.TempDir()

	stalePath1 := plantStaleTmpFile(t, dir, 100)
	stalePath2 := plantStaleTmpFile(t, dir, 200)
	freshPath := plantFreshTmpFile(t, dir, 300)

	// A non-matching file that must not be touched.
	otherPath := filepath.Join(dir, "chain.db")
	if err := os.WriteFile(otherPath, []byte("irrelevant"), 0o600); err != nil {
		t.Fatalf("write other file: %v", err)
	}

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	cleanStaleSnapshotTmpFiles(dir, log)

	// Both stale .tmp files must be gone.
	for _, p := range []string{stalePath1, stalePath2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale .tmp %s still present after cleanup (err=%v)", p, err)
		}
	}
	// Fresh .tmp must remain.
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("fresh .tmp was incorrectly removed: %v", err)
	}
	// Unrelated file must remain.
	if _, err := os.Stat(otherPath); err != nil {
		t.Errorf("unrelated file was incorrectly removed: %v", err)
	}
}

// ─── Test 5: cleanStaleTmpFile permission error ───────────────────────────────

// TestCleanStaleTmpFile_PermissionError verifies that cleanStaleTmpFile emits
// the "failed to remove stale tmp file" warning and leaves the file intact when
// os.Remove fails due to a permissions error.
//
// Linux-only (chmod directory permission semantics); skipped as root.
func TestCleanStaleTmpFile_PermissionError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping on non-Linux: chmod directory permissions may differ")
	}
	if os.Getuid() == 0 {
		t.Skip("skipping when running as root: root ignores directory write permissions")
	}

	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	if err := os.Mkdir(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir dataDir: %v", err)
	}

	tmpPath := plantStaleTmpFile(t, dataDir, 500)

	// Remove write permission from dataDir so os.Remove fails with EACCES.
	if err := os.Chmod(dataDir, 0o555); err != nil {
		t.Fatalf("chmod dataDir: %v", err)
	}
	defer func() { _ = os.Chmod(dataDir, 0o755) }()

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	cleanStaleTmpFile(tmpPath, snapshotStaleTmpMaxAge, log)

	// Warning must be logged.
	const wantWarnMsg = "snapshot: failed to remove stale tmp file (ignoring)"
	if !logContainsMsg(&logBuf, wantWarnMsg) {
		t.Errorf("warning log %q not found — error path not reached or not logged:\n%s",
			wantWarnMsg, logBuf.String())
	}
	// File must still exist.
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	if _, err := os.Stat(tmpPath); os.IsNotExist(err) {
		t.Errorf("stale .tmp was unexpectedly removed despite permissions error")
	}
}
