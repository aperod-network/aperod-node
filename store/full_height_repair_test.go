package store_test

// full_height_repair_test.go — coverage for RepairAllHeightIndex and
// CheckAllHeightIndex, which repair and verify the h/ height index across
// ALL stored heights (not just the tip).
//
// Key scenarios covered:
//
//   DanglingHeightIndex
//     h/ has a non-zero hash but b/<hash> body is missing.  RepairAllHeightIndex
//     must count it as skipped (not repaired) so the sentinel is never written
//     for an incomplete sweep.  CheckAllHeightIndex must count it as broken.
//
//   RepairAllHeightIndex_AbsentAndDangling
//     One height has no h/ entry (absent) and another has a dangling h/ entry.
//     RepairAllHeightIndex repairs only the absent one (block body available),
//     skips the dangling one, and returns repaired=1, skipped=1.
//
//   RepairAllHeightIndex_SentinelNotWritten
//     When skipped > 0 the caller (runRepairHeightIndex) must not write the
//     sentinel.  This test verifies the store function correctly communicates
//     the incomplete sweep so callers can gate on skipped==0.
//
//   CheckAllHeightIndex_Consistent / _AbsentEntry / _DanglingEntry
//     Validates the three entry states CheckAllHeightIndex must distinguish.

import (
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// putChainBlocks writes n blocks at heights 0..n-1 to db, records the
// last one as tip, and returns the hashes in height order.
func putChainBlocks(t *testing.T, db *store.DB, n int) []crypto.Hash32 {
	t.Helper()
	hashes := make([]crypto.Hash32, n)
	for h := 0; h < n; h++ {
		var hash crypto.Hash32
		hash[0] = byte(h + 1)
		hash[1] = 0xCC
		sb := &store.StoredBlock{Height: uint64(h), Hash: hash}
		if err := db.PutBlock(hash, sb); err != nil {
			t.Fatalf("PutBlock(height=%d): %v", h, err)
		}
		hashes[h] = hash
	}
	tipHash := hashes[n-1]
	if err := db.PutTip(tipHash, uint64(n-1)); err != nil {
		t.Fatalf("PutTip: %v", err)
	}
	return hashes
}

// deleteBlockBody removes only the b/<hash> block body, leaving h/<height> intact.
// This simulates the "dangling height-index" scenario: h/ points to a hash
// whose block body is absent from the b/ namespace.
func deleteBlockBody(t *testing.T, dir string, db *store.DB, hash crypto.Hash32) *store.DB {
	t.Helper()
	return deleteRawKeys(t, dir, db, blockKeyRaw(hash))
}

// ─── RepairAllHeightIndex tests ───────────────────────────────────────────────

// TestRepairAllHeightIndex_DanglingEntry verifies that when h/<height> holds a
// non-zero hash whose b/<hash> body is missing, RepairAllHeightIndex counts it
// as skipped (not repaired) so callers never treat the sweep as fully successful.
func TestRepairAllHeightIndex_DanglingEntry(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// heights 0 and 1; dangling the body at height 1.
	hashes := putChainBlocks(t, db, 2)
	db = deleteBlockBody(t, dir, db, hashes[1])

	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	repaired, skipped, repErr := db.RepairAllHeightIndex(tipHeight, nil)
	db.Close()

	if repErr != nil {
		t.Fatalf("RepairAllHeightIndex: unexpected error: %v", repErr)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (no h/ entry was absent/mismatched)", repaired)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (dangling h/ pointing at missing b/)", skipped)
	}
}

// TestRepairAllHeightIndex_AbsentAndDangling exercises two distinct corruption
// modes in the same store:
//   - height 1: h/ is absent (deleted) but b/ body is present → must be REPAIRED.
//   - height 2: h/ is present (non-zero) but b/ body is missing → must be SKIPPED.
//
// Expected: repaired=1, skipped=1.
func TestRepairAllHeightIndex_AbsentAndDangling(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// 3 blocks: heights 0, 1, 2.
	hashes := putChainBlocks(t, db, 3)

	// Delete h/<1> only — block body remains.
	db = deleteRawKeys(t, dir, db, heightKeyRaw(1))
	// Delete b/<hash[2]> only — h/<2> remains (dangling).
	db = deleteBlockBody(t, dir, db, hashes[2])

	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	repaired, skipped, repErr := db.RepairAllHeightIndex(tipHeight, nil)
	db.Close()

	if repErr != nil {
		t.Fatalf("RepairAllHeightIndex: unexpected error: %v", repErr)
	}
	if repaired != 1 {
		t.Errorf("repaired = %d, want 1 (height 1 should be repaired)", repaired)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (height 2 is dangling and unrepairable)", skipped)
	}
}

