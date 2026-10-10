// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package lpod

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// This fixture is accounting-only, NOT canonical funding evidence.
func fundedFixture() State { return State{FundingDebit: InitialNAPRO, Balance: InitialNAPRO} }

func TestTierBoundaries(t *testing.T) {
	for i, tier := range tiers {
		got, err := TierFor(tier.Threshold)
		if err != nil || got != tier { t.Fatalf("tier %d: %v %v", i, got, err) }
		got, err = TierFor(tier.Threshold-1)
		if i == 0 { if err == nil { t.Fatal("inactive vault accepted") }; continue }
		if err != nil || got != tiers[i-1] { t.Fatalf("below tier %d: %v %v", i, got, err) }
	}
	if _, err := TierFor(100_000_000*Unit+1); err == nil { t.Fatal("capacity exceeded") }
}

func TestConservationSurplusDeficitExhaustion(t *testing.T) {
	s := fundedFixture()
	v := Vault{ID:"a", TotalStake:100_000_000*Unit, GuardianStake:90_000_000*Unit, ActualIncome:20_000_000*Unit, ElapsedSeconds:YearSeconds}
	next, c, p, err := Settle(s, nil, 1, []Vault{v})
	if err != nil { t.Fatal(err) }
	if p[0].Leader != 4_000_000*Unit || p[0].Angels != 9_000_000*Unit || next.SurplusInflow != 7_000_000*Unit { t.Fatalf("%+v %+v", next, p) }
	v.ActualIncome = 0
	v.ElapsedSeconds = YearSeconds*200
	end, _, payments, err := Settle(next, c, 2, []Vault{v})
	if err != nil { t.Fatal(err) }
	if end.Balance != 0 || end.UnfundedLiability == 0 || payments[0].Angels != next.Balance { t.Fatalf("insolvent payout: %+v", end) }
	if err := end.Validate(); err != nil { t.Fatal(err) }
	if s != fundedFixture() { t.Fatal("input mutated") }
}

func TestCarryRestartRollbackAndPartition(t *testing.T) {
	s := fundedFixture()
	v := Vault{ID:"a", TotalStake:100_000*Unit, GuardianStake:101*Unit+1, ActualIncome:1, ElapsedSeconds:1}
	start := s
	var carry map[string]Carry
	for height := uint64(1); height <= 100; height++ {
		var err error
		s, carry, _, err = Settle(s, carry, height, []Vault{v})
		if err != nil { t.Fatal(err) }
		// Simulate exact checkpoint restoration including all fractional carry.
		data, err := json.Marshal(struct{ S State; C map[string]Carry }{s, carry})
		if err != nil { t.Fatal(err) }
		var restored struct{ S State; C map[string]Carry }
		if err := json.Unmarshal(data, &restored); err != nil { t.Fatal(err) }
		s, carry = restored.S, restored.C
	}
	v.ActualIncome, v.ElapsedSeconds = 100, 100
	once, onceCarry, _, err := Settle(start, nil, 1, []Vault{v})
	if err != nil { t.Fatal(err) }
	once.LastHeight = s.LastHeight
	if once != s || !reflect.DeepEqual(carry, onceCarry) { t.Fatalf("partition lost precision: %+v %+v", once, s) }
	// Rolling back to start and replaying gives the identical checkpoint.
	replay, _, _, err := Settle(start, nil, 1, []Vault{v})
	once.LastHeight = 1
	if err != nil || replay != once { t.Fatal("rollback/replay mismatch") }
}

func TestRejectionIsAtomic(t *testing.T) {
	v := Vault{ID:"a", TotalStake:100_000*Unit, GuardianStake:100*Unit, ElapsedSeconds:1}
	s := fundedFixture()
	tests := []struct{ name string; s State; height uint64; v []Vault }{
		{"unfunded", State{}, 1, []Vault{v}},
		{"duplicate-height", s, 0, []Vault{v}},
		{"gap", s, 2, []Vault{v}},
		{"duplicate-vault", s, 1, []Vault{v,v}},
		{"overflow", s, 1, []Vault{{ID:"a", TotalStake:100_000_000*Unit, GuardianStake:100_000_000*Unit, ElapsedSeconds:math.MaxUint64}}},
		{"income-overflow", s, 1, []Vault{{ID:"a", TotalStake:100_000*Unit, ActualIncome:math.MaxUint64}}},
	}
	for _, tc := range tests { t.Run(tc.name, func(t *testing.T) {
		got, c, p, err := Settle(tc.s, nil, tc.height, tc.v)
		if err == nil || got != tc.s || c != nil || p != nil { t.Fatalf("failure not atomic: %v", err) }
	}) }
}