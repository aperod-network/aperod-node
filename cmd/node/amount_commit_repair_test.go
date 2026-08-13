package main

// amount_commit_repair_test.go — integration tests for the OOM-window
// AmountCommit validation introduced to prevent silent "forged commitment"
// failures after an OOM restart.
//
// Scenarios covered:
//  1. validateAmountCommitsFromBlocks detects a corrupt snapshot AmountCommit,
//     patches the in-memory UTXO set, AND updates the u/ LevelDB record so
//     the repair survives the next graceful startup.
//  2. After repair + corrected-snapshot save, a simulated "next clean startup"
//     via ReconcileWithStore does NOT undo the repair (i.e. the u/ store now
//     carries the correct value).
//  3. Clean-shutdown marker round-trip: writeCleanShutdownMarker →
//     readAndDeleteCleanShutdownMarker returns true and deletes the file;
//     subsequent call returns false (marker consumed).
//  4. Absent marker → readAndDeleteCleanShutdownMarker returns false.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// makeTestBlock constructs a minimal core.Block at the given height with one
// transaction whose single output has the given AmountCommit and OneTimePub.
// The block is written to db via PutRawBlock and also to the u/ store via
// PutUTXO (with an optionally different storeCommit to simulate corruption).
//
// Returns (txHash, raw JSON bytes).
func makeRepairTestBlock(
	t *testing.T,
	db *store.DB,
	height uint64,
	oneTimePub crypto.Point32,
	blockCommit crypto.Commitment, // authoritative: what the block stores
	storeCommit crypto.Commitment, // what the u/ store records (may differ = corrupt)
) (txHash crypto.Hash32) {
	t.Helper()

	tx := core.Transaction{
		Outputs: []core.Output{
			{
				OneTimePub:   oneTimePub,
				AmountCommit: blockCommit,
			},
		},
	}
	txHash = tx.Hash()

	blk := &core.Block{
		Header: core.BlockHeader{Height: height},
		Txs:    []core.Transaction{tx},
	}
	raw, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	if err := db.PutRawBlock(blk.Hash(), height, raw); err != nil {
		t.Fatalf("PutRawBlock: %v", err)
	}

	// Store the UTXO record with storeCommit (may differ from block).
	su := &store.StoredUTXO{
		TxHash:       txHash,
		OutputIndex:  0,
		OneTimePub:   oneTimePub,
		AmountCommit: storeCommit,
		BlockHeight:  height,
	}
	if err := db.PutUTXO(txHash, 0, su); err != nil {
		t.Fatalf("PutUTXO: %v", err)
	}
	return txHash
}

// TestValidateAmountCommits_PatchesInMemoryAndStore is the primary durability
// test.  It sets up a corrupted UTXO (snapshot and u/ store both carry wrong
// AmountCommit, raw block carries correct value), calls
// validateAmountCommitsFromBlocks, and asserts:
//   - in-memory AmountCommit is corrected
//   - u/ store record is corrected (durable repair)
//   - returned (checked, fixed) == (1, 1)
func TestValidateAmountCommits_PatchesInMemoryAndStore(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	var oneTimePub crypto.Point32
	oneTimePub[0] = 0x02 // non-zero sentinel

	var blockCommit crypto.Commitment
	blockCommit[0] = 0xAA // the correct (block-authoritative) value

	var corruptCommit crypto.Commitment
	corruptCommit[0] = 0xFF // the wrong value in snapshot + u/ store

	txHash := makeRepairTestBlock(t, db, 1, oneTimePub, blockCommit, corruptCommit)

	// Build a UTXOSet with the corrupted AmountCommit (simulates snapshot restore).
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       txHash,
		OutputIndex:  0,
		OneTimePub:   oneTimePub,
		AmountCommit: corruptCommit, // wrong — what the snapshot carried
		BlockHeight:  1,
	})

	log := discardLogger()
	checked, fixed := validateAmountCommitsFromBlocks(utxos, db, log)

	if checked != 1 {
		t.Errorf("checked = %d, want 1", checked)
	}
	if fixed != 1 {
		t.Errorf("fixed = %d, want 1 (store write must succeed for durable repair)", fixed)
	}

	// In-memory UTXO must carry the block-authoritative commit.
	u := utxos.Get(txHash, 0)
	if u == nil {
		t.Fatal("UTXO missing from set after repair")
	}
	if u.AmountCommit != blockCommit {
		t.Errorf("in-memory AmountCommit = %x, want %x", u.AmountCommit, blockCommit)
	}

	// u/ store record must also carry the corrected value.
	su, gerr := db.GetUTXO(txHash, 0)
	if gerr != nil || su == nil {
		t.Fatalf("GetUTXO after repair: err=%v su=%v", gerr, su)
	}
	if su.AmountCommit != blockCommit {
		t.Errorf("u/ store AmountCommit = %x, want %x (repair must be durable)", su.AmountCommit, blockCommit)
	}
}

