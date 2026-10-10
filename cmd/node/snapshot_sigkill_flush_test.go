package main

// snapshot_sigkill_flush_test.go — verifies that a snapshot file left in a
// truncated or otherwise corrupt state by a SIGKILL mid-write does not block
// node startup: the loader must detect the corrupt primary and fall back to the
// prev-backup, or return a clear error when no fallback is available.
//
// Tests:
//
//   TestSIGKILLFlush_TruncatedPrimaryFallsBackToPrev
//     Primary is truncated to half its size (SIGKILL mid-gzip-write).  A valid
//     prev-backup exists.  loadStartupSnapshotWithFallback must succeed and
//     return the prev-backup's snapshot.
//
//   TestSIGKILLFlush_EmptyPrimaryFallsBackToPrev
//     Primary exists but is empty (SIGKILL before the first byte was written).
//     A valid prev-backup exists.  Fallback must succeed.
//
//   TestSIGKILLFlush_TruncatedPrimaryNoPrevReturnsError
//     Primary is truncated; no prev-backup available.  loadStartupSnapshotWithFallback
//     must return a non-nil error so the caller can emit startup_reason=corrupt_snapshot
//     and fall through to a full block scan.
//
//   TestSIGKILLFlush_BothCorruptReturnsError
//     Both primary and prev-backup contain garbage bytes.  The function must
//     return a non-nil error (never silently succeed with nil snap).
//
//   TestSIGKILLFlush_ValidSnapshotUnaffected
//     A clean, fully-written snapshot loads correctly (sanity / non-regression).

import (
        "bytes"
        "fmt"
        "os"
        "path/filepath"
        "testing"
)

// sigkillSnapPath returns the path of the gzip snapshot at height h, matching
// the snapshotPath() convention: snapshot-v<version>-<height>.json.gz.
func sigkillSnapPath(dataDir string, height uint64) string {
        return filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-%d.json.gz", snapVersion, height))
}

// makePrevBackupFromPrimary copies the current primary (and its checksum
// sidecar) to the prev-backup slot so loadPrevBackupSnapshot can find a valid
// recovery file.  The copy is done BEFORE corrupting the primary.
func makePrevBackupFromPrimary(t *testing.T, primaryPath string) {
        t.Helper()
        data, err := os.ReadFile(primaryPath)
        if err != nil {
                t.Fatalf("read primary for prev-backup copy: %v", err)
        }
        prevPath := snapshotPrevPath(primaryPath)
        if err := os.WriteFile(prevPath, data, 0o644); err != nil {
                t.Fatalf("write prev-backup: %v", err)
        }
        // Copy the checksum sidecar so verifySnapshotChecksum succeeds on the
        // prev-backup path.
        csPath := snapshotChecksumPath(primaryPath)
        if csData, csErr := os.ReadFile(csPath); csErr == nil {
                // Sidecar exists — copy it.  Ignore errors: loaders are tolerant
                // of a missing sidecar on the prev path.
                _ = os.WriteFile(snapshotChecksumPath(prevPath), csData, 0o644)
        }
}

// TestSIGKILLFlush_TruncatedPrimaryFallsBackToPrev confirms that when the
// primary snapshot is truncated (SIGKILL during gzip write), a valid prev-backup
// is used and startup succeeds.
func TestSIGKILLFlush_TruncatedPrimaryFallsBackToPrev(t *testing.T) {
        dir := t.TempDir()
        var buf bytes.Buffer
        log := newCaptureLogger(&buf)

        const height = uint64(5)
        const hashHex = "deadbeef"

        // 1. Write a valid primary.
        saveSnapAtHeight(t, dir, height, hashHex)
        primaryPath := sigkillSnapPath(dir, height)

        // 2. Promote the primary to prev-backup BEFORE corrupting it.
        makePrevBackupFromPrimary(t, primaryPath)

        // 3. Truncate the primary to simulate a SIGKILL mid-write.
        data, err := os.ReadFile(primaryPath)
        if err != nil {
                t.Fatalf("read primary: %v", err)
        }
        if err := os.WriteFile(primaryPath, data[:len(data)/2], 0o644); err != nil {
                t.Fatalf("truncate primary: %v", err)
        }

        // 4. loadStartupSnapshotWithFallback must succeed via the prev-backup.
        snap, _, loadErr := loadStartupSnapshotWithFallback(dir, height, hashHex, log)
        if loadErr != nil {
                t.Fatalf("expected success via prev-backup, got error: %v\nlog:\n%s", loadErr, buf.String())
        }
        if snap == nil {
                t.Fatal("returned nil snap without error")
        }
        if snap.TipHeight != height {
                t.Errorf("snap.TipHeight = %d, want %d", snap.TipHeight, height)
        }
}

