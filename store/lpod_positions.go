// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"time"

	"github.com/syndtr/goleveldb/leveldb"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
)

// LPoDMaxPositions bounds the number of positions processed by settlement.
const LPoDMaxPositions = 4096

// LPoDMaxElapsedNS bounds a single settlement's elapsed-time accrual.
const LPoDMaxElapsedNS uint64 = 15_000_000_000
const lpodAPRDenominator uint64 = 100 * lpod.YearSeconds * 1_000_000_000

// LPoDValidatorStake supplies a validator's recorded amount and registry state.
// Active is not independent network liveness evidence.
type LPoDValidatorStake struct {
	Amount uint64
	Active bool
}

// LPoDPosition tracks a signed deposit and its checkpoint-bound lifecycle state.
type LPoDPosition struct {
	Deposit      core.LPoDPositionAction   `json:"deposit"`
	Nonce        uint64                    `json:"nonce"`
	UnlockHeight uint64                    `json:"unlock_height"` // full exit's canonical height; not a guardian timelock
	Returned     bool                      `json:"returned"`
	Due          uint64                    `json:"due_napro,string"`
	APRCarry     uint64                    `json:"apr_carry,string"`
	Withdrawn    uint64                    `json:"withdrawn_napro,string"`
	TopUps       []core.LPoDPositionAction `json:"top_ups,omitempty"`
	PendingDue   uint64                    `json:"pending_due_napro,string,omitempty"`
	NextPayoutNS int64                     `json:"next_payout_ns,omitempty"`
	// EffectiveVault is set only after a consensus-driven reassignment. The
	// original signed Deposit.Vault remains immutable so the owner's existing
	// withdrawal authorization continues to verify.
	EffectiveVault *crypto.Point32 `json:"effective_vault,omitempty"`
	RouteHeight    uint64          `json:"route_height,omitempty"`
	AutoReturned   bool            `json:"auto_returned,omitempty"`
}

// LPoDWithdrawalAmount validates the signed request against remaining principal.
// Nonces advance on every partial or full exit; the original deposit is immutable.
func LPoDWithdrawalAmount(p LPoDPosition, a core.LPoDPositionAction) (uint64, error) {
	principal, err := LPoDPositionPrincipal(p)
	if err != nil {
		return 0, err
	}
	if p.Returned || p.UnlockHeight != 0 || p.Withdrawn >= principal || p.Nonce == ^uint64(0) {
		return 0, fmt.Errorf("lpod: unknown, closed or exhausted position")
	}
	expected := p.Deposit
	expected.Action = core.LPoDWithdraw
	expected.Nonce = p.Nonce + 1
	expected.WithdrawAmount = a.WithdrawAmount
	if !reflect.DeepEqual(expected, a) {
		return 0, fmt.Errorf("lpod: withdrawal changes ownership, source, vault or nonce")
	}
	amount := a.WithdrawAmount
	if amount == 0 {
		amount = principal - p.Withdrawn
	}
	if amount > principal-p.Withdrawn {
		return 0, fmt.Errorf("lpod: withdrawal exceeds remaining principal")
	}
	return amount, nil
}

// LPoDPositionPrincipal returns the original deposit plus every canonical,
// signed top-up. The original deposit action is never rewritten.
func LPoDPositionPrincipal(p LPoDPosition) (uint64, error) {
	total := p.Deposit.Amount
	for i, topUp := range p.TopUps {
		if topUp.Action != core.LPoDTopUp || topUp.Genesis != p.Deposit.Genesis ||
			topUp.PositionID != p.Deposit.PositionID || topUp.Vault != p.Deposit.Vault ||
			topUp.Beneficiary != p.Deposit.Beneficiary || topUp.Owner != p.Deposit.Owner ||
			topUp.Nonce == 0 || (i > 0 && topUp.Nonce <= p.TopUps[i-1].Nonce) {
			return 0, fmt.Errorf("lpod: corrupt top-up history")
		}
		var err error
		total, err = lpodAdd(total, topUp.Amount)
		if err != nil {
			return 0, err
		}
	}
	if p.Withdrawn > total {
		return 0, fmt.Errorf("lpod: withdrawals exceed total principal")
	}
	return total, nil
}

// LPoDTopUpAmount verifies that a signed top-up names this position, preserves
// its immutable owner/vault identity and consumes the next common operation nonce.
func LPoDTopUpAmount(p LPoDPosition, a core.LPoDPositionAction) (uint64, error) {
	if p.Returned || p.UnlockHeight != 0 || p.Nonce == ^uint64(0) ||
		a.Action != core.LPoDTopUp || a.Nonce != p.Nonce+1 ||
		a.Genesis != p.Deposit.Genesis || a.PositionID != p.Deposit.PositionID ||
		a.Vault != p.Deposit.Vault || a.Beneficiary != p.Deposit.Beneficiary ||
		a.Owner != p.Deposit.Owner || a.Amount == 0 || a.WithdrawAmount != 0 ||
		a.SourceTx == (crypto.Hash32{}) || a.SourcePub == (crypto.Point32{}) {
		return 0, fmt.Errorf("lpod: invalid top-up identity or nonce")
	}
	return a.Amount, nil
}

const (
	lpodDayNS   = int64(24 * time.Hour)
	lpodMSKNS   = int64(3 * time.Hour)
	lpodPayHour = 20
)

var lpodMSK = time.FixedZone("MSK", 3*60*60)

