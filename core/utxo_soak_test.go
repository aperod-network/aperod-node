//go:build soak

package core

// utxo_soak_test.go — memory-growth regression test for UTXOSet.ApplyBlock.
//
// Build tag: soak (excluded from regular "go test ./..." runs).
//
// Run with:
//
//	go test -tags soak -run TestUTXOSet_MemoryGrowth_10KBlocks -v ./core/
//
// The test applies 10 000 blocks of synthetic spend-and-create traffic and
// asserts that live heap growth stays below 50 MiB.  It guards the OOM
// regression fixed in ApplyBlock: spent UTXOs must be deleted from the
// primary s.utxos map so only the live unspent set occupies memory, rather
// than the entire historical output set since genesis.
//
// Chain structure:
//
//	Block 0           : coinbase → wallets[0]  (creates UTXO_0)
//	Block 1..9999     : spend UTXO_{i-1} → output to wallets[i]  (creates UTXO_i)
//
// At steady state s.utxos holds exactly ONE entry (the latest unspent UTXO).
// Without the ApplyBlock fix it would hold 10 000 entries (all ever created).

import (
	"runtime"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
)

// soakHeapInuseMiB runs a full GC cycle and returns the current live heap in MiB.
func soakHeapInuseMiB() float64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapInuse) / (1024 * 1024)
}

// buildSoakBlock is a local block-construction helper for the soak test.
// It mirrors the pattern used in utxo_rollback_test.go.
func buildSoakBlock(
	t *testing.T,
	valPriv crypto.ValidatorPrivKey,
	valPub crypto.ValidatorPubKey,
	height uint64,
	prevHash crypto.Hash32,
	txs []Transaction,
) *Block {
	t.Helper()
	hdr := BlockHeader{
		Height:       height,
		PrevHash:     prevHash,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: valPub,
		MerkleRoot:   MerkleRoot(txs),
	}
	if err := hdr.Sign(valPriv); err != nil {
		t.Fatalf("buildSoakBlock height %d Sign: %v", height, err)
	}
	return &Block{Header: hdr, Txs: txs}
}