// TestSIGKILLFlush_EmptyPrimaryFallsBackToPrev covers the edge case where the
// SIGKILL strikes before the first byte is written (file exists, size = 0).
func TestSIGKILLFlush_EmptyPrimaryFallsBackToPrev(t *testing.T) {
        dir := t.TempDir()
        var buf bytes.Buffer
        log := newCaptureLogger(&buf)

        const height = uint64(3)
        const hashHex = "a1b2c3d4"

        // 1. Write a valid primary.
        saveSnapAtHeight(t, dir, height, hashHex)
        primaryPath := sigkillSnapPath(dir, height)

        // 2. Promote to prev-backup.
        makePrevBackupFromPrimary(t, primaryPath)

        // 3. Replace primary with a zero-byte file (SIGKILL before first write).
        if err := os.WriteFile(primaryPath, []byte{}, 0o644); err != nil {
                t.Fatalf("write empty primary: %v", err)
        }

        // 4. Fallback must succeed.
        snap, _, loadErr := loadStartupSnapshotWithFallback(dir, height, hashHex, log)
        if loadErr != nil {
                t.Fatalf("expected success via prev-backup for empty primary, got: %v\nlog:\n%s", loadErr, buf.String())
        }
        if snap == nil {
                t.Fatal("returned nil snap without error")
        }
        if snap.TipHeight != height {
                t.Errorf("snap.TipHeight = %d, want %d", snap.TipHeight, height)
        }
}

// TestSIGKILLFlush_TruncatedPrimaryNoPrevReturnsError confirms that when the
// primary is corrupt and no prev-backup is available, the function returns a
// non-nil error (never silently returns nil snap with nil err).
func TestSIGKILLFlush_TruncatedPrimaryNoPrevReturnsError(t *testing.T) {
        dir := t.TempDir()
        var buf bytes.Buffer
        log := newCaptureLogger(&buf)

        const height = uint64(7)
        const hashHex = "cafebabe"

        // Write a valid primary and then truncate it; do NOT create a prev-backup.
        saveSnapAtHeight(t, dir, height, hashHex)
        primaryPath := sigkillSnapPath(dir, height)
        data, err := os.ReadFile(primaryPath)
        if err != nil {
                t.Fatalf("read primary: %v", err)
        }
        if err := os.WriteFile(primaryPath, data[:len(data)/2], 0o644); err != nil {
                t.Fatalf("truncate primary: %v", err)
        }

        snap, _, loadErr := loadStartupSnapshotWithFallback(dir, height, hashHex, log)
        if loadErr == nil {
                h := uint64(0)
                if snap != nil {
                        h = snap.TipHeight
                }
                t.Fatalf("expected error for corrupt primary + no prev-backup, got snap at height %d\nlog:\n%s", h, buf.String())
        }
        if snap != nil {
                t.Errorf("expected nil snap on error, got height %d", snap.TipHeight)
        }
}

// TestSIGKILLFlush_BothCorruptReturnsError confirms that when both the primary
// and the prev-backup contain garbage bytes, the function returns a non-nil error.
func TestSIGKILLFlush_BothCorruptReturnsError(t *testing.T) {
        dir := t.TempDir()
        var buf bytes.Buffer
        log := newCaptureLogger(&buf)

        const height = uint64(9)
        const hashHex = "feedface"

        primaryPath := sigkillSnapPath(dir, height)
        prevPath := snapshotPrevPath(primaryPath)

        if err := os.WriteFile(primaryPath, []byte("not gzip at all"), 0o644); err != nil {
                t.Fatalf("write corrupt primary: %v", err)
        }
        if err := os.WriteFile(prevPath, []byte("also garbage"), 0o644); err != nil {
                t.Fatalf("write corrupt prev: %v", err)
        }

        snap, _, loadErr := loadStartupSnapshotWithFallback(dir, height, hashHex, log)
        if loadErr == nil {
                h := uint64(0)
                if snap != nil {
                        h = snap.TipHeight
                }
                t.Fatalf("expected error for both-corrupt, got snap at height %d\nlog:\n%s", h, buf.String())
        }
        if snap != nil {
                t.Errorf("expected nil snap on error, got height %d", snap.TipHeight)
        }
}

// TestSIGKILLFlush_ValidSnapshotUnaffected is a non-regression sanity check: a
// clean, fully-written snapshot must still load correctly after this set of tests
// (guards against any accidental mutation of shared test state).
func TestSIGKILLFlush_ValidSnapshotUnaffected(t *testing.T) {
        dir := t.TempDir()
        var buf bytes.Buffer
        log := newCaptureLogger(&buf)

        const height = uint64(11)
        const hashHex = "c0ffee11"

        saveSnapAtHeight(t, dir, height, hashHex)

        snap, _, loadErr := loadStartupSnapshotWithFallback(dir, height, hashHex, log)
        if loadErr != nil {
                t.Fatalf("valid snapshot failed to load: %v\nlog:\n%s", loadErr, buf.String())
        }
        if snap == nil {
                t.Fatal("returned nil snap without error for valid snapshot")
        }
        if snap.TipHeight != height {
                t.Errorf("snap.TipHeight = %d, want %d", snap.TipHeight, height)
        }
        if snap.TipHashHex != hashHex {
                t.Errorf("snap.TipHashHex = %q, want %q", snap.TipHashHex, hashHex)
        }
}
