package main

// Tests for the v1→v2 migration path introduced in task #1090.
//
// Covers:
//   1. loadLegacySnapshot rejects files whose "v" field is not snapVersionLegacy.
//   2. loadStartupSnapshotWithFallback loads a valid v1 primary and emits a
//      migration warning when no v2 snapshot exists.
//   3. loadStartupSnapshotWithFallback falls back to the v1 "-prev.json" backup
//      when the v1 primary is absent or damaged, and emits a migration warning.
//   4. loadStartupSnapshotWithFallback returns os.ErrNotExist when neither a v2
//      snapshot nor any v1 legacy file is present.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/aperod/aperod/core"
)

// writeLegacySnapFile writes a v1 uncompressed JSON snapshot to path.
// Used only in migration tests; production code never writes v1 files.
func writeLegacySnapFile(t *testing.T, path string, snap startupSnapshot) {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// makeLegacySnap returns a startupSnapshot with Version=snapVersionLegacy.
func makeLegacySnap(h uint64) startupSnapshot {
	return startupSnapshot{
		Version:    snapVersionLegacy,
		TipHeight:  h,
		TipHashHex: fmt.Sprintf("%064x", h),
		TxTotal:    int64(h),
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
}

// nullLogger returns a logger that discards all output.
func nullLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ─── Test 1: invalid legacy version is rejected ───────────────────────────────

// TestLoadLegacySnapshot_RejectsWrongVersion confirms that loadLegacySnapshot
// returns nil when the file's "v" field does not equal snapVersionLegacy.
// This prevents a corrupt or future-format file from being silently loaded just
// because its height and hash happen to match.
func TestLoadLegacySnapshot_RejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	h := uint64(77)
	hashHex := fmt.Sprintf("%064x", h)

	// Write a file with Version=0 (neither legacy v1 nor current v2).
	wrongVersion := startupSnapshot{
		Version:    0,
		TipHeight:  h,
		TipHashHex: hashHex,
	}
	path := legacySnapshotPath(dir, h)
	writeLegacySnapFile(t, path, wrongVersion)

	got := loadLegacySnapshot(path, h, hashHex)
	if got != nil {
		t.Errorf("loadLegacySnapshot: expected nil for wrong version 0, got non-nil snap (Version=%d)", got.Version)
	}

	// Write a file with Version=snapVersion (v2) — also must be rejected by the
	// legacy loader because it is not the expected legacy format.
	v2Version := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  h,
		TipHashHex: hashHex,
	}
	writeLegacySnapFile(t, path, v2Version)

	got2 := loadLegacySnapshot(path, h, hashHex)
	if got2 != nil {
		t.Errorf("loadLegacySnapshot: expected nil for v2 version %d, got non-nil snap (Version=%d)", snapVersion, got2.Version)
	}
}

// ─── Test 2: valid v1 primary is loaded with migration warning ────────────────

// TestMigration_V1PrimaryLoaded verifies that when a valid v1 uncompressed
// JSON snapshot exists at the correct height/hash and no v2 snapshot exists,
// loadStartupSnapshotWithFallback returns the snapshot and logs a migration
// warning.
func TestMigration_V1PrimaryLoaded(t *testing.T) {
	dir := t.TempDir()
	h := uint64(55)
	snap := makeLegacySnap(h)

	// Write a v1 primary file (plain JSON, no gzip).
	writeLegacySnapFile(t, legacySnapshotPath(dir, h), snap)

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got, _, err := loadStartupSnapshotWithFallback(dir, h, snap.TipHashHex, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil snapshot, got nil")
	}
	if got.TipHeight != h {
		t.Errorf("TipHeight: got %d want %d", got.TipHeight, h)
	}

	// A migration warning must have been logged.
	if !logContainsMsg(&logBuf, "loaded legacy uncompressed v1 snapshot; a compressed v2 snapshot will be written on next save") {
		t.Errorf("expected migration warning in log\nlog output:\n%s", logBuf.String())
	}
}

// ─── Test 3: v1 prev-backup loaded when v1 primary is absent ─────────────────

