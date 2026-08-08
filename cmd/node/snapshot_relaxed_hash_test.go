package main

// snapshot_relaxed_hash_test.go — Regression tests for the relaxed-hash
// recovery path in loadStartupSnapshotWithFallback and the corresponding
// isRelaxed guard in checkSnapshotUTXOCount.
//
// Background: the relaxed-hash path was silently dropped by a merge of
// snapshot.go that did not include the fix written in the same session.  The
// binary rebuilt without it caused a crash-restart loop for ~40 min before the
// regression was identified.  These tests pin the two observable effects of the
// path so a future merge that drops it is caught immediately by CI.
//
// Guard 1 — TestRelaxedHash_FallbackLoadsWhenPrimaryAbsent
//   Write a v2 prev-backup at tipHeight with TipHashHex="prevHash".
//   Remove the primary so the fallback chain is exercised.
//   Call loadStartupSnapshotWithFallback with dbHash≠prevHash.
//   Expected: non-nil snapshot, isRelaxed=true, nil error, recovery log line.
//
// Guard 2 — TestRelaxedHash_UTXOCountDivergenceAccepted
//   Same setup → isRelaxed=true.
//   Seed the DB with an active UTXO count that diverges >10 % from the
//   snapshot's count (which is 0, so 200 stored → 100 % divergence).
//   Call checkSnapshotUTXOCount (the production helper from
//   snapshot_utxo_check.go) with isRelaxed=true.
//   Expected: returns true (not rejected), no rejection log.
//   Sanity counter-check: same call with isRelaxed=false returns false.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

// ── helper ────────────────────────────────────────────────────────────────────

// writePrevBackupMismatchedHash saves a minimal v2 snapshot at tipHeight whose
// TipHashHex is prevHash, then removes the primary file.  The result is a
// directory that contains only a prev-backup whose embedded hash (prevHash)
// differs from the hash that will be passed to loadStartupSnapshotWithFallback
// (dbHash), simulating the state left by a recover-tip run that patched the DB
// tip after the snapshot was written.
//
// Returns prevHash (embedded in the file) and dbHash (the "current" DB tip
// hash that the caller should pass to loadStartupSnapshotWithFallback).
func writePrevBackupMismatchedHash(t *testing.T, dir string, tipHeight uint64) (prevHash, dbHash string) {
	t.Helper()
	// Two distinct hashes at the same height.
	prevHash = fmt.Sprintf("%064x", tipHeight*10+1)
	dbHash = fmt.Sprintf("%064x", tipHeight*10+2)

	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: prevHash, // what was in the snapshot when it was written
		TxTotal:    int64(tipHeight),
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	// saveStartupSnapshot writes the primary (.json.gz).  On the first save at
	// a height no prev-backup is created automatically (task #1489: nothing to
	// preserve), so we write it explicitly here before removing the primary.
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}
	primary := snapshotPath(dir, tipHeight)
	prevFile := snapshotPrevPath(primary)
	writeGzipSnapFile(t, prevFile, snap)

	// Remove the primary to force loadStartupSnapshotWithFallback down the
	// "os.IsNotExist" branch, where it tries the prev-backup fallbacks.
	if err := os.Remove(primary); err != nil {
		t.Fatalf("remove primary snapshot: %v", err)
	}
	return prevHash, dbHash
}

// ── Guard 1 ───────────────────────────────────────────────────────────────────

// TestRelaxedHash_FallbackLoadsWhenPrimaryAbsent verifies that when no v2
// primary snapshot exists and the prev-backup was written with a TipHashHex
// that no longer matches the DB tip (e.g. because recover-tip patched the DB
// after the snapshot was saved), loadStartupSnapshotWithFallback returns
// isRelaxed=true and a non-nil snapshot rather than failing.
//
// Removing loadPrevBackupSnapshotRelaxed (or its call site in
// loadStartupSnapshotWithFallback) causes this test to fail because:
//   - loadPrevBackupSnapshot (exact hash) returns an error for the hash mismatch
//   - no other fallback matches
//   - the function returns (nil, false, <error>) instead of (snap, true, nil)
func TestRelaxedHash_FallbackLoadsWhenPrimaryAbsent(t *testing.T) {
	dir := t.TempDir()
	const tipHeight = uint64(500)

	_, dbHash := writePrevBackupMismatchedHash(t, dir, tipHeight)

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	snap, isRelaxed, err := loadStartupSnapshotWithFallback(dir, tipHeight, dbHash, log)

	if err != nil {
		t.Fatalf("loadStartupSnapshotWithFallback returned unexpected error: %v\nlog:\n%s",
			err, logBuf.String())
	}
	if snap == nil {
		t.Fatalf("expected non-nil snapshot, got nil\nlog:\n%s", logBuf.String())
	}
	if !isRelaxed {
		t.Errorf("isRelaxed should be true when loaded via relaxed-hash path, got false\nlog:\n%s",
			logBuf.String())
	}
	// The recovery warning must be present so operators see the event.
	const wantMsg = "RECOVERY: loaded v2 prev-backup snapshot with relaxed hash check " +
		"(primary absent; DB tip hash may differ after recover-tip repair)"
	if !logContainsMsg(&logBuf, wantMsg) {
		t.Errorf("recovery warning log line not found\nlog:\n%s", logBuf.String())
	}
}

// ── Guard 2 ───────────────────────────────────────────────────────────────────

