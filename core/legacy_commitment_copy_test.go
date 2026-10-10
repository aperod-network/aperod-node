package core_test

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// TestLegacyCommitmentCopyTheftIsBlockedAtActivation reproduces the reported
// legacy single-key MLSAG issue and verifies the hard-fork boundary closes it.
//
// The legacy verifier only requires one present ring member to carry the
// claimed commitment.  The attacker signs with a different, absent ring key,
// copies the victim's commitment and blind, and therefore passes VerifyTx.
// The old state transition then removes the present victim UTXO because it
// identifies the spent output by the copied commitment.  The consensus version
// policy must reject this legacy transaction at and after v4 activation.
func TestLegacyCommitmentCopyTheftIsBlockedAtActivation(t *testing.T) {
	const (
		amount    = uint64(1_000_000)
		activation = uint64(100)
	)

	attacker, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	victim, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	victimBlind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	victimCommit, err := crypto.Commit(amount, victimBlind)
	if err != nil {
		t.Fatal(err)
	}

	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = attacker.Spend.Public
	ring[1] = victim.Spend.Public
	for i := 2; i < crypto.RingSize; i++ {
		member, genErr := crypto.GenerateWalletKeys()
		if genErr != nil {
			t.Fatal(genErr)
		}
		ring[i] = member.Spend.Public
	}

	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       crypto.HashStr("victim-utxo"),
		OutputIndex:  0,
		OneTimePub:   victim.Spend.Public,
		AmountCommit: victimCommit,
	})

	keyImage, err := crypto.ComputeKeyImage(attacker.Spend.Private, attacker.Spend.Public)
	if err != nil {
		t.Fatal(err)
	}
	var zeroBlind crypto.BlindFactor
	feeCommit, err := crypto.Commit(0, zeroBlind)
	if err != nil {
		t.Fatal(err)
	}
	outputProof, err := crypto.ProveRange(amount, victimBlind)
	if err != nil {
		t.Fatal(err)
	}

	tx := core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{{
			KeyImage:     keyImage,
			Ring:         ring,
			AmountCommit: victimCommit,
		}},
		Outputs: []core.Output{{
			OneTimePub:   crypto.Point32{3},
			AmountCommit: victimCommit,
		}},
		FeeCommit:   feeCommit,
		RangeProofs: []*crypto.RangeProof{outputProof},
		Signatures:  make([]*crypto.MLSAGSignature, 1),
	}

	message := core.RingSignMessage(tx.Hash(), 0)
	signature, err := crypto.MLSAGSign(message, ring, 0, attacker.Spend.Private)
	if err != nil {
		t.Fatal(err)
	}
	tx.Signatures[0] = signature

	// This confirms the report's cryptographic premise: legacy VerifyTx alone
	// accepts the commitment copied from the victim's ring member.
	if err := core.NewTxVerifier(utxos).VerifyTx(&tx); err != nil {
		t.Fatalf("legacy commitment-copy reproduction did not reach the policy boundary: %v", err)
	}

	// The pre-fork format remains replayable before activation.
	if err := core.ValidateTxVersionAtHeight(&tx, activation-1, activation); err != nil {
		t.Fatalf("legacy transaction rejected before activation: %v", err)
	}

	err = core.ValidateTxVersionAtHeight(&tx, activation, activation)
	if err == nil {
		t.Fatal("legacy commitment-copy transaction accepted at v4 activation")
	}
	if !strings.Contains(err.Error(), "legacy RingCT") {
		t.Fatalf("unexpected activation-policy error: %v", err)
	}

	// Without the policy boundary, the state transition would consume the
	// victim's active UTXO even though the attacker signed with another key.
	if err := utxos.ApplyBlock(&core.Block{Txs: []core.Transaction{tx}}); err != nil {
		t.Fatalf("legacy state-transition reproduction failed: %v", err)
	}
	if utxos.GetByPubKey(victim.Spend.Public) != nil {
		t.Fatal("victim UTXO remained active; reproduction did not exercise copied-commit removal")
	}
}