package core_test

// tx_verifier_test.go — tests for TxVerifier C-0 commitment binding and
// double-spend key-image rejection (#486, #487).

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// makeUTXO creates a minimal UTXO for testing.
func makeUTXO(txHash crypto.Hash32, outIdx uint32, pub crypto.Point32, commit crypto.Commitment) *core.UTXO {
	return &core.UTXO{
		TxHash:       txHash,
		OutputIndex:  outIdx,
		OneTimePub:   pub,
		AmountCommit: commit,
	}
}

// makeTxWithRingAndCommit builds a minimal tx whose single input uses pub as
// the only ring member and sets AmountCommit to commit.
func makeTxWithRingAndCommit(pub crypto.Point32, commit crypto.Commitment, ki crypto.KeyImage) core.Transaction {
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = pub
	for i := 1; i < crypto.RingSize; i++ {
		ring[i][0] = byte(i + 100)
	}
	return core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{
				KeyImage:     ki,
				Ring:         ring,
				AmountCommit: commit,
			},
		},
		Outputs: []core.Output{
			{
				OneTimePub:   crypto.Point32{0xFE},
				TxPubKey:     crypto.Point32{},
				AmountCommit: crypto.Commitment{},
			},
		},
		Fee:         500,
		Signatures:  []*crypto.MLSAGSignature{{}},
		RangeProofs: []*crypto.RangeProof{{}},
	}
}

// populateFullRingUTXOs adds all RingSize ring members to the UTXO set with
// the given commit, so the C-0 check can pass for every ring member.
// Returns the ring (same slice used in makeTxWithRingAndCommit).
func populateFullRingUTXOs(utxos *core.UTXOSet, realPub crypto.Point32, commit crypto.Commitment) []crypto.Point32 {
	ring := make([]crypto.Point32, crypto.RingSize)
	ring[0] = realPub
	for i := 1; i < crypto.RingSize; i++ {
		ring[i][0] = byte(i + 100)
	}
	for idx, pub := range ring {
		utxos.Add(makeUTXO(crypto.Hash32{byte(idx + 1)}, 0, pub, commit))
	}
	return ring
}

// TestTxVerifier_C0_FabricatedRingCommitment (#486) — TxVerifier must reject a
// tx where the ring member's AmountCommit does not match the on-chain UTXO commit.
func TestTxVerifier_C0_FabricatedRingCommitment(t *testing.T) {
	commitOnChain := crypto.Commitment{0xAA}
	fabricatedCommit := crypto.Commitment{0xBB}
	realPub := crypto.Point32{0x01, 0x02, 0x03}

	// Populate UTXOSet: all ring members have commitOnChain.
	utxos := core.NewUTXOSet()
	populateFullRingUTXOs(utxos, realPub, commitOnChain)

	v := core.NewTxVerifier(utxos)

	// Bad tx: correct ring member pub, but FABRICATED AmountCommit in the input.
	ki1 := makeKeyImage(201)
	txBad := makeTxWithRingAndCommit(realPub, fabricatedCommit, ki1)

	err := v.VerifyTx(&txBad)
	if err == nil {
		t.Fatal("TxVerifier should reject a tx with a fabricated AmountCommit, got nil error")
	}
	if !strings.Contains(err.Error(), "C-0") {
		t.Errorf("error message should cite the C-0 check, got: %v", err)
	}

	// Positive case: same ring with the CORRECT on-chain commit.
	// The C-0 check passes; MLSAG will still fail (dummy sig), but the error
	// must NOT cite "C-0".
	ki2 := makeKeyImage(202)
	txOK := makeTxWithRingAndCommit(realPub, commitOnChain, ki2)
	errOK := v.VerifyTx(&txOK)
	if errOK != nil && strings.Contains(errOK.Error(), "C-0") {
		t.Errorf("correct AmountCommit should pass the C-0 check, but got C-0 error: %v", errOK)
	}
}

// TestTxVerifier_DoubleSpendKeyImage_RejectedByVerifier (#487 partial) —
// TxVerifier must reject a tx that reuses a key image already marked spent.
func TestTxVerifier_DoubleSpendKeyImage_RejectedByVerifier(t *testing.T) {
	ki := makeKeyImage(301)

	utxos := core.NewUTXOSet()
	utxos.MarkSpent(ki)

	v := core.NewTxVerifier(utxos)
	tx := makeTx(1000, 301)

	err := v.VerifyTx(&tx)
	if err == nil {
		t.Fatal("TxVerifier should reject a tx with an already-spent key image, got nil")
	}
	if !strings.Contains(err.Error(), "double-spend") && !strings.Contains(err.Error(), "already spent") {
		t.Errorf("error should mention double-spend, got: %v", err)
	}
}

// TestMempool_DoubleSpendKeyImage_Rejected (#487) — pool.Add() must return an
// error when the verifier has the key image marked spent.
func TestMempool_DoubleSpendKeyImage_Rejected(t *testing.T) {
	ki := makeKeyImage(401)

	utxos := core.NewUTXOSet()
	utxos.MarkSpent(ki)

	v := core.NewTxVerifier(utxos)

	cfg := core.MempoolConfig{
		MaxSize:        10,
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 0, // disable fee check so only the verifier decides
	}
	pool := core.NewMempool(cfg, silentLogger())
	pool.SetVerifier(v)

	tx := makeTx(1000, 401) // uses makeKeyImage(401) == ki
	err := pool.Add(tx)
	if err == nil {
		t.Fatal("pool.Add() should reject a tx with an already-spent key image, got nil")
	}

	// Verify no entry was added to the pool.
	if len(pool.Hashes()) != 0 {
		t.Errorf("pool should be empty after rejection, got %d entries", len(pool.Hashes()))
	}
}