// LPoDNextDailyBoundaryAfter24h is the first 20:00 Moscow boundary at or after
// 24 full hours have elapsed from startNS.
func LPoDNextDailyBoundaryAfter24h(startNS int64) (int64, error) {
	maxInt64 := int64(^uint64(0) >> 1)
	if startNS <= 0 || startNS > maxInt64-lpodDayNS {
		return 0, fmt.Errorf("lpod: invalid daily payout anchor")
	}
	eligible := time.Unix(0, startNS+lpodDayNS).In(lpodMSK)
	y, m, d := eligible.Date()
	boundary := time.Date(y, m, d, lpodPayHour, 0, 0, 0, lpodMSK)
	if boundary.Before(eligible) {
		boundary = boundary.AddDate(0, 0, 1)
	}
	ns := boundary.UnixNano()
	if ns <= startNS {
		return 0, fmt.Errorf("lpod: daily payout boundary overflow")
	}
	return ns, nil
}

func lpodNextDailyBoundaryAfter(timestampNS int64) (int64, error) {
	if timestampNS <= 0 || timestampNS == int64(^uint64(0)>>1) {
		return 0, fmt.Errorf("lpod: invalid daily payout timestamp")
	}
	eligible := time.Unix(0, timestampNS+1).In(lpodMSK)
	y, m, d := eligible.Date()
	boundary := time.Date(y, m, d, lpodPayHour, 0, 0, 0, lpodMSK)
	if boundary.Before(eligible) {
		boundary = boundary.AddDate(0, 0, 1)
	}
	ns := boundary.UnixNano()
	if ns <= timestampNS {
		return 0, fmt.Errorf("lpod: daily payout boundary overflow")
	}
	return ns, nil
}

func lpodAdd(a, b uint64) (uint64, error) {
	if b > ^uint64(0)-a {
		return 0, fmt.Errorf("lpod: amount overflow")
	}
	return a + b, nil
}

func lpodPositionID(a core.LPoDPositionAction) string { return hex.EncodeToString(a.PositionID[:]) }
func lpodVaultID(a core.LPoDPositionAction) string    { return hex.EncodeToString(a.Vault[:]) }

// LPoDEffectiveVault returns the current accounting destination without
// changing the immutable validator identity signed into the deposit.
func LPoDEffectiveVault(p LPoDPosition) crypto.Point32 {
	if p.EffectiveVault != nil {
		return *p.EffectiveVault
	}
	return p.Deposit.Vault
}

func lpodPositionVaultID(p LPoDPosition) string {
	v := LPoDEffectiveVault(p)
	return hex.EncodeToString(v[:])
}

func addLPoDDeferredValidatorPayout(
	payouts []LPoDDeferredValidatorPayout,
	validator string,
	address crypto.Address,
	amount uint64,
) ([]LPoDDeferredValidatorPayout, error) {
	if amount == 0 {
		return payouts, nil
	}
	rawValidator, err := hex.DecodeString(validator)
	if err != nil || len(rawValidator) != 32 || hex.EncodeToString(rawValidator) != validator {
		return nil, fmt.Errorf("lpod: invalid deferred validator identity")
	}
	if _, _, _, err := crypto.DecodeAddress(address); err != nil {
		return nil, fmt.Errorf("lpod: invalid deferred validator reward address: %w", err)
	}
	index := sort.Search(len(payouts), func(i int) bool { return payouts[i].Validator >= validator })
	if index < len(payouts) && payouts[index].Validator == validator {
		payouts[index].AmountNAPRO, err = lpodAdd(payouts[index].AmountNAPRO, amount)
		if err != nil {
			return nil, err
		}
		// Keep the first address authorized in this daily window. This bounds
		// consensus state to one entry per validator and prevents a later block
		// from redirecting already accrued rewards.
		return payouts, nil
	}
	if len(payouts) >= core.MaxValidators {
		return nil, fmt.Errorf("lpod: deferred validator payout set exceeds active-validator limit")
	}
	payouts = append(payouts, LPoDDeferredValidatorPayout{})
	copy(payouts[index+1:], payouts[index:])
	payouts[index] = LPoDDeferredValidatorPayout{
		Validator: validator, Address: address, AmountNAPRO: amount,
	}
	return payouts, nil
}

func collectLPoDDailyValidatorPayouts(
	before *LPoDCheckpoint,
	r *LPoDSettlement,
	payments []lpod.Payment,
) ([]LPoDDeferredValidatorPayout, bool, error) {
	payouts := append([]LPoDDeferredValidatorPayout(nil), before.PendingValidatorPayouts...)
	for _, payment := range payments {
		if payment.Leader == 0 {
			continue
		}
		if payment.VaultID != r.Proposer {
			return nil, false, fmt.Errorf("lpod: non-proposer leader reward")
		}
		var err error
		payouts, err = addLPoDDeferredValidatorPayout(
			payouts, payment.VaultID, r.Leader, payment.Leader,
		)
		if err != nil {
			return nil, false, err
		}
	}
	due := before.ValidatorDailyPayoutVersion > 0 &&
		before.NextValidatorPayoutNS > 0 &&
		r.Timestamp >= before.NextValidatorPayoutNS
	return payouts, due, nil
}

