// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"fmt"
	"time"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
)

// This is a consensus schedule, not administrator-editable display metadata.
// The reserve is reclassified out of the non-circulating genesis remainder
// bound by the ALREADY attested nominal snapshot. The Guardian's eligible
// circulating budget is distinct and MUST NOT be debited or reused.
const GenesisVestingRecipient crypto.Address = "aproegAWbHEE8wunGatJwGLM68f3hskmbQmT8HJik9s827DymS2jScRb2hxqAgD34qfXwfQKKDCoj2okTWeVwsi2CXtA9Ybr76"
const GenesisVestingReserve uint64 = 849_010_000 * lpod.Unit
const GenesisVestingBasis = "attested-genesis-team-allocation-reclassification/v1"
const GenesisNominalTotal uint64 = 10_000_000_000 * lpod.Unit

// GenesisVestingState is part of the signed accounting checkpoint. Canonical
// tip selection therefore rolls back both payments and their reserve debit.
// Heights, rather than local job flags, provide restart/downtime idempotency.
type GenesisVestingState struct {
	Basis                  string                 `json:"basis"`
	Genesis                crypto.Hash32          `json:"genesis"`
	FundingHeight          uint64                 `json:"funding_height"`
	FundingParent          crypto.Hash32          `json:"funding_parent"`
	Recipient              crypto.Address         `json:"recipient"`
	FundingDebit           uint64                 `json:"funding_debit_napro,string"`
	SourceSnapshot         crypto.Hash32          `json:"source_snapshot"`
	FundingProof           *GenesisVestingFunding `json:"funding_proof"`
	OriginalNonCirculating uint64                 `json:"original_nominal_noncirculating_napro,string"`
	OtherNonCirculating    uint64                 `json:"other_nominal_noncirculating_napro,string"`
	Remaining              uint64                 `json:"remaining_napro,string"`
	Paid                   uint64                 `json:"paid_napro,string"`
	PaidHeights            [10]uint64             `json:"paid_heights"`
	LiquidityRecipient     crypto.Address         `json:"liquidity_recipient,omitempty"`
	LiquidityPaid          uint64                 `json:"liquidity_paid_napro,string,omitempty"`
}

func GenesisVestingTranche(index int) (int64, uint64, error) {
	if index < 0 || index >= 10 {
		return 0, 0, fmt.Errorf("genesis vesting: invalid tranche")
	}
	amount := uint64(84_900_000 * lpod.Unit)
	if index == 9 {
		amount = 84_910_000 * lpod.Unit
	}
	return time.Date(2027+index, time.February, 10, 0, 0, 0, 0, time.UTC).UnixNano(), amount, nil
}

func (v *GenesisVestingState) Validate(genesis crypto.Hash32, height uint64) error {
	if v == nil {
		return nil
	}
	if v.Basis != GenesisVestingBasis || v.Genesis != genesis || genesis == (crypto.Hash32{}) ||
		v.FundingParent == (crypto.Hash32{}) || v.FundingHeight == 0 || v.FundingHeight > height ||
		v.Recipient != GenesisVestingRecipient || v.FundingDebit != GenesisVestingReserve ||
		v.SourceSnapshot == (crypto.Hash32{}) || v.OriginalNonCirculating < GenesisVestingReserve ||
		v.OriginalNonCirculating > GenesisNominalTotal || v.OtherNonCirculating != v.OriginalNonCirculating-GenesisVestingReserve {
		return fmt.Errorf("genesis vesting: invalid funded allocation")
	}
	var paid uint64
	var previous uint64
	gap := false
	for i, h := range v.PaidHeights {
		if h == 0 {
			gap = true
			continue
		}
		if gap || h < v.FundingHeight || h > height || h < previous {
			return fmt.Errorf("genesis vesting: invalid payment sequence")
		}
		_, amount, _ := GenesisVestingTranche(i)
		paid += amount // ten fixed tranches cannot overflow uint64
		previous = h
	}
	if paid != v.Paid || paid > GenesisVestingReserve || v.Remaining != GenesisVestingReserve-paid {
		return fmt.Errorf("genesis vesting: non-conserved reserve")
	}
	if v.FundingProof != nil && v.FundingProof.Version == 2 {
		if v.LiquidityRecipient != v.FundingProof.LiquidityRecipient || v.LiquidityPaid != GenesisLiquidityAmount {
			return fmt.Errorf("genesis vesting: liquidity release differs from exclusive funding intent")
		}
	} else if v.LiquidityPaid != 0 || v.LiquidityRecipient != "" {
		return fmt.Errorf("genesis vesting: undeclared liquidity release")
	}
	return nil
}

