package core_test

// staking_test.go — unit tests for partial stake withdrawal validation (#355).
//
// Tests:
//  1. Partial withdraw that would leave stake below minimum → error.
//  2. Valid partial withdraw reduces StakeNAPR and enqueues an UnbondingEntry.
//  3. UpdateEpoch drops unbonding entries whose EndBlock has passed.

import (
	"encoding/binary"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// makeValidatorKey generates a deterministic validator key pair from seed.
func makeValidatorKey(t *testing.T) (crypto.ValidatorPrivKey, crypto.ValidatorPubKey) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	return priv, pub
}

// buildPartialWithdrawTx constructs a v1 stake tx with StakePartialWithdraw action.
func buildPartialWithdrawTx(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey, amount uint64) core.Transaction {
	t.Helper()
	msg := core.StakeSignMsg(core.StakePartialWithdraw, pub, amount)
	sig, err := priv.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	extra, err := core.EncodeStakeExtra(core.StakePartialWithdraw, pub, amount, sig)
	if err != nil {
		t.Fatalf("EncodeStakeExtra: %v", err)
	}
	return core.Transaction{
		Version: core.TxVersionStake,
		Extra:   extra,
	}
}

// TestStaking_PartialWithdraw_BelowMinimum (#355) — withdrawing an amount that
// would leave the validator's stake below MinStakeNAPR must return an error.
func TestStaking_PartialWithdraw_BelowMinimum(t *testing.T) {
	priv, pub := makeValidatorKey(t)

	reg := core.NewValidatorRegistry()
	// Seed with exactly MinStakeNAPR so any partial withdrawal drops below min.
	reg.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR)

	// Attempt to withdraw 1 nAPRO — remaining would be MinStakeNAPR-1 < min.
	tx := buildPartialWithdrawTx(t, priv, pub, 1)
	err := reg.ProcessStakeTx(tx, 100)
	if err == nil {
		t.Fatal("ProcessStakeTx should reject partial withdraw leaving stake below minimum, got nil")
	}
}

// TestStaking_PartialWithdraw_Valid (#355) — a valid partial withdraw should
// reduce StakeNAPR by the requested amount and enqueue an UnbondingEntry.
func TestStaking_PartialWithdraw_Valid(t *testing.T) {
	priv, pub := makeValidatorKey(t)

	const genesisStake = core.MinStakeNAPR + 5_000_000_000_000 // min + 50k APRO excess

	reg := core.NewValidatorRegistry()
	reg.InitFromGenesis([]crypto.ValidatorPubKey{pub}, genesisStake)

	const withdrawAmount uint64 = 1_000_000_000_000 // 10k APRO — stays above min

	tx := buildPartialWithdrawTx(t, priv, pub, withdrawAmount)
	const blockHeight uint64 = 500
	if err := reg.ProcessStakeTx(tx, blockHeight); err != nil {
		t.Fatalf("ProcessStakeTx: unexpected error: %v", err)
	}

	entry, ok := reg.GetEntry(pub)
	if !ok {
		t.Fatal("validator entry not found after partial withdraw")
	}

	wantStake := genesisStake - withdrawAmount
	if entry.StakeNAPR != wantStake {
		t.Errorf("StakeNAPR: got %d, want %d", entry.StakeNAPR, wantStake)
	}

	if len(entry.UnbondingQueue) != 1 {
		t.Fatalf("UnbondingQueue length: got %d, want 1", len(entry.UnbondingQueue))
	}
	ub := entry.UnbondingQueue[0]
	if ub.Amount != withdrawAmount {
		t.Errorf("UnbondingEntry.Amount: got %d, want %d", ub.Amount, withdrawAmount)
	}
	wantEndBlock := blockHeight + core.PartialUnbondingBlocks
	if ub.EndBlock != wantEndBlock {
		t.Errorf("UnbondingEntry.EndBlock: got %d, want %d", ub.EndBlock, wantEndBlock)
	}
}

// TestStaking_UpdateEpoch_CleansCompletedUnbonding (#355) — UpdateEpoch must
// drop unbonding entries whose EndBlock has been reached or passed.
func TestStaking_UpdateEpoch_CleansCompletedUnbonding(t *testing.T) {
	priv, pub := makeValidatorKey(t)

	const genesisStake = core.MinStakeNAPR + 5_000_000_000_000

	reg := core.NewValidatorRegistry()
	reg.InitFromGenesis([]crypto.ValidatorPubKey{pub}, genesisStake)

	// Submit a partial withdraw at block 100.
	const withdrawAmount uint64 = 500_000_000_000 // 5k APRO
	tx := buildPartialWithdrawTx(t, priv, pub, withdrawAmount)
	if err := reg.ProcessStakeTx(tx, 100); err != nil {
		t.Fatalf("ProcessStakeTx: %v", err)
	}

	endBlock := uint64(100) + core.PartialUnbondingBlocks

	// UpdateEpoch just before EndBlock — entry must still be present.
	reg.UpdateEpoch(endBlock - 1)
	entryBefore, _ := reg.GetEntry(pub)
	if len(entryBefore.UnbondingQueue) == 0 {
		t.Error("UnbondingQueue should still contain the entry before EndBlock")
	}

	// UpdateEpoch at or after EndBlock — entry must be dropped.
	reg.UpdateEpoch(endBlock)
	entryAfter, _ := reg.GetEntry(pub)
	if len(entryAfter.UnbondingQueue) != 0 {
		t.Errorf("UnbondingQueue should be empty after EndBlock, got %d entries", len(entryAfter.UnbondingQueue))
	}
}

// Suppress "declared but not used" for binary import used in future tests.
var _ = binary.LittleEndian
