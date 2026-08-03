package main

// Tests for the "-prev.json" backup created by saveStartupSnapshot.
//
// These tests verify the requirements from task #1049:
//   1. A prev backup is created when a new primary replaces an older one.
//   2. A prev backup is also created on a same-height overwrite (e.g. a
//      shutdown snapshot taken at the same tip as the last periodic checkpoint).
//   3. deleteOldSnapshots retains only the single most-recent prev backup
//      alongside the current primary.
//   4. findLatestSnapshot is unaffected by prev files (they are skipped).
//
// NOTE: loadStartupSnapshotWithFallback consults the prev file when the primary
// fails with a non-NotExist error.  saveStartupSnapshot now also writes a
// same-height "-prev.json" from the validated tmp content so the fallback is
// always available at the exact current tip (see snapshot_restart_test.go).

import (
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
// backup.
func TestBackup_DifferentHeight(t *testing.T) {
	dir := t.TempDir()

	// First save: height 100.  No prior primary → no prior-height backup, but
	// saveStartupSnapshot now writes a same-height "-prev.json" from the
	// validated tmp content so there is always a recovery floor at the current tip.
	if err := saveStartupSnapshot(dir, makeSnap(100)); err != nil {
		t.Fatalf("saveStartupSnapshot(100): %v", err)
	}
	if _, err := os.Stat(prevPath(dir, 100)); os.IsNotExist(err) {
		t.Error("expected same-height backup snapshot-v1-100-prev.json after first save")
	}
	// The primary must also exist.
	if _, err := os.Stat(snapshotPath(dir, 100)); os.IsNotExist(err) {
		t.Errorf("primary at height 100 missing after first save")
	}

	// Second save: height 200.  The height-100 primary is copied to
	// snapshot-v1-100-prev.json (prior-height backup), and a new
	// snapshot-v1-200-prev.json is written as the same-height backup.
	if err := saveStartupSnapshot(dir, makeSnap(200)); err != nil {
		t.Fatalf("saveStartupSnapshot(200): %v", err)
	}
	if _, err := os.Stat(prevPath(dir, 100)); os.IsNotExist(err) {
		t.Errorf("expected prior-height backup %s after saving height 200", prevPath(dir, 100))
	}
	if _, err := os.Stat(prevPath(dir, 200)); os.IsNotExist(err) {
		t.Errorf("expected same-height backup %s after saving height 200", prevPath(dir, 200))
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
// same height (e.g. shutdown immediately after a periodic checkpoint) also
// produces a "-prev.json" backup for the original file.
func TestBackup_SameHeightOverwrite(t *testing.T) {
	dir := t.TempDir()

	// First save at height 500.
	if err := saveStartupSnapshot(dir, makeSnap(500)); err != nil {
		t.Fatalf("saveStartupSnapshot(500) first: %v", err)
	}

	// Second save at the same height 500 (simulates shutdown after periodic snap).
	if err := saveStartupSnapshot(dir, makeSnap(500)); err != nil {
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

// ─── Test 4: prev-copy failure aborts primary rename ─────────────────────────

// TestSaveSnapshot_PrevCopyFailureAbortsRename confirms that when the required
// same-height prev-backup copy fails, saveStartupSnapshot returns a non-nil
// error AND the prior primary is left intact (the failed save never promotes
// the tmp file to primary).
//
// Failure is injected by pre-creating the target prev-backup path as a
// directory: copyFile's internal rename over a directory is rejected by the OS
// (EISDIR on Linux), simulating an I/O failure on the backup write.
func TestSaveSnapshot_PrevCopyFailureAbortsRename(t *testing.T) {
	dir := t.TempDir()

	// ── Step 1: establish a valid primary at height 10.
	if err := saveStartupSnapshot(dir, makeSnap(10)); err != nil {
		t.Fatalf("initial save h=10: %v", err)
	}
	primaryH10 := snapshotPath(dir, 10)
	if _, err := os.Stat(primaryH10); err != nil {
		t.Fatalf("primary h=10 not found after initial save: %v", err)
	}

	// ── Step 2: block the same-height prev-backup target for height 20 by
	// placing a directory at that path.  copyFile cannot rename a regular file
	// over a directory (EISDIR), so the copy will fail.
	primaryH20 := snapshotPath(dir, 20)
	prevH20 := snapshotPrevPath(primaryH20)
	if err := os.MkdirAll(prevH20, 0755); err != nil {
		t.Fatalf("mkdir prev path: %v", err)
	}

	// ── Step 3: attempt to save at height 20 — must fail.
	saveErr := saveStartupSnapshot(dir, makeSnap(20))
	if saveErr == nil {
		t.Fatal("expected error when same-height prev backup cannot be written; got nil")
	}

	// ── Step 4: the prior primary at height 10 must still be intact.
	if _, statErr := os.Stat(primaryH10); os.IsNotExist(statErr) {
		t.Error("prior primary at height 10 was lost after a failed save at height 20")
	}

	// ── Step 5: the new primary at height 20 must NOT exist — the tmp rename
	// was aborted so a later restart does not load a partially-written file.
	if _, statErr := os.Stat(primaryH20); statErr == nil {
		t.Error("new primary at height 20 was created despite the failed prev-backup write")
	}
}

// ─── Test 5: findLatestSnapshot ignores prev files ───────────────────────────

// TestFindLatestSnapshot_IgnoresPrevFiles verifies that prev backup files do
// not influence findLatestSnapshot — only primary files are considered.
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
	got := findLatestSnapshot(dir, 2000)
	if got == nil {
		t.Fatal("findLatestSnapshot returned nil, want height 100")
	}
	if got.TipHeight != 100 {
		t.Errorf("findLatestSnapshot returned height %d, want 100 (prev files must be ignored)",
			got.TipHeight)
	}
}