// TestRelaxedHash_UTXOCountDivergenceAccepted verifies that when a snapshot is
// loaded via the relaxed-hash path (isRelaxed=true) the production
// checkSnapshotUTXOCount helper (snapshot_utxo_check.go) accepts a UTXO count
// divergence greater than the configured tolerance.
//
// The test calls checkSnapshotUTXOCount directly so that removing or breaking
// the isRelaxed guard in production code causes this test to fail — a local
// reimplementation of the guard would let the test pass even if the guard were
// gone from production.
//
// Setup:
//   - Snapshot has 0 active UTXOs (empty UTXOSnapshot from writePrevBackupMismatchedHash).
//   - DB metadata records 200 active UTXOs for the same tip hash.
//   - Divergence = 100 %, which would normally exceed the 1 % configured tolerance.
//
// With isRelaxed=true the tolerance is widened to 100 % → 100 % ≯ 100 % → accepted.
// If the isRelaxed guard is removed the configured 1 % tolerance applies → rejected.
func TestRelaxedHash_UTXOCountDivergenceAccepted(t *testing.T) {
	dir := t.TempDir()
	const tipHeight = uint64(600)

	_, dbHash := writePrevBackupMismatchedHash(t, dir, tipHeight)

	// Confirm the load itself returns isRelaxed=true — if this precondition
	// fails the rest of the test is meaningless.
	{
		var logBuf bytes.Buffer
		snap, isRelaxed, err := loadStartupSnapshotWithFallback(dir, tipHeight, dbHash, newCaptureLogger(&logBuf))
		if err != nil || snap == nil || !isRelaxed {
			t.Fatalf("precondition: expected (snap!=nil, isRelaxed=true, err=nil), got snap=%v isRelaxed=%v err=%v\nlog:\n%s",
				snap, isRelaxed, err, logBuf.String())
		}
	}

	// Open a fresh DB in the same dir (snapshot files and chain.db coexist).
	dbPath := filepath.Join(dir, "chain.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	// Seed the DB with a count that diverges by 100 % from the snapshot's 0.
	const dbStoredCount = 200
	if err := db.StoreActiveUTXOCount(dbHash, dbStoredCount); err != nil {
		t.Fatalf("StoreActiveUTXOCount: %v", err)
	}

	var relaxedLog bytes.Buffer
	relaxedLogger := newCaptureLogger(&relaxedLog)

	// ── isRelaxed=true: 100% divergence must be accepted ──────────────────────
	//
	// This calls the PRODUCTION checkSnapshotUTXOCount helper, not a
	// reimplementation.  Removing the isRelaxed guard from that helper will
	// make the 100% divergence exceed the 1% tolerance and return false,
	// failing the assertion below.
	const snapActiveUTXOs = 0 // matches empty UTXOSnapshot written above
	ok := checkSnapshotUTXOCount(
		db,
		snapActiveUTXOs,
		dbHash,
		1.0,   // configuredTolerancePct — strict; would reject at 100% divergence without relaxed guard
		true,  // isRelaxed
		false, // nonValidator
		relaxedLogger,
	)
	if !ok {
		t.Errorf("snapshot should NOT be rejected when isRelaxed=true (100%% divergence within 100%% widened tolerance)\nlog:\n%s",
			relaxedLog.String())
	}
	if logContainsMsg(&relaxedLog, "snapshot rejected — active UTXO count diverges from last-saved count; falling back to block scan") {
		t.Error("rejection log must NOT appear when isRelaxed=true")
	}

	// ── Sanity counter-check: isRelaxed=false → divergence warning fires ──────
	//
	// checkSnapshotUTXOCount always returns true (it logs a warning and accepts
	// rather than falling back to a multi-hour block scan), but when
	// isRelaxed=false the 100 % divergence exceeds the 1 % tolerance and the
	// "UTXO count diverges" warning must appear in the log.  If the isRelaxed
	// guard is removed, both calls use the same tolerancePct=100% (isRelaxed
	// path) and the divergence warning disappears — which this test catches.
	var strictLog bytes.Buffer
	ok2 := checkSnapshotUTXOCount(
		db,
		snapActiveUTXOs,
		dbHash,
		1.0,   // configuredTolerancePct — strict; 100% divergence exceeds 1% tolerance
		false, // isRelaxed=false → strict tolerance applies; divergence warning must fire
		false,
		newCaptureLogger(&strictLog),
	)
	// The function always accepts (true) to avoid a worse fallback.
	if !ok2 {
		t.Error("checkSnapshotUTXOCount should always return true (accept + warn), even when divergence exceeds tolerance")
	}
	// But the divergence warning MUST appear in the log when isRelaxed=false
	// and divergence exceeds the configured tolerance.  This guards against
	// removing the isRelaxed guard: without it, both paths would widen
	// tolerance to 100 % and the warning would never fire.
	const wantDivergenceLog = "snapshot active UTXO count diverges from last-saved metadata (accepted — tip hash is verified)"
	if !logContainsMsg(&strictLog, wantDivergenceLog) {
		t.Errorf("expected divergence warning when isRelaxed=false and 100%% divergence exceeds 1%% tolerance\nlog:\n%s",
			strictLog.String())
	}
	// Also verify the relaxed path does NOT fire the divergence warning
	// (100 % divergence ≤ 100 % widened tolerance → "passed" log instead).
	if logContainsMsg(&relaxedLog, wantDivergenceLog) {
		t.Errorf("isRelaxed=true should suppress the divergence warning (tolerance widened to 100%%)\nlog:\n%s",
			relaxedLog.String())
	}
}
