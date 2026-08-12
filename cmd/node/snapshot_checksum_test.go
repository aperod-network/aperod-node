package main

// Tests for the SHA-256 snapshot checksum sidecar (task #964).
//
// The sidecar guards against a corrupt or partially-written snapshot being
// silently deserialised into wrong chain state (double-spend / UTXO drift).
//
//  1. saveStartupSnapshot writes a sidecar whose digest matches the file.
//  2. A snapshot whose content no longer matches the sidecar is rejected
//     with a descriptive (non-NotExist) error and the loader falls back to
//     the prev-backup.
//  3. A snapshot without a sidecar (written by a pre-checksum binary) still
//     loads — verification is skipped, not failed.
//  4. A checksum mismatch with no fallback available surfaces as
//     startup_reason=corrupt_snapshot, not "no_snapshot".
//  5. Promotions (primary → prev-backup) carry the sidecar along.
//  6. deleteOldSnapshots keeps the sidecars of the retained files.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// writeGzipSnapshotFile writes snap to path in the exact production format
// (gzip-compressed JSON) WITHOUT touching any checksum sidecar — simulating
// an attacker or a faulty tool replacing the snapshot on disk.
func writeGzipSnapshotFile(t *testing.T, path string, snap startupSnapshot) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(snap); err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	// Pad past the 100-byte truncation guard so the checksum (not the size
	// check) is what rejects the file.
	for buf.Len() < 120 {
		// Re-encode with a longer hash field would change semantics; instead
		// grow the file by appending a second gzip member with padding JSON.
		gz2 := gzip.NewWriter(&buf)
		_, _ = gz2.Write([]byte("{}"))
		_ = gz2.Close()
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ─── 1. Sidecar written on save, digest matches file ─────────────────────────

func TestChecksum_WrittenOnSave(t *testing.T) {
	dir := t.TempDir()
	snap := makeSnap(100)
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	path := snapshotPath(dir, 100)
	raw, err := os.ReadFile(snapshotChecksumPath(path))
	if err != nil {
		t.Fatalf("checksum sidecar not written: %v", err)
	}
	want := strings.TrimSpace(string(raw))

	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	sum := sha256.Sum256(fileBytes)
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("sidecar digest %s does not match file digest %s", want, got)
	}

	// And the snapshot loads cleanly with the sidecar in place.
	if _, err := loadStartupSnapshot(dir, 100, snap.TipHashHex); err != nil {
		t.Errorf("loadStartupSnapshot with valid checksum: %v", err)
	}
}

// ─── 2. Mismatch rejected, falls back to prev-backup ─────────────────────────

func TestChecksum_MismatchFallsBackToPrev(t *testing.T) {
	dir := t.TempDir()

	// First save: original snapshot (TxTotal=100 via makeSnap).
	orig := makeSnap(200)
	orig.TxTotal = 111
	if err := saveStartupSnapshot(dir, orig); err != nil {
		t.Fatalf("save original: %v", err)
	}
	// Second save at the same height promotes the original to prev-backup
	// (with its sidecar) and writes a new primary.
	updated := makeSnap(200)
	updated.TxTotal = 222
	if err := saveStartupSnapshot(dir, updated); err != nil {
		t.Fatalf("save updated: %v", err)
	}

	// Silently replace the primary with DIFFERENT but structurally valid
	// content, leaving the sidecar untouched.  Without the checksum this
	// impostor would load successfully (version, height, and hash all match).
	impostor := makeSnap(200)
	impostor.TxTotal = 999
	writeGzipSnapshotFile(t, snapshotPath(dir, 200), impostor)

	snap, isRelaxed, err := loadStartupSnapshotWithFallback(
		dir, 200, orig.TipHashHex, discardLogger())
	if err != nil {
		t.Fatalf("expected fallback to prev-backup, got error: %v", err)
	}
	if isRelaxed {
		t.Error("fallback should use the strict-hash prev path, not relaxed")
	}
	if snap.TxTotal != 111 {
		t.Errorf("loaded TxTotal=%d, want 111 (the prev-backup) — impostor primary (999) must not be served", snap.TxTotal)
	}
}

// ─── 3. Missing sidecar keeps loading (pre-checksum snapshots) ────────────────

func TestChecksum_MissingSidecarStillLoads(t *testing.T) {
	dir := t.TempDir()
	snap := makeSnap(300)
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}
	if err := os.Remove(snapshotChecksumPath(snapshotPath(dir, 300))); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}
	if _, err := loadStartupSnapshot(dir, 300, snap.TipHashHex); err != nil {
		t.Errorf("snapshot without sidecar must still load (backward compat), got: %v", err)
	}
}

// ─── 4. Mismatch without fallback = corrupt, not missing ─────────────────────

