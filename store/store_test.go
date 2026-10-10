package store_test

import (
        "encoding/json"
        "fmt"
        "os"
        "testing"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/store"
)

func TestDB_PutGetRawBlock(t *testing.T) {
        dir := t.TempDir()
        db, err := store.Open(dir)
        if err != nil {
                t.Fatal(err)
        }
        defer db.Close()

        // Create a minimal block
        priv, pub, _ := crypto.GenerateValidatorKey()
        hdr := core.BlockHeader{
                Height:       0,
                Timestamp:    1000000,
                ValidatorPub: pub,
        }
        _ = hdr.Sign(priv)
        block := &core.Block{Header: hdr}

        data, _ := json.Marshal(block)
        hash := block.Hash()

        if err := db.PutRawBlock(hash, 0, data); err != nil {
                t.Fatalf("PutRawBlock: %v", err)
        }

        got, err := db.GetRawBlock(hash)
        if err != nil {
                t.Fatalf("GetRawBlock: %v", err)
        }
        if string(got) != string(data) {
                t.Error("round-trip mismatch")
        }

        // By height
        gotH, err := db.GetRawBlockByHeight(0)
        if err != nil {
                t.Fatalf("GetRawBlockByHeight: %v", err)
        }
        if string(gotH) != string(data) {
                t.Error("height index round-trip mismatch")
        }
}

func TestDB_IterKeyImages(t *testing.T) {
        dir := t.TempDir()
        db, _ := store.Open(dir)
        defer db.Close()

        var kis [3]crypto.KeyImage
        for i := range kis {
                kis[i][0] = byte(i + 1)
                if err := db.MarkKeyImageSpent(kis[i]); err != nil {
                        t.Fatalf("MarkKeyImageSpent(%x): %v", kis[i][:4], err)
                }
        }

        // IterKeyImages may return canonical forms rather than raw bytes when the
        // stored key image was a valid prime-order curve point.  Verify via
        // IsKeyImageSpent (which applies the same canonicalization) instead of
        // comparing raw bytes, and separately verify the iteration count.
        var count int
        _ = db.IterKeyImages(func(_ crypto.KeyImage) error {
                count++
                return nil
        })
        if count != len(kis) {
                t.Errorf("IterKeyImages returned %d entries, want %d", count, len(kis))
        }
        for _, ki := range kis {
                spent, err := db.IsKeyImageSpent(ki)
                if err != nil {
                        t.Errorf("IsKeyImageSpent(%x): %v", ki[:4], err)
                } else if !spent {
                        t.Errorf("key image %x stored but IsKeyImageSpent returned false", ki[:4])
                }
        }
}

func TestDB_TipRoundtrip(t *testing.T) {
        dir := t.TempDir()
        db, _ := store.Open(dir)
        defer db.Close()

        var h crypto.Hash32
        h[0] = 0xAB
        if err := db.PutTip(h, 42); err != nil {
                t.Fatal(err)
        }
        gotH, gotHeight, err := db.GetTip()
        if err != nil {
                t.Fatal(err)
        }
        if gotH != h || gotHeight != 42 {
                t.Errorf("tip mismatch: got hash=%x height=%d", gotH[:4], gotHeight)
        }
}