func nominalNonCirculatingBound(a *LPoDAllocation) (uint64, error) {
	if a == nil || a.Version != 3 || a.NominalSnapshot == nil ||
		!a.NominalSnapshot.valid(a.NominalEligible, a.InitialValidatorRemaining, a.SnapshotRoot) {
		return 0, fmt.Errorf("genesis vesting: authenticated v3 nominal snapshot required")
	}
	circulating := a.NominalSnapshot.NominalCirculatingBefore
	if circulating > GenesisNominalTotal || a.InitialValidatorRemaining > GenesisNominalTotal-circulating {
		return 0, fmt.Errorf("genesis vesting: original 10B allocation exceeded")
	}
	// Guardian's 1B was a reclassification INSIDE circulating, not an
	// additional allocation. Validator remaining is outside that circulation.
	return GenesisNominalTotal - circulating - a.InitialValidatorRemaining, nil
}

func (v *GenesisVestingState) validateSource(a *LPoDAllocation, height uint64) error {
	if v == nil {
		return nil
	}
	if a == nil {
		return fmt.Errorf("genesis vesting: missing allocation")
	}
	if err := v.Validate(a.Genesis, height); err != nil {
		return err
	}
	if err := v.FundingProof.validate(a, v.FundingHeight); err != nil {
		return err
	}
	bound, err := nominalNonCirculatingBound(a)
	if err != nil {
		return err
	}
	if v.OriginalNonCirculating != bound || v.SourceSnapshot != a.SnapshotRoot ||
		v.FundingHeight <= a.FundingHeight {
		return fmt.Errorf("genesis vesting: source differs from signed genesis accounting")
	}
	return nil
}

// previewGenesisVesting never changes a loaded checkpoint. Funding and
// payments only become effective through the existing atomic canonical batch.
func previewGenesisVesting(before, next *LPoDCheckpoint, r *LPoDSettlement, height uint64) error {
	if before.GenesisVesting == nil && (r.GenesisVestingActivationHeight == 0 || height < r.GenesisVestingActivationHeight) {
		return nil
	}
	if before.Allocation == nil || before.Allocation.Version != 3 || !r.PositionProtocol {
		return fmt.Errorf("genesis vesting: funded v3 canonical nominal allocation required")
	}
	if before.GenesisVesting == nil {
		if err := r.GenesisVestingFunding.verifyQuorum(before.Allocation, height, r.Stake); err != nil {
			return err
		}
		bound, err := nominalNonCirculatingBound(before.Allocation)
		if err != nil {
			return err
		}
		if height != r.GenesisVestingActivationHeight || height <= before.Allocation.FundingHeight ||
			bound < GenesisVestingReserve || r.Parent == (crypto.Hash32{}) {
			return fmt.Errorf("genesis vesting: invalid activation or insufficient unallocated genesis budget")
		}
		if _, _, _, err := crypto.DecodeAddress(GenesisVestingRecipient); err != nil {
			return fmt.Errorf("genesis vesting: invalid recipient: %w", err)
		}
		next.GenesisVesting = &GenesisVestingState{
			Basis: GenesisVestingBasis, Genesis: before.Allocation.Genesis,
			FundingHeight: height, FundingParent: r.Parent, Recipient: GenesisVestingRecipient,
			FundingDebit: GenesisVestingReserve, Remaining: GenesisVestingReserve,
			SourceSnapshot: before.Allocation.SnapshotRoot, OriginalNonCirculating: bound,
			FundingProof:        r.GenesisVestingFunding,
			OtherNonCirculating: bound - GenesisVestingReserve,
		}
		if r.GenesisVestingFunding.Version == 2 {
			next.GenesisVesting.LiquidityRecipient = r.GenesisVestingFunding.LiquidityRecipient
			next.GenesisVesting.LiquidityPaid = GenesisLiquidityAmount
		}
	} else {
		if r.GenesisVestingActivationHeight != before.GenesisVesting.FundingHeight {
			return fmt.Errorf("genesis vesting: activation cannot be disabled or changed after funding")
		}
		if err := before.GenesisVesting.validateSource(before.Allocation, before.State.LastHeight); err != nil {
			return err
		}
		copyState := *before.GenesisVesting
		next.GenesisVesting = &copyState
	}
	v := next.GenesisVesting
	for i := range v.PaidHeights {
		deadline, amount, _ := GenesisVestingTranche(i)
		if v.PaidHeights[i] != 0 || r.Timestamp < deadline {
			continue
		}
		if v.Remaining < amount {
			return fmt.Errorf("genesis vesting: reserve exhausted")
		}
		v.PaidHeights[i] = height
		v.Paid += amount
		v.Remaining -= amount
	}
	return v.validateSource(next.Allocation, height)
}

func (v *GenesisVestingState) paymentAt(height uint64) uint64 {
	if v == nil || height == 0 {
		return 0
	}
	var amount uint64
	for i, h := range v.PaidHeights {
		if h == height {
			_, n, _ := GenesisVestingTranche(i)
			amount += n
		}
	}
	return amount
}
