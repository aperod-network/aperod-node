// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
)

func vestingFixture(t *testing.T) (*LPoDCheckpoint, *LPoDSettlement) {
	t.Helper()
	key, pub, err := crypto.GenerateValidatorKey()
	if err != nil { t.Fatal(err) }
	g := crypto.HashBytes([]byte("vesting-test-genesis"))
	snapshot := &LPoDNominalSnapshot{Basis: LPoDV3NominalBasis, NominalCirculatingBefore: 6_000_000_000*lpod.Unit,
		EligibleNominal: lpod.InitialNAPRO, ValidatorRemaining: 2_000_000_000*lpod.Unit, EligibilityRule: LPoDV3EligibilityRule}
	proof := &GenesisVestingFunding{Version: 1, Genesis: g, Snapshot: snapshot.Root(), Height: 11,
		Recipient: GenesisVestingRecipient, Reserve: GenesisVestingReserve,
		OtherAllocations: GenesisNominalTotal-GenesisVestingReserve, Trust: GenesisVestingTrust}
	signature, err := key.Sign(proof.Message())
	if err != nil { t.Fatal(err) }
	proof.Attestations = []LPoDAttestation{{Validator: pub, Signature: signature}}
	return &LPoDCheckpoint{State: lpod.State{LastHeight: 10},
		Allocation: &LPoDAllocation{Version: 3, Genesis: g, FundingHeight: 2, Remaining: 0,
			NominalEligible: lpod.InitialNAPRO, InitialValidatorRemaining: snapshot.ValidatorRemaining,
			NominalSnapshot: snapshot, SnapshotRoot: snapshot.Root()}},
		&LPoDSettlement{Parent: crypto.HashBytes([]byte("canonical-parent")), PositionProtocol: true,
			GenesisVestingActivationHeight: 11, GenesisVestingFunding: proof,
			Stake: map[string]LPoDValidatorStake{pub.Hex(): {Amount: 100_000*lpod.Unit, Active: true}}}
}

func vestingStep(t *testing.T, before *LPoDCheckpoint, r *LPoDSettlement, height uint64) *LPoDCheckpoint {
	t.Helper()
	next := *before
	a := *before.Allocation
	next.Allocation = &a
	next.State.LastHeight = height
	if err := previewGenesisVesting(before, &next, r, height); err != nil { t.Fatal(err) }
	return &next
}

func TestGenesisVestingExactBoundaryAndConservation(t *testing.T) {
	before, r := vestingFixture(t)
	deadline, amount, _ := GenesisVestingTranche(0)
	r.Timestamp = deadline-1
	funded := vestingStep(t, before, r, 11)
	if funded.GenesisVesting.Paid != 0 || funded.Allocation.Remaining != 0 ||
		funded.GenesisVesting.OtherNonCirculating != 2_000_000_000*lpod.Unit-GenesisVestingReserve ||
		before.GenesisVesting != nil || before.Allocation.Remaining != 0 {
		t.Fatal("funding, no-early-payment or immutable preview failed")
	}
	r.Timestamp = deadline
	paid := vestingStep(t, funded, r, 12)
	if paid.GenesisVesting.Paid != amount || paid.GenesisVesting.paymentAt(12) != amount ||
		paid.GenesisVesting.Remaining+paid.GenesisVesting.Paid != GenesisVestingReserve {
		t.Fatal("exact deadline/amount or conservation failed")
	}
	repeated := vestingStep(t, paid, r, 13)
	if repeated.GenesisVesting.Paid != amount || repeated.GenesisVesting.paymentAt(13) != 0 ||
		repeated.Allocation.Remaining != paid.Allocation.Remaining {
		t.Fatal("duplicate payment or duplicate reserve debit")
	}
	if funded.GenesisVesting.Paid != 0 { t.Fatal("preview mutated the parent") }
}

