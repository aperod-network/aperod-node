package main

// Tests for the UTXO count divergence check added in task #1057.
//
// The check runs in the exact-tip snapshot fast path (main.go) before
// RestoreFromSnapshot is called.  It compares the snapshot's active UTXO
// count against the durable "active_utxo_count/<tipHashHex>" metadata
// persisted in the DB whenever a snapshot is saved successfully.  Keying by
// tip hash ensures concurrent goroutines saving snapshots at different heights
// never overwrite each other's entry.
//
// The tests replicate the divergence-check logic inline (the same pattern used
// by the other snapshot_*_test.go files) so they remain independent of the
// run() call and work with the same helpers — buildChainInStore, newCaptureLogger,
// logContainsMsg — already defined in snapshot_restart_test.go.
//
// Test matrix:
//   1. Within-tolerance  — snapshot count matches stored count → accepted.
//   2. Exceeds tolerance — snapshot count diverges beyond 1%   → rejected, warning logged.
//   3. No metadata key   — DB has no entry for this hash        → check skipped, snapshot accepted.
//   4. Historical vs active separation — total stored UTXOs >> active count is not penalised.
//   5. Store/Load round-trip with hash key — unit test for the DB helpers.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

// ── shared helper ─────────────────────────────────────────────────────────────

// simulateUTXOCountCheck replicates the divergence-check block from main.go so
// tests can exercise the same logic without calling run().  Returns (snapLoaded,
// log output).
//
// The caller is responsible for pre-seeding db with the appropriate
// StoreActiveUTXOCount entry (keyed by tipHashHex) before calling this
// function.  If no entry is stored, the check is skipped and the snapshot is
// accepted (as on first run).
func simulateUTXOCountCheck(
	t *testing.T,
	db *store.DB,
	snapUTXOCount int,
	tolerancePct float64,
	dir string,
	tipHeight uint64,
	tipHashHex string,
) (bool, bytes.Buffer) {
	t.Helper()

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	// Build a minimal snapshot with the requested active UTXO count.
	activeUTXOs := make([]*core.UTXO, snapUTXOCount)
	for i := range activeUTXOs {
		activeUTXOs[i] = &core.UTXO{BlockHeight: uint64(i + 1)}
	}
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		TxTotal:    0,
		UTXOs: core.UTXOSnapshot{
			ActiveUTXOs: activeUTXOs,
		},
		Registry: core.RegistrySnapshot{
			Validators: map[string]*core.ValidatorEntry{},
		},
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	loaded, serr := loadStartupSnapshot(dir, tipHeight, tipHashHex)
	if serr != nil {
		if !os.IsNotExist(serr) {
			log.Warn("snapshot load error, falling back to block scan", "err", serr)
		}
		return false, logBuf
	}

	// ── divergence check (mirrors main.go) ────────────────────────────────────
	utxoCountOK := true
	snapCount := len(loaded.UTXOs.ActiveUTXOs)
	// Load the hash-keyed metadata entry for this specific snapshot.
	dbActiveCount, dbHasCount, countErr := db.LoadActiveUTXOCount(tipHashHex)
	if countErr != nil {
		log.Warn("snapshot UTXO count check skipped — db metadata error", "err", countErr)
	} else if !dbHasCount {
		log.Info("snapshot UTXO count check skipped — no prior active count in db (first snapshot)")
	} else {
		diff := snapCount - dbActiveCount
		if diff < 0 {
			diff = -diff
		}
		larger := dbActiveCount
		if snapCount > larger {
			larger = snapCount
		}
		var diffPct float64
		if larger > 0 {
			diffPct = float64(diff) / float64(larger) * 100.0
		}
		if diffPct > tolerancePct {
			log.Warn("snapshot rejected — active UTXO count diverges from last-saved count; falling back to block scan",
				"snapshot_active_utxos", snapCount,
				"db_last_active_utxos", dbActiveCount,
				"diff_pct", fmt.Sprintf("%.2f%%", diffPct),
				"tolerance_pct", tolerancePct,
			)
			utxoCountOK = false
		} else {
			log.Info("snapshot UTXO count check passed",
				"snapshot_active_utxos", snapCount,
				"db_last_active_utxos", dbActiveCount,
				"diff_pct", fmt.Sprintf("%.2f%%", diffPct),
			)
		}
	}

	snapLoaded := false
	if utxoCountOK {
		snapLoaded = true
		log.Info("startup fast path complete — snapshot loaded",
			"tip_height", tipHeight,
			"active_utxos", snapCount,
		)
	}

	return snapLoaded, logBuf
}

// ── Test 1: within tolerance → accepted ───────────────────────────────────────

