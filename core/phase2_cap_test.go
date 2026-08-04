package core

// Tests confirming Phase 2 privacy stays intact once the spent-decoy pool
// reaches its cap (maxSpentDecoys).
//
// This file lives in package core (not core_test) so it can temporarily lower
// the unexported maxSpentDecoys variable to a small value without pre-filling
// 10 000 entries.

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

// TestSpentDecoyPool_CapEnforced verifies that the spentPubKeys pool stops
// accepting new entries once it reaches maxSpentDecoys and that no panic or
// data-corruption occurs when ApplyBlockForSpentDecoys is called while the
// pool is already full.
//
// The test temporarily lowers maxSpentDecoys to 5, fills the pool exactly to
// that cap, then presents a new block whose spending transaction would move a
// 6th entry into the pool.  The pool count must remain at 5.
func TestSpentDecoyPool_CapEnforced(t *testing.T) {
	const smallCap = 5
	orig := maxSpentDecoys
	maxSpentDecoys = smallCap
	defer func() { maxSpentDecoys = orig }()

	blind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatalf("NewBlindFactor: %v", err)
	}
	commit, err := crypto.Commit(100, blind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	utxos := NewUTXOSet()

	// Fill spentPubKeys to exactly the cap using AddSpentDecoyForTest, which
	// bypasses the cap check — this mimics a pool that was filled over time.
	for i := 0; i < smallCap; i++ {
		utxos.AddSpentDecoyForTest(&UTXO{
			TxHash:       crypto.Hash32{byte(i + 1)},
			OutputIndex:  0,
			OneTimePub:   crypto.Point32{byte(i + 1)},
			AmountCommit: commit,
		})
	}
	if got := utxos.SpentDecoyCount(); got != smallCap {
		t.Fatalf("pre-test pool count = %d, want %d", got, smallCap)
	}

	// Register a fresh active UTXO in the byPubKey index so that
	// ApplyBlockForSpentDecoys can find it and attempt to move it to the pool.
	extraPub := crypto.Point32{0xAA}
	utxos.Add(&UTXO{
		TxHash:       crypto.Hash32{0xAA},
		OutputIndex:  0,
		OneTimePub:   extraPub,
		AmountCommit: commit,
	})

	// Build a minimal block whose ring references extraPub as the real member.
	ring := make([]crypto.Point32, crypto.RingSize)
	ring[0] = extraPub
	for i := 1; i < crypto.RingSize; i++ {
		ring[i] = crypto.Point32{byte(0xBB + i)}
	}
	block := &Block{
		Header: BlockHeader{Height: 1},
		Txs: []Transaction{{
			Version: TxVersionBase,
			Inputs: []RingInput{{
				Ring:         ring,
				AmountCommit: commit,
				KeyImage:     crypto.KeyImage{0xCC},
			}},
		}},
	}

	// ApplyBlockForSpentDecoys must not panic and must leave the pool at cap.
	utxos.ApplyBlockForSpentDecoys(block)

	if got := utxos.SpentDecoyCount(); got != smallCap {
		t.Fatalf("after-cap pool count = %d, want %d (cap must hold, no new entry added)",
			got, smallCap)
	}
	t.Logf("Cap correctly enforced: pool stayed at %d after attempting to add entry %d",
		smallCap, smallCap+1)
}

