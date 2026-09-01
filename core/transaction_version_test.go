package core_test

import (
	"testing"

	"github.com/aperod/aperod/core"
)

func TestRingCTV4ActivationBoundary(t *testing.T) {
	const activation = uint64(100)
	v4 := core.Transaction{Version: core.TxVersionCommitmentBinding}
	legacy := core.Transaction{Version: core.TxVersionBase}

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
	if err := core.ValidateTxVersionAtHeight(&legacy, activation+1, activation); err != nil {
		t.Fatalf("legacy historical format rejected after activation: %v", err)
	}
}