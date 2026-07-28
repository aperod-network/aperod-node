package core_test

// Second round of coverage tests to push core above 80%.

import (
        "testing"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// ─── mempool: evictOldest via MaxSize overflow ────────────────────────────────

// evictOldest is triggered when the pool hits MaxSize and a new tx arrives.
// We can't add real txs without a full RingCT setup, so just ensure Evict()
// on an aged-out pool works without panicking.
func TestMempool_Evict_Ages(t *testing.T) {
        cfg := core.MempoolConfig{
                MaxSize:   5_000,
                MaxTxSize: 100_000,
                TTL:       time.Millisecond, // very short TTL
                BaseFeePerByte: core.InitialBaseFeePerByte,
        }
        mp := core.NewMempool(cfg)
        time.Sleep(5 * time.Millisecond)
        n := mp.Evict()
        _ = n // 0 is fine — pool was empty
}

// ─── block_verifier: VerifyBlock paths ───────────────────────────────────────

func TestBlockVerifier_FutureTimestamp(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})

        hdr := core.BlockHeader{
                Height:       1,
                PrevHash:     genesis.Hash(),
                Timestamp:    time.Now().Add(time.Hour).UnixNano(),
                ValidatorPub: pub,
                MerkleRoot:   core.MerkleRoot(nil),
        }
        _ = hdr.Sign(priv)
        future := &core.Block{Header: hdr}

        chain := core.NewChain()
        _ = chain.SetGenesis(genesis)
        uset := core.NewUTXOSet()
        txV := core.NewTxVerifier(uset)
        verifier := core.NewBlockVerifier(core.DefaultBlockVerifierConfig(), chain, txV)
        if err := verifier.VerifyBlock(future); err == nil {
                t.Error("VerifyBlock must reject far-future timestamp")
        }
}

func TestBlockVerifier_WrongPrevHash(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})

        var wrongPrev crypto.Hash32
        wrongPrev[0] = 0xDE
        hdr := core.BlockHeader{
                Height:       1,
                PrevHash:     wrongPrev,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: pub,
                MerkleRoot:   core.MerkleRoot(nil),
        }
        _ = hdr.Sign(priv)
        bad := &core.Block{Header: hdr}

        chain := core.NewChain()
        _ = chain.SetGenesis(genesis)
        uset := core.NewUTXOSet()
        txV := core.NewTxVerifier(uset)
        verifier := core.NewBlockVerifier(core.DefaultBlockVerifierConfig(), chain, txV)
        if err := verifier.VerifyBlock(bad); err == nil {
                t.Error("VerifyBlock must reject wrong PrevHash")
        }
}

// ─── genesis: Validate edge cases ────────────────────────────────────────────

func TestGenesis_Validate_EmptyChainID(t *testing.T) {
        gc := &core.GenesisConfig{
                InitialSupply: 1_000_000,
                BlockTimeMs:   1000,
                MinValidators: 1,
                BFTThreshold:  0.667,
                RingSize:      2,
        }
        if err := gc.Validate(); err == nil {
                t.Error("Validate must fail with empty chain_id")
        }
}

func TestGenesis_Validate_ZeroSupply(t *testing.T) {
        _, pub, _ := crypto.GenerateValidatorKey()
        gc := &core.GenesisConfig{
                ChainID:       "test",
                InitialSupply: 0,
                BlockTimeMs:   1000,
                MinValidators: 1,
                BFTThreshold:  0.667,
                RingSize:      2,
                Validators:    []string{pub.Hex()},
        }
        if err := gc.Validate(); err == nil {
                t.Error("Validate must fail with zero initial_supply")
        }
}

func TestGenesis_Validate_BadThreshold(t *testing.T) {
        _, pub, _ := crypto.GenerateValidatorKey()
        gc := &core.GenesisConfig{
                ChainID:       "test",
                InitialSupply: 1_000_000,
                BlockTimeMs:   1000,
                MinValidators: 1,
                BFTThreshold:  1.5,
                RingSize:      2,
                Validators:    []string{pub.Hex()},
        }
        if err := gc.Validate(); err == nil {
                t.Error("Validate must fail with bft_threshold > 1")
        }
}

// ─── chain.go: Height ────────────────────────────────────────────────────────

func TestChain_Height_AfterBlocks(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        chain := core.NewChain()
        _ = chain.SetGenesis(genesis)

        if chain.Height() != 0 {
                t.Errorf("height after genesis = %d, want 0", chain.Height())
        }
        b1 := makeBlock(t, priv, pub, 1, genesis.Hash())
        _ = chain.AddBlock(b1)
        if chain.Height() != 1 {
                t.Errorf("height after b1 = %d, want 1", chain.Height())
        }
}

// ─── tx_verifier: VerifyBlock with coinbase ───────────────────────────────────

func TestVerifyBlock_WithCoinbase(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        cb := core.CoinbaseTx(wk.Spend.Public, 5_000_000)

        priv, pub, _ := crypto.GenerateValidatorKey()
        txs := []core.Transaction{cb}
        hdr := core.BlockHeader{
                Height:       0,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: pub,
                MerkleRoot:   core.MerkleRoot(txs),
        }
        _ = hdr.Sign(priv)
        block := &core.Block{Header: hdr, Txs: txs}

        uset := core.NewUTXOSet()
        verifier := core.NewTxVerifier(uset)
        if err := verifier.VerifyBlock(block); err != nil {
                t.Errorf("VerifyBlock with coinbase: %v", err)
        }
}
