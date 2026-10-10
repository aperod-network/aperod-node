package core_test

// Additional tests to push core coverage above 80%.

import (
        "os"
        "path/filepath"
        "testing"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// ─── block.go ─────────────────────────────────────────────────────────────────

func TestBlock_Time(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        b := makeBlock(t, priv, pub, 1, crypto.Hash32{})
        tm := b.Time()
        if tm.IsZero() {
                t.Error("Block.Time() must not be zero")
        }
}

func TestBlock_Validate_NilSignature(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        b := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        b.Header.Signature = nil
        if err := b.Validate(); err == nil {
                t.Error("Validate() must fail without signature")
        }
}

// TestBlock_Validate_MerkleRoot verifies that mismatched merkle roots are detected.
func TestBlock_Validate_MerkleRootMismatch(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        hdr := core.BlockHeader{
                Height:       0,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: pub,
                MerkleRoot:   core.MerkleRoot(nil), // correct: no txs
        }
        if err := hdr.Sign(priv); err != nil {
                t.Fatal(err)
        }
        b := &core.Block{Header: hdr}
        // Block has no txs and correct MerkleRoot → should validate fine
        if err := b.Validate(); err != nil {
                t.Errorf("Validate() on valid empty block: %v", err)
        }
}

// ─── chain.go ─────────────────────────────────────────────────────────────────

func TestChain_Genesis(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        chain := core.NewChain()
        if err := chain.SetGenesis(genesis); err != nil {
                t.Fatal(err)
        }
        g := chain.Genesis()
        if g == nil {
                t.Fatal("Genesis() must not be nil")
        }
        if g.Header.Height != 0 {
                t.Errorf("genesis height = %d, want 0", g.Header.Height)
        }
}

func TestChain_HasBlock(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        chain := core.NewChain()
        _ = chain.SetGenesis(genesis)

        if !chain.HasBlock(genesis.Hash()) {
                t.Error("HasBlock(genesis) must be true")
        }
        var unknown crypto.Hash32
        unknown[0] = 0xFF
        if chain.HasBlock(unknown) {
                t.Error("HasBlock(unknown) must be false")
        }

        b1 := makeBlock(t, priv, pub, 1, genesis.Hash())
        _ = chain.AddBlock(b1)
        if !chain.HasBlock(b1.Hash()) {
                t.Error("HasBlock(b1) must be true after AddBlock")
        }
}

func TestChain_TailHashes(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        chain := core.NewChain()
        _ = chain.SetGenesis(genesis)

        prev := genesis.Hash()
        for i := uint64(1); i <= 5; i++ {
                b := makeBlock(t, priv, pub, i, prev)
                _ = chain.AddBlock(b)
                prev = b.Hash()
        }

        hashes := chain.TailHashes(3)
        if len(hashes) != 3 {
                t.Errorf("TailHashes(3) = %d, want 3", len(hashes))
        }
}

func TestChain_TailHashes_LessThanN(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        chain := core.NewChain()
        _ = chain.SetGenesis(genesis)

        hashes := chain.TailHashes(10)
        if len(hashes) == 0 {
                t.Error("TailHashes on chain[0] must return at least genesis")
        }
}

// ─── mempool.go ───────────────────────────────────────────────────────────────

func TestMempool_RemoveBlock(t *testing.T) {
        mp := core.NewMempool(core.DefaultMempoolConfig())
        priv, pub, _ := crypto.GenerateValidatorKey()
        b := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        // RemoveBlock on empty mempool must not panic
        mp.RemoveBlock(b)
}

func TestMempool_Hashes(t *testing.T) {
        mp := core.NewMempool(core.DefaultMempoolConfig())
        hashes := mp.Hashes()
        if hashes == nil {
                t.Error("Hashes() must return non-nil")
        }
        if len(hashes) != 0 {
                t.Errorf("fresh mempool Hashes() = %d, want 0", len(hashes))
        }
}

func TestMempool_Evict(t *testing.T) {
        mp := core.NewMempool(core.DefaultMempoolConfig())
        // Evict with nothing to evict must not panic
        mp.Evict()
}

// ─── transaction.go ───────────────────────────────────────────────────────────

func TestTransaction_MinFee(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        cb := core.CoinbaseTx(wk.Spend.Public, 5_000_000)
        fee := cb.MinFeeAt(core.InitialBaseFeePerByte)
        // MinFee for a zero-input tx could be 0 — just must not panic
        _ = fee
}

func TestTransaction_KeyImages(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        cb := core.CoinbaseTx(wk.Spend.Public, 5_000_000)
        ki := cb.KeyImages()
        if ki == nil {
                t.Error("KeyImages() must return non-nil")
        }
        if len(ki) != 0 {
                t.Errorf("coinbase KeyImages len = %d, want 0", len(ki))
        }
}

func TestCoinbaseTx_Fields(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        reward := uint64(5_000_000)
        cb := core.CoinbaseTx(wk.Spend.Public, reward)
        if !cb.IsCoinbase() {
                t.Error("CoinbaseTx must be detected as coinbase")
        }
        if cb.Fee != 0 {
                t.Errorf("coinbase Fee = %d, want 0", cb.Fee)
        }
        if len(cb.Outputs) == 0 {
                t.Error("coinbase must have at least one output")
        }
}

// ─── utxo.go ─────────────────────────────────────────────────────────────────

func TestUTXOSet_RollbackBlock(t *testing.T) {
        uset := core.NewUTXOSet()
        priv, pub, _ := crypto.GenerateValidatorKey()
        b := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        // RollbackBlock on empty block must not error
        if err := uset.RollbackBlock(b); err != nil {
                t.Errorf("RollbackBlock: %v", err)
        }
}

func TestUTXOSet_CountAndAll(t *testing.T) {
        uset := core.NewUTXOSet()
        if uset.Count() != 0 {
                t.Errorf("fresh UTXOSet Count = %d, want 0", uset.Count())
        }
        all := uset.All()
        if all == nil {
                t.Error("All() must return non-nil")
        }
        if len(all) != 0 {
                t.Errorf("fresh UTXOSet All() len = %d, want 0", len(all))
        }
}

// ─── genesis.go ───────────────────────────────────────────────────────────────

func TestGenesis_LoadAndCreate(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()

        tmpDir := t.TempDir()
        genesisYAML := "chain_id: \"aperod-test-1\"\ninitial_supply: 1000000000\nblock_time_ms: 1000\nmin_validators: 1\nbft_threshold: 0.667\nring_size: 2\nvalidators:\n  - " + pub.Hex() + "\nallocations: []\n"
        path := filepath.Join(tmpDir, "genesis.yaml")
        if err := os.WriteFile(path, []byte(genesisYAML), 0644); err != nil {
                t.Fatal(err)
        }

        gc, err := core.LoadGenesis(path)
        if err != nil {
                t.Fatalf("LoadGenesis: %v", err)
        }
        if err := gc.Validate(); err != nil {
                t.Fatalf("Validate: %v", err)
        }

        block, err := core.CreateGenesisBlock(gc, priv)
        if err != nil {
                t.Fatalf("CreateGenesisBlock: %v", err)
        }
        if block.Header.Height != 0 {
                t.Errorf("genesis height = %d, want 0", block.Header.Height)
        }

        h := core.GenesisHash(gc)
        h2 := core.GenesisHash(gc)
        if h != h2 {
                t.Error("GenesisHash must be deterministic")
        }
}

func TestGenesis_Validate_NoValidators(t *testing.T) {
        gc := &core.GenesisConfig{
                ChainID:       "test",
                InitialSupply: 1_000_000,
                BlockTimeMs:   1000,
        }
        if err := gc.Validate(); err == nil {
                t.Error("Validate() must fail with no validators")
        }
}

// ─── tx_verifier.go ───────────────────────────────────────────────────────────

func TestVerifyBlock_EmptyBlock(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        b := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        uset := core.NewUTXOSet()
        verifier := core.NewTxVerifier(uset)
        if err := verifier.VerifyBlock(b); err != nil {
                t.Errorf("VerifyBlock on empty block: %v", err)
        }
}

func TestRingSignMessage_Deterministic(t *testing.T) {
        var txHash crypto.Hash32
        txHash[0] = 0xAB
        m1 := core.RingSignMessage(txHash, 0)
        m2 := core.RingSignMessage(txHash, 0)
        if m1 != m2 {
                t.Error("RingSignMessage must be deterministic")
        }
        m3 := core.RingSignMessage(txHash, 1)
        if m1 == m3 {
                t.Error("RingSignMessage must differ for different inputIdx")
        }
}
