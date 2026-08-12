package store_test

// height_index_repair_test.go — verifies the startup self-heal for a zeroed or
// missing height-index entry.
//
// The production startup logic lives in blockchain/cmd/node/main.go
// (~lines 678-765).  This file tests the store primitives that logic relies on
// so that the self-heal contract can be verified without spinning up a full
// node:
//
//  (a) After RepairHeightIndex is called, the entry persists across a
//      DB close / reopen — subsequent lookups succeed without a second repair.
//  (b) A second simulated startup finds the height index consistent and does
//      NOT enter the repair branch at all.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// runIntegrityCheck replicates the startup integrity logic from main.go.
//
// It returns:
//   - repaired  true when RepairHeightIndex was called and succeeded
//   - mismatch  true when the index contained a hash that didn't match tipHash
//   - logBuf    the JSON-structured log lines emitted during the check
func runIntegrityCheck(
	t *testing.T,
	db *store.DB,
	tipHash crypto.Hash32,
	tipHeight uint64,
) (repaired bool, mismatch bool, logBuf string) {
	t.Helper()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	indexedBlock, idxErr := db.GetBlockByHeight(tipHeight)
	if idxErr != nil {
		t.Fatalf("runIntegrityCheck: GetBlockByHeight(%d): %v", tipHeight, idxErr)
	}

	switch {
	case indexedBlock == nil:
		// Missing or zeroed height-index entry — attempt self-repair.
		repErr := db.RepairHeightIndex(tipHeight, tipHash)
		if repErr != nil {
			t.Fatalf("runIntegrityCheck: RepairHeightIndex: %v", repErr)
		}
		log.Info("startup integrity: height index was missing/zeroed — repaired from tip pointer",
			"tip_height", tipHeight,
			"tip_hash", fmt.Sprintf("%x", tipHash[:8]),
		)
		repaired = true

	case indexedBlock.Hash != tipHash:
		// Hash mismatch — attempt self-repair.
		repErr := db.RepairHeightIndex(tipHeight, tipHash)
		if repErr != nil {
			t.Fatalf("runIntegrityCheck: RepairHeightIndex (mismatch): %v", repErr)
		}
		log.Info("startup integrity: height index hash mismatch — repaired from tip pointer",
			"tip_height", tipHeight,
			"tip_pointer_hash", fmt.Sprintf("%x", tipHash[:8]),
			"height_index_hash", fmt.Sprintf("%x", indexedBlock.Hash[:8]),
		)
		repaired = true
		mismatch = true

	default:
		// Height index is already consistent.
		log.Info("startup integrity check passed",
			"height", tipHeight,
			"hash", fmt.Sprintf("%x", tipHash[:8]),
		)
	}

	return repaired, mismatch, buf.String()
}

// putTestBlock writes a minimal raw block (just a JSON-encoded hash envelope)
// to the store at height and records it as the chain tip.
func putTestBlock(t *testing.T, db *store.DB, height uint64) (hash crypto.Hash32) {
	t.Helper()

	hash[0] = byte(height + 1) // deterministic, unique per height
	hash[1] = 0xDE
	hash[2] = 0xAD

	// Minimal stored block — only Hash and Height fields are needed for
	// the integrity check (GetBlockByHeight → GetBlock checks the hash).
	sb := &store.StoredBlock{
		Height: height,
		Hash:   hash,
	}
	if err := db.PutBlock(hash, sb); err != nil {
		t.Fatalf("PutBlock(height=%d): %v", height, err)
	}
	if err := db.PutTip(hash, height); err != nil {
		t.Fatalf("PutTip(height=%d): %v", height, err)
	}
	return hash
}

// TestRepairHeightIndex_MissingEntry verifies that:
//  1. After zeroing the height-index entry, GetBlockByHeight returns nil
//     (the "missing/zeroed" branch that main.go detects).
//  2. RepairHeightIndex writes the correct entry and it persists across
//     a DB close / reopen.
//  3. A second simulated startup sees a clean index, skips the repair
//     branch, and emits "startup integrity check passed".
func TestRepairHeightIndex_MissingEntry(t *testing.T) {
	dir := t.TempDir()

	// ── First open: write a block, zero the height index, run the repair ──────

	db1, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	const tipHeight = uint64(7)
	tipHash := putTestBlock(t, db1, tipHeight)

	// Deliberately corrupt the height-index entry by overwriting it with an
	// all-zero hash.  GetBlock(crypto.Hash32{}) will return nil because no
	// block is stored under the zero hash, so GetBlockByHeight will also
	// return nil — exactly the "missing/zeroed" condition main.go checks.
	var zeroHash crypto.Hash32
	if err := db1.RepairHeightIndex(tipHeight, zeroHash); err != nil {
		t.Fatalf("zeroing height index: %v", err)
	}

	// Confirm the corruption is visible.
	gotAfterZero, err := db1.GetBlockByHeight(tipHeight)
	if err != nil {
		t.Fatalf("GetBlockByHeight after zeroing: %v", err)
	}
	if gotAfterZero != nil {
		t.Fatal("expected GetBlockByHeight to return nil for zeroed height index, got a block")
	}

	// Run the startup integrity check — must detect missing and repair.
	repaired, _, logBuf := runIntegrityCheck(t, db1, tipHash, tipHeight)
	if !repaired {
		t.Fatal("integrity check should have repaired the zeroed height-index entry but did not")
	}
	if !strings.Contains(logBuf, "missing/zeroed") {
		t.Errorf("expected INFO log to contain %q; got: %s", "missing/zeroed", logBuf)
	}

	// Verify the entry is immediately accessible after repair (still in this open).
	gotAfterRepair, err := db1.GetBlockByHeight(tipHeight)
	if err != nil {
		t.Fatalf("GetBlockByHeight after repair: %v", err)
	}
	if gotAfterRepair == nil {
		t.Fatal("GetBlockByHeight returned nil after repair; expected the block")
	}
	if gotAfterRepair.Hash != tipHash {
		t.Errorf("repaired height-index hash = %x, want %x", gotAfterRepair.Hash[:8], tipHash[:8])
	}

	db1.Close()

	// ── Second open: verify the repair persisted across close / reopen ────────

	db2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	// GetTip must still return the same hash+height.
	storedHash, storedHeight, err := db2.GetTip()
	if err != nil {
		t.Fatalf("GetTip (second open): %v", err)
	}
	if storedHeight != tipHeight {
		t.Errorf("GetTip height = %d, want %d", storedHeight, tipHeight)
	}
	if storedHash != tipHash {
		t.Errorf("GetTip hash = %x, want %x", storedHash[:8], tipHash[:8])
	}

	// The height-index repair must be durable: GetBlockByHeight should now
	// return the block without any further repair.
	gotPersisted, err := db2.GetBlockByHeight(tipHeight)
	if err != nil {
		t.Fatalf("GetBlockByHeight (second open): %v", err)
	}
	if gotPersisted == nil {
		t.Fatal("repaired height-index entry did NOT persist across DB close/reopen")
	}
	if gotPersisted.Hash != tipHash {
		t.Errorf("persisted height-index hash = %x, want %x", gotPersisted.Hash[:8], tipHash[:8])
	}

	// Run the integrity check again (simulating the second restart).
	// It must NOT enter the repair branch.
	repaired2, _, logBuf2 := runIntegrityCheck(t, db2, storedHash, storedHeight)
	if repaired2 {
		t.Error("second startup integrity check should NOT have repaired anything (index is already consistent)")
	}
	if !strings.Contains(logBuf2, "startup integrity check passed") {
		t.Errorf("expected second startup to log %q; got: %s", "startup integrity check passed", logBuf2)
	}
}

