// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"fmt"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
)

func positionActionForTotals(vault crypto.Point32, amount uint64) core.LPoDPositionAction {
	return core.LPoDPositionAction{Action: core.LPoDDeposit, Vault: vault, Amount: amount}
}

func TestLPoDEligibleVaultsTop21CombinedStakeAndTieBreak(t *testing.T) {
	stakes := make(map[string]LPoDValidatorStake)
	guardians := make(map[string]uint64)
	for i := 0; i < 24; i++ {
		id := fmt.Sprintf("%064x", i)
		stakes[id] = LPoDValidatorStake{Amount: 100_000 * lpod.Unit, Active: true}
		guardians[id] = uint64(i/2) * lpod.Unit // pairs have equal combined stake
	}
	// These entries must never enter the display/routing top 21.
	stakes[fmt.Sprintf("%064x", 100)] = LPoDValidatorStake{Amount: 99_999 * lpod.Unit, Active: true}
	stakes[fmt.Sprintf("%064x", 101)] = LPoDValidatorStake{Amount: 100_000_000 * lpod.Unit, Active: false}

	ranked := LPoDEligibleVaults(stakes, guardians)
	if len(ranked) != 21 {
		t.Fatalf("ranked %d vaults, want 21", len(ranked))
	}
	for i := 1; i < len(ranked); i++ {
		if ranked[i-1].TotalStake < ranked[i].TotalStake {
			t.Fatal("combined stake ordering is not descending")
		}
		if ranked[i-1].TotalStake == ranked[i].TotalStake && ranked[i-1].ID > ranked[i].ID {
			t.Fatal("equal combined stake did not use public-key tie break")
		}
	}
	if ranked[0].GuardianStake != 11*lpod.Unit || ranked[0].ID != fmt.Sprintf("%064x", 22) {
		t.Fatalf("unexpected deterministic leader: %+v", ranked[0])
	}
}

func TestLPoDEffectiveGuardianTotalsUsesRoutesAndExcludesRefunds(t *testing.T) {
	original := crypto.Point32{1}
	destination := crypto.Point32{2}
	c := &LPoDCheckpoint{Positions: map[string]LPoDPosition{
		"a": {Deposit: positionActionForTotals(original, 5), EffectiveVault: &destination},
		"b": {Deposit: positionActionForTotals(destination, 7), Withdrawn: 2},
		"c": {Deposit: positionActionForTotals(original, 9), Withdrawn: 9, Returned: true, AutoReturned: true},
	}}
	totals, err := c.LPoDEffectiveGuardianTotals()
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 1 || totals[fmt.Sprintf("%x", destination)] != 10 {
		t.Fatalf("effective totals do not conserve routed open principal: %#v", totals)
	}
}

func TestLPoDRouteDestinationTieCapAndSequentialExits(t *testing.T) {
	const source = "ff"
	stakes := map[string]LPoDValidatorStake{
		source: {Amount: 100_000 * lpod.Unit, Active: false},
		"01":   {Amount: 500_000 * lpod.Unit, Active: true},
		"02":   {Amount: 500_000 * lpod.Unit, Active: true},
		"03":   {Amount: 600_000 * lpod.Unit, Active: true},
	}
	totals := map[string]uint64{
		"01": 99_400_000 * lpod.Unit, // exactly 99.9M combined before route
		"02": 99_400_000 * lpod.Unit,
		"03": 99_400_000 * lpod.Unit, // full 100M; cannot fit any principal
	}
	eligible := map[string]bool{"01": true, "02": true, "03": true}
	got, err := lpodRouteDestination(source, 100_000_000*lpod.Unit, 100_000*lpod.Unit, stakes, totals, eligible)
	if err != nil || got != "01" {
		t.Fatalf("equal totals must choose lower public key: got %q, err %v", got, err)
	}
	totals[got] += 100_000 * lpod.Unit

	// A second exiting position cannot overfill the first destination and must
	// deterministically continue with the next fitting candidate.
	got, err = lpodRouteDestination(source, 100_000_000*lpod.Unit, 100_000*lpod.Unit, stakes, totals, eligible)
	if err != nil || got != "02" {
		t.Fatalf("sequential route ignored cap/current totals: got %q, err %v", got, err)
	}
	totals[got] += 100_000 * lpod.Unit
	got, err = lpodRouteDestination(source, 100_000_000*lpod.Unit, 100_000*lpod.Unit, stakes, totals, eligible)
	if err != nil || got != "" {
		t.Fatalf("full candidates must force canonical refund, got %q, err %v", got, err)
	}
}
