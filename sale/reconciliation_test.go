package sale

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestReconciliationBindsLPoDAndDisjointClaims(t *testing.T) {
	f := setup(t)
	lpod := crypto.HashBytes([]byte("canonical-lpod-checkpoint"))
	r := Reconciliation{Genesis: f.c.Genesis, Parent: f.c.Parent,
		LPoDCommitment: lpod, OtherClaimsRoot: crypto.HashBytes([]byte("claims")),
		HistoricIssued: CapNAPRO / 2, ValidatorReserved: CapNAPRO / 10,
		LPoDReserved: CapNAPRO / 10, GuardianReserved: CapNAPRO / 20}
	available, reserved, err := r.Unassigned()
	if err != nil || available != CapNAPRO/4 {
		t.Fatalf("wrong budget: %d %v", available, err)
	}
	f.c.SnapshotRoot, f.c.OtherReserved, f.c.SaleRemaining = r.Digest(), reserved, available
	f.signCheckpoint(t)
	check := func(r Reconciliation, lpod crypto.Hash32, floor uint64) error {
		return VerifyReconciledCheckpoint(f.c, r, f.c.Genesis, f.c.Parent, lpod, f.c.Height, floor, f.trusted)
	}
	if err := check(r, lpod, r.HistoricIssued); err != nil {
		t.Fatal(err)
	}
	changed := r
	changed.GuardianReserved++
	if check(changed, lpod, r.HistoricIssued) == nil {
		t.Fatal("unsigned reserve alteration accepted")
	}
	if check(r, crypto.HashBytes([]byte("other-lpod")), r.HistoricIssued) == nil {
		t.Fatal("unrelated LPoD commitment accepted")
	}
	if check(r, lpod, r.HistoricIssued+1) == nil {
		t.Fatal("earlier attested issuance silently reduced")
	}
	f.c.SaleRemaining++
	f.signCheckpoint(t)
	if check(r, lpod, r.HistoricIssued) == nil {
		t.Fatal("signed over-allocation accepted")
	}
}

func TestReconciliationRejectsOverflowAndNominalHeadline(t *testing.T) {
	for _, r := range []Reconciliation{
		{HistoricIssued: 6_230_000_000 * 100_000_000, OtherReserved: 6_864_595_139 * 100_000_000},
		{HistoricIssued: 1, GuardianReserved: ^uint64(0)},
		{HistoricIssued: CapNAPRO, ValidatorReserved: 1},
	} {
		if _, _, err := r.Unassigned(); err == nil {
			t.Fatal("overlapping/overflowing claims accepted")
		}
	}
}