// TestRepairAllHeightIndex_FullyConsistent confirms that an undamaged height
// index produces repaired=0, skipped=0 (no-op sweep).
func TestRepairAllHeightIndex_FullyConsistent(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	putChainBlocks(t, db, 4)
	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	repaired, skipped, repErr := db.RepairAllHeightIndex(tipHeight, nil)
	if repErr != nil {
		t.Fatalf("unexpected error: %v", repErr)
	}
	if repaired != 0 || skipped != 0 {
		t.Errorf("repaired=%d skipped=%d, want both 0 for a consistent store", repaired, skipped)
	}
}

// TestRepairAllHeightIndex_AbsentEntry confirms a deleted h/ entry is repaired
// when the b/ body is present, and both repaired=1 and skipped=0 are returned.
func TestRepairAllHeightIndex_AbsentEntry(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	putChainBlocks(t, db, 3)
	// Delete h/<1> — body still present.
	db = deleteRawKeys(t, dir, db, heightKeyRaw(1))

	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	repaired, skipped, repErr := db.RepairAllHeightIndex(tipHeight, nil)
	if repErr != nil {
		t.Fatalf("unexpected error: %v", repErr)
	}
	if repaired != 1 {
		t.Errorf("repaired = %d, want 1", repaired)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}

	// Verify the sentinel semantics contract: skipped==0 means sentinel MAY be
	// written; skipped>0 means it MUST NOT be written.  We test the store's
	// side of this (the actual write is gated in runRepairHeightIndex / startup).
	// Re-read via a fresh open to confirm the repaired entry persisted.
	db.Close()
	db2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	got, err := db2.GetBlockByHeight(1)
	if err != nil {
		t.Fatalf("GetBlockByHeight after repair: %v", err)
	}
	if got == nil {
		t.Fatal("height 1 still missing after repair — repaired entry did not persist")
	}
}

// TestRepairAllHeightIndex_SentinelGating confirms the caller contract: when
// RepairAllHeightIndex returns skipped>0, the sweep is incomplete and callers
// must not treat it as fully verified.  This test validates the sweepOK gate
// condition (repErr==nil && skipped==0) that the caller uses to decide whether
// to write the sentinel; it does NOT call StoreHeightIndexSentinel (which is a
// file-system operation owned by main.go, not the store package).
func TestRepairAllHeightIndex_SentinelGating(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	hashes := putChainBlocks(t, db, 2)
	db = deleteBlockBody(t, dir, db, hashes[1]) // height 1: dangling

	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	_, skipped, repErr := db.RepairAllHeightIndex(tipHeight, nil)
	db.Close()
	if repErr != nil {
		t.Fatalf("RepairAllHeightIndex: %v", repErr)
	}

	// Sentinel MUST NOT be written when skipped > 0.
	// The gate in main.go is: sweepOK := repErr == nil && skipped == 0
	sweepOK := repErr == nil && skipped == 0
	if sweepOK {
		t.Error("sweepOK was true despite skipped>0 — sentinel would be written incorrectly")
	}
	if skipped == 0 {
		t.Error("skipped = 0 for a dangling entry — RepairAllHeightIndex did not count it as unrepairable")
	}
}

// ─── CheckAllHeightIndex tests ────────────────────────────────────────────────

// TestCheckAllHeightIndex_Consistent verifies that a fully-populated, undamaged
// height index returns broken=0.
func TestCheckAllHeightIndex_Consistent(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	putChainBlocks(t, db, 5)
	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	broken, _, chkErr := db.CheckAllHeightIndex(tipHeight)
	if chkErr != nil {
		t.Fatalf("CheckAllHeightIndex: %v", chkErr)
	}
	if broken != 0 {
		t.Errorf("broken = %d, want 0 for a consistent store", broken)
	}
}

// TestCheckAllHeightIndex_AbsentEntry verifies that a deleted h/ entry is
// counted as broken by CheckAllHeightIndex.
func TestCheckAllHeightIndex_AbsentEntry(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	putChainBlocks(t, db, 3)
	db = deleteRawKeys(t, dir, db, heightKeyRaw(1))

	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	broken, firstBroken, chkErr := db.CheckAllHeightIndex(tipHeight)
	db.Close()

	if chkErr != nil {
		t.Fatalf("CheckAllHeightIndex: %v", chkErr)
	}
	if broken != 1 {
		t.Errorf("broken = %d, want 1", broken)
	}
	if firstBroken != 1 {
		t.Errorf("firstBroken = %d, want 1", firstBroken)
	}
}

// TestCheckAllHeightIndex_DanglingEntry is the critical new test: h/<height>
// holds a non-zero hash but b/<hash> is absent.  CheckAllHeightIndex must count
// this as broken — CountMissingHeights (h/-range scan only) would miss it.
func TestCheckAllHeightIndex_DanglingEntry(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	hashes := putChainBlocks(t, db, 3)
	// Delete only the block body at height 1; h/<1> still points to hashes[1].
	db = deleteBlockBody(t, dir, db, hashes[1])

	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	broken, firstBroken, chkErr := db.CheckAllHeightIndex(tipHeight)
	db.Close()

	if chkErr != nil {
		t.Fatalf("CheckAllHeightIndex: %v", chkErr)
	}
	if broken != 1 {
		t.Errorf("broken = %d, want 1 (dangling h/ entry must be detected)", broken)
	}
	if firstBroken != 1 {
		t.Errorf("firstBroken = %d, want 1", firstBroken)
	}
}

