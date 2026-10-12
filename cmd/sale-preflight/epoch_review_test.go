package main

import (
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

func epochFixture() *store.LPoDCheckpoint {
	return &store.LPoDCheckpoint{
		Allocation: &store.LPoDAllocation{
			Version: 3, FundingHeight: 100, FundingBlock: crypto.Hash32{1},
			InitialValidatorRemaining: 2000 * lpod.Unit,
			ValidatorRemaining:        1980 * lpod.Unit,
		},
		State: lpod.State{
			FundingDebit: lpod.InitialNAPRO, LastHeight: 110,
			RewardInflow: 10 * lpod.Unit, SurplusInflow: 10 * lpod.Unit,
			Balance:    lpod.InitialNAPRO + 7*lpod.Unit,
			LeaderPaid: lpod.Unit, AngelPaid: 2 * lpod.Unit,
			AccruedLiability: 2 * lpod.Unit, DeficitOutflow: 3 * lpod.Unit,
		},
	}
}

func TestEpochReviewSeparatesInternalFlowsAndWalletPayments(t *testing.T) {
	r, err := epochReview(epochFixture())
	if err != nil {
		t.Fatal(err)
	}
	if r["funding_height"] != uint64(100) ||
		r["validator_draw_napro"] != "2000000000" ||
		r["validator_to_lpod_napro"] != "1000000000" ||
		r["gross_reward_outflow_to_wallets_napro"] != "1300000000" {
		t.Fatalf("internal redistribution counted as wallet issuance: %v", r)
	}
	for _, key := range []string{"initial_wallet_carry_napro", "post_anchor_wallet_burn_napro", "post_anchor_other_issuance_napro"} {
		if r[key] != nil {
			t.Fatalf("unknown %s silently became zero", key)
		}
	}
	if r["activation_authorized"] != false {
		t.Fatal("unsigned review authorized activation")
	}
}

func TestEpochReviewRejectsUnanchoredOrInconsistentCounters(t *testing.T) {
	for _, kind := range []string{"height", "hash", "settled", "remaining", "inflow", "conservation"} {
		t.Run(kind, func(t *testing.T) {
			c := epochFixture()
			switch kind {
			case "height":
				c.Allocation.FundingHeight = 0
			case "hash":
				c.Allocation.FundingBlock = crypto.Hash32{}
			case "settled":
				c.State.LastHeight = 99
			case "remaining":
				c.Allocation.ValidatorRemaining = c.Allocation.InitialValidatorRemaining + 1
			case "inflow":
				c.Allocation.ValidatorRemaining = c.Allocation.InitialValidatorRemaining
			case "conservation":
				c.State.Balance++
			}
			if _, err := epochReview(c); err == nil {
				t.Fatal("invalid epoch accounting accepted")
			}
		})
	}
}
