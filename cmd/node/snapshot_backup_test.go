package main

// Tests for the "-prev.json" backup created by saveStartupSnapshot.
//
// These tests verify the requirements from task #1049 and the refinement from
// task #1489:
//   1. A prev backup is created when a new primary replaces an older one
//      (different-height replacement).
//   2. A prev backup is created on a same-height overwrite (e.g. a shutdown
//      snapshot taken at the same tip as the last periodic checkpoint), and
//      the prev contains the ORIGINAL primary data, not the new data.
//   3. On the very first save at a height (no prior primary), NO prev is
//      written — there is nothing to preserve.
//   4. deleteOldSnapshots retains only the single most-recent prev backup
//      alongside the current primary.
//   5. findLatestSnapshot is unaffected by prev files (they are skipped).
//
// NOTE: loadStartupSnapshotWithFallback consults the prev file when the primary
// fails with a non-NotExist error (see snapshot_restart_test.go).

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/aperod/aperod/core"
)

// makeSnap builds a minimal startupSnapshot at the given height.
func makeSnap(h uint64) startupSnapshot {
	return startupSnapshot{
		Version:    snapVersion,
		TipHeight:  h,
		TipHashHex: fmt.Sprintf("%016x", h),
		TxTotal:    int64(h),
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
}

// prevPath returns the expected prev-backup path for a primary at height h.
func prevPath(dir string, h uint64) string {
	return snapshotPrevPath(snapshotPath(dir, h))
}

// writeSnapFile JSON-encodes snap and writes it to path.
func writeSnapFile(t *testing.T, path string, snap startupSnapshot) {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ─── Test 1: different-height replacement creates prev backup ─────────────────

// TestBackup_DifferentHeight verifies that when saveStartupSnapshot writes a
// snapshot at a new height, the previous primary is copied to a "-prev.json"
// backup.  It also verifies that on the very first save at a height (no prior
// primary at that height), NO prev file is written — there is nothing to
// preserve.
func TestBackup_DifferentHeight(t *testing.T) {
	dir := t.TempDir()

	// First save: height 100.  No prior primary at any height → no prev backup
	// of any kind is written.  The same-height guard skips the copy when the
	// primary is absent (task #1489: prevents "no such file or directory" errors
	// at every first-time checkpoint during a fresh genesis scan).
	if err := saveStartupSnapshot(dir, makeSnap(100)); err != nil {
		t.Fatalf("saveStartupSnapshot(100): %v", err)
	}
	// No prev-100 expected — this was the first save at height 100.
	if _, err := os.Stat(prevPath(dir, 100)); err == nil {
		t.Error("prev backup must NOT be written on the first save at a height (nothing to preserve)")
	}
	// The primary must exist.
	if _, err := os.Stat(snapshotPath(dir, 100)); os.IsNotExist(err) {
		t.Errorf("primary at height 100 missing after first save")
	}

	// Second save: height 200.  The height-100 primary is copied to
	// snapshot-v2-100-prev.json (best-effort prior-height backup).
	// No prev-200 is written because height 200 has no prior primary.
	if err := saveStartupSnapshot(dir, makeSnap(200)); err != nil {
		t.Fatalf("saveStartupSnapshot(200): %v", err)
	}
	if _, err := os.Stat(prevPath(dir, 100)); os.IsNotExist(err) {
		t.Errorf("expected prior-height backup %s after saving height 200", prevPath(dir, 100))
	}
	// No prev-200 — this is the first save at height 200.
	if _, err := os.Stat(prevPath(dir, 200)); err == nil {
		t.Errorf("prev backup must NOT exist at height 200 on its first save")
	}
	// The height-100 primary must still exist (copy, not rename).
	if _, err := os.Stat(snapshotPath(dir, 100)); os.IsNotExist(err) {
		t.Errorf("primary at height 100 must still exist after copy-to-prev")
	}
	// The height-200 primary must also exist.
	if _, err := os.Stat(snapshotPath(dir, 200)); os.IsNotExist(err) {
		t.Errorf("primary at height 200 missing after save")
	}
}

// ─── Test 2: same-height overwrite creates prev backup ───────────────────────

// TestBackup_SameHeightOverwrite verifies that saving a second snapshot at the
// same height (e.g. shutdown immediately after a periodic checkpoint) produces
// a "-prev.json" backup containing the ORIGINAL primary data, not the new data.
func TestBackup_SameHeightOverwrite(t *testing.T) {
	dir := t.TempDir()

	// First save at height 500.  No prev expected (first save, nothing to preserve).
	first := makeSnap(500)
	first.TxTotal = 111 // distinguishing marker for the original snapshot
	if err := saveStartupSnapshot(dir, first); err != nil {
		t.Fatalf("saveStartupSnapshot(500) first: %v", err)
	}
	if _, err := os.Stat(prevPath(dir, 500)); err == nil {
		t.Error("prev backup must NOT exist after the very first save at a height")
	}

	// Second save at the same height 500 (simulates shutdown after periodic snap).
	// The existing primary (TxTotal=111) must be copied to prev before overwriting.
	second := makeSnap(500)
	second.TxTotal = 222 // new data written by the second save
	if err := saveStartupSnapshot(dir, second); err != nil {
		t.Fatalf("saveStartupSnapshot(500) second: %v", err)
	}

	// A prev backup at height 500 must now exist.
	if _, err := os.Stat(prevPath(dir, 500)); os.IsNotExist(err) {
		t.Errorf("expected backup %s after same-height overwrite", prevPath(dir, 500))
	}
	// The primary must also exist.
	if _, err := os.Stat(snapshotPath(dir, 500)); os.IsNotExist(err) {
		t.Errorf("primary at height 500 missing after same-height overwrite")
	}

	// The prev must contain the ORIGINAL data (TxTotal=111), not the new data.
	// This confirms we copied the existing primary to prev before overwriting it,
	// not the new tmp content.
	prevSnap := tryLoadSnapshotFile(prevPath(dir, 500), 500)
	if prevSnap == nil {
		t.Fatal("prev backup at height 500 is unreadable")
	}
	if prevSnap.TxTotal != 111 {
		t.Errorf("prev backup TxTotal=%d, want 111 (original primary data, not new data %d)",
			prevSnap.TxTotal, second.TxTotal)
	}
}

// ─── Test 3: deleteOldSnapshots keeps only the most-recent prev ──────────────

// TestDeleteOldSnapshots_KeepsMostRecentPrev verifies that deleteOldSnapshots
// retains exactly one prev backup (the highest height) alongside the current
// primary, and removes every other snapshot file.
func TestDeleteOldSnapshots_KeepsMostRecentPrev(t *testing.T) {
	dir := t.TempDir()

	// Manually create snapshot files at several heights (primary + prev).
	writeSnapFile(t, snapshotPath(dir, 100), makeSnap(100))
	writeSnapFile(t, prevPath(dir, 100), makeSnap(100))
	writeSnapFile(t, snapshotPath(dir, 200), makeSnap(200))
	writeSnapFile(t, prevPath(dir, 200), makeSnap(200))
	writeSnapFile(t, snapshotPath(dir, 300), makeSnap(300))

	deleteOldSnapshots(dir, 300)

	// Primary for height 300 must survive.
	if _, err := os.Stat(snapshotPath(dir, 300)); os.IsNotExist(err) {
		t.Error("primary at height 300 was incorrectly deleted")
	}
	// Most recent prev (height 200) must survive.
	if _, err := os.Stat(prevPath(dir, 200)); os.IsNotExist(err) {
		t.Error("most-recent prev backup (height 200) was incorrectly deleted")
	}
	// Older prev (height 100) must be gone.
	if _, err := os.Stat(prevPath(dir, 100)); err == nil {
		t.Error("stale prev backup (height 100) should have been deleted")
	}
	// Older primaries (height 100 and 200) must be gone.
	if _, err := os.Stat(snapshotPath(dir, 100)); err == nil {
		t.Error("stale primary (height 100) should have been deleted")
	}
	if _, err := os.Stat(snapshotPath(dir, 200)); err == nil {
		t.Error("stale primary (height 200) should have been deleted")
	}
}

// ─── Test 4: prev-copy failure aborts same-height overwrite ──────────────────

// TestSaveSnapshot_PrevCopyFailureAbortsRename confirms that when the required
// same-height prev-backup copy fails during a same-height overwrite,
// saveStartupSnapshot returns a non-nil error AND the existing primary is left
// intact (the failed save never writes or promotes a tmp file).
//
// Failure is injected by pre-creating the target prev-backup path as a
// directory: copyFile's internal rename over a directory is rejected by the OS
// (EISDIR on Linux), simulating an I/O failure on the backup write.
//
// Note: the same-height guard only triggers when a primary already exists.  On
// the very first save at a height the guard is skipped (nothing to preserve),
// so this test must first establish a primary before blocking the prev path.
func TestSaveSnapshot_PrevCopyFailureAbortsRename(t *testing.T) {
	dir := t.TempDir()

	// ── Step 1: establish a valid primary at height 10 (first save — no prev written).
	orig := makeSnap(10)
	orig.TxTotal = 42 // distinguishing marker for the original
	if err := saveStartupSnapshot(dir, orig); err != nil {
		t.Fatalf("initial save h=10: %v", err)
	}
	primaryH10 := snapshotPath(dir, 10)
	if _, err := os.Stat(primaryH10); err != nil {
		t.Fatalf("primary h=10 not found after initial save: %v", err)
	}

	// ── Step 2: block the same-height prev-backup target for height 10 by
	// placing a directory at that path.  copyFile cannot rename a regular file
	// over a directory (EISDIR), so the copy will fail.
	prevH10 := snapshotPrevPath(primaryH10)
	if err := os.MkdirAll(prevH10, 0755); err != nil {
		t.Fatalf("mkdir prev path: %v", err)
	}

	// ── Step 3: attempt a same-height overwrite at height 10 — must fail
	// because the same-height guard tries to copy primary-10 to prev-10
	// (which is now a directory) before writing the new tmp.
	newSnap := makeSnap(10)
	newSnap.TxTotal = 99
	saveErr := saveStartupSnapshot(dir, newSnap)
	if saveErr == nil {
		t.Fatal("expected error when same-height prev backup cannot be written; got nil")
	}

	// ── Step 4: the original primary at height 10 must still be intact.
	if _, statErr := os.Stat(primaryH10); os.IsNotExist(statErr) {
		t.Error("original primary at height 10 was lost after the failed same-height overwrite")
	}

	// ── Step 5: the primary must still contain the ORIGINAL data — the failed
	// save must not have promoted a partial tmp to primary.
	gotSnap := tryLoadSnapshotFile(primaryH10, 10)
	if gotSnap == nil {
		t.Fatal("primary at height 10 is unreadable after failed overwrite")
	}
	if gotSnap.TxTotal != orig.TxTotal {
		t.Errorf("primary TxTotal=%d after failed overwrite, want %d (original data)",
			gotSnap.TxTotal, orig.TxTotal)
	}
}

// writeGzipSnapFile gzip-encodes snap and writes it to path.
// Used by tests that need a file readable by tryLoadSnapshotFile.
func writeGzipSnapFile(t *testing.T, path string, snap startupSnapshot) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	gz := gzip.NewWriter(f)
	if encErr := json.NewEncoder(gz).Encode(snap); encErr != nil {
		f.Close()
		t.Fatalf("encode %s: %v", path, encErr)
	}
	if gzErr := gz.Close(); gzErr != nil {
		f.Close()
		t.Fatalf("gzip close %s: %v", path, gzErr)
	}
	if fErr := f.Close(); fErr != nil {
		t.Fatalf("file close %s: %v", path, fErr)
	}
}

// ─── Test 5: findLatestSnapshot ignores plain-JSON prev files ─────────────────

// TestFindLatestSnapshot_IgnoresPrevFiles verifies that plain-JSON (non-gzip)
// prev backup files — which cannot be decoded by tryLoadSnapshotFile — do not
// cause findLatestSnapshot to return a spurious high height.
func TestFindLatestSnapshot_IgnoresPrevFiles(t *testing.T) {
	dir := t.TempDir()

	// Write a valid primary at height 100.
	if err := saveStartupSnapshot(dir, makeSnap(100)); err != nil {
		t.Fatalf("saveStartupSnapshot(100): %v", err)
	}
	// Write a prev-backup file at height 999 by hand (simulates a very recent
	// backup that the find function must ignore).
	writeSnapFile(t, prevPath(dir, 999), makeSnap(999))

	// findLatestSnapshot with limit > 999 must return the primary at 100, not
	// the prev-only height 999.
	got := findLatestSnapshot(dir, 2000, nil)
	if got == nil {
		t.Fatal("findLatestSnapshot returned nil, want height 100")
	}
	if got.TipHeight != 100 {
		t.Errorf("findLatestSnapshot returned height %d, want 100 (prev files must be ignored)",
			got.TipHeight)
	}
}

// ─── Test 6: findLatestSnapshot picks up an orphaned shutdown prev-backup ─────

// TestFindLatestSnapshot_PicksUpOrphanedPrevBackup verifies that when the
// highest available snapshot is an orphaned "-prev.json.gz" file (no
// corresponding primary at that height — the situation after a crash wipes the
// primary but leaves the backup), findLatestSnapshot returns the higher
// prev-backup height rather than falling back to the older primary.
//
// This mirrors the production scenario where the node skipped a corrupted
// 170 K-block region by starting from the 973102 prev-backup instead of the
// 800000 periodic checkpoint.
func TestFindLatestSnapshot_PicksUpOrphanedPrevBackup(t *testing.T) {
	dir := t.TempDir()

	// Older periodic checkpoint (primary only) at height 800000.
	writeGzipSnapFile(t, snapshotPath(dir, 800000), makeSnap(800000))

	// Orphaned shutdown backup at height 973102 — no primary at this height.
	prevH := snapshotPrevPath(snapshotPath(dir, 973102))
	writeGzipSnapFile(t, prevH, makeSnap(973102))

	got := findLatestSnapshot(dir, 2000000, nil)
	if got == nil {
		t.Fatal("findLatestSnapshot returned nil, want height 973102")
	}
	if got.TipHeight != 973102 {
		t.Errorf("findLatestSnapshot returned height %d, want 973102 "+
			"(orphaned prev-backup must be used when it is higher than any primary)",
			got.TipHeight)
	}
}

// ─── Test 7: primary is preferred over prev-backup at the same height ─────────

// TestFindLatestSnapshot_PrimaryPreferredOverPrev verifies that when both a
// primary and a prev-backup exist at the same height, findLatestSnapshot loads
// the primary (tried first in the candidate loop) rather than the prev-backup.
// The two files are seeded with different TxTotal values so the caller can
// distinguish which one was returned.
func TestFindLatestSnapshot_PrimaryPreferredOverPrev(t *testing.T) {
	dir := t.TempDir()

	// Primary at height 500 — TxTotal=10 is the distinguishing marker.
	sPrimary := makeSnap(500)
	sPrimary.TxTotal = 10
	writeGzipSnapFile(t, snapshotPath(dir, 500), sPrimary)

	// Prev-backup at height 500 — TxTotal=99, different content.
	sPrev := makeSnap(500)
	sPrev.TxTotal = 99
	writeGzipSnapFile(t, snapshotPrevPath(snapshotPath(dir, 500)), sPrev)

	got := findLatestSnapshot(dir, 2000, nil)
	if got == nil {
		t.Fatal("findLatestSnapshot returned nil, want height 500")
	}
	if got.TipHeight != 500 {
		t.Errorf("height: got %d, want 500", got.TipHeight)
	}
	// TxTotal==10 means the primary was loaded; ==99 would mean the prev-backup
	// was incorrectly preferred.
	if got.TxTotal != 10 {
		t.Errorf("TxTotal: got %d, want 10 (primary must be preferred over prev-backup at same height)",
			got.TxTotal)
	}
}

// ─── Test 8: orphaned prev-backup at height >= limitHeight is skipped ─────────

// TestFindLatestSnapshot_PrevAboveLimitSkipped verifies that an orphaned
// prev-backup file whose height equals or exceeds limitHeight is excluded from
// consideration, matching the same boundary rule applied to primary checkpoints.
func TestFindLatestSnapshot_PrevAboveLimitSkipped(t *testing.T) {
	dir := t.TempDir()

	// Valid primary at height 100 (well below the limit).
	writeGzipSnapFile(t, snapshotPath(dir, 100), makeSnap(100))

	// Orphaned prev-backup at height 500 — exactly equal to limitHeight, so it
	// must be treated as out-of-range and skipped.
	prevH500 := snapshotPrevPath(snapshotPath(dir, 500))
	writeGzipSnapFile(t, prevH500, makeSnap(500))

	// limitHeight=500 → only heights strictly below 500 are eligible.
	got := findLatestSnapshot(dir, 500, nil)
	if got == nil {
		t.Fatal("findLatestSnapshot returned nil, want height 100")
	}
	if got.TipHeight != 100 {
		t.Errorf("findLatestSnapshot returned height %d, want 100 "+
			"(orphaned prev-backup at limitHeight must be skipped)",
			got.TipHeight)
	}
}