// TestMigration_V1PrevFallback confirms that when the v1 primary is absent (or
// corrupt) but a valid v1 "-prev.json" backup exists, the fallback returns the
// backup snapshot and logs an appropriate migration warning.  This mirrors the
// pre-upgrade recovery path: a node whose v1 primary was damaged before upgrade
// must not lose the fast path immediately after upgrading.
func TestMigration_V1PrevFallback(t *testing.T) {
	dir := t.TempDir()
	h := uint64(88)
	snap := makeLegacySnap(h)

	// Write only the v1 prev file — no v1 primary, no v2 snapshot.
	writeLegacySnapFile(t, legacySnapshotPrevPath(dir, h), snap)

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got, _, err := loadStartupSnapshotWithFallback(dir, h, snap.TipHashHex, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil snapshot from v1 prev-backup, got nil")
	}
	if got.TipHeight != h {
		t.Errorf("TipHeight: got %d want %d", got.TipHeight, h)
	}

	// The prev-backup migration warning must have been logged.
	if !logContainsMsg(&logBuf, "loaded legacy uncompressed v1 prev-backup snapshot; primary was absent or invalid; a compressed v2 snapshot will be written on next save") {
		t.Errorf("expected prev-backup migration warning in log\nlog output:\n%s", logBuf.String())
	}
}

// TestMigration_V1PrevFallback_CorruptPrimary is the same as
// TestMigration_V1PrevFallback but the v1 primary file exists and is corrupt
// (truncated JSON) rather than absent.
func TestMigration_V1PrevFallback_CorruptPrimary(t *testing.T) {
	dir := t.TempDir()
	h := uint64(91)
	snap := makeLegacySnap(h)

	// Write a corrupt v1 primary (truncated JSON).
	corruptPath := legacySnapshotPath(dir, h)
	if err := os.WriteFile(corruptPath, []byte(`{"v":1,"tip_height":91,"tip_hash"`), 0644); err != nil {
		t.Fatalf("write corrupt primary: %v", err)
	}

	// Write a valid v1 prev file.
	writeLegacySnapFile(t, legacySnapshotPrevPath(dir, h), snap)

	got, _, err := loadStartupSnapshotWithFallback(dir, h, snap.TipHashHex, nullLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil snapshot from v1 prev-backup, got nil")
	}
	if got.TipHeight != h {
		t.Errorf("TipHeight: got %d want %d", got.TipHeight, h)
	}
}

// ─── Test 4: no snapshot at all returns os.ErrNotExist ───────────────────────

// TestMigration_NonePresent confirms that when neither a v2 snapshot nor any v1
// legacy file exists, loadStartupSnapshotWithFallback returns an error that
// satisfies os.IsNotExist, which the caller interprets as "no fast path".
func TestMigration_NonePresent(t *testing.T) {
	dir := t.TempDir()
	h := uint64(33)
	hashHex := fmt.Sprintf("%064x", h)

	_, _, err := loadStartupSnapshotWithFallback(dir, h, hashHex, nullLogger())
	if err == nil {
		t.Fatal("expected error when no snapshot exists, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.ErrNotExist, got: %v", err)
	}
}

// ─── Test 5: v1 file with mismatched hash is not loaded ──────────────────────

// TestMigration_V1HashMismatch confirms that a v1 file whose hash field does not
// match the caller-supplied tip hash is not accepted by the migration path.
// Without this check a stale legacy snapshot at the same height could be loaded.
func TestMigration_V1HashMismatch(t *testing.T) {
	dir := t.TempDir()
	h := uint64(42)

	// Write a v1 file with a hash that does not match what the caller will supply.
	snap := makeLegacySnap(h)
	writeLegacySnapFile(t, legacySnapshotPath(dir, h), snap)

	wrongHash := fmt.Sprintf("%064x", uint64(9999))

	_, _, err := loadStartupSnapshotWithFallback(dir, h, wrongHash, nullLogger())
	if err == nil {
		t.Fatal("expected error (hash mismatch), got nil")
	}
	// The file EXISTS on disk but fails hash validation, so loadStartupSnapshotWithFallback
	// must return a non-NotExist error.  This lets logSnapshotStartupReason emit
	// startup_reason=corrupt_snapshot rather than startup_reason=no_snapshot,
	// which would incorrectly imply no snapshot was ever written.
	if os.IsNotExist(err) {
		t.Errorf("expected non-NotExist error (file exists but invalid), got os.ErrNotExist; err: %v", err)
	}
}