// TestUTXOSet_MemoryGrowth_10KBlocks is the primary soak assertion.
//
// It verifies that ApplyBlock correctly removes spent UTXOs from the primary
// s.utxos map, keeping live heap growth proportional to the UNSPENT set size
// rather than the total-ever-created size.
//
// Memory limit: 50 MiB over 10 000 spend+create cycles.  This is generous
// enough to accommodate GC hysteresis, decoy-pool overhead, and key-image map
// growth while still catching an unbounded-accumulation regression (which
// would require ~800+ MiB for 10K 200-byte UTXOs).
func TestUTXOSet_MemoryGrowth_10KBlocks(t *testing.T) {
	const numBlocks = 10_000
	const maxHeapGrowthMiB = 50.0

	// Cap the decoy pool at a fraction of numBlocks so the pool itself does
	// not dominate the heap budget.  The regression under test lives in
	// s.utxos, not in spentPubKeys.
	origMax := maxSpentDecoys
	maxSpentDecoys = 1_000
	defer func() { maxSpentDecoys = origMax }()

	// ── Validator keys (shared across all blocks) ─────────────────────────────
	valPriv, valPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// ── Pre-generate one wallet key pair per block ────────────────────────────
	// Each pair provides:
	//   • Spend.Public  — used as OneTimePub for that block's coinbase output
	//                     (and as the real ring member in the next block's spend)
	//   • Spend.Private — used as the real signing key in MLSAGSign
	//
	// Pre-generation is done up-front so that allocation during key generation
	// does not inflate the baseline heap measurement taken before block 0.
	t.Logf("pre-generating %d wallet key pairs...", numBlocks)
	wallets := make([]*crypto.WalletKeyPair, numBlocks)
	for i := range wallets {
		w, werr := crypto.GenerateWalletKeys()
		if werr != nil {
			t.Fatalf("GenerateWalletKeys[%d]: %v", i, werr)
		}
		wallets[i] = w
	}

	// ── Block 0: genesis coinbase → wallets[0] ────────────────────────────────
	cb0 := CoinbaseTx(wallets[0].Spend.Public, 100_000_000)
	b0 := buildSoakBlock(t, valPriv, valPub, 0, crypto.Hash32{}, []Transaction{cb0})

	utxos := NewUTXOSet()
	if err := utxos.ApplyBlock(b0); err != nil {
		t.Fatalf("ApplyBlock(0): %v", err)
	}

	cb0Hash := cb0.Hash()
	prevUTXO := utxos.Get(cb0Hash, 0)
	if prevUTXO == nil {
		t.Fatal("genesis coinbase UTXO missing after block 0")
	}
	prevBlockHash := b0.Hash()
	prevWallet := wallets[0]

	// Measure the baseline heap AFTER all setup allocations have settled.
	// Pre-allocating the wallets slice above ensures the baseline includes
	// that cost, so growth is measured only against ApplyBlock's allocations.
	baselineHeap := soakHeapInuseMiB()
	t.Logf("baseline heap (after block 0, %d wallets pre-generated): %.1f MiB",
		numBlocks, baselineHeap)

	// ── Blocks 1 … numBlocks-1: spend previous output, create new one ─────────
	//
	// Each block contains exactly one RingCT spend transaction:
	//   Input : spends prevUTXO (real ring member = prevWallet.Spend.Public)
	//   Output: new UTXO to wallets[i].Spend.Public
	//
	// Using MLSAGSign produces a cryptographically valid key image, which is
	// required for ApplyBlock's CanonicalKeyImage check to succeed.
	//
	// Ring slots 1..RingSize-1 are filled with freshly-generated decoy public
	// keys.  These keys are NOT in byPubKey, so ApplyBlock's commitment scan
	// terminates at ring[0] (the real member) — exactly as in production.
	for i := 1; i < numBlocks; i++ {
		// Build ring: real member first, random decoys after.
		ring := make([]crypto.RingMember, crypto.RingSize)
		ring[0] = crypto.RingMember(prevWallet.Spend.Public)
		for j := 1; j < crypto.RingSize; j++ {
			decoy, dErr := crypto.GenerateWalletKeys()
			if dErr != nil {
				t.Fatalf("GenerateWalletKeys (decoy block=%d slot=%d): %v", i, j, dErr)
			}
			ring[j] = crypto.RingMember(decoy.Spend.Public)
		}

		// Sign with the previous block's spending key.
		var msgHash crypto.Hash32
		msgHash[0] = byte(i)
		msgHash[1] = byte(i >> 8)
		sig, sigErr := crypto.MLSAGSign(msgHash, ring, 0, prevWallet.Spend.Private)
		if sigErr != nil {
			t.Fatalf("MLSAGSign block %d: %v", i, sigErr)
		}

		// Build the spend transaction.
		spendTx := Transaction{
			Inputs: []RingInput{
				{
					KeyImage:     sig.KeyImage,
					Ring:         ring,
					AmountCommit: prevUTXO.AmountCommit,
				},
			},
			Outputs: []Output{
				{
					// wallets[i].Spend.Public is the new owner's OneTimePub.
					// Using View.Public for the TxPubKey slot keeps the output
					// structurally valid without requiring a full stealth derivation.
					OneTimePub:   wallets[i].Spend.Public,
					TxPubKey:     wallets[i].View.Public,
					AmountCommit: prevUTXO.AmountCommit,
				},
			},
			Signatures: []*crypto.MLSAGSignature{sig},
		}

		block := buildSoakBlock(t, valPriv, valPub, uint64(i), prevBlockHash,
			[]Transaction{spendTx})
		if err := utxos.ApplyBlock(block); err != nil {
			t.Fatalf("ApplyBlock(%d): %v", i, err)
		}

		spendHash := spendTx.Hash()
		newUTXO := utxos.Get(spendHash, 0)
		if newUTXO == nil {
			t.Fatalf("block %d: output UTXO missing after ApplyBlock", i)
		}

		// Progress report every 1 000 blocks so the operator can see the test
		// is alive without waiting for the full 10 000-block run.
		if i%1_000 == 0 {
			heap := soakHeapInuseMiB()
			t.Logf("  block %5d: heap %.1f MiB  (growth %.1f MiB)",
				i, heap, heap-baselineHeap)
		}

		prevBlockHash = block.Hash()
		prevUTXO = newUTXO
		prevWallet = wallets[i]
	}

	// ── Memory assertion ──────────────────────────────────────────────────────
	finalHeap := soakHeapInuseMiB()
	growth := finalHeap - baselineHeap

	// Diagnostic counters help pinpoint the source of any regression.
	utxos.mu.RLock()
	utxosLen := len(utxos.utxos)
	keyImagesLen := utxos.keyImages.length()
	spentPubKeysLen := len(utxos.spentPubKeys)
	rollbackJournalLen := len(utxos.rollbackJournal)
	utxos.mu.RUnlock()

	t.Logf("after %d blocks:", numBlocks)
	t.Logf("  heap:              %.1f MiB  (growth %.1f MiB, limit %.0f MiB)",
		finalHeap, growth, maxHeapGrowthMiB)
	t.Logf("  s.utxos entries:   %d  (expected 1 — only the last unspent output)",
		utxosLen)
	t.Logf("  s.keyImages:       %d", keyImagesLen)
	t.Logf("  s.spentPubKeys:    %d  (capped at %d)", spentPubKeysLen, maxSpentDecoys)
	t.Logf("  s.rollbackJournal: %d heights (capped at %d)", rollbackJournalLen, maxRollbackDepth)

	// The primary invariant: after 9999 spends the primary UTXO map must
	// contain exactly ONE entry (the unspent output of the final block).
	// Any higher count means ApplyBlock is NOT deleting spent UTXOs — the
	// exact OOM regression this test was written to catch.
	if utxosLen != 1 {
		t.Errorf("s.utxos has %d entries after %d spend+create cycles; "+
			"expected 1 — ApplyBlock must delete spent UTXOs from s.utxos",
			utxosLen, numBlocks-1)
	}

	if growth > maxHeapGrowthMiB {
		t.Errorf("heap grew %.1f MiB over %d blocks — exceeds %.0f MiB limit\n"+
			"  This indicates unbounded accumulation of spent UTXOs.\n"+
			"  Check that ApplyBlock deletes the real spent UTXO from s.utxos\n"+
			"  (the delete at UTXOKey{TxHash, OutputIndex} in Pass 2).",
			growth, numBlocks, maxHeapGrowthMiB)
	}

	// Verify key-image map is also bounded.  It cannot be pruned (that would
	// break double-spend detection) so it should grow by exactly numBlocks-1.
	expectedKI := numBlocks - 1 // blocks 1..9999 each add one key image
	if keyImagesLen != expectedKI {
		t.Errorf("s.keyImages: got %d entries, expected %d",
			keyImagesLen, expectedKI)
	}
}

