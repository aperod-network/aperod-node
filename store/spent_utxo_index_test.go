package store_test

// Tests for the spent-UTXO index (su/) and stake-block index (sb/) that
// enable the db-index fast-path startup scan (Task #461).

import (
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func randHash(t *testing.T, seed byte) crypto.Hash32 {
	t.Helper()
	var h crypto.Hash32
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

func putUTXO(t *testing.T, db *store.DB, txHash crypto.Hash32, outIdx uint32) {
	t.Helper()
	if err := db.PutUTXO(txHash, outIdx, &store.StoredUTXO{
		TxHash:      txHash,
		OutputIndex: outIdx,
		BlockHeight: 1,
	}); err != nil {
		t.Fatalf("PutUTXO: %v", err)
	}
}

// ─── MarkUTXOSpent / SpentUTXOIndexSize ──────────────────────────────────────

func TestMarkUTXOSpent_SizeIncreases(t *testing.T) {
	db := openTestDB(t)

	size0, err := db.SpentUTXOIndexSize()
	if err != nil {
		t.Fatalf("SpentUTXOIndexSize: %v", err)
	}
	if size0 != 0 {
		t.Fatalf("want 0, got %d", size0)
	}

	h := randHash(t, 0xAA)
	if err := db.MarkUTXOSpent(h, 0); err != nil {
		t.Fatalf("MarkUTXOSpent: %v", err)
	}
	if err := db.MarkUTXOSpent(h, 1); err != nil {
		t.Fatalf("MarkUTXOSpent idx=1: %v", err)
	}

	size2, err := db.SpentUTXOIndexSize()
	if err != nil {
		t.Fatalf("SpentUTXOIndexSize after marks: %v", err)
	}
	if size2 != 2 {
		t.Fatalf("want 2, got %d", size2)
	}
}

func TestMarkUTXOSpent_Idempotent(t *testing.T) {
	db := openTestDB(t)
	h := randHash(t, 0xBB)

	for i := 0; i < 3; i++ {
		if err := db.MarkUTXOSpent(h, 0); err != nil {
			t.Fatalf("MarkUTXOSpent attempt %d: %v", i, err)
		}
	}

	size, _ := db.SpentUTXOIndexSize()
	if size != 1 {
		t.Fatalf("idempotent: want 1 entry, got %d", size)
	}
}

func TestUnmarkUTXOSpent_RestoresActiveOutput(t *testing.T) {
	db := openTestDB(t)
	h := randHash(t, 0x42)
	putUTXO(t, db, h, 0)

	if err := db.MarkUTXOSpent(h, 0); err != nil {
		t.Fatalf("MarkUTXOSpent: %v", err)
	}
	if !db.IsUTXOSpent(h, 0) {
		t.Fatal("output was not marked spent")
	}
	if err := db.UnmarkUTXOSpent(h, 0); err != nil {
		t.Fatalf("UnmarkUTXOSpent: %v", err)
	}
	if db.IsUTXOSpent(h, 0) {
		t.Fatal("spent marker survived rollback")
	}

	active := 0
	if err := db.IterActiveUTXOs(func(*store.StoredUTXO) error {
		active++
		return nil
	}); err != nil {
		t.Fatalf("IterActiveUTXOs: %v", err)
	}
	if active != 1 {
		t.Fatalf("expected restored output in active iterator, got %d", active)
	}
}

// ─── IterActiveUTXOs ─────────────────────────────────────────────────────────

func TestIterActiveUTXOs_ExcludesSpent(t *testing.T) {
	db := openTestDB(t)

	h1 := randHash(t, 0x01)
	h2 := randHash(t, 0x02)
	h3 := randHash(t, 0x03)

	// Put 3 UTXOs; mark 2 as spent.
	putUTXO(t, db, h1, 0) // spent
	putUTXO(t, db, h2, 0) // active
	putUTXO(t, db, h3, 0) // spent

	if err := db.MarkUTXOSpent(h1, 0); err != nil {
		t.Fatalf("MarkUTXOSpent h1: %v", err)
	}
	if err := db.MarkUTXOSpent(h3, 0); err != nil {
		t.Fatalf("MarkUTXOSpent h3: %v", err)
	}

	var active []crypto.Hash32
	if err := db.IterActiveUTXOs(func(u *store.StoredUTXO) error {
		active = append(active, u.TxHash)
		return nil
	}); err != nil {
		t.Fatalf("IterActiveUTXOs: %v", err)
	}

	if len(active) != 1 {
		t.Fatalf("want 1 active UTXO, got %d", len(active))
	}
	if active[0] != h2 {
		t.Errorf("want h2 active, got %x", active[0][:4])
	}
}

func TestIterActiveUTXOs_EmptyDBReturnsNone(t *testing.T) {
	db := openTestDB(t)
	count := 0
	if err := db.IterActiveUTXOs(func(*store.StoredUTXO) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("IterActiveUTXOs on empty db: %v", err)
	}
	if count != 0 {
		t.Fatalf("want 0, got %d", count)
	}
}

func TestIterActiveUTXOs_AllSpentReturnsNone(t *testing.T) {
	db := openTestDB(t)
	h := randHash(t, 0xCC)
	putUTXO(t, db, h, 0)
	putUTXO(t, db, h, 1)
	_ = db.MarkUTXOSpent(h, 0)
	_ = db.MarkUTXOSpent(h, 1)

	count := 0
	if err := db.IterActiveUTXOs(func(*store.StoredUTXO) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("IterActiveUTXOs: %v", err)
	}
	if count != 0 {
		t.Fatalf("all spent: want 0 active, got %d", count)
	}
}

func TestIterActiveUTXOs_MultipleOutputsSameTx(t *testing.T) {
	db := openTestDB(t)
	h := randHash(t, 0xDD)

	// 3 outputs in same tx; spend only index 1.
	for i := uint32(0); i < 3; i++ {
		putUTXO(t, db, h, i)
	}
	_ = db.MarkUTXOSpent(h, 1)

	var got []uint32
	_ = db.IterActiveUTXOs(func(u *store.StoredUTXO) error {
		got = append(got, u.OutputIndex)
		return nil
	})
	if len(got) != 2 {
		t.Fatalf("want 2 active, got %d: %v", len(got), got)
	}
	for _, idx := range got {
		if idx == 1 {
			t.Errorf("spent output index 1 should not appear in active set")
		}
	}
}

// ─── PutStakeBlockHeight / HasStakeBlockIndex / IterStakeBlockHeights ─────────

func TestStakeBlockIndex_RoundTrip(t *testing.T) {
	db := openTestDB(t)

	has, err := db.HasStakeBlockIndex()
	if err != nil {
		t.Fatalf("HasStakeBlockIndex on empty: %v", err)
	}
	if has {
		t.Fatal("want false on empty db")
	}

	heights := []uint64{1, 5, 100, 999}
	for _, h := range heights {
		if err := db.PutStakeBlockHeight(h); err != nil {
			t.Fatalf("PutStakeBlockHeight(%d): %v", h, err)
		}
	}

	has, err = db.HasStakeBlockIndex()
	if err != nil || !has {
		t.Fatalf("HasStakeBlockIndex after inserts: has=%v err=%v", has, err)
	}

	var got []uint64
	if err := db.IterStakeBlockHeights(func(h uint64) error {
		got = append(got, h)
		return nil
	}); err != nil {
		t.Fatalf("IterStakeBlockHeights: %v", err)
	}

	if len(got) != len(heights) {
		t.Fatalf("want %d heights, got %d: %v", len(heights), len(got), got)
	}
	for i, want := range heights {
		if got[i] != want {
			t.Errorf("heights[%d]: want %d, got %d", i, want, got[i])
		}
	}
}

func TestStakeBlockIndex_Idempotent(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 5; i++ {
		if err := db.PutStakeBlockHeight(42); err != nil {
			t.Fatalf("PutStakeBlockHeight: %v", err)
		}
	}
	var count int
	_ = db.IterStakeBlockHeights(func(uint64) error { count++; return nil })
	if count != 1 {
		t.Fatalf("idempotent: want 1 entry, got %d", count)
	}
}

func TestStakeBlockIndex_AscendingOrder(t *testing.T) {
	db := openTestDB(t)
	// Insert out of order.
	for _, h := range []uint64{300, 1, 50, 200, 10} {
		_ = db.PutStakeBlockHeight(h)
	}
	var got []uint64
	_ = db.IterStakeBlockHeights(func(h uint64) error {
		got = append(got, h)
		return nil
	})
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("not ascending at [%d]: %d <= %d", i, got[i], got[i-1])
		}
	}
}
