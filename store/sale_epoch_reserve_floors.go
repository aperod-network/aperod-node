package store

import (
	"fmt"

	"github.com/aperod/aperod/sale"
)

// A signed nominal basis preserves canonical funds and existing principal.
// It neither authenticates old global holdings nor reissues reserved funds.
func verifyEpochSaleReserveFloors(c *LPoDCheckpoint, r sale.Reconciliation) error {
	e := r.AllocationEpoch
	if e == nil || r.NetAllocation != nil {
		return fmt.Errorf("sale: explicit, unmixed operator allocation epoch required")
	}
	if err := e.Validate(); err != nil {
		return err
	}
	if c.GenesisVesting != nil || c.Allocation.NominalSnapshot.PreexistingGuardianReservation != 0 {
		return fmt.Errorf("sale: additional canonical obligations require explicit classification")
	}
	if err := c.State.Validate(); err != nil {
		return err
	}
	if c.PrincipalReturned > c.PrincipalDeposited || c.PrincipalLocked != c.PrincipalDeposited-c.PrincipalReturned {
		return fmt.Errorf("sale: existing principal conservation invalid")
	}
	if e.FundingHeight != c.Allocation.FundingHeight || e.FundingHash != c.Allocation.FundingBlock ||
		e.ProtectedLPoDPrincipal != c.PrincipalLocked {
		return fmt.Errorf("sale: operator epoch does not preserve the canonical LPoD funding and principal")
	}
	if r.ValidatorReserved != c.Allocation.ValidatorRemaining || r.GuardianReserved != c.State.Balance ||
		r.LPoDReserved != c.Allocation.Remaining {
		return fmt.Errorf("sale: nominal epoch must preserve exact canonical reserves")
	}
	_, _, err := r.Unassigned()
	return err
}
