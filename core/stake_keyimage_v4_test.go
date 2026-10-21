package core_test

import (
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func makeStakeKeyImageV4TestTx(t *testing.T, utxos *core.UTXOSet) (core.Transaction, crypto.KeyImage) {
	t.Helper()
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	owner, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}

	var burnTxHash crypto.Hash32
	burnTxHash[0] = 0xA4
	amount := core.MinStakeNAPR
	blind := crypto.BlindFactor(crypto.ScalarFromUint64(17))
	commitment, err := crypto.Commit(amount, blind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	utxos.Add(&core.UTXO{
		TxHash:       burnTxHash,
		OutputIndex:  0,
		OneTimePub:   owner.Spend.Public,
		AmountCommit: commitment,
	})

	context := core.StakeKeyImageProofContext(core.StakeDeposit, validatorPub, amount, burnTxHash, 0, blind)
	keyImage, proof, err := crypto.ProveKeyImage(owner.Spend.Private, owner.Spend.Public, context)
	if err != nil {
		t.Fatalf("ProveKeyImage: %v", err)
	}
	msg := core.StakeSignMsgV2(core.StakeDeposit, validatorPub, amount, burnTxHash, 0)
	signature, err := validatorPriv.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	extra, err := core.EncodeStakeExtraV4(
		core.StakeDeposit, validatorPub, amount, signature, burnTxHash, 0, blind, keyImage, proof,
	)
	if err != nil {
		t.Fatalf("EncodeStakeExtraV4: %v", err)
	}
	return core.Transaction{Version: core.TxVersionStake, Extra: extra}, keyImage
}

func TestStakeKeyImageV4AcceptsUnspentAndRejectsSpentCollateral(t *testing.T) {
	utxos := core.NewUTXOSet()
	tx, keyImage := makeStakeKeyImageV4TestTx(t, utxos)
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	if err := registry.ConfigureStakeKeyImageV4(10); err != nil {
		t.Fatalf("ConfigureStakeKeyImageV4: %v", err)
	}

	if err := registry.ValidateBlockStakeTxs([]core.Transaction{tx}, 10); err != nil {
		t.Fatalf("valid unspent collateral rejected: %v", err)
	}
	utxos.MarkSpent(keyImage)
	if err := registry.ValidateBlockStakeTxs([]core.Transaction{tx}, 10); err == nil {
		t.Fatal("already-spent collateral was accepted for stake")
	}
}

func TestStakeKeyImageV4RejectsPreActivationAndSameBlockSpend(t *testing.T) {
	utxos := core.NewUTXOSet()
	stakeTx, keyImage := makeStakeKeyImageV4TestTx(t, utxos)
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	if err := registry.ConfigureStakeKeyImageV4(10); err != nil {
		t.Fatalf("ConfigureStakeKeyImageV4: %v", err)
	}
	if err := registry.ValidateBlockStakeTxs([]core.Transaction{stakeTx}, 9); err == nil {
		t.Fatal("v4 stake was accepted before activation")
	}

	spendTx := core.Transaction{
		Version: core.TxVersionCLSAG,
		Inputs:  []core.RingInput{{KeyImage: keyImage}},
	}
	if err := registry.ValidateBlockStakeTxs([]core.Transaction{stakeTx, spendTx}, 10); err == nil {
		t.Fatal("block accepted both a stake and a normal spend with the same key image")
	}
}

func TestStakeKeyImageV4UTXOApplyAndRollback(t *testing.T) {
	utxos := core.NewUTXOSet()
	stakeTx, keyImage := makeStakeKeyImageV4TestTx(t, utxos)
	block := &core.Block{Header: core.BlockHeader{Height: 10}, Txs: []core.Transaction{stakeTx}}
	if err := utxos.ApplyBlock(block); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	if !utxos.IsSpent(keyImage) {
		t.Fatal("applied stake did not reserve the collateral key image")
	}
	if err := utxos.RollbackBlock(block); err != nil {
		t.Fatalf("RollbackBlock: %v", err)
	}
	if utxos.IsSpent(keyImage) {
		t.Fatal("rollback left the stake key image marked spent")
	}
}