func TestChainPersistRestore(t *testing.T) {
        dir := t.TempDir()

        // ── First run: create genesis + 2 blocks ──────────────────────────────────
        genesisFile := writeTempGenesis(t)
        defer os.Remove(genesisFile)

        db1, _ := store.Open(dir)

        genesisCfg, err := core.LoadGenesis(genesisFile)
        if err != nil {
                t.Fatal(err)
        }
        priv, _, _ := crypto.GenerateValidatorKey()
        genesis, _ := core.CreateGenesisBlock(genesisCfg, priv)

        chain1 := core.NewChain()
        utxos1 := core.NewUTXOSet()
        _ = chain1.SetGenesis(genesis)

        // helper: persist
        persistBlock := func(db *store.DB, block *core.Block, utxos *core.UTXOSet) {
                data, _ := json.Marshal(block)
                bh := block.Hash()
                _ = db.PutRawBlock(bh, block.Header.Height, data)
                _ = utxos.ApplyBlock(block)
                for _, tx := range block.Txs {
                        txHash := tx.Hash()
                        for _, inp := range tx.Inputs {
                                _ = db.MarkKeyImageSpent(inp.KeyImage)
                        }
                        for i, out := range tx.Outputs {
                                su := &store.StoredUTXO{
                                        TxHash: txHash, OutputIndex: uint32(i),
                                        OneTimePub: out.OneTimePub, AmountCommit: out.AmountCommit,
                                        BlockHeight: block.Header.Height,
                                }
                                _ = db.PutUTXO(txHash, uint32(i), su)
                        }
                }
                _ = db.PutTip(bh, block.Header.Height)
        }

        persistBlock(db1, genesis, utxos1)

        // Add block 1 and block 2 (no txs, just header chain)
        addAndPersist := func(db *store.DB, chain *core.Chain, utxos *core.UTXOSet, height uint64) *core.Block {
                prev := chain.Tip()
                prevHash := prev.Hash()
                hdr := core.BlockHeader{
                        Height:       height,
                        PrevHash:     prevHash,
                        Timestamp:    int64(1000000 + height*1000),
                        ValidatorPub: genesis.Header.ValidatorPub,
                }
                hdr.MerkleRoot = core.MerkleRoot(nil)
                _ = hdr.Sign(priv)
                b := &core.Block{Header: hdr}
                _ = chain.AddBlock(b)
                persistBlock(db, b, utxos)
                return b
        }
        addAndPersist(db1, chain1, utxos1, 1)
        addAndPersist(db1, chain1, utxos1, 2)
        db1.Close()

        // ── Second run: restore from DB ────────────────────────────────────────────
        db2, _ := store.Open(dir)
        defer db2.Close()

        _, tipHeight, err := db2.GetTip()
        if err != nil {
                t.Fatal(err)
        }
        if tipHeight != 2 {
                t.Fatalf("expected tip height 2, got %d", tipHeight)
        }

        chain2 := core.NewChain()
        utxos2 := core.NewUTXOSet()

        for h := uint64(0); h <= tipHeight; h++ {
                data, err := db2.GetRawBlockByHeight(h)
                if err != nil || data == nil {
                        t.Fatalf("missing block at height %d", h)
                }
                var block core.Block
                if err := json.Unmarshal(data, &block); err != nil {
                        t.Fatalf("unmarshal block %d: %v", h, err)
                }
                if h == 0 {
                        _ = chain2.SetGenesis(&block)
                } else {
                        _ = chain2.AddBlock(&block)
                }
        }

        _ = db2.IterUTXOs(func(su *store.StoredUTXO) error {
                utxos2.Add(&core.UTXO{TxHash: su.TxHash, OutputIndex: su.OutputIndex})
                return nil
        })

        if chain2.Height() != 2 {
                t.Errorf("restored chain height = %d, want 2", chain2.Height())
        }
        t.Logf("chain restored: height=%d utxos=%d", chain2.Height(), utxos2.Count())
}

// ─── Snapshot save duration round-trip ───────────────────────────────────────

