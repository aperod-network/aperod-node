package core_test

import (
	"testing"

	"github.com/aperod/aperod/core"
)

func TestRingCTV4ActivationBoundary(t *testing.T) {
	const activation = uint64(100)
	v4 := core.Transaction{
		Version: core.TxVersionCommitmentBinding,
		Inputs:  []core.RingInput{{}},
	}
	legacy := core.Transaction{
		Version: core.TxVersionBase,
		Inputs:  []core.RingInput{{}},
	}

	if err := core.ValidateTxVersionAtHeight(&v4, activation-1, activation); err == nil {
		t.Fatal("v4 accepted before activation height")
	}
	if err := core.ValidateTxVersionAtHeight(&v4, activation, activation); err != nil {
		t.Fatalf("v4 rejected at activation height: %v", err)
	}
	if err := core.ValidateTxVersionAtHeight(&v4, 1, 0); err != nil {
		t.Fatalf("v4 rejected when immediate activation was configured: %v", err)
	}
	if err := core.ValidateTxVersionAtHeight(&legacy, activation-1, activation); err != nil {
		t.Fatalf("legacy transaction rejected before activation: %v", err)
	}
	if err := core.ValidateTxVersionAtHeight(&legacy, activation+1, activation); err == nil {
		t.Fatal("legacy RingCT transaction accepted after commitment-binding activation")
	}

	coinbase := core.Transaction{Version: core.TxVersionBase}
	if err := core.ValidateTxVersionAtHeight(&coinbase, activation+1, activation); err != nil {
		t.Fatalf("coinbase rejected after activation: %v", err)
	}

	stake := core.Transaction{Version: core.TxVersionStake}
	if err := core.ValidateTxVersionAtHeight(&stake, activation+1, activation); err != nil {
		t.Fatalf("stake transaction rejected after activation: %v", err)
	}
}

func TestRingCTAmountBindingActivationBoundary(t *testing.T) {
	const activation = uint64(200)
	legacy := &core.Transaction{Version: core.TxVersionCLSAG, Inputs: []core.RingInput{{}}}
	bound := &core.Transaction{Version: core.TxVersionAmountBinding, Inputs: []core.RingInput{{}}}
	avm := &core.Transaction{Version: core.TxVersionAVM, Inputs: []core.RingInput{{}}}
	boundAVM := &core.Transaction{Version: core.TxVersionAVMAmountBinding, Inputs: []core.RingInput{{}}}

	args := func(tx *core.Transaction, height uint64) error {
		return core.ValidateTxVersionAtHeight(tx, height, 1, 1, 100, activation)
	}
	if err := args(legacy, activation-1); err != nil {
		t.Fatalf("legacy CLSAG rejected before amount-binding activation: %v", err)
	}
	if err := args(bound, activation-1); err == nil {
		t.Fatal("amount-binding version accepted before activation")
	}
	if err := args(boundAVM, activation-1); err == nil {
		t.Fatal("amount-binding AVM version accepted before activation")
	}
	if err := args(legacy, activation); err == nil {
		t.Fatal("legacy CLSAG accepted at amount-binding activation")
	}
	if err := args(avm, activation); err == nil {
		t.Fatal("legacy AVM accepted at amount-binding activation")
	}
	if err := args(bound, activation); err != nil {
		t.Fatalf("amount-binding version rejected at activation: %v", err)
	}
	if err := args(boundAVM, activation); err != nil {
		t.Fatalf("amount-binding AVM version rejected at activation: %v", err)
	}
	if err := core.ValidateTxVersionAtHeight(bound, activation, 1, 0, 100, activation); err == nil {
		t.Fatal("amount-binding version accepted without an active CLSAG height")
	}
}

func TestStakeTransactionCannotCreateOutputs(t *testing.T) {
	extra := make([]byte, core.StakePayloadSize)
	extra[0] = byte(core.StakeWithdraw)

	withOutput := core.Transaction{
		Version: core.TxVersionStake,
		Extra:   extra,
		Outputs: []core.Output{{}},
	}
	if err := withOutput.Validate(); err == nil {
		t.Fatal("stake transaction with an output was accepted")
	}

	withoutOutput := core.Transaction{
		Version: core.TxVersionStake,
		Extra:   extra,
	}
	if err := core.NewTxVerifier(core.NewUTXOSet()).VerifyTx(&withoutOutput); err != nil {
		t.Fatalf("structurally valid zero-output stake transaction was rejected: %v", err)
	}
}

func TestMinFeeAtSaturatesOnOverflow(t *testing.T) {
	tx := core.Transaction{
		Inputs:  []core.RingInput{{}},
		Outputs: []core.Output{{}},
	}
	if got := tx.MinFeeAt(^uint64(0)); got != ^uint64(0) {
		t.Fatalf("overflowing minimum fee wrapped to %d", got)
	}
}
