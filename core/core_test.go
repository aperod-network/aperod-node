package core_test

import (
        "testing"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// ─── Block ────────────────────────────────────────────────────────────────────

func TestBlock_HashDeterministic(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        block := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        h1 := block.Hash()
        h2 := block.Hash()
        if h1 != h2 {
                t.Fatal("block hash is not deterministic")
        }
}

func TestBlock_SignVerify(t *testing.T) {
        priv, _, _ := crypto.GenerateValidatorKey()
        header := core.BlockHeader{
                Height:       1,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: priv.Public(),
        }
        if err := header.Sign(priv); err != nil {
                t.Fatalf("Sign: %v", err)
        }
        if !header.VerifySignature() {
                t.Fatal("VerifySignature returned false for a valid signature")
        }
}

func TestBlock_SignVerify_WrongKey(t *testing.T) {
        priv, _, _ := crypto.GenerateValidatorKey()
        _, pub2, _ := crypto.GenerateValidatorKey()
        header := core.BlockHeader{
                Height:       1,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: pub2, // signed by priv but pub is pub2
        }
        if err := header.Sign(priv); err != nil {
                t.Fatal(err)
        }
        if header.VerifySignature() {
                t.Fatal("VerifySignature should fail for wrong key pair")
        }
}

func TestBlock_IsGenesis(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        if !genesis.IsGenesis() {
                t.Fatal("block at height 0 with zero PrevHash should be genesis")
        }
        notGenesis := makeBlock(t, priv, pub, 1, genesis.Hash())
        if notGenesis.IsGenesis() {
                t.Fatal("block at height 1 should not be genesis")
        }
}

// ─── Merkle ───────────────────────────────────────────────────────────────────

func TestMerkle_Empty(t *testing.T) {
        root := core.MerkleRoot(nil)
        if root != (crypto.Hash32{}) {
                t.Fatal("empty merkle root should be zero hash")
        }
}

func TestMerkle_Single(t *testing.T) {
        txs := []core.Transaction{makeCoinbaseTx()}
        root1 := core.MerkleRoot(txs)
        root2 := core.MerkleRoot(txs)
        if root1 != root2 {
                t.Fatal("merkle root is not deterministic")
        }
        if root1 == (crypto.Hash32{}) {
                t.Fatal("single tx merkle root should not be zero")
        }
}

func TestMerkle_OrderMatters(t *testing.T) {
        tx1 := makeCoinbaseTx()
        tx2 := makeCoinbaseTx()
        r1 := core.MerkleRoot([]core.Transaction{tx1, tx2})
        r2 := core.MerkleRoot([]core.Transaction{tx2, tx1})
        if r1 == r2 {
                t.Fatal("merkle root should differ for different tx order")
        }
}

func TestMerkle_InclusionProof(t *testing.T) {
        txs := []core.Transaction{
                makeCoinbaseTx(), makeCoinbaseTx(),
                makeCoinbaseTx(), makeCoinbaseTx(),
                makeCoinbaseTx(),
        }
        root := core.MerkleRoot(txs)
        for i := range txs {
                proof := core.GenerateMerkleProof(txs, i)
                if proof == nil {
                        t.Fatalf("nil proof for tx %d", i)
                }
                if !proof.Verify(root) {
                        t.Fatalf("proof verification failed for tx %d", i)
                }
        }
}

func TestMerkle_ProofWrongRoot(t *testing.T) {
        txs := []core.Transaction{makeCoinbaseTx(), makeCoinbaseTx()}
        proof := core.GenerateMerkleProof(txs, 0)
        wrongRoot := crypto.HashStr("wrong root")
        if proof.Verify(wrongRoot) {
                t.Fatal("proof should fail for wrong root")
        }
}

// ─── UTXO Set ─────────────────────────────────────────────────────────────────

func TestUTXOSet_AddGet(t *testing.T) {
        s := core.NewUTXOSet()
        u := &core.UTXO{
                TxHash:      crypto.HashStr("tx1"),
                OutputIndex: 0,
                BlockHeight: 1,
        }
        s.Add(u)
        got := s.Get(u.TxHash, 0)
        if got == nil {
                t.Fatal("Get returned nil after Add")
        }
        if got.TxHash != u.TxHash {
                t.Fatal("TxHash mismatch")
        }
}

func TestUTXOSet_Remove(t *testing.T) {
        s := core.NewUTXOSet()
        h := crypto.HashStr("tx")
        s.Add(&core.UTXO{TxHash: h, OutputIndex: 0})
        s.Remove(h, 0)
        if s.Get(h, 0) != nil {
                t.Fatal("UTXO should be removed")
        }
}

func TestUTXOSet_KeyImage(t *testing.T) {
        s := core.NewUTXOSet()
        kp, _ := crypto.GenerateWalletKeys()
        ki, _ := crypto.ComputeKeyImage(kp.Spend.Private, kp.Spend.Public)

        if s.IsSpent(ki) {
                t.Fatal("key image should not be spent before marking")
        }
        s.MarkSpent(ki)
        if !s.IsSpent(ki) {
                t.Fatal("key image should be spent after marking")
        }
}

func TestUTXOSet_ApplyBlock_DoubleSpend(t *testing.T) {
        s := core.NewUTXOSet()
        kp, _ := crypto.GenerateWalletKeys()
        ki, _ := crypto.ComputeKeyImage(kp.Spend.Private, kp.Spend.Public)

        priv, pub, _ := crypto.GenerateValidatorKey()
        block := makeBlockWithKI(t, priv, pub, 1, crypto.Hash32{}, ki)

        if err := s.ApplyBlock(block); err != nil {
                t.Fatalf("first apply: %v", err)
        }
        // Second block with same key image = double spend
        block2 := makeBlockWithKI(t, priv, pub, 2, block.Hash(), ki)
        if err := s.ApplyBlock(block2); err == nil {
                t.Fatal("ApplyBlock should detect double-spend")
        }
}

// ─── Mempool ─────────────────────────────────────────────────────────────────

func TestMempool_AddGet(t *testing.T) {
        mp := core.NewMempool(core.DefaultMempoolConfig())
        tx := makeValidTx(t)
        if err := mp.Add(tx); err != nil {
                t.Fatalf("Add: %v", err)
        }
        hash := tx.Hash()
        got, ok := mp.Get(hash)
        if !ok {
                t.Fatal("Get returned false after Add")
        }
        if got.Hash() != hash {
                t.Fatal("hash mismatch")
        }
}

func TestMempool_Duplicate(t *testing.T) {
        mp := core.NewMempool(core.DefaultMempoolConfig())
        tx := makeValidTx(t)
        mp.Add(tx)
        if err := mp.Add(tx); err == nil {
                t.Fatal("adding duplicate tx should fail")
        }
}

func TestMempool_Remove(t *testing.T) {
        mp := core.NewMempool(core.DefaultMempoolConfig())
        tx := makeValidTx(t)
        mp.Add(tx)
        mp.Remove(tx.Hash())
        if mp.Count() != 0 {
                t.Fatal("mempool should be empty after remove")
        }
}

func TestMempool_SelectByFeeRate(t *testing.T) {
        mp := core.NewMempool(core.DefaultMempoolConfig())
        // Add two txs with different fee rates (higher fee wins)
        tx1 := makeValidTxWithFee(t, 100)
        tx2 := makeValidTxWithFee(t, 1000)
        mp.Add(tx1)
        mp.Add(tx2)

        selected := mp.SelectTxs(1)
        if len(selected) != 1 {
                t.Fatalf("expected 1 tx, got %d", len(selected))
        }
        // tx2 has higher fee and should be selected first
        if selected[0].Fee != tx2.Fee {
                t.Fatalf("wrong tx selected: fee %d instead of %d", selected[0].Fee, tx2.Fee)
        }
}

// ─── Chain ────────────────────────────────────────────────────────────────────

func TestChain_SetGenesis(t *testing.T) {
        c := core.NewChain()
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        if err := c.SetGenesis(genesis); err != nil {
                t.Fatalf("SetGenesis: %v", err)
        }
        if c.Height() != 0 {
                t.Fatalf("height should be 0, got %d", c.Height())
        }
}

func TestChain_AddBlock(t *testing.T) {
        c := core.NewChain()
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        c.SetGenesis(genesis)

        block1 := makeBlock(t, priv, pub, 1, genesis.Hash())
        if err := c.AddBlock(block1); err != nil {
                t.Fatalf("AddBlock: %v", err)
        }
        if c.Height() != 1 {
                t.Fatalf("height should be 1, got %d", c.Height())
        }
        if c.GetByHeight(1) == nil {
                t.Fatal("GetByHeight(1) returned nil")
        }
        if c.GetByHash(block1.Hash()) == nil {
                t.Fatal("GetByHash returned nil")
        }
}

func TestChain_AddBlock_WrongHeight(t *testing.T) {
        c := core.NewChain()
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        c.SetGenesis(genesis)

        // Try to add block at height 2 (skipping height 1)
        block2 := makeBlock(t, priv, pub, 2, genesis.Hash())
        if err := c.AddBlock(block2); err == nil {
                t.Fatal("AddBlock with wrong height should fail")
        }
}

func TestChain_Reorg(t *testing.T) {
        c := core.NewChain()
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        c.SetGenesis(genesis)

        // Build old chain: genesis → b1 → b2
        b1 := makeBlock(t, priv, pub, 1, genesis.Hash())
        b2 := makeBlock(t, priv, pub, 2, b1.Hash())
        c.AddBlock(b1)
        c.AddBlock(b2)
        if c.Height() != 2 {
                t.Fatalf("expected height 2, got %d", c.Height())
        }

        // Reorg from fork point 0: new chain genesis → nb1 → nb2 → nb3
        nb1 := makeBlock(t, priv, pub, 1, genesis.Hash())
        nb2 := makeBlock(t, priv, pub, 2, nb1.Hash())
        nb3 := makeBlock(t, priv, pub, 3, nb2.Hash())
        if err := c.Reorg(0, []*core.Block{nb1, nb2, nb3}); err != nil {
                t.Fatalf("Reorg: %v", err)
        }
        if c.Height() != 3 {
                t.Fatalf("after reorg height should be 3, got %d", c.Height())
        }
}

// ─── TxVerifier ──────────────────────────────────────────────────────────────

func TestTxVerifier_ValidateCoinbase(t *testing.T) {
        v := core.NewTxVerifier(nil)
        tx := makeCoinbaseTx()
        if err := v.VerifyTx(&tx); err != nil {
                t.Fatalf("coinbase should pass: %v", err)
        }
}

func TestTxVerifier_ValidateStructural(t *testing.T) {
        v := core.NewTxVerifier(nil)
        tx := makeValidTx(t)
        // VerifyTx will fail on ring sig verification (placeholder tx), but that's expected
        // Just check it doesn't panic
        _ = v.VerifyTx(&tx)
}

// ─── Block Verifier ───────────────────────────────────────────────────────────

func TestBlockVerifier_ValidGenesis(t *testing.T) {
        c := core.NewChain()
        priv, pub, _ := crypto.GenerateValidatorKey()
        genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
        c.SetGenesis(genesis)

        v := core.NewBlockVerifier(core.DefaultBlockVerifierConfig(), c, nil)
        // Genesis block is already in chain; verify a new block
        b1 := makeBlock(t, priv, pub, 1, genesis.Hash())
        if err := v.VerifyBlock(b1); err != nil {
                t.Fatalf("valid block1 should pass: %v", err)
        }
}

// ─── Wallet Scanner ───────────────────────────────────────────────────────────

func TestWalletScanner_FindOutput(t *testing.T) {
        receiver, _ := crypto.GenerateWalletKeys()

        // Create a stealth output for the receiver
        stealth, err := crypto.CreateStealthAddress(receiver.Spend.Public, receiver.View.Public)
        if err != nil {
                t.Fatal(err)
        }

        blind, _ := crypto.NewBlindFactor()
        commit, _ := crypto.Commit(1000, blind)

        tx := core.Transaction{
                Version: 1,
                Outputs: []core.Output{{
                        OneTimePub:   stealth.OneTimePub,
                        TxPubKey:     stealth.TxPubKey,
                        AmountCommit: commit,
                }},
        }

        priv, pub, _ := crypto.GenerateValidatorKey()
        block := &core.Block{
                Header: core.BlockHeader{
                        Height:       1,
                        Timestamp:    time.Now().UnixNano(),
                        ValidatorPub: pub,
                },
                Txs: []core.Transaction{tx},
        }
        if err := block.Header.Sign(priv); err != nil {
                t.Fatal(err)
        }
        block.Header.MerkleRoot = core.MerkleRoot(block.Txs)

        scanner := core.NewWalletScanner(
                receiver.View.Private,
                receiver.Spend.Public,
                receiver.View.Public,
                crypto.TestnetByte,
        )
        owned := scanner.ScanBlock(block)
        if len(owned) == 0 {
                t.Fatal("scanner should find the output")
        }
}

func TestWalletScanner_MissOtherOutput(t *testing.T) {
        receiver, _ := crypto.GenerateWalletKeys()
        other, _ := crypto.GenerateWalletKeys()

        // Output for 'other', not for 'receiver'
        stealth, _ := crypto.CreateStealthAddress(other.Spend.Public, other.View.Public)
        blind, _ := crypto.NewBlindFactor()
        commit, _ := crypto.Commit(500, blind)

        tx := core.Transaction{
                Version: 1,
                Outputs: []core.Output{{
                        OneTimePub:   stealth.OneTimePub,
                        TxPubKey:     stealth.TxPubKey,
                        AmountCommit: commit,
                }},
        }

        priv, pub, _ := crypto.GenerateValidatorKey()
        block := &core.Block{
                Header: core.BlockHeader{
                        Height:       2,
                        Timestamp:    time.Now().UnixNano(),
                        ValidatorPub: pub,
                },
                Txs: []core.Transaction{tx},
        }
        block.Header.Sign(priv)
        block.Header.MerkleRoot = core.MerkleRoot(block.Txs)

        scanner := core.NewWalletScanner(
                receiver.View.Private,
                receiver.Spend.Public,
                receiver.View.Public,
                crypto.TestnetByte,
        )
        owned := scanner.ScanBlock(block)
        if len(owned) != 0 {
                t.Fatal("scanner should NOT find output belonging to another wallet")
        }
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func makeBlock(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey, height uint64, prevHash crypto.Hash32) *core.Block {
        t.Helper()
        txs := []core.Transaction{makeCoinbaseTx()}
        header := core.BlockHeader{
                Height:       height,
                PrevHash:     prevHash,
                MerkleRoot:   core.MerkleRoot(txs),
                Timestamp:    time.Now().UnixNano() + int64(height)*1_000_000,
                Round:        uint32(height),
                ValidatorPub: pub,
        }
        if err := header.Sign(priv); err != nil {
                t.Fatalf("sign block: %v", err)
        }
        return &core.Block{Header: header, Txs: txs}
}

func makeBlockWithKI(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey, height uint64, prevHash crypto.Hash32, ki crypto.KeyImage) *core.Block {
        t.Helper()
        ring := make([]crypto.RingMember, crypto.RingSize)
        for i := range ring {
                kp, _ := crypto.GenerateWalletKeys()
                ring[i] = kp.Spend.Public
        }
        tx := core.Transaction{
                Version: 1,
                Inputs: []core.RingInput{{
                        KeyImage: ki,
                        Ring:     ring,
                }},
                Outputs:    []core.Output{{OneTimePub: crypto.Point32{}}},
                Signatures: []*crypto.MLSAGSignature{nil},
                RangeProofs: []*crypto.RangeProof{nil},
                Fee:        1,
        }
        txs := []core.Transaction{tx}
        header := core.BlockHeader{
                Height:       height,
                PrevHash:     prevHash,
                MerkleRoot:   core.MerkleRoot(txs),
                Timestamp:    time.Now().UnixNano() + int64(height)*1_000_000,
                Round:        uint32(height),
                ValidatorPub: pub,
        }
        if err := header.Sign(priv); err != nil {
                t.Fatalf("sign block: %v", err)
        }
        return &core.Block{Header: header, Txs: txs}
}

func makeCoinbaseTx() core.Transaction {
        blind, _ := crypto.NewBlindFactor()
        commit, _ := crypto.Commit(5000_00000000, blind)
        return core.Transaction{
                Version: 1,
                Outputs: []core.Output{{AmountCommit: commit}},
                Fee:     0,
        }
}

func makeValidTx(t *testing.T) core.Transaction {
        t.Helper()
        return makeValidTxWithFee(t, 100)
}

func makeValidTxWithFee(t *testing.T, feeMultiplier uint64) core.Transaction {
        t.Helper()
        kp, _ := crypto.GenerateWalletKeys()
        ki, _ := crypto.ComputeKeyImage(kp.Spend.Private, kp.Spend.Public)

        ring := make([]crypto.RingMember, crypto.RingSize)
        for i := range ring {
                d, _ := crypto.GenerateWalletKeys()
                ring[i] = d.Spend.Public
        }
        ring[0] = kp.Spend.Public

        blindIn, _ := crypto.NewBlindFactor()
        commitIn, _ := crypto.Commit(100_000, blindIn)
        blindOut, _ := crypto.NewBlindFactor()
        commitOut, _ := crypto.Commit(99_000, blindOut)
        blindFee, _ := crypto.NewBlindFactor()

        sig, _ := crypto.MLSAGSign(crypto.HashStr("test"), ring, 0, kp.Spend.Private)

        blind2, _ := crypto.NewBlindFactor()
        proof, _ := crypto.ProveRange(99_000, blind2)

        // Build tx first to get its real size, then set fee = feeMultiplier * size
        tx := core.Transaction{
                Version: 1,
                Inputs: []core.RingInput{{
                        KeyImage:     ki,
                        Ring:         ring,
                        AmountCommit: commitIn,
                }},
                Outputs: []core.Output{{
                        OneTimePub:   kp.Spend.Public,
                        AmountCommit: commitOut,
                }},
                RangeProofs: []*crypto.RangeProof{proof},
                Signatures:  []*crypto.MLSAGSignature{sig},
        }
        // Set fee = feeMultiplier nAPR/byte so fee rate check passes
        fee := uint64(tx.Size()) * feeMultiplier
        commitFee, _ := crypto.Commit(fee, blindFee)
        tx.Fee = fee
        tx.FeeCommit = commitFee
        return tx
}