func TestChecksum_MismatchIsCorruptNotMissing(t *testing.T) {
	dir := t.TempDir()
	snap := makeSnap(400)
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}
	// Corrupt the sidecar digest so verification must fail; no prev-backup
	// exists (first save at this height writes none).
	path := snapshotPath(dir, 400)
	bad := strings.Repeat("ab", sha256.Size)
	if err := os.WriteFile(snapshotChecksumPath(path), []byte(bad+"\n"), 0644); err != nil {
		t.Fatalf("write bad sidecar: %v", err)
	}

	_, _, err := loadStartupSnapshotWithFallback(dir, 400, snap.TipHashHex, discardLogger())
	if err == nil {
		t.Fatal("expected checksum-mismatch error, got nil")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("mismatch must be a corrupt (non-NotExist) error so startup_reason=corrupt_snapshot is logged, got: %v", err)
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error should identify the checksum mismatch, got: %v", err)
	}
}

// ─── 5. Promotion carries the sidecar to the prev-backup ─────────────────────

func TestChecksum_PromotionCopiesSidecar(t *testing.T) {
	dir := t.TempDir()
	if err := saveStartupSnapshot(dir, makeSnap(500)); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := saveStartupSnapshot(dir, makeSnap(500)); err != nil {
		t.Fatalf("save 2 (same height): %v", err)
	}
	prev := snapshotPrevPath(snapshotPath(dir, 500))
	raw, err := os.ReadFile(snapshotChecksumPath(prev))
	if err != nil {
		t.Fatalf("prev-backup sidecar not copied: %v", err)
	}
	fileBytes, err := os.ReadFile(prev)
	if err != nil {
		t.Fatalf("read prev: %v", err)
	}
	sum := sha256.Sum256(fileBytes)
	if got, want := hex.EncodeToString(sum[:]), strings.TrimSpace(string(raw)); got != want {
		t.Errorf("prev sidecar digest %s does not match prev file digest %s", want, got)
	}
}

// TestChecksum_PromotionWithoutSourceSidecarDropsStaleDst — when the promoted
// primary has NO sidecar (pre-checksum binary), any stale sidecar left at the
// prev path from an earlier promotion must be removed, or the fresh prev copy
// would fail verification despite being valid.
func TestChecksum_PromotionWithoutSourceSidecarDropsStaleDst(t *testing.T) {
	dir := t.TempDir()
	if err := saveStartupSnapshot(dir, makeSnap(600)); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := saveStartupSnapshot(dir, makeSnap(600)); err != nil {
		t.Fatalf("save 2: %v", err) // prev + prev sidecar now exist
	}
	primary := snapshotPath(dir, 600)
	// Simulate a pre-checksum binary: primary loses its sidecar, then is
	// promoted again by a third save.
	if err := os.Remove(snapshotChecksumPath(primary)); err != nil {
		t.Fatalf("remove primary sidecar: %v", err)
	}
	if err := saveStartupSnapshot(dir, makeSnap(600)); err != nil {
		t.Fatalf("save 3: %v", err)
	}
	prev := snapshotPrevPath(primary)
	if _, err := os.Stat(snapshotChecksumPath(prev)); err == nil {
		// A stale sidecar surviving would be tolerable ONLY if it matches; the
		// implementation removes it, which is the simpler correct behaviour.
		t.Error("stale prev sidecar should have been removed when source had none")
	}
	snap := tryLoadSnapshotFile(prev, 600)
	if snap == nil {
		t.Error("prev-backup without sidecar must load (verification skipped)")
	}
}

// ─── 6. deleteOldSnapshots keeps sidecars of retained files ──────────────────

func TestChecksum_DeleteOldSnapshotsKeepsSidecars(t *testing.T) {
	dir := t.TempDir()
	if err := saveStartupSnapshot(dir, makeSnap(700)); err != nil {
		t.Fatalf("save 700: %v", err)
	}
	if err := saveStartupSnapshot(dir, makeSnap(700)); err != nil {
		t.Fatalf("save 700 again: %v", err) // creates prev + sidecar
	}
	if err := saveStartupSnapshot(dir, makeSnap(710)); err != nil {
		t.Fatalf("save 710: %v", err)
	}
	deleteOldSnapshots(dir, 710)

	keepPrimary := snapshotPath(dir, 710)
	if _, err := os.Stat(snapshotChecksumPath(keepPrimary)); err != nil {
		t.Errorf("current primary's sidecar was deleted: %v", err)
	}
	// The old height-700 primary and its sidecar must be gone.
	oldPrimary := snapshotPath(dir, 700)
	if _, err := os.Stat(oldPrimary); err == nil {
		t.Error("old primary at 700 should be deleted")
	}
	if _, err := os.Stat(snapshotChecksumPath(oldPrimary)); err == nil {
		t.Error("old primary's sidecar at 700 should be deleted")
	}
}
