package store

import (
	"fmt"

	"github.com/aperod/aperod/sale"
)

func verifyNetSaleReserveFloors(c *LPoDCheckpoint, r sale.Reconciliation) error {
	// This initial V2 form supports the current funded V3 pool only.
	// Independent extra funding cannot be hidden or guessed into this basis.
	review, err := ReviewPostBurnSaleSource(c, r.NetAllocation.Burned, &r.NetAllocation.WalletNet)
	if err != nil {
		return err
	}
	consumed, err := r.NetAllocation.Consumed()
	if err != nil {
		return err
	}
	if r.HistoricIssued != consumed || r.ValidatorReserved != review.ValidatorReserve ||
		r.GuardianReserved != review.LPoDRewardBalance || r.LPoDReserved != review.LPoDPrincipal+review.LPoDUnassigned {
		return fmt.Errorf("sale: post-burn canonical reserves or consumed allocation mismatch")
	}
	if c.State.FundingDebit != sale.MinimumGuardianReserveNAPRO {
		return fmt.Errorf("sale: canonical Guardian initial allocation missing")
	}
	floor, err := saleRewardWalletFloor(c, r.NetAllocation.Burned)
	if err != nil {
		return err
	}
	if r.NetAllocation.WalletNet < floor {
		return fmt.Errorf("sale: known post-LPoD wallet reward issuance omitted")
	}
	// The old declared 1B has already funded this pool; reserve its CURRENT
	// balance, and principal separately, rather than earmarking another 1B.
	_, _, err = r.Unassigned()
	return err
}

// This is a conservative lower bound, not a replacement for the attested
// wallet opening. Give ALL declared burns and locked guardian principal the
// benefit of reducing the known reward outflow: their provenance may include
// pre-anchor funds. Never subtract validator-to-LPoD redistribution twice.
func saleRewardWalletFloor(c *LPoDCheckpoint, burned uint64) (uint64, error) {
	if c == nil || c.Allocation == nil {
		return 0, fmt.Errorf("sale: reward accounting unavailable")
	}
	a := c.Allocation
	if a.InitialValidatorRemaining > sale.CapNAPRO || a.ValidatorRemaining > a.InitialValidatorRemaining {
		return 0, fmt.Errorf("sale: invalid post-LPoD validator draw")
	}
	draw := a.InitialValidatorRemaining - a.ValidatorRemaining
	if c.State.RewardInflow > draw {
		return 0, fmt.Errorf("sale: LPoD reward inflow exceeds validator draw")
	}
	paid := draw - c.State.RewardInflow
	for _, outflow := range []uint64{c.State.LeaderPaid, c.State.AngelPaid} {
		if outflow > sale.CapNAPRO-paid {
			return 0, fmt.Errorf("sale: known reward issuance exceeds cap")
		}
		paid += outflow
	}
	for _, possiblyRemoved := range []uint64{burned, c.PrincipalLocked} {
		if possiblyRemoved >= paid {
			return 0, nil
		}
		paid -= possiblyRemoved
	}
	return paid, nil
}
