package store_test

// Second round of store tests covering PutBlock/GetBlock, UTXO CRUD, Meta,
// IsKeyImageSpent, DeleteUTXO — all currently at 0% coverage.

import (
        "testing"

        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/store"
)

// ─── StoredBlock: PutBlock / GetBlock / GetBlockByHeight ─────────────────────

func TestDB_PutBlock_GetBlock(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        var hash crypto.Hash32
        hash[0] = 0xAA

        sb := &store.StoredBlock{
                Height:    5,
                Hash:      hash,
                PrevHash:  crypto.Hash32{},
                Timestamp: 1_000_000,
                TxCount:   2,
        }

        if err := db.PutBlock(hash, sb); err != nil {
                t.Fatalf("PutBlock: %v", err)
        }

        got, err := db.GetBlock(hash)
        if err != nil {
                t.Fatalf("GetBlock: %v", err)
        }
        if got == nil {
                t.Fatal("GetBlock returned nil")
        }
        if got.Height != 5 || got.TxCount != 2 {
                t.Errorf("GetBlock: height=%d txCount=%d, want 5,2", got.Height, got.TxCount)
        }
}

func TestDB_GetBlock_NotFound(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        var missing crypto.Hash32
        missing[0] = 0xFF
        got, err := db.GetBlock(missing)
        if err != nil {
                t.Fatalf("GetBlock not-found: unexpected error: %v", err)
        }
        if got != nil {
                t.Error("GetBlock must return nil for unknown hash")
        }
}

func TestDB_GetBlockByHeight_NotFound(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        got, err := db.GetBlockByHeight(99999)
        if err != nil {
                t.Fatalf("GetBlockByHeight not-found: unexpected error: %v", err)
        }
        if got != nil {
                t.Error("GetBlockByHeight must return nil for unknown height")
        }
}

func TestDB_GetBlockByHeight_Found(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        var hash crypto.Hash32
        hash[1] = 0xBB
        sb := &store.StoredBlock{Height: 7, Hash: hash, Timestamp: 99}
        _ = db.PutBlock(hash, sb)

        got, err := db.GetBlockByHeight(7)
        if err != nil {
                t.Fatalf("GetBlockByHeight: %v", err)
        }
        if got == nil || got.Height != 7 {
                t.Errorf("GetBlockByHeight: got %v, want height 7", got)
        }
}

// ─── UTXO CRUD ───────────────────────────────────────────────────────────────

func TestDB_UTXO_PutGetDelete(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        var txHash crypto.Hash32
        txHash[0] = 0x11
        outIdx := uint32(0)

        var pub crypto.Point32
        pub[0] = 0x42
        u := &store.StoredUTXO{
                TxHash:      txHash,
                OutputIndex: outIdx,
                OneTimePub:  pub,
                BlockHeight: 3,
        }

        // Put
        if err := db.PutUTXO(txHash, outIdx, u); err != nil {
                t.Fatalf("PutUTXO: %v", err)
        }

        // Get
        got, err := db.GetUTXO(txHash, outIdx)
        if err != nil {
                t.Fatalf("GetUTXO: %v", err)
        }
        if got == nil {
                t.Fatal("GetUTXO returned nil")
        }
        if got.BlockHeight != 3 || got.OneTimePub != pub {
                t.Errorf("GetUTXO: height=%d pub=%x", got.BlockHeight, got.OneTimePub[:4])
        }

        // Delete
        if err := db.DeleteUTXO(txHash, outIdx); err != nil {
                t.Fatalf("DeleteUTXO: %v", err)
        }

        // Should be gone
        after, err := db.GetUTXO(txHash, outIdx)
        if err != nil {
                t.Fatalf("GetUTXO after delete: %v", err)
        }
        if after != nil {
                t.Error("GetUTXO must return nil after DeleteUTXO")
        }
}

func TestDB_GetUTXO_NotFound(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        var txHash crypto.Hash32
        txHash[0] = 0xCC
        got, err := db.GetUTXO(txHash, 0)
        if err != nil {
                t.Fatalf("GetUTXO not-found: unexpected error: %v", err)
        }
        if got != nil {
                t.Error("GetUTXO must return nil for unknown key")
        }
}

func TestDB_UTXO_MultipleOutputs(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        var txHash crypto.Hash32
        txHash[0] = 0x22

        for i := uint32(0); i < 3; i++ {
                var pub crypto.Point32
                pub[0] = byte(i + 1)
                u := &store.StoredUTXO{TxHash: txHash, OutputIndex: i, OneTimePub: pub, BlockHeight: 1}
                if err := db.PutUTXO(txHash, i, u); err != nil {
                        t.Fatalf("PutUTXO[%d]: %v", i, err)
                }
        }

        count := 0
        _ = db.IterUTXOs(func(u *store.StoredUTXO) error {
                count++
                return nil
        })
        if count != 3 {
                t.Errorf("IterUTXOs: count=%d, want 3", count)
        }
}

// ─── Key Image: IsKeyImageSpent ───────────────────────────────────────────────

func TestDB_IsKeyImageSpent(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        var ki crypto.KeyImage
        ki[0] = 0x55

        spent, err := db.IsKeyImageSpent(ki)
        if err != nil {
                t.Fatal(err)
        }
        if spent {
                t.Error("key image must not be spent before MarkKeyImageSpent")
        }

        _ = db.MarkKeyImageSpent(ki)

        spent, err = db.IsKeyImageSpent(ki)
        if err != nil {
                t.Fatal(err)
        }
        if !spent {
                t.Error("key image must be spent after MarkKeyImageSpent")
        }
}

// ─── Meta: PutMeta / GetMeta ──────────────────────────────────────────────────

func TestDB_Meta_RoundTrip(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        if err := db.PutMeta("sync_state", []byte(`{"height":42}`)); err != nil {
                t.Fatalf("PutMeta: %v", err)
        }

        val, err := db.GetMeta("sync_state")
        if err != nil {
                t.Fatalf("GetMeta: %v", err)
        }
        if string(val) != `{"height":42}` {
                t.Errorf("GetMeta: got %q", val)
        }
}

func TestDB_Meta_NotFound(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        val, err := db.GetMeta("nonexistent")
        if err != nil {
                t.Fatalf("GetMeta not-found: unexpected error: %v", err)
        }
        if val != nil {
                t.Error("GetMeta must return nil for unknown key")
        }
}

// ─── GetTip: empty DB ─────────────────────────────────────────────────────────

func TestDB_GetTip_Empty(t *testing.T) {
        db, _ := store.Open(t.TempDir())
        defer db.Close()

        hash, height, err := db.GetTip()
        if err != nil {
                t.Fatalf("GetTip empty: %v", err)
        }
        var zero crypto.Hash32
        if hash != zero || height != 0 {
                t.Errorf("GetTip empty: hash=%x height=%d, want zero", hash[:4], height)
        }
}
