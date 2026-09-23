// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package lpod

import "testing"

func TestExistingArrearsArePaidBeforeBeingRemoved(t *testing.T) {
	before := State{FundingDebit: InitialNAPRO, Balance: 0, AngelPaid: InitialNAPRO,
		DeficitOutflow: InitialNAPRO, AccruedLiability: InitialNAPRO + 100*Unit, UnfundedLiability: 100 * Unit}
	zero := uint64(0)
	v := Vault{ID: "vault", TotalStake: 100_000 * Unit, ActualIncome: 3 * Unit, ExactAccrual: &zero, Arrears: 100 * Unit}
	next, _, pay, err := Settle(before, nil, 1, []Vault{v})
	if err != nil {
		t.Fatal(err)
	}
	if pay[0].Angels != 276_000_000 || next.AccruedLiability != before.AccruedLiability ||
		next.UnfundedLiability != 100*Unit-276_000_000 {
		t.Fatal("old debt was forgotten, double-accrued or not actually paid")
	}
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}
}
