package store

import (
	"testing"

	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/sale"
)

func TestSaleRewardWalletFloorProtectsOutsideValueWithoutDoubleCounting(t *testing.T) {
	c := &LPoDCheckpoint{
		Allocation: &LPoDAllocation{InitialValidatorRemaining: 2000 * lpod.Unit,
			ValidatorRemaining: 1980 * lpod.Unit},
		State: lpod.State{RewardInflow: 10 * lpod.Unit,
			LeaderPaid: lpod.Unit, AngelPaid: 2 * lpod.Unit},
	}
	for _, test := range []struct{ burn, principal, want uint64 }{
		{0, 0, 13 * lpod.Unit},
		{2 * lpod.Unit, 0, 11 * lpod.Unit},
		{0, 3 * lpod.Unit, 10 * lpod.Unit},
		{2 * lpod.Unit, 3 * lpod.Unit, 8 * lpod.Unit},
		{13 * lpod.Unit, 0, 0},
		{sale.CapNAPRO, sale.CapNAPRO, 0},
	} {
		c.PrincipalLocked = test.principal
		got, err := saleRewardWalletFloor(c, test.burn)
		if err != nil || got != test.want {
			t.Fatalf("burn=%d locked=%d got=%d want=%d err=%v",
				test.burn, test.principal, got, test.want, err)
		}
	}
}

func TestSaleRewardWalletFloorRejectsInconsistentEpochCounters(t *testing.T) {
	for _, kind := range []string{"reserve", "inflow", "payout"} {
		t.Run(kind, func(t *testing.T) {
			c := &LPoDCheckpoint{Allocation: &LPoDAllocation{InitialValidatorRemaining: 100},
				State: lpod.State{RewardInflow: 10}}
			switch kind {
			case "reserve":
				c.Allocation.ValidatorRemaining = 101
			case "inflow":
				c.State.RewardInflow = 101
			case "payout":
				c.State.LeaderPaid = sale.CapNAPRO
			}
			if _, err := saleRewardWalletFloor(c, 0); err == nil {
				t.Fatal("invalid reward accounting accepted")
			}
		})
	}
}