// TestUTXOCountCheck_WithinTolerance verifies that a snapshot whose active UTXO
// count differs from the stored metadata count by less than the configured
// tolerance is accepted (snapLoaded=true) and logs the pass message.
func TestUTXOCountCheck_WithinTolerance(t *testing.T) {
	dir := t.TempDir()
	db, _ := buildChainInStore(t, dir, 2)

	tipHashHex := fmt.Sprintf("%064x", 10)

	// Store a metadata count of 100 active UTXOs keyed by the tip hash.
	if err := db.StoreActiveUTXOCount(tipHashHex, 100); err != nil {
		t.Fatalf("StoreActiveUTXOCount: %v", err)
	}

	// Snapshot claims 99 active UTXOs — 0.99% diff from 100, within 1.0% tolerance.
	snapLoaded, logBuf := simulateUTXOCountCheck(t, db, 99, 1.0, dir, 10, tipHashHex)

	if !snapLoaded {
		t.Error("snapLoaded should be true when UTXO count is within tolerance")
		t.Logf("log:\n%s", logBuf.String())
	}
	if !logContainsMsg(&logBuf, "snapshot UTXO count check passed") {
		t.Error("expected \"snapshot UTXO count check passed\" log message")
		t.Logf("log:\n%s", logBuf.String())
	}
	if logContainsMsg(&logBuf, "snapshot rejected") {
		t.Error("rejection log must NOT appear when count is within tolerance")
	}
}

// ── Test 2: exceeds tolerance → rejected ─────────────────────────────────────

// TestUTXOCountCheck_ExceedsTolerance verifies that a snapshot whose active UTXO
// count diverges beyond the configured tolerance is rejected (snapLoaded=false),
// the rejection warning is logged, and the fast-path success message is absent.
func TestUTXOCountCheck_ExceedsTolerance(t *testing.T) {
	dir := t.TempDir()
	db, _ := buildChainInStore(t, dir, 3)

	tipHashHex := fmt.Sprintf("%064x", 20)

	// Store a metadata count of 200 active UTXOs keyed by the tip hash.
	if err := db.StoreActiveUTXOCount(tipHashHex, 200); err != nil {
		t.Fatalf("StoreActiveUTXOCount: %v", err)
	}

	// Snapshot claims only 50 active UTXOs — 75% divergence, far above 1%.
	snapLoaded, logBuf := simulateUTXOCountCheck(t, db, 50, 1.0, dir, 20, tipHashHex)

	if snapLoaded {
		t.Error("snapLoaded should be false when UTXO count exceeds tolerance")
		t.Logf("log:\n%s", logBuf.String())
	}
	if !logContainsMsg(&logBuf, "snapshot rejected — active UTXO count diverges from last-saved count; falling back to block scan") {
		t.Error("expected rejection warning was not logged")
		t.Logf("log:\n%s", logBuf.String())
	}
	if logContainsMsg(&logBuf, "startup fast path complete — snapshot loaded") {
		t.Error("fast-path success log must NOT appear when snapshot is rejected")
	}
}

// ── Test 3: no metadata key → check skipped, snapshot accepted ───────────────

// TestUTXOCountCheck_NoMetadata verifies that when the DB has no entry for this
// specific tip hash (e.g. the process crashed before the metadata write, or
// this is the node's first snapshot), the check is skipped and the snapshot is
// still accepted.
func TestUTXOCountCheck_NoMetadata(t *testing.T) {
	dir := t.TempDir()
	db, _ := buildChainInStore(t, dir, 1)
	// Deliberately do NOT call db.StoreActiveUTXOCount — the key should be absent.

	tipHashHex := fmt.Sprintf("%064x", 5)
	snapLoaded, logBuf := simulateUTXOCountCheck(t, db, 42, 1.0, dir, 5, tipHashHex)

	if !snapLoaded {
		t.Error("snapLoaded should be true when no metadata count is present (check skipped)")
		t.Logf("log:\n%s", logBuf.String())
	}
	if !logContainsMsg(&logBuf, "snapshot UTXO count check skipped — no prior active count in db (first snapshot)") {
		t.Error("expected skip log message was not emitted")
		t.Logf("log:\n%s", logBuf.String())
	}
}

// ── Test 4: historical DB records do not affect the check ────────────────────

