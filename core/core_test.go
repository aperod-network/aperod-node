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
	// Empty Merkle root is defined as SHA3-256("aperod/merkle-empty/v1") —
	// a deterministic non-zero sentinel that cannot be confused with an
	// uninitialised hash field.
	if root == (crypto.Hash32{}) {
		t.Fatal("empty merkle root must not be the zero hash")
	}
	// Calling again must return the same value (deterministic).
	if root != core.MerkleRoot(nil) {
		t.Fatal("empty merkle root is not deterministic")
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

// ─── CompactKeyImages ─────────────────────────────────────────────────────────

// TestCompactKeyImages_EmptyRecent_NoOp verifies that calling CompactKeyImages
// on a set whose 'recent' map is already empty returns 0 and does not panic or
// corrupt the existing 'sorted' slice.
func TestCompactKeyImages_EmptyRecent_NoOp(t *testing.T) {
	s := core.NewUTXOSet()
	// Mark some key images spent — they go into 'recent'.
	kp1, _ := crypto.GenerateWalletKeys()
	ki1, _ := crypto.ComputeKeyImage(kp1.Spend.Private, kp1.Spend.Public)
	s.MarkSpent(ki1)

	// First compaction: merges ki1 from 'recent' into 'sorted'.
	moved := s.CompactKeyImages()
	if moved != 1 {
		t.Fatalf("first compact: want 1 moved, got %d", moved)
	}
	if s.KeyImagesRecentCount() != 0 {
		t.Fatalf("after first compact: want 0 in recent, got %d", s.KeyImagesRecentCount())
	}

	// Second compaction: recent is now empty → should be a no-op.
	moved = s.CompactKeyImages()
	if moved != 0 {
		t.Fatalf("second compact (empty recent): want 0 moved, got %d", moved)
	}
	// The original key image must still be detectable as spent.
	if !s.IsSpent(ki1) {
		t.Fatal("key image no longer spent after no-op compaction")
	}
}

// TestCompactKeyImages_PreservesMembership verifies that all key images marked
// spent before compaction are still reported as spent afterwards, and that
// key images never marked spent are still reported as unspent.
func TestCompactKeyImages_PreservesMembership(t *testing.T) {
	const n = 50
	s := core.NewUTXOSet()

	spent := make([]crypto.KeyImage, n)
	for i := 0; i < n; i++ {
		kp, _ := crypto.GenerateWalletKeys()
		ki, _ := crypto.ComputeKeyImage(kp.Spend.Private, kp.Spend.Public)
		spent[i] = ki
		s.MarkSpent(ki)
	}
	if s.KeyImagesRecentCount() != n {
		t.Fatalf("before compact: want %d in recent, got %d", n, s.KeyImagesRecentCount())
	}
	if s.KeyImagesCount() != n {
		t.Fatalf("before compact: total count want %d, got %d", n, s.KeyImagesCount())
	}

	moved := s.CompactKeyImages()
	if moved != n {
		t.Fatalf("compact: want %d moved, got %d", n, moved)
	}

	// After compaction the 'recent' map must be empty.
	if s.KeyImagesRecentCount() != 0 {
		t.Fatalf("after compact: want 0 in recent, got %d", s.KeyImagesRecentCount())
	}
	// Total count must be unchanged.
	if s.KeyImagesCount() != n {
		t.Fatalf("after compact: total count want %d, got %d", n, s.KeyImagesCount())
	}
	// Every originally-spent key image must still be spent.
	for i, ki := range spent {
		if !s.IsSpent(ki) {
			t.Errorf("key image [%d] no longer spent after compaction", i)
		}
	}
	// A fresh key image must not be reported as spent.
	kpFresh, _ := crypto.GenerateWalletKeys()
	kiFresh, _ := crypto.ComputeKeyImage(kpFresh.Spend.Private, kpFresh.Spend.Public)
	if s.IsSpent(kiFresh) {
		t.Fatal("fresh key image incorrectly reported as spent after compaction")
	}
}

// TestCompactKeyImages_NewEntriesAfterCompact verifies that key images added
// after a compaction are correctly placed in 'recent' and remain detectable.
func TestCompactKeyImages_NewEntriesAfterCompact(t *testing.T) {
	s := core.NewUTXOSet()

	// Pre-compact batch.
	kp1, _ := crypto.GenerateWalletKeys()
	ki1, _ := crypto.ComputeKeyImage(kp1.Spend.Private, kp1.Spend.Public)
	s.MarkSpent(ki1)
	s.CompactKeyImages()

	// Post-compact batch.
	kp2, _ := crypto.GenerateWalletKeys()
	ki2, _ := crypto.ComputeKeyImage(kp2.Spend.Private, kp2.Spend.Public)
	s.MarkSpent(ki2)

	if s.KeyImagesRecentCount() != 1 {
		t.Fatalf("want 1 in recent after post-compact insert, got %d", s.KeyImagesRecentCount())
	}
	if s.KeyImagesCount() != 2 {
		t.Fatalf("total count want 2, got %d", s.KeyImagesCount())
	}
	if !s.IsSpent(ki1) {
		t.Fatal("pre-compact key image no longer spent")
	}
	if !s.IsSpent(ki2) {
		t.Fatal("post-compact key image not detected as spent")
	}

	// Second compaction merges ki2 into sorted; ki1 already in sorted.
	moved := s.CompactKeyImages()
	if moved != 1 {
		t.Fatalf("second compact: want 1 moved, got %d", moved)
	}
	if s.KeyImagesRecentCount() != 0 {
		t.Fatalf("want 0 in recent after second compact, got %d", s.KeyImagesRecentCount())
	}
	if !s.IsSpent(ki1) || !s.IsSpent(ki2) {
		t.Fatal("key image missing after second compaction")
	}
}

// ─── RebuildKeyImages ─────────────────────────────────────────────────────────

// TestRebuildKeyImages_StaleKIRestoresUTXO confirms that the --rebuild-key-images
// flow (ClearKeyImages + MarkSpent replay from confirmed blocks) removes a stale
// phantom key image that was blocking an active on-chain UTXO.
//
// Production scenario reproduced here:
//  1. A user sends a transaction.  The Go node processes the ring signature and
//     adds the key image to the in-memory UTXOSet.
//  2. Before the transaction is included in a block the node is OOM-killed.
//  3. The startup snapshot, saved just before the OOM kill, contains the stale
//     key image even though the transaction was never confirmed on-chain.
//  4. After restart the UTXO is reported as already-spent (IsSpent == true),
//     making it unspendable.
//  5. The operator runs --rebuild-key-images, which calls ClearKeyImages() and
//     then replays MarkSpent() only for key images found in confirmed blocks.
//  6. The stale key image is absent from every block, so after the rebuild it is
//     gone from the set and the UTXO becomes spendable again.
func TestRebuildKeyImages_StaleKIRestoresUTXO(t *testing.T) {
	s := core.NewUTXOSet()

	// ── Step 1: create the UTXO that will be "blocked" ────────────────────
	// Generate a wallet key pair.  Its spend key pair gives us a deterministic
	// key image we can inject as the phantom entry.
	kpOwner, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	staleKI, err := crypto.ComputeKeyImage(kpOwner.Spend.Private, kpOwner.Spend.Public)
	if err != nil {
		t.Fatalf("ComputeKeyImage: %v", err)
	}

	// Register the UTXO in the set (active, unspent).
	u := &core.UTXO{
		TxHash:      crypto.HashStr("stale-ki-test-tx"),
		OutputIndex: 0,
		OneTimePub:  kpOwner.Spend.Public,
		BlockHeight: 42,
	}
	s.Add(u)

	// ── Step 2: inject the stale key image (simulates OOM-kill snapshot) ──
	// The stale KI lands in the set the same way a real snapshot restore would:
	// via MarkSpent, which is what loadStartupSnapshot calls after deserialising
	// the snapshot's key-image list.
	s.MarkSpent(staleKI)

	if !s.IsSpent(staleKI) {
		t.Fatal("pre-condition: stale key image should be marked spent before rebuild")
	}
	// The UTXO still exists in the set (it was never actually spent on-chain),
	// but any attempt to spend it would be rejected because IsSpent returns true.
	if s.Get(u.TxHash, 0) == nil {
		t.Fatal("pre-condition: UTXO should still be present in the set")
	}

	// ── Step 3: create a legitimately spent key image (confirmed on-chain) ──
	// This key image belongs to a different output that WAS included in a block.
	// After rebuild it must still be marked spent.
	kpSpent, _ := crypto.GenerateWalletKeys()
	legitKI, _ := crypto.ComputeKeyImage(kpSpent.Spend.Private, kpSpent.Spend.Public)
	// The legitimate KI is NOT injected before the rebuild; it is discovered
	// during the block-scan replay below (step 4).

	// ── Step 4: simulate rebuildKeyImagesFromBlocks ───────────────────────
	// The production function (blockchain/cmd/node/main.go) does exactly:
	//   utxos.ClearKeyImages()
	//   for each confirmed block:
	//       for each tx input:
	//           utxos.MarkSpent(inp.KeyImage)
	//
	// We replicate that here with a single confirmed-on-chain key image.
	// The stale key image is intentionally absent — it was never in a block.
	s.ClearKeyImages()
	s.MarkSpent(legitKI) // only the legitimately confirmed key image is replayed

	// ── Step 5: assert the stale key image is gone ────────────────────────
	if s.IsSpent(staleKI) {
		t.Fatal("after rebuild: stale key image must no longer be marked spent — UTXO should be unblocked")
	}

	// ── Step 6: assert the legitimate key image is still spent ────────────
	if !s.IsSpent(legitKI) {
		t.Fatal("after rebuild: legitimate on-chain key image must still be marked spent")
	}

	// ── Step 7: sanity-check UTXO accessibility ───────────────────────────
	// The blocked UTXO is still in the set (rebuild does not touch the UTXO
	// data, only the key-image index).  A wallet can now spend it because
	// IsSpent returns false.
	if s.Get(u.TxHash, 0) == nil {
		t.Fatal("after rebuild: blocked UTXO should still be present in the set")
	}
}

// TestRebuildKeyImages_MultipleStaleEntries verifies that rebuild clears ALL
// stale key images, not just one, when several phantom entries were accumulated
// (e.g. the node restarted multiple times while the same tx was re-submitted).
func TestRebuildKeyImages_MultipleStaleEntries(t *testing.T) {
	const numStale = 5
	s := core.NewUTXOSet()

	staleKIs := make([]crypto.KeyImage, numStale)
	for i := 0; i < numStale; i++ {
		kp, _ := crypto.GenerateWalletKeys()
		ki, _ := crypto.ComputeKeyImage(kp.Spend.Private, kp.Spend.Public)
		staleKIs[i] = ki
		s.MarkSpent(ki)
	}

	// Confirm all stale entries are present before rebuild.
	for i, ki := range staleKIs {
		if !s.IsSpent(ki) {
			t.Fatalf("pre-condition: stale key image [%d] not marked spent", i)
		}
	}

	// One legitimate on-chain key image.
	kpLegit, _ := crypto.GenerateWalletKeys()
	legitKI, _ := crypto.ComputeKeyImage(kpLegit.Spend.Private, kpLegit.Spend.Public)

	// Simulate rebuildKeyImagesFromBlocks.
	s.ClearKeyImages()
	s.MarkSpent(legitKI)

	// All stale entries must be gone.
	for i, ki := range staleKIs {
		if s.IsSpent(ki) {
			t.Errorf("after rebuild: stale key image [%d] still marked spent", i)
		}
	}
	// The legitimate entry must remain.
	if !s.IsSpent(legitKI) {
		t.Fatal("after rebuild: legitimate key image must still be spent")
	}
	// Total count should be exactly 1 (only the legit KI).
	if got := s.KeyImagesCount(); got != 1 {
		t.Fatalf("after rebuild: key image count want 1, got %d", got)
	}
}

func TestClearPhantomSpent_PreservesRecentlyConfirmedSpend(t *testing.T) {
	s := core.NewUTXOSet()
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	ki, err := crypto.ComputeKeyImage(keys.Spend.Private, keys.Spend.Public)
	if err != nil {
		t.Fatalf("ComputeKeyImage: %v", err)
	}

	s.MarkSpent(ki)
	cleared, err := s.ClearPhantomSpent(ki)
	if err != nil {
		t.Fatalf("ClearPhantomSpent: %v", err)
	}
	if cleared {
		t.Fatal("recently confirmed key image was cleared as phantom")
	}
	if !s.IsSpent(ki) {
		t.Fatal("recently confirmed key image no longer marked spent")
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
	// Add two txs with different fees (higher fee wins); both must meet MinFee.
	tx1 := makeValidTxWithFee(t, core.InitialBaseFeePerByte*1000)
	tx2 := makeValidTxWithFee(t, core.InitialBaseFeePerByte*2000)
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
	// VerifyTx must REJECT coinbase (zero-input) transactions.
	// Coinbase txs are synthesized by the consensus engine only and must
	// never be accepted from external sources (P2P peers, RPC callers, etc.).
	// VerifyBlock skips coinbase rows; VerifyTx is the external-tx path.
	v := core.NewTxVerifier(nil)
	tx := makeCoinbaseTx()
	if err := v.VerifyTx(&tx); err == nil {
		t.Fatal("VerifyTx should reject coinbase (zero-input) transaction, but returned nil")
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
		Outputs:     []core.Output{{OneTimePub: crypto.Point32{}}},
		Signatures:  []*crypto.MLSAGSignature{nil},
		RangeProofs: []*crypto.RangeProof{nil},
		Fee:         1,
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
	// Multiplier must exceed the serialised tx size (~1972 bytes at InitialBaseFeePerByte=200).
	// Use 3000 so fee = 200 * 3000 = 600_000 nAPRO ≥ 1972 * 200 = 394_400 nAPRO minimum.
	return makeValidTxWithFee(t, core.InitialBaseFeePerByte*3000)
}

// makeValidTxWithFee creates a synthetic RingCT transaction with the given absolute fee.
func makeValidTxWithFee(t *testing.T, fee uint64) core.Transaction {
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

	// Build tx and apply the provided absolute fee.
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
	commitFee, _ := crypto.Commit(fee, blindFee)
	tx.Fee = fee
	tx.FeeCommit = commitFee
	return tx
}