// TestSpentDecoyPool_CapEnforced_ViaApplyBlock verifies the same cap behaviour
// through ApplyBlock (the live-chain path), not just the startup-replay path.
// The pool must not grow beyond maxSpentDecoys even when new blocks are committed.
func TestSpentDecoyPool_CapEnforced_ViaApplyBlock(t *testing.T) {
	const smallCap = 4
	orig := maxSpentDecoys
	maxSpentDecoys = smallCap
	defer func() { maxSpentDecoys = orig }()

	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(200, blind)

	utxos := NewUTXOSet()

	// Fill the pool via ApplyBlock: commit smallCap+1 blocks each spending one UTXO.
	// The first smallCap blocks fill the pool; the (smallCap+1)-th must be silently
	// dropped so the pool does not exceed the cap.
	for round := 0; round < smallCap+1; round++ {
		// Generate a real wallet key so we can derive a valid one-time key and key image.
		wk, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatalf("round %d GenerateWalletKeys: %v", round, err)
		}
		pub := wk.Spend.Public
		utxos.Add(&UTXO{
			TxHash:       crypto.Hash32{byte(round + 1)},
			OutputIndex:  0,
			OneTimePub:   pub,
			AmountCommit: commit,
		})

		// Build a ring with the real pub at index 0 and generated decoy pubs.
		ring := make([]crypto.Point32, crypto.RingSize)
		ring[0] = pub
		for i := 1; i < crypto.RingSize; i++ {
			dk, err := crypto.GenerateWalletKeys()
			if err != nil {
				t.Fatalf("round %d GenerateWalletKeys decoy %d: %v", round, i, err)
			}
			ring[i] = dk.Spend.Public
		}

		// Compute a real key image from the wallet's spend private key.
		ki, err := crypto.ComputeKeyImage(wk.Spend.Private, pub)
		if err != nil {
			t.Fatalf("round %d ComputeKeyImage: %v", round, err)
		}

		// Change output uses a fresh key to keep the output pub unique per round.
		changeWK, _ := crypto.GenerateWalletKeys()
		block := &Block{
			Header: BlockHeader{Height: uint64(round + 1)},
			Txs: []Transaction{{
				Version: TxVersionBase,
				Inputs: []RingInput{{
					Ring:         ring,
					AmountCommit: commit,
					KeyImage:     ki,
				}},
				Outputs: []Output{{
					OneTimePub:   changeWK.Spend.Public,
					AmountCommit: commit,
				}},
			}},
		}
		if err := utxos.ApplyBlock(block); err != nil {
			t.Fatalf("round %d ApplyBlock: %v", round, err)
		}
	}

	got := utxos.SpentDecoyCount()
	if got != smallCap {
		t.Fatalf("pool count = %d, want %d (cap must hold after %d spending blocks)",
			got, smallCap, smallCap+1)
	}
	t.Logf("ApplyBlock cap correctly enforced: pool held at %d after %d spending rounds",
		smallCap, smallCap+1)
}

// TestSpentDecoyPool_FallbackDecoyCount_ExactMinimum verifies that when the
// spent-decoy pool contains exactly RingSize-1 = 15 entries — the minimum
// needed to fill all decoy slots of one ring — TxBuilder.Build returns
// FallbackDecoyCount == 0.
//
// Privacy implication: every ring slot is filled with a real on-chain spent
// UTXO; no provably-random Phase 1 key leaks the real input position.
func TestSpentDecoyPool_FallbackDecoyCount_ExactMinimum(t *testing.T) {
	const ringDecoys = crypto.RingSize - 1 // 15

	// Generate Alice's wallet keys.
	aliceKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	net := crypto.TestnetByte
	aliceAddr := crypto.AddressFromKeys(net, aliceKeys)
	_, aliceSpendPub, _, err := crypto.DecodeAddress(aliceAddr)
	if err != nil {
		t.Fatalf("DecodeAddress: %v", err)
	}

	bobKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	bobAddr := crypto.AddressFromKeys(net, bobKeys)

	// Mint a UTXO for Alice at height 1.
	const mintHeight = uint64(1)
	const mintAmount = uint64(1_000_000_000) // 1 APRO
	mintTx, err := BuildMintTx(aliceAddr, mintAmount, mintHeight)
	if err != nil {
		t.Fatalf("BuildMintTx: %v", err)
	}
	mintOut := mintTx.Outputs[0]

	mintBlind, err := crypto.DeterministicMintBlind(aliceSpendPub, mintAmount)
	if err != nil {
		t.Fatalf("DeterministicMintBlind: %v", err)
	}

	owned := []OwnedUTXO{{
		UTXO: UTXO{
			TxHash:       mintTx.Hash(),
			OutputIndex:  0,
			OneTimePub:   mintOut.OneTimePub,
			TxPubKey:     mintOut.TxPubKey,
			AmountCommit: mintOut.AmountCommit,
		},
		HsScalar: crypto.ScalarFromUint64(mintHeight),
		Amount:   mintAmount,
		Blind:    mintBlind,
	}}

	// Build a UTXOSet with exactly ringDecoys=15 spent decoys.
	utxos := NewUTXOSet()
	dBlind, _ := crypto.NewBlindFactor()
	dCommit, _ := crypto.Commit(50, dBlind)
	for i := 0; i < ringDecoys; i++ {
		dk, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatalf("GenerateWalletKeys decoy %d: %v", i, err)
		}
		utxos.AddSpentDecoyForTest(&UTXO{
			TxHash:       crypto.Hash32{byte(i + 10)},
			OutputIndex:  0,
			OneTimePub:   dk.Spend.Public,
			AmountCommit: dCommit,
		})
	}
	if got := utxos.SpentDecoyCount(); got != ringDecoys {
		t.Fatalf("pre-build decoy count = %d, want %d", got, ringDecoys)
	}

	// Also register Alice's UTXO in byPubKey so TxVerifier (C-0) can find it.
	utxos.Add(&UTXO{
		TxHash:       mintTx.Hash(),
		OutputIndex:  0,
		OneTimePub:   mintOut.OneTimePub,
		TxPubKey:     mintOut.TxPubKey,
		AmountCommit: mintOut.AmountCommit,
	})

	builder := NewTxBuilder(
		aliceKeys.Spend.Private, aliceKeys.View.Private,
		aliceSpendPub, owned, 1,
	).WithDecoySet(utxos)

	const sendAmount = uint64(30_000_000)
	result, err := builder.Build(sendAmount, bobAddr, aliceAddr)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if result.FallbackDecoyCount != 0 {
		t.Errorf("FallbackDecoyCount = %d, want 0 (pool has exactly %d decoys = RingSize-1)",
			result.FallbackDecoyCount, ringDecoys)
	}
	t.Logf("pool=%d decoys → FallbackDecoyCount=%d RealDecoyCount=%d (correct: full Phase 2 ring)",
		ringDecoys, result.FallbackDecoyCount, result.RealDecoyCount)
}

