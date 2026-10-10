// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestLPoDWalletProjectionPartialExitAndIsolation(t *testing.T) {
	owner := crypto.Address("owner")
	c := &store.LPoDCheckpoint{Positions: map[string]store.LPoDPosition{
		"b":     {Deposit: core.LPoDPositionAction{Beneficiary: owner, Amount: 100}, Withdrawn: 30, Due: 8},
		"a":     {Deposit: core.LPoDPositionAction{Beneficiary: owner, Amount: 50}, Withdrawn: 50, Returned: true},
		"other": {Deposit: core.LPoDPositionAction{Beneficiary: "other", Amount: 999}},
	}}
	rows, total, err := lpodWalletProjection(c, owner)
	if err != nil || total != 70 || len(rows) != 2 || rows[0].ID != "a" || rows[1].Principal != "70" || rows[1].Due != "8" {
		t.Fatalf("projection: %+v %d %v", rows, total, err)
	}
	p := c.Positions["b"]
	p.Withdrawn = 101
	c.Positions["b"] = p
	if _, _, err := lpodWalletProjection(c, owner); err == nil {
		t.Fatal("invalid principal accepted")
	}
}