// TestValidateAmountCommits_RepairSurvivesNextCleanStartup proves that the
// repair is durable: after validateAmountCommitsFromBlocks fixes the u/ record,
// a subsequent ReconcileWithStore (the first-pass check on the next clean
// startup) does NOT revert the in-memory UTXO to the old corrupt value.
func TestValidateAmountCommits_RepairSurvivesNextCleanStartup(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	var oneTimePub crypto.Point32
	oneTimePub[0] = 0x03

	var blockCommit crypto.Commitment
	blockCommit[1] = 0xBB

	var corruptCommit crypto.Commitment
	corruptCommit[1] = 0xCC

	txHash := makeRepairTestBlock(t, db, 2, oneTimePub, blockCommit, corruptCommit)

	// ── Phase 1: OOM restart — validateAmountCommitsFromBlocks runs ──────────
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       txHash,
		OutputIndex:  0,
		OneTimePub:   oneTimePub,
		AmountCommit: corruptCommit,
		BlockHeight:  2,
	})

	log := discardLogger()
	_, fixed := validateAmountCommitsFromBlocks(utxos, db, log)
	if fixed != 1 {
		t.Fatalf("phase 1: expected 1 durable fix, got %d", fixed)
	}

	// ── Phase 2: next clean startup — ReconcileWithStore must not undo repair ─
	// Simulate a fresh UTXOSet loaded from the snapshot (still has old value
	// in memory — the snapshot save happens after validation in the real path,
	// but for this test we focus on whether the store record is now correct).
	// Create a new UTXOSet with the CORRUPTED in-memory value (as if loaded
	// from the pre-repair snapshot file) and run ReconcileWithStore against
	// the now-corrected u/ store record.
	utxos2 := core.NewUTXOSet()
	utxos2.Add(&core.UTXO{
		TxHash:       txHash,
		OutputIndex:  0,
		OneTimePub:   oneTimePub,
		AmountCommit: corruptCommit, // still corrupt in the snapshot file
		BlockHeight:  2,
	})

	checked2, fixed2, _ := utxos2.ReconcileWithStore(
		func(txh crypto.Hash32, outIdx uint32) (*core.UTXO, bool) {
			su, gerr := db.GetUTXO(txh, outIdx)
			if gerr != nil || su == nil {
				return nil, false
			}
			return &core.UTXO{
				TxHash:       su.TxHash,
				OutputIndex:  su.OutputIndex,
				OneTimePub:   su.OneTimePub,
				TxPubKey:     su.TxPubKey,
				AmountCommit: su.AmountCommit, // should now be blockCommit
				EncAmount:    su.EncAmount,
				BlockHeight:  su.BlockHeight,
			}, true
		},
		nil, // no per-fix callback needed
	)
	if checked2 == 0 {
		t.Fatal("phase 2: ReconcileWithStore checked 0 UTXOs — u/ store must be readable")
	}
	// The store record now holds blockCommit, so ReconcileWithStore FIXES the
	// still-corrupt in-memory value rather than leaving it corrupt.
	if fixed2 != 1 {
		t.Errorf("phase 2: ReconcileWithStore fixed = %d, want 1 "+
			"(corrected store record must propagate to memory)", fixed2)
	}
	u2 := utxos2.Get(txHash, 0)
	if u2 == nil {
		t.Fatal("phase 2: UTXO missing from set")
	}
	if u2.AmountCommit != blockCommit {
		t.Errorf("phase 2: AmountCommit after ReconcileWithStore = %x, want %x "+
			"(the repair must survive a subsequent clean restart)", u2.AmountCommit, blockCommit)
	}
}

// TestValidateAmountCommits_CorrectCommitIsNoop confirms that a UTXO whose
// snapshot AmountCommit already matches the block is counted as checked but
// does NOT increment the fixed counter.
func TestValidateAmountCommits_CorrectCommitIsNoop(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	var oneTimePub crypto.Point32
	oneTimePub[0] = 0x04

	var commit crypto.Commitment
	commit[0] = 0x55

	txHash := makeRepairTestBlock(t, db, 3, oneTimePub, commit, commit) // both equal

	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       txHash,
		OutputIndex:  0,
		OneTimePub:   oneTimePub,
		AmountCommit: commit, // matches block — no mismatch
		BlockHeight:  3,
	})

	log := discardLogger()
	checked, fixed := validateAmountCommitsFromBlocks(utxos, db, log)
	if checked != 1 {
		t.Errorf("checked = %d, want 1", checked)
	}
	if fixed != 0 {
		t.Errorf("fixed = %d, want 0 (no mismatch should be recorded)", fixed)
	}
}

// TestCleanShutdownMarker_RoundTrip confirms the marker write → read → delete
// lifecycle and the fail-closed absent-marker case.
func TestCleanShutdownMarker_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Initially absent → reports unclean.
	if got := readAndDeleteCleanShutdownMarker(dir); got {
		t.Error("absent marker: readAndDeleteCleanShutdownMarker() = true, want false")
	}

	// Write marker → first read returns true (clean shutdown) and deletes marker.
	if err := writeCleanShutdownMarker(dir); err != nil {
		t.Fatalf("writeCleanShutdownMarker: %v", err)
	}
	if got := readAndDeleteCleanShutdownMarker(dir); !got {
		t.Error("present marker: readAndDeleteCleanShutdownMarker() = false, want true")
	}

	// Marker was consumed — second read returns false (unclean).
	if got := readAndDeleteCleanShutdownMarker(dir); got {
		t.Error("consumed marker: readAndDeleteCleanShutdownMarker() = true, want false (marker should be deleted)")
	}
}

// TestCleanShutdownMarker_WrittenAfterSnapshot is a logic check: writing the
// marker and immediately checking it is idempotent and the path is correct.
func TestCleanShutdownMarker_WrittenAfterSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := cleanShutdownMarkerPath(dir)

	if err := writeCleanShutdownMarker(dir); err != nil {
		t.Fatalf("writeCleanShutdownMarker: %v", err)
	}

	// File must exist after write.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("marker file missing after write: %v", err)
	}

	// Consume it.
	readAndDeleteCleanShutdownMarker(dir)

	// File must be gone after consume.
	if _, err := os.Stat(path); err == nil {
		t.Error("marker file still present after readAndDeleteCleanShutdownMarker")
	}
}