// TestUTXOCountCheck_HistoricalRecordsIgnored is the key correctness test:
// the DB's u/ prefix may contain many more records than the active UTXO count
// (spent/staked outputs are never deleted from the historical store).
// The divergence check must compare against the hash-keyed active_utxo_count
// metadata — not the raw u/ record count — so a node with many historical
// spends is never incorrectly penalised.
func TestUTXOCountCheck_HistoricalRecordsIgnored(t *testing.T) {
	dir := t.TempDir()
	db, blocks := buildChainInStore(t, dir, 4)

	tip := blocks[len(blocks)-1]
	tipHashArr := tip.Hash()
	tipHashHex := fmt.Sprintf("%x", tipHashArr[:])

	// Simulate a chain where 300 outputs were created historically but only
	// 50 are currently unspent.  Write 300 u/ records to the DB to make the
	// historical count clearly different from the active count.
	for i := 0; i < 300; i++ {
		var txHash [32]byte
		txHash[0] = byte(i)
		txHash[1] = byte(i >> 8)
		su := &store.StoredUTXO{
			TxHash:      txHash,
			OutputIndex: 0,
			BlockHeight: 1,
		}
		if err := db.PutUTXO(txHash, 0, su); err != nil {
			t.Fatalf("PutUTXO[%d]: %v", i, err)
		}
	}

	// Store the active count as 50 (not 300), keyed by the real tip hash.
	if err := db.StoreActiveUTXOCount(tipHashHex, 50); err != nil {
		t.Fatalf("StoreActiveUTXOCount: %v", err)
	}

	// Snapshot claims 50 active UTXOs — matches the stored active count exactly.
	// This must be accepted even though there are 300 u/ records in the DB.
	snapLoaded, logBuf := simulateUTXOCountCheck(
		t, db, 50, 1.0, dir, tip.Header.Height, tipHashHex,
	)

	if !snapLoaded {
		t.Error("snapLoaded should be true: 50 active UTXOs matches stored active count of 50, regardless of 300 historical u/ records")
		t.Logf("log:\n%s", logBuf.String())
	}
	if !logContainsMsg(&logBuf, "snapshot UTXO count check passed") {
		t.Error("expected pass log message was not emitted")
		t.Logf("log:\n%s", logBuf.String())
	}
}

// ── Test 5: hash-keyed metadata does not cross-contaminate heights ────────────

// TestUTXOCountCheck_HashKeyedIsolation verifies that metadata stored for one
// tip hash is not visible when loading the check for a different tip hash.
// This guarantees that concurrent snapshot goroutines at different heights
// cannot overwrite each other's entry.
func TestUTXOCountCheck_HashKeyedIsolation(t *testing.T) {
	dir := t.TempDir()
	db, blocks := buildChainInStore(t, dir, 5)

	// Use two distinct hashes that could coexist during concurrent saves.
	hashA := fmt.Sprintf("%064x", 100)
	hashB := fmt.Sprintf("%x", blocks[len(blocks)-1].Hash())

	// Store count=100 for hashA, count=50 for hashB.
	if err := db.StoreActiveUTXOCount(hashA, 100); err != nil {
		t.Fatalf("StoreActiveUTXOCount(hashA): %v", err)
	}
	if err := db.StoreActiveUTXOCount(hashB, 50); err != nil {
		t.Fatalf("StoreActiveUTXOCount(hashB): %v", err)
	}

	// Load for hashA — must return 100, not 50.
	nA, okA, err := db.LoadActiveUTXOCount(hashA)
	if err != nil || !okA || nA != 100 {
		t.Errorf("LoadActiveUTXOCount(hashA): got n=%d ok=%v err=%v, want n=100 ok=true err=nil", nA, okA, err)
	}

	// Load for hashB — must return 50, not 100.
	nB, okB, err := db.LoadActiveUTXOCount(hashB)
	if err != nil || !okB || nB != 50 {
		t.Errorf("LoadActiveUTXOCount(hashB): got n=%d ok=%v err=%v, want n=50 ok=true err=nil", nB, okB, err)
	}
}

// ── Test 6: StoreActiveUTXOCount / LoadActiveUTXOCount round-trip ────────────

// TestActiveUTXOCountMetadataRoundTrip verifies that the store helpers correctly
// persist and retrieve the active UTXO count, and that LoadActiveUTXOCount
// correctly signals ok=false when the key has never been written.
func TestActiveUTXOCountMetadataRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chain.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	hashHex := fmt.Sprintf("%064x", 42)

	// Before any write: ok must be false.
	n, ok, err := db.LoadActiveUTXOCount(hashHex)
	if err != nil {
		t.Fatalf("LoadActiveUTXOCount (before store): %v", err)
	}
	if ok {
		t.Errorf("ok should be false before any value is stored, got n=%d", n)
	}

	// Store a value.
	if err := db.StoreActiveUTXOCount(hashHex, 12345); err != nil {
		t.Fatalf("StoreActiveUTXOCount(12345): %v", err)
	}

	// After write: ok must be true and value must round-trip.
	n, ok, err = db.LoadActiveUTXOCount(hashHex)
	if err != nil {
		t.Fatalf("LoadActiveUTXOCount (after store): %v", err)
	}
	if !ok {
		t.Error("ok should be true after storing a value")
	}
	if n != 12345 {
		t.Errorf("loaded count = %d, want 12345", n)
	}

	// A different hash must still return ok=false (key isolation).
	otherHash := fmt.Sprintf("%064x", 99)
	_, okOther, err := db.LoadActiveUTXOCount(otherHash)
	if err != nil {
		t.Fatalf("LoadActiveUTXOCount (other hash): %v", err)
	}
	if okOther {
		t.Error("ok should be false for a hash that was never stored")
	}
}