// TestRepairHeightIndex_HashMismatch verifies the hash-mismatch variant of the
// startup integrity check: when the height-index holds a hash that differs from
// the tip pointer, the repair must update the index to match the tip pointer and
// the correction must survive a close / reopen.
func TestRepairHeightIndex_HashMismatch(t *testing.T) {
	dir := t.TempDir()

	db1, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Write two blocks at the same height (simulating a stale index after a
	// chain-tip override).  Block A is the real tip; block B is what the
	// height index incorrectly points to.
	const tipHeight = uint64(3)

	var hashA, hashB crypto.Hash32
	hashA[0] = 0xAA
	hashB[0] = 0xBB

	sbA := &store.StoredBlock{Height: tipHeight, Hash: hashA}
	sbB := &store.StoredBlock{Height: tipHeight, Hash: hashB}

	if err := db1.PutBlock(hashA, sbA); err != nil {
		t.Fatal(err)
	}
	if err := db1.PutBlock(hashB, sbB); err != nil {
		t.Fatal(err)
	}

	// Height index points at B; tip pointer points at A → mismatch.
	if err := db1.RepairHeightIndex(tipHeight, hashB); err != nil {
		t.Fatal(err)
	}
	if err := db1.PutTip(hashA, tipHeight); err != nil {
		t.Fatal(err)
	}

	// Integrity check must detect the mismatch and repair.
	repaired, mismatch, logBuf := runIntegrityCheck(t, db1, hashA, tipHeight)
	if !repaired {
		t.Fatal("integrity check should have repaired the hash mismatch")
	}
	if !mismatch {
		t.Fatal("integrity check should have flagged a hash mismatch")
	}
	if !strings.Contains(logBuf, "hash mismatch") {
		t.Errorf("expected INFO log to contain %q; got: %s", "hash mismatch", logBuf)
	}

	db1.Close()

	// After reopen, height index must point at hashA (the tip pointer).
	db2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	gotPersisted, err := db2.GetBlockByHeight(tipHeight)
	if err != nil {
		t.Fatalf("GetBlockByHeight (second open): %v", err)
	}
	if gotPersisted == nil {
		t.Fatal("repaired height-index entry did NOT persist across DB close/reopen")
	}
	if gotPersisted.Hash != hashA {
		t.Errorf("persisted height-index hash = %x, want %x (hashA)", gotPersisted.Hash[:8], hashA[:8])
	}

	// Second startup: no repair needed.
	repaired2, _, logBuf2 := runIntegrityCheck(t, db2, hashA, tipHeight)
	if repaired2 {
		t.Error("second startup should NOT repair an already-consistent index")
	}
	if !strings.Contains(logBuf2, "startup integrity check passed") {
		t.Errorf("expected %q in second-startup log; got: %s", "startup integrity check passed", logBuf2)
	}
}

// TestRepairHeightIndex_AlreadyConsistent is a sanity check confirming that
// when the height index is already consistent no repair is attempted.
func TestRepairHeightIndex_AlreadyConsistent(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const tipHeight = uint64(1)

	// putTestBlock calls PutBlock (which atomically writes block + height index)
	// so the store starts in a fully consistent state.
	tipHash := putTestBlock(t, db, tipHeight)

	repaired, _, logBuf := runIntegrityCheck(t, db, tipHash, tipHeight)
	if repaired {
		t.Error("integrity check should NOT repair an already-consistent height index")
	}
	if !strings.Contains(logBuf, "startup integrity check passed") {
		t.Errorf("expected %q; got: %s", "startup integrity check passed", logBuf)
	}
}

// silence unused-import check (json is used by the production code we import
// indirectly; keep it to match the test file style of the package).
var _ = json.Marshal
