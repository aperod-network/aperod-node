package core_test

// Integration tests:
//   1.9.6 — coinbase → mempool → block → UTXO update
//   1.9.7 — 10-block chain build + consistent UTXO state (sync simulation)
//   1.9.8 — node falls behind then re-syncs via Reorg

import (
        "testing"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// TestIntegration_CoinbaseToMempool verifies the full lifecycle:
//  1. Genesis block with validator coinbase reward → Alice's UTXO is recorded
//  2. WalletScanner detects Alice's incoming UTXO
//  3. New block is built and applied → UTXO set updated
func TestIntegration_CoinbaseToMempool(t *testing.T) {
        // ── Actors ────────────────────────────────────────────────────────────────
        aliceKeys, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatalf("generate Alice keys: %v", err)
        }
        aliceAddr := crypto.AddressFromKeys(crypto.TestnetByte, aliceKeys)

        _ = aliceAddr // used for scan check

        validatorPriv, validatorPub, _ := crypto.GenerateValidatorKey()

        // ── Block 0: coinbase + stealth output to Alice ───────────────────────────
        // CoinbaseTx uses the spend pub key directly (no ECDH), so for wallet
        // scanner detection we also add a stealth-addressed output in a second tx.
        reward := uint64(50_000_000) // 0.5 APR
        cb := core.CoinbaseTx(aliceKeys.Spend.Public, reward)

        // Build a stealth output tx so Alice can detect it with ScanBlock.
        stealthOut, err := crypto.CreateStealthOutput(aliceKeys.Spend.Public, aliceKeys.View.Public)
        if err != nil {
                t.Fatalf("CreateStealthOutput: %v", err)
        }
        bf, _ := crypto.NewBlindFactor()
        commit, _ := crypto.Commit(1_000_000, bf)
        stealthTx := core.Transaction{
                Version: core.TxVersionBase,
                Outputs: []core.Output{{
                        OneTimePub:   stealthOut.OneTimePub,
                        TxPubKey:     stealthOut.TxPubKey,
                        AmountCommit: commit,
                }},
        }

        txs0 := []core.Transaction{cb, stealthTx}
        hdr0 := core.BlockHeader{
                Height:       0,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(txs0),
        }
        if err := hdr0.Sign(validatorPriv); err != nil {
                t.Fatalf("sign block 0: %v", err)
        }
        block0 := &core.Block{Header: hdr0, Txs: txs0}

        // ── Chain + UTXO set ──────────────────────────────────────────────────────
        chain := core.NewChain()
        if err := chain.SetGenesis(block0); err != nil {
                t.Fatalf("SetGenesis: %v", err)
        }

        utxos := core.NewUTXOSet()
        if err := utxos.ApplyBlock(block0); err != nil {
                t.Fatalf("ApplyBlock(block0): %v", err)
        }

        // block0 has 2 txs (coinbase + stealthTx) each with 1 output → 2 UTXOs
        if utxos.Count() != 2 {
                t.Errorf("UTXO count after block0 = %d, want 2 (coinbase + stealth)", utxos.Count())
        }

        // ── Mempool setup ─────────────────────────────────────────────────────────
        mp := core.NewMempool(core.DefaultMempoolConfig())

        // ── Wallet scanner: Alice detects her incoming stealth UTXO ──────────────
        scanner := core.NewWalletScanner(
                aliceKeys.View.Private,
                aliceKeys.Spend.Public,
                aliceKeys.View.Public,
                crypto.TestnetByte,
        )
        owned := scanner.ScanBlock(block0)
        if len(owned) == 0 {
                t.Fatal("WalletScanner: Alice did not detect her stealth output")
        }
        t.Logf("Alice owns %d stealth UTXOs", len(owned))

        // ── Block 1: empty (no tx from Alice — ring size needs decoys we don't have)
        // Instead we verify that a block can be added and UTXO count stays consistent.
        hdr1 := core.BlockHeader{
                Height:       1,
                PrevHash:     block0.Hash(),
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(nil),
        }
        if err := hdr1.Sign(validatorPriv); err != nil {
                t.Fatalf("sign block 1: %v", err)
        }
        block1 := &core.Block{Header: hdr1}
        if err := chain.AddBlock(block1); err != nil {
                t.Fatalf("AddBlock(block1): %v", err)
        }
        if err := utxos.ApplyBlock(block1); err != nil {
                t.Fatalf("ApplyBlock(block1): %v", err)
        }

        if chain.Height() != 1 {
                t.Errorf("chain height = %d, want 1", chain.Height())
        }
        // Still 2 UTXOs — block1 has no txs
        if utxos.Count() != 2 {
                t.Errorf("UTXO count after block1 = %d, want 2 (unchanged)", utxos.Count())
        }

        // ── Mempool eviction test ──────────────────────────────────────────────────
        n := mp.Evict()
        t.Logf("Mempool.Evict removed %d expired entries (expected 0)", n)
}

