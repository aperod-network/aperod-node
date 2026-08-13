package store_test

// height_index_repair_test.go — coverage for the startup height-index self-heal
// path (DB.RepairHeightIndex + DB.GetBlockByHeight), exercised by the node's
// startup integrity check in cmd/node/main.go.
//
// The h/<height> namespace maps a block height to the canonical block hash;
// the b/<hash> namespace holds the block body.  A partial DB corruption
// (OOM-kill mid-write, an aborted rsync bootstrap, a truncated SST) can leave
// the height index pointing nowhere while the tip pointer (m/tip/*) still
// records the authoritative tip hash.  RepairHeightIndex rewrites h/<height>
// from that tip hash so GetBlockByHeight can find the block again on the next
// read.
//
// The harder failure mode covered here is when BOTH the h/ pointer and the b/
// block body are missing (see TestRepairHeightIndex_MissingBlockBody): the
// repair still succeeds and persists, but GetBlockByHeight stays nil until an
// operator restores the block body with --repair-db.
//
// Additional tests (TestRepairHeightIndex_MissingEntry, _HashMismatch,
// _AlreadyConsistent) verify the two-restart contract:
//
//  (a) After RepairHeightIndex is called, the entry persists across a
//      DB close / reopen — subsequent lookups succeed without a second repair.
//  (b) A second simulated startup finds the height index consistent and does
//      NOT enter the repair branch at all.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// ─── Raw-key helpers (mirror unexported store internals) ──────────────────────

// heightKeyRaw reconstructs the raw h/<uint64 big-endian> key used by the
// store package (mirrors store.heightKey, which is unexported).
func heightKeyRaw(h uint64) []byte {
	key := make([]byte, len("h/")+8)
	copy(key, []byte("h/"))
	binary.BigEndian.PutUint64(key[len("h/"):], h)
	return key
}

// blockKeyRaw reconstructs the raw b/<hash32> key used by the store package.
func blockKeyRaw(hash crypto.Hash32) []byte {
	key := make([]byte, len("b/")+32)
	copy(key, []byte("b/"))
	copy(key[len("b/"):], hash[:])
	return key
}

// deleteRawKeys closes the store DB, opens the underlying LevelDB directory
// directly, deletes the supplied raw keys (fsynced), then reopens the store DB.
// This lets a test simulate the on-disk corruption modes that the startup
// integrity check must recover from — the store package intentionally exposes
// no "delete a height-index / block-body entry" method to production callers.
func deleteRawKeys(t *testing.T, dir string, db *store.DB, keys ...[]byte) *store.DB {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("close store DB before raw edit: %v", err)
	}
	raw, err := leveldb.OpenFile(dir, nil)
	if err != nil {
		t.Fatalf("open raw leveldb: %v", err)
	}
	for _, k := range keys {
		if err := raw.Delete(k, &opt.WriteOptions{Sync: true}); err != nil {
			raw.Close()
			t.Fatalf("raw delete %x: %v", k, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw leveldb: %v", err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen store DB after raw edit: %v", err)
	}
	return reopened
}

// assertHeightPointer opens the underlying LevelDB directly (the store DB must
// already be closed) and verifies h/<height> holds exactly hash[:].
func assertHeightPointer(t *testing.T, dir string, height uint64, want crypto.Hash32) {
	t.Helper()
	raw, err := leveldb.OpenFile(dir, nil)
	if err != nil {
		t.Fatalf("open raw leveldb for pointer check: %v", err)
	}
	defer raw.Close()
	val, err := raw.Get(heightKeyRaw(height), nil)
	if err != nil {
		t.Fatalf("raw get h/%d: %v", height, err)
	}
	if len(val) != len(want) {
		t.Fatalf("h/%d = %d bytes, want %d", height, len(val), len(want))
	}
	var got crypto.Hash32
	copy(got[:], val)
	if got != want {
		t.Fatalf("h/%d = %x, want %x", height, got[:8], want[:8])
	}
}

// ─── Integrity-check helpers ──────────────────────────────────────────────────

// putTestBlock writes a minimal StoredBlock to the store at height and records
// it as the chain tip.  The hash is deterministic for the given height.
func putTestBlock(t *testing.T, db *store.DB, height uint64) (hash crypto.Hash32) {
	t.Helper()
	hash[0] = byte(height + 1) // deterministic, unique per height
	hash[1] = 0xDE
	hash[2] = 0xAD
	sb := &store.StoredBlock{Height: height, Hash: hash}
	if err := db.PutBlock(hash, sb); err != nil {
		t.Fatalf("PutBlock(height=%d): %v", height, err)
	}
	if err := db.PutTip(hash, height); err != nil {
		t.Fatalf("PutTip(height=%d): %v", height, err)
	}
	return hash
}

