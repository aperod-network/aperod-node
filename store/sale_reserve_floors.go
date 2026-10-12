package store

import (
	"fmt"

	"github.com/aperod/aperod/sale"
)

// VerifySaleReserveFloors checks disjoint reservations at the activation
// parent. Signatures cannot waive these protocol floors. Do not apply the
// historic-issued comparison to an already activated ledger's later tip:
// its checkpoint binds the original activation parent, not today's rewards.
func VerifySaleReserveFloors(c *LPoDCheckpoint, r sale.Reconciliation) error {
	if c == nil || c.Allocation == nil || c.Allocation.Version != 3 ||
		c.Allocation.NominalSnapshot == nil {
		return fmt.Errorf("sale: canonical v3 reserve accounting unavailable")
	}
	if r.NetAllocation != nil {
		if r.AllocationEpoch != nil {
			return fmt.Errorf("sale: mixed allocation bases")
		}
		return verifyNetSaleReserveFloors(c, r)
	}
	if r.AllocationEpoch != nil {
		return verifyEpochSaleReserveFloors(c, r)
	}
	guardian := sale.MinimumGuardianReserveNAPRO
	if existing := c.Allocation.NominalSnapshot.PreexistingGuardianReservation; existing > guardian {
		guardian = existing
	}
	// V3's entire eligible LPoD capital, including its unused portion, is
	// already included in NominalCirculatingBefore. Do not count Remaining
	// as a second disjoint unissued reserve; protect it with the issuance
	// floor and reject a declaration that cannot contain the eligible capital.
	if c.Allocation.NominalSnapshot.NominalCirculatingBefore < c.Allocation.NominalEligible {
		return fmt.Errorf("sale: nominal declaration omits earmarked LPoD capital")
	}
	if r.ValidatorReserved < c.Allocation.ValidatorRemaining || r.GuardianReserved < guardian {
		return fmt.Errorf("sale: canonical validator, LPoD or Guardian reserve omitted")
	}
	if c.GenesisVesting != nil && r.OtherReserved < c.GenesisVesting.Remaining {
		return fmt.Errorf("sale: canonical funded genesis reserve omitted")
	}
	issued, err := SaleKnownIssuedFloor(c)
	if err != nil {
		return err
	}
	if r.HistoricIssued < issued {
		return fmt.Errorf("sale: previously delivered issuance omitted")
	}
	_, _, err = r.Unassigned()
	return err
}