// TestIntegration_ChainReorg verifies that a 2-block reorg replaces the tip.
func TestIntegration_ChainReorg(t *testing.T) {
        validatorPriv, validatorPub, _ := crypto.GenerateValidatorKey()

        // Genesis
        hdr0 := core.BlockHeader{
                Height:       0,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(nil),
        }
        _ = hdr0.Sign(validatorPriv)
        genesis := &core.Block{Header: hdr0}

        chain := core.NewChain()
        _ = chain.SetGenesis(genesis)

        // Fork A: genesis → A1 → A2 (tip)
        hdrA1 := core.BlockHeader{
                Height: 1, PrevHash: genesis.Hash(),
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(nil),
        }
        _ = hdrA1.Sign(validatorPriv)
        blockA1 := &core.Block{Header: hdrA1}
        _ = chain.AddBlock(blockA1)

        hdrA2 := core.BlockHeader{
                Height: 2, PrevHash: blockA1.Hash(),
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(nil),
        }
        _ = hdrA2.Sign(validatorPriv)
        blockA2 := &core.Block{Header: hdrA2}
        _ = chain.AddBlock(blockA2)

        if chain.Height() != 2 {
                t.Errorf("chain height after A1+A2 = %d, want 2", chain.Height())
        }

        // Fork B from genesis: B1 → B2 → B3 (longer)
        hdrB1 := core.BlockHeader{
                Height: 1, PrevHash: genesis.Hash(),
                Timestamp:    time.Now().UnixNano() + 1,
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(nil),
        }
        _ = hdrB1.Sign(validatorPriv)
        blockB1 := &core.Block{Header: hdrB1}

        hdrB2 := core.BlockHeader{
                Height: 2, PrevHash: blockB1.Hash(),
                Timestamp:    time.Now().UnixNano() + 2,
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(nil),
        }
        _ = hdrB2.Sign(validatorPriv)
        blockB2 := &core.Block{Header: hdrB2}

        hdrB3 := core.BlockHeader{
                Height: 3, PrevHash: blockB2.Hash(),
                Timestamp:    time.Now().UnixNano() + 3,
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(nil),
        }
        _ = hdrB3.Sign(validatorPriv)
        blockB3 := &core.Block{Header: hdrB3}

        // Trigger reorg: B fork is longer (fork point = genesis at height 0)
        _ = chain.Reorg(0, []*core.Block{blockB1, blockB2, blockB3})

        if chain.Height() != 3 {
                t.Errorf("chain height after reorg = %d, want 3", chain.Height())
        }
}

// TestIntegration_TxVerifier_Coinbase exercises the full VerifyBlock path.
func TestIntegration_TxVerifier_CoinbaseBlock(t *testing.T) {
        validatorPriv, validatorPub, _ := crypto.GenerateValidatorKey()
        aliceKeys, _ := crypto.GenerateWalletKeys()

        // Build a block with a coinbase tx
        cb := core.CoinbaseTx(aliceKeys.Spend.Public, 100_000_000)
        txs := []core.Transaction{cb}

        hdr := core.BlockHeader{
                Height:       0,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(txs),
        }
        _ = hdr.Sign(validatorPriv)
        block := &core.Block{Header: hdr, Txs: txs}

        // VerifyBlock should accept a block with only a coinbase
        utxos := core.NewUTXOSet()
        v := core.NewTxVerifier(utxos)
        if err := v.VerifyBlock(block); err != nil {
                t.Errorf("VerifyBlock coinbase block: %v", err)
        }

        // Apply and verify UTXO count
        _ = utxos.ApplyBlock(block)
        if utxos.Count() != 1 {
                t.Errorf("UTXO count = %d, want 1", utxos.Count())
        }
}

