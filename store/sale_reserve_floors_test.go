package store

import (
	"testing"

	"github.com/aperod/aperod/sale"
)

func reserveFloorFixture() (*LPoDCheckpoint, sale.Reconciliation) {
	c := &LPoDCheckpoint{Allocation: &LPoDAllocation{
		Version: 3, ValidatorRemaining: 199732069100000000,
		NominalSnapshot: &LPoDNominalSnapshot{NominalCirculatingBefore: 623000000000000000},
	}}
	c.State.RewardInflow = 73704000000000
	r := sale.Reconciliation{HistoricIssued: 623073704000000000,
		ValidatorReserved: c.Allocation.ValidatorRemaining, GuardianReserved: sale.MinimumGuardianReserveNAPRO}
	return c, r
}

func TestSaleCanonicalReserveFloors(t *testing.T) {
	c, r := reserveFloorFixture()
	if err := VerifySaleReserveFloors(c, r); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"validator", "lpod", "guardian", "previous-guardian", "genesis", "paid", "version"} {
		t.Run(kind, func(t *testing.T) {
			c, r := reserveFloorFixture()
			switch kind {
			case "validator":
				r.ValidatorReserved--
			case "lpod":
				c.Allocation.NominalEligible = c.Allocation.NominalSnapshot.NominalCirculatingBefore + 1
			case "guardian":
				r.GuardianReserved--
			case "previous-guardian":
				c.Allocation.NominalSnapshot.PreexistingGuardianReservation = r.GuardianReserved + 1
			case "genesis":
				c.GenesisVesting = &GenesisVestingState{Remaining: 1}
			case "paid":
				c.GenesisVesting = &GenesisVestingState{Paid: 1}
			case "version":
				c.Allocation.Version = 2
			}
			if err := VerifySaleReserveFloors(c, r); err == nil {
				t.Fatal("canonical claim was omitted")
			}
		})
	}
}

func TestGenesisReserveDeliveryNeverCreatesNewSaleCapacity(t *testing.T) {
	c, r := reserveFloorFixture()
	const reserved uint64 = 1000000000000
	c.GenesisVesting = &GenesisVestingState{Remaining: reserved}
	r.OtherReserved = reserved
	before, _, err := r.Unassigned()
	if err != nil {
		t.Fatal(err)
	}
	c.GenesisVesting.Paid = 99
	c.GenesisVesting.Remaining -= 99
	c.GenesisVesting.LiquidityPaid = 30000000000000000
	r.OtherReserved -= 99
	r.HistoricIssued, err = SaleKnownIssuedFloor(c)
	if err != nil || r.HistoricIssued != 623073704000000099 {
		t.Fatalf("floor=%d err=%v", r.HistoricIssued, err)
	}
	after, _, err := r.Unassigned()
	if err != nil || after != before {
		t.Fatalf("sale capacity changed from %d to %d: %v", before, after, err)
	}
	if err := VerifySaleReserveFloors(c, r); err != nil {
		t.Fatal(err)
	}
	c.GenesisVesting.Paid = sale.CapNAPRO
	if _, err := SaleKnownIssuedFloor(c); err == nil {
		t.Fatal("issuance overflow accepted")
	}
}