// TestCheckAllHeightIndex_CountMissingHeightsMissesIt demonstrates the gap that
// CheckAllHeightIndex was added to close: CountMissingHeights does NOT detect a
// dangling h/ entry (non-zero hash, missing body), while CheckAllHeightIndex does.
func TestCheckAllHeightIndex_CountMissingHeightsMissesIt(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	hashes := putChainBlocks(t, db, 3)
	db = deleteBlockBody(t, dir, db, hashes[1])

	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	// CountMissingHeights sees h/<1> (non-zero) so counts it as present.
	missing, _, cntErr := db.CountMissingHeights(tipHeight)
	if cntErr != nil {
		t.Fatalf("CountMissingHeights: %v", cntErr)
	}
	if missing != 0 {
		t.Fatalf("CountMissingHeights detected dangling entry as missing — test assumption wrong")
	}

	// CheckAllHeightIndex catches the dangling entry.
	broken, _, chkErr := db.CheckAllHeightIndex(tipHeight)
	db.Close()
	if chkErr != nil {
		t.Fatalf("CheckAllHeightIndex: %v", chkErr)
	}
	if broken == 0 {
		t.Error("CheckAllHeightIndex did NOT detect the dangling h/ entry — validator guard is incomplete")
	}
}

// TestRepairAllHeightIndex_PersistenceAfterRepair verifies that height-index
// entries written by RepairAllHeightIndex survive a DB close / reopen.
func TestRepairAllHeightIndex_PersistenceAfterRepair(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	putChainBlocks(t, db, 3)
	// Corrupt h/<0> and h/<1> (delete both).
	db = deleteRawKeys(t, dir, db, heightKeyRaw(0), heightKeyRaw(1))

	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	repaired, skipped, repErr := db.RepairAllHeightIndex(tipHeight, nil)
	if repErr != nil {
		t.Fatalf("RepairAllHeightIndex: %v", repErr)
	}
	if repaired != 2 {
		t.Errorf("repaired = %d, want 2", repaired)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	db.Close()

	// Reopen and confirm both repaired entries are readable.
	db2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	for _, h := range []uint64{0, 1} {
		got, err := db2.GetBlockByHeight(h)
		if err != nil {
			t.Fatalf("GetBlockByHeight(%d): %v", h, err)
		}
		if got == nil {
			t.Errorf("height %d: still missing after repair and reopen", h)
		}
	}

	// CheckAllHeightIndex must now pass.
	broken, _, chkErr := db2.CheckAllHeightIndex(tipHeight)
	if chkErr != nil {
		t.Fatalf("CheckAllHeightIndex after repair: %v", chkErr)
	}
	if broken != 0 {
		t.Errorf("broken = %d after repair, want 0", broken)
	}
}

// ─── Raw helper for writing a deliberately dangling h/ entry ──────────────────

// writeDanglingHeightEntry directly writes an h/<height> key that points to a
// hash with no corresponding b/<hash> entry.  Used to simulate an rsync that
// copied an h/ key but not its block body.
func writeDanglingHeightEntry(t *testing.T, dir string, db *store.DB, height uint64, danglingHash crypto.Hash32) *store.DB {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("close for raw write: %v", err)
	}
	raw, err := leveldb.OpenFile(dir, nil)
	if err != nil {
		t.Fatalf("open raw leveldb: %v", err)
	}
	if err := raw.Put(heightKeyRaw(height), danglingHash[:], &opt.WriteOptions{Sync: true}); err != nil {
		raw.Close()
		t.Fatalf("raw put h/%d: %v", height, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw leveldb: %v", err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen after raw write: %v", err)
	}
	return reopened
}

// TestRepairAllHeightIndex_DanglingViaRawWrite uses writeDanglingHeightEntry to
// directly inject an h/ entry pointing at a hash that was never written to b/.
// RepairAllHeightIndex must detect this and count it as skipped, not repaired.
func TestRepairAllHeightIndex_DanglingViaRawWrite(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// One real block at height 0 and tip.
	putChainBlocks(t, db, 1)

	// Inject a fake h/<1> pointing at a hash that has no b/ entry.
	var phantomHash crypto.Hash32
	phantomHash[0] = 0xFF
	phantomHash[1] = 0xEE
	db = writeDanglingHeightEntry(t, dir, db, 1, phantomHash)

	// Update tip to height 1 (with the phantom hash) so RepairAllHeightIndex
	// covers height 1 in its sweep.
	if err := db.PutTip(phantomHash, 1); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	repaired, skipped, repErr := db.RepairAllHeightIndex(1, nil)
	db.Close()

	if repErr != nil {
		t.Fatalf("RepairAllHeightIndex: %v", repErr)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0", repaired)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (phantom h/<1> must be counted as dangling)", skipped)
	}
}