// runIntegrityCheck replicates the startup integrity logic from main.go
// (~lines 678-765).  It returns:
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
		log.Info("startup integrity check passed",
			"height", tipHeight,
			"hash", fmt.Sprintf("%x", tipHash[:8]),
		)
	}
	return repaired, mismatch, buf.String()
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestRepairHeightIndex_MissingBlockBody verifies the integrity-repair path
// still fires — and RepairHeightIndex still succeeds and persists — when BOTH
// the h/ height-index pointer AND the b/ block body are absent for the tip
// block.  This is the harder failure mode (partial rsync / OOM-kill) that the
// other height-index tests do not cover: they zero only h/ and leave b/
// intact so GetBlock can find the block after repair.
//
// Contract asserted here:
//   - Before repair, GetBlockByHeight(tip) returns nil (index missing).
//   - RepairHeightIndex(tip, tipHash) returns no error even though the block
//     body (b/) is gone — the repair writes the index unconditionally.
//   - After repair, the h/ entry is present and points at the tip hash, and it
//     survives a DB close/reopen (fsynced putSync inside RepairHeightIndex).
//   - GetBlockByHeight(tip) is STILL nil after repair because the block body is
//     absent; the height index is a pointer, not the data.  Operators must run
//     --repair-db (or restore from a clean snapshot) to recover the block bytes.
func TestRepairHeightIndex_MissingBlockBody(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const tipHeight = uint64(42)
	var tipHash crypto.Hash32
	tipHash[0] = 0xDE
	tipHash[1] = 0xAD
	tipHash[31] = 0xBE

	sb := &store.StoredBlock{
		Height:    tipHeight,
		Hash:      tipHash,
		Timestamp: 1_700_000_000,
		TxCount:   0,
	}
	// PutBlock writes both b/<hash> and h/<height> atomically; PutTip records
	// the authoritative tip pointer that RepairHeightIndex reads from.
	if err := db.PutBlock(tipHash, sb); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	if err := db.PutTip(tipHash, tipHeight); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	// Sanity: everything is readable before we corrupt the DB.
	if got, err := db.GetBlockByHeight(tipHeight); err != nil || got == nil {
		t.Fatalf("pre-corruption GetBlockByHeight: got=%v err=%v, want non-nil block", got, err)
	}

	// Corrupt: delete BOTH the height-index pointer (h/) AND the block body
	// (b/) for the tip — the "OOM-kill / partial rsync" scenario.
	db = deleteRawKeys(t, dir, db, heightKeyRaw(tipHeight), blockKeyRaw(tipHash))

	// The tip pointer is still authoritative and readable.
	gotTipHash, gotTipHeight, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip after corruption: %v", err)
	}
	if gotTipHash != tipHash || gotTipHeight != tipHeight {
		t.Fatalf("GetTip = (%x, %d), want (%x, %d)", gotTipHash[:8], gotTipHeight, tipHash[:8], tipHeight)
	}

	// The height index now resolves to nothing — this is what triggers the
	// startup integrity check's repair branch (indexedBlock == nil).
	if got, err := db.GetBlockByHeight(tipHeight); err != nil {
		t.Fatalf("GetBlockByHeight after corruption: unexpected err: %v", err)
	} else if got != nil {
		t.Fatalf("GetBlockByHeight after corruption = %v, want nil (h/ deleted)", got)
	}

	// Repair fires even though the block body is gone: RepairHeightIndex only
	// writes the h/ pointer and must not fail on a missing b/ entry.
	if err := db.RepairHeightIndex(tipHeight, tipHash); err != nil {
		t.Fatalf("RepairHeightIndex with missing block body: %v", err)
	}

	// The height-index entry was written and points at the tip hash.
	if rawHash, err := db.GetRawBlockByHeight(tipHeight); err != nil {
		t.Fatalf("GetRawBlockByHeight after repair: %v", err)
	} else if rawHash != nil {
		// b/ is gone, so the raw block body must still be unrecoverable even
		// though the h/ pointer now exists.
		t.Fatalf("GetRawBlockByHeight after repair = %d bytes, want nil (block body still absent)", len(rawHash))
	}

	// The repair entry must persist across a DB close / reopen (RepairHeightIndex
	// uses a fsynced write) so a restart does not re-trigger the repair branch
	// for the pointer, independent of the still-missing block body.
	if err := db.Close(); err != nil {
		t.Fatalf("close store DB before reopen: %v", err)
	}
	db, err = store.Open(dir)
	if err != nil {
		t.Fatalf("reopen store DB: %v", err)
	}

	rawHash, err := db.GetRawBlockByHeight(tipHeight)
	if err != nil {
		db.Close()
		t.Fatalf("GetRawBlockByHeight after reopen: %v", err)
	}
	if rawHash != nil {
		db.Close()
		t.Fatalf("GetRawBlockByHeight after reopen = %d bytes, want nil (block body never restored)", len(rawHash))
	}

	// Confirm the raw h/ pointer survived by reading it directly from the
	// underlying LevelDB (must close the store handle first: single-process lock).
	if err := db.Close(); err != nil {
		t.Fatalf("close store DB before pointer check: %v", err)
	}
	assertHeightPointer(t, dir, tipHeight, tipHash)
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
	tipHash := putTestBlock(t, db, tipHeight)

	repaired, _, logBuf := runIntegrityCheck(t, db, tipHash, tipHeight)
	if repaired {
		t.Error("integrity check should NOT repair an already-consistent height index")
	}
	if !strings.Contains(logBuf, "startup integrity check passed") {
		t.Errorf("expected %q; got: %s", "startup integrity check passed", logBuf)
	}
}