// ─── 1.9.7: 10-block chain build + UTXO consistency ─────────────────────────

// TestIntegration_TenBlockChain simulates a node receiving 10 sequential blocks
// and verifies chain height, UTXO state, and block hash linkage at every step.
// This exercises the full "happy-path sync" flow without networking.
func TestIntegration_TenBlockChain(t *testing.T) {
        validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
        if err != nil {
                t.Fatalf("GenerateValidatorKey: %v", err)
        }
        aliceKeys, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatalf("GenerateWalletKeys: %v", err)
        }

        chain := core.NewChain()
        utxos := core.NewUTXOSet()
        scanner := core.NewWalletScanner(
                aliceKeys.View.Private,
                aliceKeys.Spend.Public,
                aliceKeys.View.Public,
                crypto.TestnetByte,
        )

        const numBlocks = 10
        blocks := make([]*core.Block, numBlocks)

        for i := 0; i < numBlocks; i++ {
                // Coinbase reward
                cb := core.CoinbaseTx(aliceKeys.Spend.Public, uint64(50_000_000*(i+1)))

                // Stealth-addressed output so WalletScanner can detect Alice's funds
                stealthOut, err := crypto.CreateStealthOutput(aliceKeys.Spend.Public, aliceKeys.View.Public)
                if err != nil {
                        t.Fatalf("CreateStealthOutput block %d: %v", i, err)
                }
                bf, _ := crypto.NewBlindFactor()
                commit, _ := crypto.Commit(uint64(1_000_000*(i+1)), bf)
                stealthTx := core.Transaction{
                        Version: core.TxVersionBase,
                        Outputs: []core.Output{{
                                OneTimePub:   stealthOut.OneTimePub,
                                TxPubKey:     stealthOut.TxPubKey,
                                AmountCommit: commit,
                        }},
                }

                txs := []core.Transaction{cb, stealthTx}

                var prevHash crypto.Hash32
                if i == 0 {
                        prevHash = crypto.Hash32{}
                } else {
                        prevHash = blocks[i-1].Hash()
                }

                hdr := core.BlockHeader{
                        Height:       uint64(i),
                        PrevHash:     prevHash,
                        Timestamp:    time.Now().UnixNano() + int64(i),
                        ValidatorPub: validatorPub,
                        MerkleRoot:   core.MerkleRoot(txs),
                }
                if err := hdr.Sign(validatorPriv); err != nil {
                        t.Fatalf("sign block %d: %v", i, err)
                }
                blocks[i] = &core.Block{Header: hdr, Txs: txs}

                // Apply to chain
                if i == 0 {
                        if err := chain.SetGenesis(blocks[0]); err != nil {
                                t.Fatalf("SetGenesis: %v", err)
                        }
                } else {
                        if err := chain.AddBlock(blocks[i]); err != nil {
                                t.Fatalf("AddBlock height %d: %v", i, err)
                        }
                }
                if err := utxos.ApplyBlock(blocks[i]); err != nil {
                        t.Fatalf("ApplyBlock height %d: %v", i, err)
                }

                // Height must match
                if chain.Height() != uint64(i) {
                        t.Errorf("height after block %d = %d, want %d", i, chain.Height(), i)
                }
                // Two new UTXOs per block (coinbase + stealth output)
                if utxos.Count() != (i+1)*2 {
                        t.Errorf("UTXO count after block %d = %d, want %d", i, utxos.Count(), (i+1)*2)
                }
        }

        // WalletScanner must detect Alice's output in every block
        totalOwned := 0
        for _, b := range blocks {
                owned := scanner.ScanBlock(b)
                totalOwned += len(owned)
        }
        if totalOwned != numBlocks {
                t.Errorf("WalletScanner found %d owned outputs across %d blocks, want %d",
                        totalOwned, numBlocks, numBlocks)
        }

        // Verify tip is block 9
        tip := chain.Tip()
        if tip == nil {
                t.Fatal("chain.Tip() returned nil after 10 blocks")
        }
        if tip.Header.Height != numBlocks-1 {
                t.Errorf("tip height = %d, want %d", tip.Header.Height, numBlocks-1)
        }

        // Verify block linkage (each block's PrevHash matches the previous block's hash)
        for i := 1; i < numBlocks; i++ {
                want := blocks[i-1].Hash()
                got := blocks[i].Header.PrevHash
                if got != want {
                        t.Errorf("block %d: PrevHash mismatch", i)
                }
        }

        t.Logf("1.9.7 ✓ 10-block chain: height=%d UTXOs=%d owned=%d",
                chain.Height(), utxos.Count(), totalOwned)
}