// TestSpentDecoyPool_FallbackDecoyCount_BelowMinimum verifies that when the
// pool holds fewer than RingSize-1 = 15 entries, TxBuilder.Build returns
// FallbackDecoyCount > 0 — some ring slots must fall back to Phase 1 random
// keys, degrading privacy proportionally.
func TestSpentDecoyPool_FallbackDecoyCount_BelowMinimum(t *testing.T) {
	const ringDecoys = crypto.RingSize - 1 // 15
	const available = 7                    // < 15 → some fallback required

	aliceKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	net := crypto.TestnetByte
	aliceAddr := crypto.AddressFromKeys(net, aliceKeys)
	_, aliceSpendPub, _, err := crypto.DecodeAddress(aliceAddr)
	if err != nil {
		t.Fatalf("DecodeAddress: %v", err)
	}

	bobKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	bobAddr := crypto.AddressFromKeys(net, bobKeys)

	const mintHeight = uint64(2)
	const mintAmount = uint64(1_000_000_000)
	mintTx, err := BuildMintTx(aliceAddr, mintAmount, mintHeight)
	if err != nil {
		t.Fatalf("BuildMintTx: %v", err)
	}
	mintOut := mintTx.Outputs[0]

	mintBlind, err := crypto.DeterministicMintBlind(aliceSpendPub, mintAmount)
	if err != nil {
		t.Fatalf("DeterministicMintBlind: %v", err)
	}

	owned := []OwnedUTXO{{
		UTXO: UTXO{
			TxHash:       mintTx.Hash(),
			OutputIndex:  0,
			OneTimePub:   mintOut.OneTimePub,
			TxPubKey:     mintOut.TxPubKey,
			AmountCommit: mintOut.AmountCommit,
		},
		HsScalar: crypto.ScalarFromUint64(mintHeight),
		Amount:   mintAmount,
		Blind:    mintBlind,
	}}

	// Only available=7 decoys — 8 ring slots must fall back.
	utxos := NewUTXOSet()
	dBlind, _ := crypto.NewBlindFactor()
	dCommit, _ := crypto.Commit(50, dBlind)
	for i := 0; i < available; i++ {
		dk, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatalf("GenerateWalletKeys decoy %d: %v", i, err)
		}
		utxos.AddSpentDecoyForTest(&UTXO{
			TxHash:       crypto.Hash32{byte(i + 20)},
			OutputIndex:  0,
			OneTimePub:   dk.Spend.Public,
			AmountCommit: dCommit,
		})
	}

	utxos.Add(&UTXO{
		TxHash:       mintTx.Hash(),
		OutputIndex:  0,
		OneTimePub:   mintOut.OneTimePub,
		TxPubKey:     mintOut.TxPubKey,
		AmountCommit: mintOut.AmountCommit,
	})

	builder := NewTxBuilder(
		aliceKeys.Spend.Private, aliceKeys.View.Private,
		aliceSpendPub, owned, 1,
	).WithDecoySet(utxos)

	const sendAmount = uint64(30_000_000)
	result, err := builder.Build(sendAmount, bobAddr, aliceAddr)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	wantFallback := ringDecoys - available // 15 - 7 = 8
	if result.FallbackDecoyCount != wantFallback {
		t.Errorf("FallbackDecoyCount = %d, want %d (pool=%d < RingSize-1=%d)",
			result.FallbackDecoyCount, wantFallback, available, ringDecoys)
	}
	if result.FallbackDecoyCount == 0 {
		t.Errorf("FallbackDecoyCount must be > 0 when pool < RingSize-1")
	}
	t.Logf("pool=%d decoys → FallbackDecoyCount=%d RealDecoyCount=%d (correct: partial Phase 1 fallback)",
		available, result.FallbackDecoyCount, result.RealDecoyCount)
}
