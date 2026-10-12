package store

import (
	"github.com/aperod/aperod/sale"
	"testing"
)

func TestSaleKnownIssuedFloorIncludesPostDeclarationRewards(t *testing.T) {
	c := &LPoDCheckpoint{Allocation: &LPoDAllocation{NominalSnapshot: &LPoDNominalSnapshot{
		NominalCirculatingBefore: 623000000000000000,
	}}}
	c.State.RewardInflow = 267930900000000
	floor, err := SaleKnownIssuedFloor(c)
	if err != nil || floor != 623267930900000000 {
		t.Fatalf("floor=%d err=%v", floor, err)
	}
	c.Allocation.NominalSnapshot.NominalCirculatingBefore = sale.CapNAPRO
	if _, err := SaleKnownIssuedFloor(c); err == nil {
		t.Fatal("known issuance overflow accepted")
	}
	if _, err := SaleKnownIssuedFloor(nil); err == nil {
		t.Fatal("missing canonical declaration accepted")
	}
}
