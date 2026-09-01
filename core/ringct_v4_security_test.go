package core_test

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestRingCTV4RejectsCommitmentCopyTheft(t *testing.T) {
	attacker, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	victim, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	attackerBlind, _ := crypto.NewBlindFactor()
	victimBlind, _ := crypto.NewBlindFactor()
	attackerCommit, _ := crypto.Commit(10, attackerBlind)
	victimCommit, _ := crypto.Commit(1_000_000, victimBlind)

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
	attackerUTXO := &core.UTXO{
		TxHash:       crypto.HashStr("attacker-utxo"),
		OneTimePub:   attacker.Spend.Public,
		AmountCommit: attackerCommit,
	}
	victimUTXO := &core.UTXO{
		TxHash:       crypto.HashStr("victim-utxo"),
		OneTimePub:   victim.Spend.Public,
		AmountCommit: victimCommit,
	}
	utxos.Add(attackerUTXO)
	utxos.Add(victimUTXO)

	keyImage, err := crypto.ComputeKeyImage(attacker.Spend.Private, attacker.Spend.Public)
	if err != nil {
		t.Fatal(err)
	}
	feeCommit, _ := crypto.Commit(0, crypto.BlindFactor{})
	outputProof, _ := crypto.ProveRange(1_000_000, victimBlind)
	tx := core.Transaction{
		Version: core.TxVersionCommitmentBinding,
		Inputs: []core.RingInput{{
			KeyImage:     keyImage,
			Ring:         ring,
			AmountCommit: victimCommit,
			RealIndex:    0,
		}},
		Outputs: []core.Output{{
			OneTimePub:   crypto.Point32{2},
			AmountCommit: victimCommit,
		}},
		FeeCommit:  feeCommit,
		RangeProofs: []*crypto.RangeProof{outputProof},
		Signatures:  make([]*crypto.MLSAGSignature, 1),
	}
	message := core.RingSignMessage(tx.Hash(), 0)
	sig, err := crypto.MLSAGSignV4(message, ring, 0, attacker.Spend.Private, attackerBlind, 10)
	if err != nil {
		t.Fatal(err)
	}
	tx.Signatures[0] = sig

	err = core.NewTxVerifier(utxos).VerifyTx(&tx)
	if err == nil {
		t.Fatal("commitment-copy theft was accepted")
	}
	if !strings.Contains(err.Error(), "proven real member commitment") {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if utxos.GetByPubKey(victim.Spend.Public) == nil {
		t.Fatal("victim UTXO was removed after rejected commitment-copy theft")
	}
}