func deriveLPoDValidatorPayouts(
	before, after *LPoDCheckpoint,
	timestamp int64,
	proposer string,
	leader crypto.Address,
) (map[crypto.Address]uint64, map[string]uint64, error) {
	if after == nil {
		return nil, nil, fmt.Errorf("lpod: missing validator payout checkpoint")
	}
	if before == nil {
		before = &LPoDCheckpoint{}
	}
	if before.ValidatorDailyPayoutVersion > 0 && after.ValidatorDailyPayoutVersion == 0 {
		return nil, nil, fmt.Errorf("lpod: validator daily payout activation was reverted")
	}
	if after.State.LeaderPaid < before.State.LeaderPaid {
		return nil, nil, fmt.Errorf("lpod: validator paid total decreased")
	}
	beforeOutstanding, err := lpodAdd(before.State.LeaderPaid, before.State.PendingLeader)
	if err != nil {
		return nil, nil, err
	}
	afterOutstanding, err := lpodAdd(after.State.LeaderPaid, after.State.PendingLeader)
	if err != nil {
		return nil, nil, err
	}
	if afterOutstanding < beforeOutstanding {
		return nil, nil, fmt.Errorf("lpod: validator reward total decreased")
	}
	currentLeader := afterOutstanding - beforeOutstanding
	paidDelta := after.State.LeaderPaid - before.State.LeaderPaid
	addresses := make(map[crypto.Address]uint64)
	validators := make(map[string]uint64)
	if after.ValidatorDailyPayoutVersion == 0 {
		if before.ValidatorDailyPayoutVersion != 0 ||
			before.State.PendingLeader != 0 || after.State.PendingLeader != 0 ||
			len(before.PendingValidatorPayouts) != 0 || len(after.PendingValidatorPayouts) != 0 ||
			paidDelta != currentLeader {
			return nil, nil, fmt.Errorf("lpod: invalid non-daily validator payout transition")
		}
		if currentLeader > 0 {
			if proposer == "" {
				return nil, nil, fmt.Errorf("lpod: missing validator identity for payout")
			}
			if _, _, _, err := crypto.DecodeAddress(leader); err != nil {
				return nil, nil, fmt.Errorf("lpod: invalid validator reward address: %w", err)
			}
			addresses[leader] = currentLeader
			validators[proposer] = currentLeader
		}
		return addresses, validators, nil
	}
	if after.NextValidatorPayoutNS <= timestamp {
		return nil, nil, fmt.Errorf("lpod: next validator payout boundary is not in the future")
	}
	pending := append([]LPoDDeferredValidatorPayout(nil), before.PendingValidatorPayouts...)
	if currentLeader > 0 {
		if proposer == "" {
			return nil, nil, fmt.Errorf("lpod: missing validator identity for deferred payout")
		}
		pending, err = addLPoDDeferredValidatorPayout(pending, proposer, leader, currentLeader)
		if err != nil {
			return nil, nil, err
		}
	}
	due := before.ValidatorDailyPayoutVersion > 0 &&
		before.NextValidatorPayoutNS > 0 && timestamp >= before.NextValidatorPayoutNS
	if !due {
		pendingTotal, err := sumDeferredValidatorPayouts(pending)
		if err != nil {
			return nil, nil, err
		}
		if paidDelta != 0 || after.State.PendingLeader != pendingTotal ||
			!reflect.DeepEqual(pending, after.PendingValidatorPayouts) {
			return nil, nil, fmt.Errorf("lpod: validator reward was paid before its daily boundary")
		}
		return addresses, validators, nil
	}
	if after.State.PendingLeader != 0 || len(after.PendingValidatorPayouts) != 0 {
		return nil, nil, fmt.Errorf("lpod: daily validator payout did not clear pending rewards")
	}
	var release uint64
	for _, payout := range pending {
		release, err = lpodAdd(release, payout.AmountNAPRO)
		if err != nil {
			return nil, nil, err
		}
		addresses[payout.Address], err = lpodAdd(addresses[payout.Address], payout.AmountNAPRO)
		if err != nil {
			return nil, nil, err
		}
		validators[payout.Validator], err = lpodAdd(validators[payout.Validator], payout.AmountNAPRO)
		if err != nil {
			return nil, nil, err
		}
	}
	if release != paidDelta {
		return nil, nil, fmt.Errorf("lpod: daily validator payout does not reconcile paid total")
	}
	return addresses, validators, nil
}

