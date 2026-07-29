package core_test

// security_test.go — confirms that VerifyBlock rejects blocks containing
// transactions with tampered or unbalanced Pedersen commitments (#411).

import (
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// makeTestBlock wraps a list of transactions into a signed block at height h.
func makeTestBlock(t *testing.T, h uint64, txs []core.Transaction) *core.Block {
	t.Helper()
	priv, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	header := core.BlockHeader{
		Height:       h,
		PrevHash:     crypto.Hash32{byte(h)},
		MerkleRoot:   core.MerkleRoot(txs),
		Timestamp:    time.Now().UnixNano() + int64(h)*1_000_000,
		Round:        uint32(h),
		ValidatorPub: priv.Public(),
	}
	if err := header.Sign(priv); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return &core.Block{Header: header, Txs: txs}
}

// TestVerifyBlock_TamperedOutputCommitment_Rejected confirms that a block
// containing a transaction where the output AmountCommit has been tampered
// with (flipped bits) is rejected by VerifyBlock (#411).
//
// Tampering breaks the proof.ValueCommit == output.AmountCommit check AND
// the commitment-balance check — either of which alone is fatal.
func TestVerifyBlock_TamperedOutputCommitment_Rejected(t *testing.T) {
	tx := makeValidTx(t)

	// Tamper: flip high bits of the first output's commitment.
	tampered := tx.Outputs[0].AmountCommit
	tampered[0] ^= 0xFF
	tx.Outputs[0].AmountCommit = tampered

	block := makeTestBlock(t, 1, []core.Transaction{makeCoinbaseTx(), tx})

	err := core.NewTxVerifier(core.NewUTXOSet()).VerifyBlock(block)
	if err == nil {
		t.Fatal("VerifyBlock must reject a block with a tampered output commitment")
	}
	t.Logf("correctly rejected (%v)", err)
}

// TestVerifyBlock_UnbalancedFeeCommitment_Rejected confirms that a block whose
// transaction has a FeeCommit that does not balance (ΣC_in ≠ ΣC_out + C_fee)
// is rejected by the commitment-balance check (#411).
func TestVerifyBlock_UnbalancedFeeCommitment_Rejected(t *testing.T) {
	tx := makeValidTx(t)

	// Replace FeeCommit with a commitment to fee+1 (definitely unbalanced).
	wrongBlind, _ := crypto.NewBlindFactor()
	tx.FeeCommit, _ = crypto.Commit(tx.Fee+1, wrongBlind)

	block := makeTestBlock(t, 2, []core.Transaction{makeCoinbaseTx(), tx})

	err := core.NewTxVerifier(core.NewUTXOSet()).VerifyBlock(block)
	if err == nil {
		t.Fatal("VerifyBlock must reject a block with an unbalanced fee commitment")
	}
	t.Logf("correctly rejected (%v)", err)
}

// TestVerifyBlock_TamperedInputCommitment_Rejected confirms that replacing an
// input's AmountCommit with a random value causes the commitment-balance check
// to fail (#411).
func TestVerifyBlock_TamperedInputCommitment_Rejected(t *testing.T) {
	tx := makeValidTx(t)

	// Replace input commitment with an unrelated value.
	wrongBlind, _ := crypto.NewBlindFactor()
	tx.Inputs[0].AmountCommit, _ = crypto.Commit(1, wrongBlind)

	block := makeTestBlock(t, 3, []core.Transaction{makeCoinbaseTx(), tx})

	err := core.NewTxVerifier(core.NewUTXOSet()).VerifyBlock(block)
	if err == nil {
		t.Fatal("VerifyBlock must reject a block with a tampered input commitment")
	}
	t.Logf("correctly rejected (%v)", err)
}

// TestUTXOSet_DoubleSpend_BlockedAfterRestartSimulation verifies that key
// images marked as spent are still blocked after a node restart is simulated
// by creating a fresh UTXOSet and replaying the persisted spent set (#412).
//
// This mirrors what restoreChain() does: call utxos.MarkSpent(ki) for every
// key image loaded from db.IterKeyImages().
func TestUTXOSet_DoubleSpend_BlockedAfterRestartSimulation(t *testing.T) {
	tx := makeValidTx(t)
	if len(tx.Inputs) == 0 {
		t.Skip("no inputs in test transaction")
	}
	ki := tx.Inputs[0].KeyImage

	// ── Session 1: spend the key image ───────────────────────────────────────
	utxos1 := core.NewUTXOSet()
	utxos1.MarkSpent(ki)
	if !utxos1.IsSpent(ki) {
		t.Fatal("session 1: key image must be spent after MarkSpent")
	}

	// ── Session 2: simulate restart — replay db.IterKeyImages ────────────────
	utxos2 := core.NewUTXOSet()
	utxos2.MarkSpent(ki) // restoreChain replays every persisted key image

	if !utxos2.IsSpent(ki) {
		t.Fatal("after restart simulation: key image must still be spent")
	}

	// The verifier backed by the restored set must reject the transaction.
	err := core.NewTxVerifier(utxos2).VerifyTx(&tx)
	if err == nil {
		t.Fatal("VerifyTx must reject a double-spend after UTXO set restoration")
	}
	if !strings.Contains(err.Error(), "double-spend") {
		t.Fatalf("expected double-spend error, got: %v", err)
	}
	t.Logf("double-spend correctly rejected after restart simulation: %v", err)
}

// TestUTXOSet_FreshSet_DoesNotBlockUnspentKeyImage confirms the inverse: a
// fresh UTXOSet that has not yet restored key images from disk does NOT mark
// the key image as spent — demonstrating why restoreChain must run before the
// first block is processed (#412).
func TestUTXOSet_FreshSet_DoesNotBlockUnspentKeyImage(t *testing.T) {
	tx := makeValidTx(t)
	if len(tx.Inputs) == 0 {
		t.Skip("no inputs in test transaction")
	}
	ki := tx.Inputs[0].KeyImage

	fresh := core.NewUTXOSet()
	if fresh.IsSpent(ki) {
		t.Fatal("fresh UTXO set must not report any key image as spent")
	}

	// The double-spend guard must NOT trigger on a never-seen key image.
	err := core.NewTxVerifier(fresh).VerifyTx(&tx)
	if err != nil && strings.Contains(err.Error(), "double-spend") {
		t.Fatalf("fresh set incorrectly blocked unspent key image: %v", err)
	}
	t.Logf("fresh set correctly allows unspent key image (got: %v)", err)
}
