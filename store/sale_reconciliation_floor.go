package store

import (
	"fmt"

	"github.com/aperod/aperod/sale"
)

// SaleKnownIssuedFloor includes rewards canonically recorded after the nominal
// activation declaration. Their debit reduces ValidatorRemaining; omitting the
// matching issuance would make already-paid rewards available for sale again.
// This is a conservative minimum, not complete historical issuance proof.
func SaleKnownIssuedFloor(c *LPoDCheckpoint) (uint64, error) {
	if c == nil || c.Allocation == nil || c.Allocation.NominalSnapshot == nil {
		return 0, fmt.Errorf("sale: canonical nominal issuance declaration missing")
	}
	before := c.Allocation.NominalSnapshot.NominalCirculatingBefore
	rewards := c.State.RewardInflow
	if before > sale.CapNAPRO || rewards > sale.CapNAPRO-before {
		return 0, fmt.Errorf("sale: known issuance exceeds the 10B cap")
	}
	issued := before + rewards
	// Genesis vesting spends a separately reclassified NON-circulating
	// reserve. Its paid portion must move into the issuance floor as its
	// Remaining falls, or those credited coins become sale capacity again.
	// LiquidityPaid is deliberately not added: that funding declaration
	// reclassifies already nominally circulating liquidity, not new issuance.
	if c.GenesisVesting != nil {
		if c.GenesisVesting.Paid > sale.CapNAPRO-issued {
			return 0, fmt.Errorf("sale: genesis delivery exceeds the 10B cap")
		}
		issued += c.GenesisVesting.Paid
	}
	return issued, nil
}
