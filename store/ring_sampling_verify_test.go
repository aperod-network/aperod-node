package store

import (
	"encoding/json"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/aperod/aperod/core"
)

func TestLPoDRingBatchMultiOutputCommitAndRewind(t *testing.T) {
	path := t.TempDir()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	parent := sparseRingOutput(9)
	putSamplingOutput(t, db, parent)
	makeBlock := func(height uint64, keys ...byte) *core.Block {
		tx := core.Transaction{}
		for _, k := range keys {
			u := sparseRingOutput(k)
			tx.Outputs = append(tx.Outputs, core.Output{OneTimePub: u.OneTimePub, AmountCommit: u.AmountCommit})
		}
		return &core.Block{Header: core.BlockHeader{Height: height}, Txs: []core.Transaction{tx}}
	}
	first, second := makeBlock(1, 1, 2), makeBlock(2, 3, 4)
	stage := func(rollback bool, blocks ...*core.Block) {
		t.Helper()
		db.ringMu.Lock()
		defer db.ringMu.Unlock()
		batch := new(leveldb.Batch)
		ring := db.newRingBatch(batch)
		for _, block := range blocks {
			if err := appendLPoDIndices(batch, ring, block, rollback); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	reopen := func() {
		t.Helper()
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		next, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		db = next
	}
	stage(false, first)
	stage(false, second)
	reopen()
	if inv, err := db.VerifyRingSamplingIndex(); err != nil || inv.Canonical != 5 || inv.Eligible != 5 {
		t.Fatalf("commit: %+v, %v", inv, err)
	}
	// Duplicate removal also covers an output seen by both the Sale and
	// LPoD rollback paths in one batch.
	stage(true, second, second, first)
	reopen()
	if inv, err := db.VerifyRingSamplingIndex(); err != nil || inv.Canonical != 1 || inv.Eligible != 1 {
		t.Fatalf("rewind: %+v, %v", inv, err)
	}
	if u, err := db.GetUTXO(parent.TxHash, parent.OutputIndex); err != nil || u == nil {
		t.Fatalf("rewind removed parent output: %v", err)
	}
}

func TestRingSamplingVerifyDetectsUnfencedLegacyWriter(t *testing.T) {
	db := samplingDB(t)
	for i := byte(1); i <= 4; i++ {
		putSamplingOutput(t, db, sparseRingOutput(i))
	}
	before, err := db.VerifyRingSamplingIndex()
	if err != nil || before.Canonical != 4 || before.Eligible != 4 {
		t.Fatal(before, err)
	}
	// Emulate an older binary: mutate only canonical rm/, leaving a valid
	// count/slot namespace. A marker-only check incorrectly accepts this.
	newMember := sparseRingOutput(7)
	raw, err := json.Marshal(newMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Put(ringMemberKey(newMember.OneTimePub), raw, &opt.WriteOptions{Sync: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.VerifyRingSamplingIndex(); err == nil {
		t.Fatal("stale ready marker passed full audit")
	}
	if err := db.InvalidateRingSamplingIndex(); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	after, err := db.VerifyRingSamplingIndex()
	if err != nil || after.Canonical != 5 || after.Eligible != 5 || after.CanonicalSHA256 == before.CanonicalSHA256 {
		t.Fatal(after, err)
	}
}

func TestRingSamplingVerifyCorruptionAndClosedBackend(t *testing.T) {
	for _, mutate := range []func(*DB) error{
		func(d *DB) error { return d.InvalidateRingSamplingIndex() },
		func(d *DB) error { return d.db.Put(ringSamplingSlotKey(1), make([]byte, 32), nil) },
		func(d *DB) error { return d.db.Put(ringSamplingSlotKey(4), sparseRingOutput(1).OneTimePub[:], nil) },
		func(d *DB) error {
			return d.db.Put(append(append([]byte{}, ringSamplingReversePrefix...), make([]byte, 32)...), ringSamplingUint(0), nil)
		},
		func(d *DB) error {
			return d.db.Put(append(append([]byte{}, ringSamplingSlotPrefix...), 1), []byte{1}, nil)
		},
		func(d *DB) error { return d.Close() },
	} {
		d := samplingDB(t)
		for i := byte(1); i <= 4; i++ {
			putSamplingOutput(t, d, sparseRingOutput(i))
		}
		if err := mutate(d); err != nil {
			t.Fatal(err)
		}
		if _, err := d.VerifyRingSamplingIndex(); err == nil {
			t.Fatal("invalid or closed backend accepted")
		}
	}
}