func TestGenesisVestingDowntimeCatchupAndFinalTranche(t *testing.T) {
	before, r := vestingFixture(t)
	r.Timestamp, _, _ = GenesisVestingTranche(2)
	next := vestingStep(t, before, r, 11)
	if next.GenesisVesting.Paid != 3*84_900_000*lpod.Unit { t.Fatal("missed annual tranches not caught up") }
	r.Timestamp, _, _ = GenesisVestingTranche(9)
	last := vestingStep(t, next, r, 12)
	if last.GenesisVesting.Paid != GenesisVestingReserve || last.GenesisVesting.Remaining != 0 ||
		last.GenesisVesting.paymentAt(12) != 6*84_900_000*lpod.Unit+84_910_000*lpod.Unit {
		t.Fatal("final 84.91M tranche or finite reserve is wrong")
	}
	again := vestingStep(t, last, r, 13)
	if again.GenesisVesting.paymentAt(13) != 0 { t.Fatal("exhausted schedule paid again") }
}

func TestGenesisVestingRejectsUnsafeFundingAndMutation(t *testing.T) {
	for _, name := range []string{"insufficient", "legacy", "late", "parent", "non-position"} {
		t.Run(name, func(t *testing.T) {
			before, r := vestingFixture(t)
			height := uint64(11)
			switch name {
			case "insufficient":
				before.Allocation.NominalSnapshot.NominalCirculatingBefore = 7_500_000_000*lpod.Unit
				before.Allocation.SnapshotRoot = before.Allocation.NominalSnapshot.Root()
			case "legacy": before.Allocation.Version = 2
			case "late": height = 12
			case "parent": r.Parent = crypto.Hash32{}
			case "non-position": r.PositionProtocol = false
			}
			next := *before
			if previewGenesisVesting(before, &next, r, height) == nil { t.Fatal("unsafe funding accepted") }
		})
	}
	before, r := vestingFixture(t)
	funded := vestingStep(t, before, r, 11)
	for _, h := range []uint64{0, 12} {
		r.GenesisVestingActivationHeight = h
		next := *funded
		if previewGenesisVesting(funded, &next, r, 12) == nil { t.Fatal("funding fork changed after commit") }
	}
	corrupt := *funded.GenesisVesting
	corrupt.PaidHeights[1] = 11
	if corrupt.Validate(funded.Allocation.Genesis, 11) == nil { t.Fatal("skipped tranche accepted") }
	corrupt = *funded.GenesisVesting
	corrupt.Remaining--
	if corrupt.Validate(funded.Allocation.Genesis, 11) == nil { t.Fatal("lost reserve unit accepted") }
}

func TestGenesisVestingFundingRequiresPurposeAndCanonicalQuorum(t *testing.T) {
	for _, name := range []string{"missing", "wrong-purpose", "foreign-genesis", "bad-signature", "inactive", "insufficient-quorum"} {
		t.Run(name, func(t *testing.T) {
			before, r := vestingFixture(t)
			key, pub, err := crypto.GenerateValidatorKey()
			if err != nil { t.Fatal(err) }
			p := *r.GenesisVestingFunding
			r.GenesisVestingFunding = &p
			r.Stake = map[string]LPoDValidatorStake{pub.Hex(): {Amount: 100_000*lpod.Unit, Active: true}}
			switch name {
			case "wrong-purpose": p.Trust = "unrestricted-mint"
			case "foreign-genesis": p.Genesis = crypto.HashBytes([]byte("foreign-chain"))
			case "inactive": r.Stake[pub.Hex()] = LPoDValidatorStake{Amount: 100_000*lpod.Unit}
			case "insufficient-quorum":
				_, other, err := crypto.GenerateValidatorKey()
				if err != nil { t.Fatal(err) }
				r.Stake[other.Hex()] = LPoDValidatorStake{Amount: 200_000*lpod.Unit, Active: true}
			}
			signature, err := key.Sign(p.Message())
			if err != nil { t.Fatal(err) }
			p.Attestations = []LPoDAttestation{{Validator: pub, Signature: signature}}
			if name == "bad-signature" { p.Attestations[0].Signature[0] ^= 1 }
			if name == "missing" { r.GenesisVestingFunding = nil }
			next := *before
			if previewGenesisVesting(before, &next, r, 11) == nil { t.Fatal("unauthorized source declaration accepted") }
		})
	}
}