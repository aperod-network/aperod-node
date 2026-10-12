package store

import (
	"fmt"

	"github.com/aperod/aperod/sale"
)

// SaleAllocationReview separates the presentation residual from an unissued
// sale source. It is unsigned accounting, not a migration or spending authority.
type SaleAllocationReview struct {
	RecordedBurn         uint64  `json:"recorded_burn_napro,string"`
	ValidatorReserve     uint64  `json:"validator_reserve_napro,string"`
	LPoDRewardBalance    uint64  `json:"lpod_reward_balance_napro,string"`
	LPoDPrincipal        uint64  `json:"lpod_principal_locked_napro,string"`
	LPoDUnassigned       uint64  `json:"lpod_unassigned_napro,string"`
	PresentationResidual uint64  `json:"presentation_residual_napro,string"`
	BeforeOutsideWallets uint64  `json:"upper_bound_before_outside_wallets_napro,string"`
	OutsideWalletNet     *uint64 `json:"declared_outside_wallet_net_napro,string"`
	AfterDeclaredWallets *uint64 `json:"upper_bound_after_declared_wallets_napro,string"`
	ActivationAuthorized bool    `json:"activation_authorized"`
}

// OutsideWalletNet includes all already issued value outside the LPoD reward
// balance and its separately recorded guardian principal: free wallets, own
// validator stake, and any other issued holdings. It must be a net figure using
// the SAME burn baseline, not gross historical minting less burns a second time.
// A missing value remains unknown and never becomes an assumed zero.
func ReviewPostBurnSaleSource(c *LPoDCheckpoint, burned uint64, outsideWalletNet *uint64) (SaleAllocationReview, error) {
	var out SaleAllocationReview
	if c == nil || c.Allocation == nil || c.Allocation.Version != 3 ||
		c.Allocation.NominalSnapshot == nil {
		return out, fmt.Errorf("sale review: canonical v3 allocation required")
	}
	if c.GenesisVesting != nil ||
		c.Allocation.NominalSnapshot.PreexistingGuardianReservation != 0 {
		return out, fmt.Errorf("sale review: additional reserves require disjoint classification")
	}
	if err := c.State.Validate(); err != nil {
		return out, err
	}
	if c.PrincipalReturned > c.PrincipalDeposited ||
		c.PrincipalLocked != c.PrincipalDeposited-c.PrincipalReturned {
		return out, fmt.Errorf("sale review: principal conservation invalid")
	}
	left := sale.CapNAPRO
	for _, debit := range []uint64{burned, c.Allocation.ValidatorRemaining, c.State.Balance} {
		if debit > left {
			return out, fmt.Errorf("sale review: post-burn reserve accounting exceeds cap")
		}
		left -= debit
	}
	if c.PrincipalLocked > left {
		return out, fmt.Errorf("sale review: principal exceeds remaining allocation")
	}
	protected := left - c.PrincipalLocked
	if c.Allocation.Remaining > protected {
		return out, fmt.Errorf("sale review: earmarked LPoD capital exceeds remaining allocation")
	}
	protected -= c.Allocation.Remaining
	out = SaleAllocationReview{
		RecordedBurn: burned, ValidatorReserve: c.Allocation.ValidatorRemaining,
		LPoDRewardBalance: c.State.Balance, LPoDPrincipal: c.PrincipalLocked,
		PresentationResidual: left, BeforeOutsideWallets: protected, LPoDUnassigned: c.Allocation.Remaining,
	}
	if outsideWalletNet != nil {
		wallet := *outsideWalletNet
		if wallet > out.BeforeOutsideWallets {
			return SaleAllocationReview{}, fmt.Errorf("sale review: issued wallet liabilities exceed available source")
		}
		after := out.BeforeOutsideWallets - wallet
		out.OutsideWalletNet = &wallet
		out.AfterDeclaredWallets = &after
	}
	return out, nil
}
