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

import (
	"encoding/binary"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

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

// TestRepairHeightIndex_MissingBlockBody verifies the integrity-repair path
// still fires — and RepairHeightIndex still succeeds and persists — when BOTH
// the h/ height-index pointer AND the b/ block body are absent for the tip
// block.  This is the harder failure mode (partial rsync / OOM-kill) that the
// existing height-index tests do not cover: they zero only h/ and leave b/
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

	// First post-repair read via GetBlockByHeight is STILL nil: the repair
	// fixed the pointer, but the block bytes are gone.  This is the key
	// distinction versus the pointer-only corruption case — operators must run
	// --repair-db to restore the block body itself.
	if got, err := db.GetBlockByHeight(tipHeight); err != nil {
		t.Fatalf("GetBlockByHeight after repair: unexpected err: %v", err)
	} else if got != nil {
		t.Fatalf("GetBlockByHeight after repair = %v, want nil (block body still absent — run --repair-db)", got)
	}

	// Second open: assert the repaired h/ entry persisted (RepairHeightIndex
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