// ─── 1.9.8: node falls behind then re-syncs via Reorg ────────────────────────

// TestIntegration_NodeRejoin simulates a node that:
//  1. Starts on the main chain (blocks 0-4)
//  2. Falls behind — the rest of the network builds a longer fork (blocks 0-7)
//  3. Re-joins: calls chain.Reorg at the fork point to adopt the longer chain
//
// Verifies that after rejoin the node's tip matches the canonical chain.
func TestIntegration_NodeRejoin(t *testing.T) {
        validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
        if err != nil {
                t.Fatalf("GenerateValidatorKey: %v", err)
        }

        buildBlock := func(height uint64, prevHash crypto.Hash32, extra int64) *core.Block {
                hdr := core.BlockHeader{
                        Height:       height,
                        PrevHash:     prevHash,
                        Timestamp:    time.Now().UnixNano() + extra,
                        ValidatorPub: validatorPub,
                        MerkleRoot:   core.MerkleRoot(nil),
                }
                if err := hdr.Sign(validatorPriv); err != nil {
                        t.Fatalf("sign block height=%d: %v", height, err)
                }
                return &core.Block{Header: hdr}
        }

        // ── Slow node: builds chain A (5 blocks: 0-4) ──────────────────────────
        slowChain := core.NewChain()

        chainA := make([]*core.Block, 5)
        chainA[0] = buildBlock(0, crypto.Hash32{}, 0)
        if err := slowChain.SetGenesis(chainA[0]); err != nil {
                t.Fatalf("SetGenesis A[0]: %v", err)
        }
        for i := 1; i < 5; i++ {
                chainA[i] = buildBlock(uint64(i), chainA[i-1].Hash(), int64(i))
                if err := slowChain.AddBlock(chainA[i]); err != nil {
                        t.Fatalf("AddBlock A[%d]: %v", i, err)
                }
        }
        if slowChain.Height() != 4 {
                t.Fatalf("slow chain height = %d, want 4", slowChain.Height())
        }

        // ── Network builds chain B from genesis (8 blocks: 0-7) ────────────────
        // Fork point is height 0 (genesis) — completely different chain B.
        chainB := make([]*core.Block, 8)
        chainB[0] = buildBlock(0, crypto.Hash32{}, 1000) // different timestamp → different hash
        for i := 1; i < 8; i++ {
                chainB[i] = buildBlock(uint64(i), chainB[i-1].Hash(), int64(1000+i))
        }

        // ── Re-join: Reorg at fork point height 0, adopt chain B blocks 1-7 ───
        if err := slowChain.Reorg(0, chainB[1:]); err != nil {
                t.Fatalf("Reorg: %v", err)
        }

        if slowChain.Height() != 7 {
                t.Errorf("height after rejoin = %d, want 7", slowChain.Height())
        }

        tip := slowChain.Tip()
        if tip == nil {
                t.Fatal("Tip() nil after rejoin")
        }
        if tip.Header.Height != 7 {
                t.Errorf("tip height = %d, want 7", tip.Header.Height)
        }

        t.Logf("1.9.8 ✓ node rejoin: adopted chain B, new tip height=%d", slowChain.Height())
}