func sumDeferredValidatorPayouts(payouts []LPoDDeferredValidatorPayout) (uint64, error) {
	var total uint64
	for _, payout := range payouts {
		var err error
		total, err = lpodAdd(total, payout.AmountNAPRO)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func (c *LPoDCheckpoint) validatePositions() error {
	var locked, due, pending uint64
	if len(c.Positions) > LPoDMaxPositions || c.DailyPayoutVersion > 1 ||
		c.ValidatorDailyPayoutVersion > 1 {
		return fmt.Errorf("lpod: position capacity exceeded")
	}
	if c.ValidatorDailyPayoutVersion == 0 {
		if c.NextValidatorPayoutNS != 0 || len(c.PendingValidatorPayouts) != 0 ||
			c.State.PendingLeader != 0 {
			return fmt.Errorf("lpod: deferred validator payouts exist before activation")
		}
	} else if c.DailyPayoutVersion == 0 || c.NextValidatorPayoutNS <= 0 {
		return fmt.Errorf("lpod: invalid validator daily payout checkpoint")
	}
	if len(c.PendingValidatorPayouts) > core.MaxValidators {
		return fmt.Errorf("lpod: deferred validator payout set exceeds active-validator limit")
	}
	var pendingValidator uint64
	previousValidator := ""
	for _, payout := range c.PendingValidatorPayouts {
		rawValidator, err := hex.DecodeString(payout.Validator)
		if err != nil || len(rawValidator) != 32 ||
			hex.EncodeToString(rawValidator) != payout.Validator ||
			(previousValidator != "" && previousValidator >= payout.Validator) ||
			payout.AmountNAPRO == 0 {
			return fmt.Errorf("lpod: corrupt deferred validator payout")
		}
		if _, _, _, err := crypto.DecodeAddress(payout.Address); err != nil {
			return fmt.Errorf("lpod: corrupt deferred validator address")
		}
		pendingValidator, err = lpodAdd(pendingValidator, payout.AmountNAPRO)
		if err != nil {
			return err
		}
		previousValidator = payout.Validator
	}
	if pendingValidator != c.State.PendingLeader {
		return fmt.Errorf("lpod: deferred validator payout conservation failed")
	}
	for id, p := range c.Positions {
		principal, err := LPoDPositionPrincipal(p)
		if err != nil {
			return err
		}
		if id != lpodPositionID(p.Deposit) || p.Deposit.Action != core.LPoDDeposit || p.Deposit.Nonce != 0 ||
			p.Deposit.WithdrawAmount != 0 || p.Deposit.Amount == 0 || p.Withdrawn > principal ||
			p.Returned != (p.Withdrawn == principal) ||
			(!p.AutoReturned && (p.Nonce < uint64(len(p.TopUps)) ||
				(p.Withdrawn == 0 && p.Nonce != uint64(len(p.TopUps))) ||
				(p.Withdrawn > 0 && p.Nonce == uint64(len(p.TopUps))))) ||
			(p.AutoReturned && !p.Returned) ||
			p.APRCarry >= lpodAPRDenominator || (p.UnlockHeight == 0) != (!p.Returned) ||
			p.UnlockHeight > c.State.LastHeight ||
			(c.DailyPayoutVersion == 0 && (len(p.TopUps) != 0 || p.PendingDue != 0 || p.NextPayoutNS != 0)) ||
			(c.DailyPayoutVersion > 0 && p.NextPayoutNS <= 0) {
			return fmt.Errorf("lpod: corrupt canonical position")
		}
		for _, topUp := range p.TopUps {
			if topUp.Nonce > p.Nonce {
				return fmt.Errorf("lpod: top-up nonce exceeds position nonce")
			}
		}
		if p.EffectiveVault != nil && (*p.EffectiveVault == p.Deposit.Vault || p.RouteHeight == 0 || p.RouteHeight > c.State.LastHeight) {
			return fmt.Errorf("lpod: corrupt effective vault route")
		}
		if !p.Returned {
			locked, err = lpodAdd(locked, principal-p.Withdrawn)
			if err != nil {
				return err
			}
		}
		due, err = lpodAdd(due, p.Due)
		if err != nil {
			return err
		}
		pending, err = lpodAdd(pending, p.PendingDue)
		if err != nil {
			return err
		}
	}
	total, err := lpodAdd(c.PrincipalLocked, c.PrincipalReturned)
	if err != nil || total != c.PrincipalDeposited || locked != c.PrincipalLocked ||
		(c.Allocation != nil && due != c.State.UnfundedLiability) || pending != c.PendingLiability {
		return fmt.Errorf("lpod: principal or position liability conservation failed")
	}
	return nil
}

// positionTransition first accrues prior canonical positions, then applies signed
// operations. A deposit earns nothing retroactively; returned principal stops
// future APR while any remaining principal continues accruing. Withdrawal
// preserves earned arrears and pays in its canonical block. Validator unbonding is unchanged. Every vault accrues,
// regardless of this block's proposer.
func (d *DB) positionTransition(before *LPoDCheckpoint, height uint64, r *LPoDSettlement) (*LPoDCheckpoint, []lpod.Vault, map[string]string, error) {
	if before.Allocation == nil {
		return nil, nil, nil, fmt.Errorf("lpod: positions require reconciled allocation")
	}
	if err := before.validatePositions(); err != nil {
		return nil, nil, nil, err
	}
	if before.DailyPayoutVersion > 0 && !r.DailyPayout {
		return nil, nil, nil, fmt.Errorf("lpod: daily payout protocol cannot be disabled after activation")
	}
	if before.ValidatorDailyPayoutVersion > 0 && !r.DailyValidatorPayout {
		return nil, nil, nil, fmt.Errorf("lpod: validator daily payout protocol cannot be disabled after activation")
	}
	if r.DailyValidatorPayout && !r.DailyPayout {
		return nil, nil, nil, fmt.Errorf("lpod: validator daily payouts require the daily position protocol")
	}
	if r.DailyValidatorPayout && before.ValidatorDailyPayoutVersion == 0 &&
		(height != r.ValidatorDailyPayoutActivationHeight ||
			r.ValidatorDailyPayoutActivationHeight == 0) {
		return nil, nil, nil, fmt.Errorf("lpod: validator daily payout activation height was missed")
	}
	parent, err := d.readLPoDCanonicalBlock(height - 1)
	if err != nil {
		return nil, nil, nil, err
	}
	if parent.Hash() != r.Parent || r.Timestamp <= parent.Header.Timestamp {
		return nil, nil, nil, fmt.Errorf("lpod: nonmonotonic or noncanonical accrual clock")
	}
	// #nosec G115 -- Strict monotonicity above makes the mathematical delta unsigned; preserve its full 64-bit value before applying the elapsed cap below.
	elapsed := uint64(r.Timestamp - parent.Header.Timestamp)
	if elapsed > LPoDMaxElapsedNS {
		elapsed = LPoDMaxElapsedNS
	}
	next := *before
	next.PendingValidatorPayouts = append(
		[]LPoDDeferredValidatorPayout(nil), before.PendingValidatorPayouts...,
	)
	dailyValidatorActivation := r.DailyValidatorPayout &&
		before.ValidatorDailyPayoutVersion == 0
	if r.DailyPayout {
		next.DailyPayoutVersion = 1
	}
	if r.DailyValidatorPayout {
		next.ValidatorDailyPayoutVersion = 1
		if dailyValidatorActivation {
			next.NextValidatorPayoutNS, err = LPoDNextDailyBoundaryAfter24h(r.Timestamp)
			if err != nil {
				return nil, nil, nil, err
			}
		} else if r.Timestamp >= before.NextValidatorPayoutNS {
			next.NextValidatorPayoutNS, err = lpodNextDailyBoundaryAfter(r.Timestamp)
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}
	next.Positions = make(map[string]LPoDPosition, len(before.Positions))
	vaultIDs := map[string]bool{r.Proposer: true}
	totals := map[string]uint64{}
	entitlementVault := map[string]string{}
	for id, p := range before.Positions {
		principal, principalErr := LPoDPositionPrincipal(p)
		if principalErr != nil {
			return nil, nil, nil, principalErr
		}
		if dailyValidatorActivation {
			// The new consensus rule pays only rewards earned from its activation
			// onward. Retire prior unpaid/pending Guardian accruals and fractional
			// carry instead of silently adding historical debt to a future payout.
			p.Due = 0
			p.PendingDue = 0
			p.APRCarry = 0
			p.NextPayoutNS = next.NextValidatorPayoutNS
		}
		// Fully closed positions no longer occupy capacity. Reopening their
		// deposit still requires its already-spent, cryptographically linked KI.
		if p.Returned && p.Due == 0 && p.PendingDue == 0 {
			continue
		}
		if r.DailyPayout && p.NextPayoutNS == 0 {
			p.NextPayoutNS, err = LPoDNextDailyBoundaryAfter24h(r.Timestamp)
			if err != nil {
				return nil, nil, nil, err
			}
		}
		next.Positions[id] = p
		v := lpodPositionVaultID(p)
		entitlementVault[id] = v
		vaultIDs[v] = true
		if p.UnlockHeight == 0 {
			totals[v], err = lpodAdd(totals[v], principal-p.Withdrawn)
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}
	accrual := map[string]uint64{}
	arrears := map[string]uint64{}
	for id, p := range next.Positions {
		v := lpodPositionVaultID(p)
		stake := r.Stake[v]
		if prior := r.PreviousStake[v]; !stake.Active && prior.Active {
			stake = prior
		}
		var earned uint64
		if !dailyValidatorActivation && p.UnlockHeight == 0 && stake.Active {
			total, err := lpodAdd(stake.Amount, totals[v])
			if err != nil {
				return nil, nil, nil, err
			}
			if total > 100_000_000*lpod.Unit {
				total = 100_000_000 * lpod.Unit
			}
			tier, err := lpod.TierFor(total)
			if err != nil {
				return nil, nil, nil, err
			}
			principal, err := LPoDPositionPrincipal(p)
			if err != nil {
				return nil, nil, nil, err
			}
			n := new(big.Int).SetUint64(principal - p.Withdrawn)
			n.Mul(n, new(big.Int).SetUint64(tier.APRPercent))
			n.Mul(n, new(big.Int).SetUint64(elapsed))
			n.Add(n, new(big.Int).SetUint64(p.APRCarry))
			q, rem := new(big.Int), new(big.Int)
			q.QuoRem(n, new(big.Int).SetUint64(lpodAPRDenominator), rem)
			if !q.IsUint64() {
				return nil, nil, nil, fmt.Errorf("lpod: accrued liability overflow")
			}
			earned = q.Uint64()
			p.APRCarry = rem.Uint64()
		}
		if r.DailyPayout {
			p.PendingDue, err = lpodAdd(p.PendingDue, earned)
			if err != nil {
				return nil, nil, nil, err
			}
			if r.Timestamp >= p.NextPayoutNS {
				arrears[v], err = lpodAdd(arrears[v], p.Due)
				if err != nil {
					return nil, nil, nil, err
				}
				accrual[v], err = lpodAdd(accrual[v], p.PendingDue)
				if err != nil {
					return nil, nil, nil, err
				}
				p.Due, err = lpodAdd(p.Due, p.PendingDue)
				if err != nil {
					return nil, nil, nil, err
				}
				p.PendingDue = 0
				p.NextPayoutNS, err = lpodNextDailyBoundaryAfter(r.Timestamp)
				if err != nil {
					return nil, nil, nil, err
				}
			}
		} else {
			arrears[v], err = lpodAdd(arrears[v], p.Due)
			if err != nil {
				return nil, nil, nil, err
			}
			p.Due, err = lpodAdd(p.Due, earned)
			if err != nil {
				return nil, nil, nil, err
			}
			accrual[v], err = lpodAdd(accrual[v], earned)
			if err != nil {
				return nil, nil, nil, err
			}
		}
		next.Positions[id] = p
	}
	for i := range r.Transactions {
		tx := &r.Transactions[i]
		if !tx.IsLPoDPosition() {
			continue
		}
		a, err := tx.LPoDPositionAction()
		if err != nil {
			return nil, nil, nil, err
		}
		if a.Genesis != before.Allocation.Genesis {
			return nil, nil, nil, fmt.Errorf("lpod: position belongs to another chain")
		}
		id, v := lpodPositionID(*a), lpodVaultID(*a)
		switch a.Action {
		case core.LPoDDeposit:
			if _, exists := next.Positions[id]; exists || len(next.Positions) >= LPoDMaxPositions {
				return nil, nil, nil, fmt.Errorf("lpod: replayed deposit or position capacity reached")
			}
			stake := r.Stake[v]
			if !stake.Active {
				return nil, nil, nil, fmt.Errorf("lpod: deposits require an active authenticated validator")
			}
			total, err := lpodAdd(stake.Amount, totals[v])
			if err != nil {
				return nil, nil, nil, err
			}
			total, err = lpodAdd(total, a.Amount)
			if err != nil {
				return nil, nil, nil, err
			}
			tier, err := lpod.TierFor(total)
			if err != nil || a.Amount < tier.MinimumDeposit {
				return nil, nil, nil, fmt.Errorf("lpod: deposit outside vault tier limits")
			}
			position := LPoDPosition{Deposit: *a}
			if r.DailyPayout {
				position.NextPayoutNS, err = LPoDNextDailyBoundaryAfter24h(r.Timestamp)
				if err != nil {
					return nil, nil, nil, err
				}
			}
			next.Positions[id] = position
			next.PrincipalDeposited, err = lpodAdd(next.PrincipalDeposited, a.Amount)
			if err != nil {
				return nil, nil, nil, err
			}
			next.PrincipalLocked, err = lpodAdd(next.PrincipalLocked, a.Amount)
			if err != nil {
				return nil, nil, nil, err
			}
			totals[v], err = lpodAdd(totals[v], a.Amount)
			if err != nil {
				return nil, nil, nil, err
			}
			vaultIDs[v] = true
		case core.LPoDWithdraw:
			p, exists := next.Positions[id]
			if !exists {
				return nil, nil, nil, fmt.Errorf("lpod: unknown or replayed withdrawal")
			}
			amount, err := LPoDWithdrawalAmount(p, *a)
			if err != nil {
				return nil, nil, nil, err
			}
			p.Nonce++
			p.Withdrawn += amount
			principal, err := LPoDPositionPrincipal(p)
			if err != nil {
				return nil, nil, nil, err
			}
			p.Returned = p.Withdrawn == principal
			if p.Returned {
				p.UnlockHeight = height
			}
			next.PrincipalLocked -= amount
			next.PrincipalReturned, err = lpodAdd(next.PrincipalReturned, amount)
			if err != nil {
				return nil, nil, nil, err
			}
			effective := lpodPositionVaultID(p)
			totals[effective] -= amount
			next.Positions[id] = p
		case core.LPoDTopUp:
			if !r.DailyPayout {
				return nil, nil, nil, fmt.Errorf("lpod: top-ups require the daily payout protocol")
			}
			p, exists := next.Positions[id]
			if !exists {
				return nil, nil, nil, fmt.Errorf("lpod: top-up position does not exist")
			}
			amount, err := LPoDTopUpAmount(p, *a)
			if err != nil {
				return nil, nil, nil, err
			}
			effectiveVault := LPoDEffectiveVault(p)
			effective := hex.EncodeToString(effectiveVault[:])
			stake := r.Stake[effective]
			max := 100_000_000 * lpod.Unit
			if effective != v || !stake.Active || stake.Amount > max || totals[effective] > max-stake.Amount ||
				amount > max-stake.Amount-totals[effective] {
				return nil, nil, nil, fmt.Errorf("lpod: top-up exceeds active vault capacity")
			}
			if _, err := lpod.TierFor(stake.Amount + totals[effective] + amount); err != nil {
				return nil, nil, nil, fmt.Errorf("lpod: top-up outside vault tier limits")
			}
			p.Nonce++
			p.TopUps = append(p.TopUps, *a)
			p.PendingDue = 0
			p.APRCarry = 0
			p.NextPayoutNS, err = LPoDNextDailyBoundaryAfter24h(r.Timestamp)
			if err != nil {
				return nil, nil, nil, err
			}
			next.Positions[id] = p
			next.PrincipalDeposited, err = lpodAdd(next.PrincipalDeposited, amount)
			if err != nil {
				return nil, nil, nil, err
			}
			next.PrincipalLocked, err = lpodAdd(next.PrincipalLocked, amount)
			if err != nil {
				return nil, nil, nil, err
			}
			totals[effective], err = lpodAdd(totals[effective], amount)
			if err != nil {
				return nil, nil, nil, err
			}
			vaultIDs[effective] = true
		}
	}
	// Apply lifecycle routing after signed operations. A deposit targeting a
	// validator that exits in this block was already rejected above; an owner's
	// withdrawal wins over an automatic route/refund, preventing double return.
	positionIDs := make([]string, 0, len(next.Positions))
	for id := range next.Positions {
		positionIDs = append(positionIDs, id)
	}
	sort.Strings(positionIDs)
	const minValidator = 100_000 * lpod.Unit
	sourceBeforeTotals := make(map[string]uint64, len(totals))
	for source, guardian := range totals {
		previous := r.PreviousStake[source]
		sourceBeforeTotals[source], err = lpodAdd(previous.Amount, guardian)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	ranked := LPoDEligibleVaults(r.Stake, totals)
	eligible := make(map[string]bool, len(ranked))
	for _, vault := range ranked {
		eligible[vault.ID] = true
	}
	for _, id := range positionIDs {
		p := next.Positions[id]
		if p.Returned {
			continue
		}
		source := lpodPositionVaultID(p)
		sourceStake := r.Stake[source]
		if sourceStake.Active && sourceStake.Amount >= minValidator {
			continue
		}
		sourceBefore := sourceBeforeTotals[source]
		principal, err := LPoDPositionPrincipal(p)
		if err != nil {
			return nil, nil, nil, err
		}
		remaining := principal - p.Withdrawn
		best, err := lpodRouteDestination(source, sourceBefore, remaining, r.Stake, totals, eligible)
		if err != nil {
			return nil, nil, nil, err
		}
		totals[source] -= remaining
		if best == "" {
			p.Withdrawn = principal
			p.Returned = true
			p.AutoReturned = true
			p.UnlockHeight = height
			next.PrincipalLocked -= remaining
			next.PrincipalReturned, err = lpodAdd(next.PrincipalReturned, remaining)
			if err != nil {
				return nil, nil, nil, err
			}
			next.Positions[id] = p
			continue
		}
		raw, err := hex.DecodeString(best)
		if err != nil || len(raw) != 32 {
			return nil, nil, nil, fmt.Errorf("lpod: invalid lifecycle destination")
		}
		var destination crypto.Point32
		copy(destination[:], raw)
		if destination == p.Deposit.Vault {
			p.EffectiveVault = nil
			p.RouteHeight = 0
		} else {
			p.EffectiveVault = &destination
			p.RouteHeight = height
		}
		next.Positions[id] = p
		totals[best], err = lpodAdd(totals[best], remaining)
		if err != nil {
			return nil, nil, nil, err
		}
		vaultIDs[best] = true
	}
	ids := make([]string, 0, len(vaultIDs))
	for id := range vaultIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	vaults := make([]lpod.Vault, 0, len(ids))
	// Use the PRIOR principal total for this block's tier and leader share.
	priorTotals := map[string]uint64{}
	for _, p := range before.Positions {
		if p.UnlockHeight == 0 {
			v := lpodPositionVaultID(p)
			principal, principalErr := LPoDPositionPrincipal(p)
			if principalErr != nil {
				return nil, nil, nil, principalErr
			}
			priorTotals[v], err = lpodAdd(priorTotals[v], principal-p.Withdrawn)
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}
	for _, id := range ids {
		stake := r.Stake[id]
		if prior := r.PreviousStake[id]; !stake.Active && prior.Active {
			stake = prior
		}
		total, err := lpodAdd(stake.Amount, priorTotals[id])
		if err != nil {
			return nil, nil, nil, err
		}
		if !stake.Active || total < 100_000*lpod.Unit {
			total = 100_000 * lpod.Unit
		}
		if total > 100_000_000*lpod.Unit {
			total = 100_000_000 * lpod.Unit
		}
		due := accrual[id]
		v := lpod.Vault{ID: id, TotalStake: total, GuardianStake: priorTotals[id], ExactAccrual: &due, Arrears: arrears[id]}
		if v.GuardianStake > v.TotalStake {
			v.TotalStake = v.GuardianStake
		}
		if id == r.Proposer {
			if !stake.Active {
				return nil, nil, nil, fmt.Errorf("lpod: proposer has no authenticated active stake")
			}
			v.ActualIncome = LPoDRewardFor(before.Allocation.ValidatorRemaining)
		}
		vaults = append(vaults, v)
	}
	return &next, vaults, entitlementVault, nil
}

func settleLPoDPositionAccounting(
	before, next *LPoDCheckpoint,
	height uint64,
	r *LPoDSettlement,
	vaults []lpod.Vault,
) ([]lpod.Payment, error) {
	accountingBefore := before.State
	if r.DailyValidatorPayout && before.ValidatorDailyPayoutVersion == 0 {
		// The coordinated upgrade begins a future-only reward period. Historical
		// unpaid Guardian liabilities are not paid by the new daily protocol.
		accountingBefore.UnfundedLiability = 0
		accountingBefore.AccruedLiability = accountingBefore.AngelPaid
	}
	state, carries, payments, err := lpod.Settle(accountingBefore, before.Carries, height, vaults)
	if err != nil {
		return nil, err
	}
	next.State, next.Carries = state, carries
	if r.DailyValidatorPayout {
		pendingPayouts, due, err := collectLPoDDailyValidatorPayouts(before, r, payments)
		if err != nil {
			return nil, err
		}
		var currentLeader uint64
		for _, payment := range payments {
			currentLeader, err = lpodAdd(currentLeader, payment.Leader)
			if err != nil {
				return nil, err
			}
		}
		if currentLeader > next.State.LeaderPaid {
			return nil, fmt.Errorf("lpod: leader payout exceeds recognized paid total")
		}
		next.State.LeaderPaid -= currentLeader
		next.State.PendingLeader, err = lpodAdd(next.State.PendingLeader, currentLeader)
		if err != nil {
			return nil, err
		}
		next.PendingValidatorPayouts = pendingPayouts
		if due {
			var release uint64
			for _, payout := range pendingPayouts {
				release, err = lpodAdd(release, payout.AmountNAPRO)
				if err != nil {
					return nil, err
				}
			}
			if release != next.State.PendingLeader {
				return nil, fmt.Errorf("lpod: deferred validator payout total differs from checkpoint")
			}
			next.State.PendingLeader = 0
			next.State.LeaderPaid, err = lpodAdd(next.State.LeaderPaid, release)
			if err != nil {
				return nil, err
			}
			next.PendingValidatorPayouts = nil
		}
		if err := next.State.Validate(); err != nil {
			return nil, err
		}
	}
	return payments, nil
}

func (d *DB) previewPositions(before *LPoDCheckpoint, height uint64, r *LPoDSettlement) (*LPoDCheckpoint, []lpod.Payment, error) {
	next, vaults, entitlementVault, err := d.positionTransition(before, height, r)
	if err != nil {
		return nil, nil, err
	}
	payments, err := settleLPoDPositionAccounting(before, next, height, r, vaults)
	if err != nil {
		return nil, nil, err
	}
	next.Allocation, err = lpodRewardDebit(before.Allocation, vaults)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(next.Positions))
	for id := range next.Positions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, payment := range payments {
		var due uint64
		for _, id := range ids {
			p := next.Positions[id]
			if entitlementVault[id] == payment.VaultID {
				due, err = lpodAdd(due, p.Due)
				if err != nil {
					return nil, nil, err
				}
			}
		}
		left := payment.Angels
		for _, id := range ids {
			p := next.Positions[id]
			if entitlementVault[id] != payment.VaultID || p.Due == 0 {
				continue
			}
			q := new(big.Int).Mul(new(big.Int).SetUint64(left), new(big.Int).SetUint64(p.Due))
			q.Div(q, new(big.Int).SetUint64(due))
			paid := q.Uint64()
			left -= paid
			due -= p.Due
			p.Due -= paid
			next.Positions[id] = p
		}
		if left != 0 {
			return nil, nil, fmt.Errorf("lpod: proportional payout rounding did not conserve funds")
		}
	}
	next.PendingLiability = 0
	for _, p := range next.Positions {
		next.PendingLiability, err = lpodAdd(next.PendingLiability, p.PendingDue)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := next.validatePositions(); err != nil {
		return nil, nil, err
	}
	if err := previewGenesisVesting(before, next, r, height); err != nil {
		return nil, nil, err
	}
	return next, payments, nil
}

// PayoutLPoD builds the actual deterministic spendable outputs. Beneficiaries are
// aggregated and sorted, avoiding duplicate one-time keys when one wallet owns
// several positions or is also the leader. Both accrued arrears and principal
// returns are derived from canonical before/after records, never client amounts.
func (d *DB) PayoutLPoD(height uint64, r *LPoDSettlement, after *LPoDCheckpoint, payments []lpod.Payment) (core.Transaction, error) {
	before, err := d.LoadLPoDCheckpoint()
	if err != nil {
		return core.Transaction{}, err
	}
	if before == nil {
		before = &LPoDCheckpoint{Positions: map[string]LPoDPosition{}}
	}
	// Recompute the pre-payment accrued state to recover each exact entitlement.
	base := before
	if base.Allocation == nil {
		base, err = d.appendLPoDMigration(new(leveldb.Batch), crypto.Hash32{}, height, r.Migration)
		if err != nil {
			return core.Transaction{}, err
		}
	}
	accrued, _, _, err := d.positionTransition(base, height, r)
	if err != nil {
		return core.Transaction{}, err
	}
	amounts := map[crypto.Address]uint64{}
	add := func(address crypto.Address, n uint64) error {
		var err error
		amounts[address], err = lpodAdd(amounts[address], n)
		return err
	}
	if r.DailyValidatorPayout {
		pendingPayouts, due, err := collectLPoDDailyValidatorPayouts(before, r, payments)
		if err != nil {
			return core.Transaction{}, err
		}
		expectedPending := pendingPayouts
		if due {
			expectedPending = nil
			for _, payout := range pendingPayouts {
				if err := add(payout.Address, payout.AmountNAPRO); err != nil {
					return core.Transaction{}, err
				}
			}
		}
		if !reflect.DeepEqual(expectedPending, after.PendingValidatorPayouts) {
			return core.Transaction{}, fmt.Errorf("lpod: deferred validator payout checkpoint mismatch")
		}
	} else {
		for _, p := range payments {
			if p.Leader > 0 {
				if p.VaultID != r.Proposer {
					return core.Transaction{}, fmt.Errorf("lpod: non-proposer leader reward")
				}
				if err := add(r.Leader, p.Leader); err != nil {
					return core.Transaction{}, err
				}
			}
		}
	}
	for id, p := range after.Positions {
		old := accrued.Positions[id]
		if p.Due > old.Due {
			return core.Transaction{}, fmt.Errorf("lpod: invalid payout entitlement")
		}
		n := old.Due - p.Due
		priorWithdrawn := before.Positions[id].Withdrawn
		if p.Withdrawn < priorWithdrawn {
			return core.Transaction{}, fmt.Errorf("lpod: principal return counter decreased")
		}
		if p.Withdrawn > priorWithdrawn {
			n, err = lpodAdd(n, p.Withdrawn-priorWithdrawn)
			if err != nil {
				return core.Transaction{}, err
			}
		}
		if err := add(p.Deposit.Beneficiary, n); err != nil {
			return core.Transaction{}, err
		}
	}
	if after.GenesisVesting != nil {
		if err := after.GenesisVesting.Validate(after.Allocation.Genesis, height); err != nil {
			return core.Transaction{}, err
		}
		if err := add(after.GenesisVesting.Recipient, after.GenesisVesting.paymentAt(height)); err != nil {
			return core.Transaction{}, err
		}
		if height == after.GenesisVesting.FundingHeight && after.GenesisVesting.LiquidityPaid > 0 {
			if err := add(after.GenesisVesting.LiquidityRecipient, after.GenesisVesting.LiquidityPaid); err != nil {
				return core.Transaction{}, err
			}
		}
	}
	addresses := make([]string, 0, len(amounts))
	for a, n := range amounts {
		if n > 0 {
			addresses = append(addresses, string(a))
		}
	}
	sort.Strings(addresses)
	auth, _ := json.Marshal(core.LPoDPayoutAuthorization{Leader: r.Leader, Digest: after.Digest()})
	tx := core.Transaction{Version: core.TxVersionLPoDPayout, Extra: auth}
	for _, a := range addresses {
		out, err := core.BuildLPoDPayoutOutput(crypto.Address(a), amounts[crypto.Address(a)], height, r.Parent, after.Digest())
		if err != nil {
			return core.Transaction{}, err
		}
		tx.Outputs = append(tx.Outputs, out)
	}
	return tx, nil
}