// TestDB_SnapshotSaveDuration_RoundTrip verifies that StoreSnapshotSaveDuration
// and LoadSnapshotSaveDuration form a correct round-trip for representative
// values: zero, a typical mid-range value, and the maximum int64.
func TestDB_SnapshotSaveDuration_RoundTrip(t *testing.T) {
        dir := t.TempDir()
        db, err := store.Open(dir)
        if err != nil {
                t.Fatalf("open store: %v", err)
        }
        defer db.Close()

        cases := []struct {
                name string
                ms   int64
        }{
                {"zero", 0},
                {"typical_1234ms", 1234},
                {"max_int64", 9223372036854775807},
        }

        for _, tc := range cases {
                t.Run(tc.name, func(t *testing.T) {
                        if err := db.StoreSnapshotSaveDuration(tc.ms); err != nil {
                                t.Fatalf("StoreSnapshotSaveDuration(%d): %v", tc.ms, err)
                        }
                        got, found, err := db.LoadSnapshotSaveDuration()
                        if err != nil {
                                t.Fatalf("LoadSnapshotSaveDuration: %v", err)
                        }
                        if !found {
                                t.Fatal("LoadSnapshotSaveDuration: found=false after store")
                        }
                        if got != tc.ms {
                                t.Errorf("LoadSnapshotSaveDuration = %d, want %d", got, tc.ms)
                        }
                })
        }
}

// TestDB_SnapshotSaveDuration_AbsentBeforeFirstStore verifies that
// LoadSnapshotSaveDuration returns (0, false, nil) on a fresh database that
// has never had a value written (pre-feature DB or first boot).
func TestDB_SnapshotSaveDuration_AbsentBeforeFirstStore(t *testing.T) {
        dir := t.TempDir()
        db, err := store.Open(dir)
        if err != nil {
                t.Fatalf("open store: %v", err)
        }
        defer db.Close()

        got, found, err := db.LoadSnapshotSaveDuration()
        if err != nil {
                t.Fatalf("LoadSnapshotSaveDuration on empty DB: %v", err)
        }
        if found {
                t.Errorf("expected found=false on empty DB, got found=true (value=%d)", got)
        }
        if got != 0 {
                t.Errorf("expected ms=0 on empty DB, got %d", got)
        }
}

// TestDB_SnapshotSaveDuration_SurvivesReopen verifies that the value persisted
// by StoreSnapshotSaveDuration is readable after the DB is closed and reopened —
// the core property that makes the restart-survival feature work.
func TestDB_SnapshotSaveDuration_SurvivesReopen(t *testing.T) {
        dir := t.TempDir()

        // First open: write the value.
        db1, err := store.Open(dir)
        if err != nil {
                t.Fatalf("open store (first): %v", err)
        }
        const wantMs = int64(7890)
        if err := db1.StoreSnapshotSaveDuration(wantMs); err != nil {
                t.Fatalf("StoreSnapshotSaveDuration: %v", err)
        }
        db1.Close()

        // Second open: the value must still be present.
        db2, err := store.Open(dir)
        if err != nil {
                t.Fatalf("open store (second): %v", err)
        }
        defer db2.Close()

        got, found, err := db2.LoadSnapshotSaveDuration()
        if err != nil {
                t.Fatalf("LoadSnapshotSaveDuration after reopen: %v", err)
        }
        if !found {
                t.Fatal("LoadSnapshotSaveDuration: found=false after close+reopen — value did not survive restart")
        }
        if got != wantMs {
                t.Errorf("LoadSnapshotSaveDuration after reopen = %d, want %d", got, wantMs)
        }
}

// writeTempGenesis creates a minimal genesis YAML file for testing.
// It generates a fresh validator key so that min_validators=1 is satisfied.
func writeTempGenesis(t *testing.T) string {
        t.Helper()
        _, pub, err := crypto.GenerateValidatorKey()
        if err != nil {
                t.Fatal(err)
        }
        // Encode public key as hex for the YAML
        hexPub := fmt.Sprintf("%x", []byte(pub))
        content := "chain_id: test-persist\n" +
                "timestamp: 1000000\n" +
                "initial_supply: 21000000\n" +
                "block_time_ms: 5000\n" +
                "ring_size: 16\n" +
                "min_validators: 1\n" +
                "bft_threshold: 0.667\n" +
                "validators:\n" +
                "  - " + hexPub + "\n"
        f, err2 := os.CreateTemp("", "genesis-*.yaml")
        if err2 != nil {
                t.Fatal(err2)
        }
        _, _ = f.WriteString(content)
        f.Close()
        return f.Name()
}
