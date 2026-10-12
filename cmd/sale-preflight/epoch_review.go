package main

import (
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
	"github.com/aperod/aperod/store"
)

// epochReview reports movements since the canonical LPoD funding anchor.
// These gross flows are not a net wallet balance, a burn baseline or permission
// to issue. In particular, a prior wallet balance is not implicitly zero.
func epochReview(c *store.LPoDCheckpoint) (map[string]any, error) {
	if c == nil || c.Allocation == nil || c.Allocation.Version != 3 {
		return nil, fmt.Errorf("epoch review: canonical LPoD v3 allocation required")
	}
	a := c.Allocation
	if a.FundingHeight == 0 || a.FundingBlock == (crypto.Hash32{}) ||
		c.State.LastHeight < a.FundingHeight {
		return nil, fmt.Errorf("epoch review: funding anchor unavailable")
	}
	if err := c.State.Validate(); err != nil {
		return nil, err
	}
	if a.InitialValidatorRemaining > sale.CapNAPRO ||
		a.ValidatorRemaining > a.InitialValidatorRemaining {
		return nil, fmt.Errorf("epoch review: invalid validator reserve movement")
	}
	draw := a.InitialValidatorRemaining - a.ValidatorRemaining
	if c.State.RewardInflow > draw {
		return nil, fmt.Errorf("epoch review: LPoD inflow exceeds validator draw")
	}
	direct := draw - c.State.RewardInflow
	if c.State.LeaderPaid > sale.CapNAPRO ||
		c.State.AngelPaid > sale.CapNAPRO-c.State.LeaderPaid {
		return nil, fmt.Errorf("epoch review: invalid LPoD reward payments")
	}
	paid := c.State.LeaderPaid + c.State.AngelPaid
	if direct > sale.CapNAPRO-paid {
		return nil, fmt.Errorf("epoch review: reward payments exceed allocation cap")
	}
	return map[string]any{
		"scope":                                 "gross pool movements since LPoD funding; not net wallet issuance or sale authorization",
		"funding_height":                        a.FundingHeight,
		"funding_block_hash":                    hex.EncodeToString(a.FundingBlock[:]),
		"through_height":                        c.State.LastHeight,
		"validator_draw_napro":                  strconv.FormatUint(draw, 10),
		"validator_to_lpod_napro":               strconv.FormatUint(c.State.RewardInflow, 10),
		"direct_validator_reward_outflow_napro": strconv.FormatUint(direct, 10),
		"lpod_reward_outflow_napro":             strconv.FormatUint(paid, 10),
		"gross_reward_outflow_to_wallets_napro": strconv.FormatUint(direct+paid, 10),
		"initial_wallet_carry_napro":            nil,
		"post_anchor_wallet_burn_napro":         nil,
		"post_anchor_other_issuance_napro":      nil,
		"activation_authorized":                 false,
	}, nil
}