// BenchmarkUTXOSet_ApplyBlock_SoakChain measures ApplyBlock throughput for a
// sustained spend+create workload and reports bytes-per-block allocations.
// Unlike the memory-growth test this benchmark runs b.N iterations (each
// iteration is one spend+create block) so it integrates naturally with the
// Go testing framework's -bench flag.
//
// Run with:
//
//	go test -tags soak -bench BenchmarkUTXOSet_ApplyBlock_SoakChain \
//	  -benchmem -benchtime=10000x ./core/
func BenchmarkUTXOSet_ApplyBlock_SoakChain(b *testing.B) {
	origMax := maxSpentDecoys
	maxSpentDecoys = 1_000
	defer func() { maxSpentDecoys = origMax }()

	valPriv, valPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		b.Fatalf("GenerateValidatorKey: %v", err)
	}

	// Pre-generate b.N+1 wallets (one extra for the initial coinbase).
	wallets := make([]*crypto.WalletKeyPair, b.N+1)
	for i := range wallets {
		w, werr := crypto.GenerateWalletKeys()
		if werr != nil {
			b.Fatalf("GenerateWalletKeys[%d]: %v", i, werr)
		}
		wallets[i] = w
	}

	// Block 0: genesis coinbase.
	cb0 := CoinbaseTx(wallets[0].Spend.Public, 100_000_000)
	b0 := &Block{
		Header: func() BlockHeader {
			h := BlockHeader{
				Height:       0,
				Timestamp:    time.Now().UnixNano(),
				ValidatorPub: valPub,
				MerkleRoot:   MerkleRoot([]Transaction{cb0}),
			}
			_ = h.Sign(valPriv)
			return h
		}(),
		Txs: []Transaction{cb0},
	}

	utxos := NewUTXOSet()
	if err := utxos.ApplyBlock(b0); err != nil {
		b.Fatalf("ApplyBlock(0): %v", err)
	}
	prevUTXO := utxos.Get(cb0.Hash(), 0)
	prevBlockHash := b0.Hash()
	prevWallet := wallets[0]

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ring := make([]crypto.RingMember, crypto.RingSize)
		ring[0] = crypto.RingMember(prevWallet.Spend.Public)
		for j := 1; j < crypto.RingSize; j++ {
			decoy, _ := crypto.GenerateWalletKeys()
			ring[j] = crypto.RingMember(decoy.Spend.Public)
		}
		var msgHash crypto.Hash32
		msgHash[0] = byte(i)
		sig, _ := crypto.MLSAGSign(msgHash, ring, 0, prevWallet.Spend.Private)

		spendTx := Transaction{
			Inputs: []RingInput{
				{
					KeyImage:     sig.KeyImage,
					Ring:         ring,
					AmountCommit: prevUTXO.AmountCommit,
				},
			},
			Outputs: []Output{
				{
					OneTimePub:   wallets[i+1].Spend.Public,
					TxPubKey:     wallets[i+1].View.Public,
					AmountCommit: prevUTXO.AmountCommit,
				},
			},
			Signatures: []*crypto.MLSAGSignature{sig},
		}

		height := uint64(i + 1)
		hdr := BlockHeader{
			Height:       height,
			PrevHash:     prevBlockHash,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: valPub,
			MerkleRoot:   MerkleRoot([]Transaction{spendTx}),
		}
		_ = hdr.Sign(valPriv)
		block := &Block{Header: hdr, Txs: []Transaction{spendTx}}

		if berr := utxos.ApplyBlock(block); berr != nil {
			b.Fatalf("ApplyBlock(%d): %v", height, berr)
		}

		prevUTXO = utxos.Get(spendTx.Hash(), 0)
		if prevUTXO == nil {
			b.Fatalf("block %d: output UTXO missing", height)
		}
		prevBlockHash = block.Hash()
		prevWallet = wallets[i+1]

		// Validate the invariant on every iteration: s.utxos must never
		// accumulate spent entries.
		utxos.mu.RLock()
		n := len(utxos.utxos)
		utxos.mu.RUnlock()
		if n != 1 {
			b.Fatalf("block %d: s.utxos has %d entries (expected 1); "+
				"ApplyBlock is not deleting spent UTXOs", height, n)
		}
	}

	// Report the final utxos map size so go test -v output is informative.
	b.ReportMetric(float64(len(utxos.utxos)), "utxos")
	b.ReportMetric(float64(utxos.keyImages.length()), "keyImages")
}
