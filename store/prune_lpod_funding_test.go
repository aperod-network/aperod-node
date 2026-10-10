package store_test

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func putPruneTestBlock(t *testing.T, db *store.DB, height uint64) crypto.Hash32 {
	t.Helper()
	var hash crypto.Hash32
	binary.LittleEndian.PutUint64(hash[:8], height+1)
	sb := &store.StoredBlock{
		Height: height,
		Hash:   hash,
		TxData: []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"height":%d}`, height))},
	}
	if err := db.PutBlock(hash, sb); err != nil {
		t.Fatalf("PutBlock(height=%d): %v", height, err)
	}
	return hash
}

func TestPruneBlocksOlderThanPreservesLPoDFundingBodyAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	hashes := make([]crypto.Hash32, 5)
	for height := uint64(1); height <= 4; height++ {
		hashes[height] = putPruneTestBlock(t, db, height)
	}
	var activation [8]byte
	binary.LittleEndian.PutUint64(activation[:], 2)
	if err := db.PutMeta("lpod/activation_height/v1", activation[:]); err != nil {
		t.Fatal(err)
	}
	fundingBefore, err := db.GetRawBlock(hashes[2])
	if err != nil {
		t.Fatal(err)
	}

	pruned, err := db.PruneBlocksOlderThan(2)
	if err != nil {
		t.Fatalf("first prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("first prune erased %d blocks, want 1", pruned)
	}
	fundingAfter, err := db.GetRawBlock(hashes[2])
	if err != nil {
		t.Fatal(err)
	}
	if string(fundingAfter) != string(fundingBefore) {
		t.Fatal("funding block body changed during pruning")
	}
	ordinary, err := db.GetRawBlock(hashes[1])
	if err != nil {
		t.Fatal(err)
	}
	var prunedOrdinary store.StoredBlock
	if err := json.Unmarshal(ordinary, &prunedOrdinary); err != nil {
		t.Fatalf("decode pruned ordinary block: %v", err)
	}
	if len(prunedOrdinary.TxData) != 0 {
		t.Fatal("ordinary block transaction data was not pruned")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()

	pruned, err = db.PruneBlocksOlderThan(5)
	if err != nil {
		t.Fatalf("prune after restart: %v", err)
	}
	if pruned != 2 {
		t.Fatalf("prune after restart erased %d blocks, want 2", pruned)
	}
	pruned, err = db.PruneBlocksOlderThan(5)
	if err != nil {
		t.Fatalf("repeated prune: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("repeated prune erased %d blocks, want 0", pruned)
	}
	fundingAfter, err = db.GetRawBlock(hashes[2])
	if err != nil {
		t.Fatal(err)
	}
	if string(fundingAfter) != string(fundingBefore) {
		t.Fatal("funding block body changed after restart/repeated pruning")
	}
}

func TestPruneBlocksOlderThanFailsClosedOnMalformedLPoDActivation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *store.DB)
	}{
		{
			name: "malformed",
			setup: func(t *testing.T, db *store.DB) {
				if err := db.PutMeta("lpod/activation_height/v1", []byte{1, 2, 3}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "conflicting versions",
			setup: func(t *testing.T, db *store.DB) {
				var v1, v2 [8]byte
				binary.LittleEndian.PutUint64(v1[:], 1)
				binary.LittleEndian.PutUint64(v2[:], 2)
				if err := db.PutMeta("lpod/activation_height/v1", v1[:]); err != nil {
					t.Fatal(err)
				}
				if err := db.PutMeta("lpod/activation_height/v2", v2[:]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing funding block",
			setup: func(t *testing.T, db *store.DB) {
				var activation [8]byte
				binary.LittleEndian.PutUint64(activation[:], 2)
				if err := db.PutMeta("lpod/activation_height/v1", activation[:]); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			hash := putPruneTestBlock(t, db, 1)
			before, err := db.GetRawBlock(hash)
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, db)

			if _, err := db.PruneBlocksOlderThan(2); err == nil {
				t.Fatal("pruning succeeded with malformed/conflicting activation metadata")
			}
			after, err := db.GetRawBlock(hash)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("block body changed despite malformed/conflicting metadata")
			}
			cursor, err := db.GetMeta("prune_cursor")
			if err != nil {
				t.Fatal(err)
			}
			if cursor != nil {
				t.Fatalf("prune cursor advanced after failure: %x", cursor)
			}
		})
	}
}
