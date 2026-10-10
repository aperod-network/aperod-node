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

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/syndtr/goleveldb/leveldb"
)

const LPoDMaxPositions = 4096
const LPoDMaxElapsedNS uint64 = 15_000_000_000
const lpodAPRDenominator uint64 = 100 * lpod.YearSeconds * 1_000_000_000

type LPoDValidatorStake struct {
	Amount uint64
	Active bool
}
type LPoDPosition struct {
	Deposit      core.LPoDPositionAction `json:"deposit"`
	Nonce        uint64                  `json:"nonce"`
	UnlockHeight uint64                  `json:"unlock_height"` // full exit's canonical height; not a guardian timelock
	Returned     bool                    `json:"returned"`
	Due          uint64                  `json:"due_napro,string"`
	APRCarry     uint64                  `json:"apr_carry,string"`
	Withdrawn    uint64                  `json:"withdrawn_napro,string"`
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
	if p.Returned || p.UnlockHeight != 0 || p.Withdrawn >= p.Deposit.Amount || p.Nonce == ^uint64(0) {
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
		amount = p.Deposit.Amount - p.Withdrawn
	}
	if amount > p.Deposit.Amount-p.Withdrawn {
		return 0, fmt.Errorf("lpod: withdrawal exceeds remaining principal")
	}
	return amount, nil
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

func (c *LPoDCheckpoint) validatePositions() error {
	var locked, due uint64
	if len(c.Positions) > LPoDMaxPositions {
		return fmt.Errorf("lpod: position capacity exceeded")
	}
	for id, p := range c.Positions {
		if id != lpodPositionID(p.Deposit) || p.Deposit.Action != core.LPoDDeposit || p.Deposit.Nonce != 0 ||
			p.Deposit.WithdrawAmount != 0 || p.Deposit.Amount == 0 || p.Withdrawn > p.Deposit.Amount ||
			p.Returned != (p.Withdrawn == p.Deposit.Amount) ||
			(!p.AutoReturned && (p.Nonce == 0) != (p.Withdrawn == 0)) ||
			(p.AutoReturned && !p.Returned) ||
			p.APRCarry >= lpodAPRDenominator || (p.UnlockHeight == 0) != (!p.Returned) ||
			p.UnlockHeight > c.State.LastHeight {
			return fmt.Errorf("lpod: corrupt canonical position")
		}
		if p.EffectiveVault != nil && (*p.EffectiveVault == p.Deposit.Vault || p.RouteHeight == 0 || p.RouteHeight > c.State.LastHeight) {
			return fmt.Errorf("lpod: corrupt effective vault route")
		}
		var err error
		if !p.Returned {
			locked, err = lpodAdd(locked, p.Deposit.Amount-p.Withdrawn)
			if err != nil {
				return err
			}
		}
		due, err = lpodAdd(due, p.Due)
		if err != nil {
			return err
		}
	}
	total, err := lpodAdd(c.PrincipalLocked, c.PrincipalReturned)
	if err != nil || total != c.PrincipalDeposited || locked != c.PrincipalLocked ||
		(c.Allocation != nil && due != c.State.UnfundedLiability) {
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
	parent, err := d.readLPoDCanonicalBlock(height - 1)
	if err != nil {
		return nil, nil, nil, err
	}
	if parent.Hash() != r.Parent || r.Timestamp <= parent.Header.Timestamp {
		return nil, nil, nil, fmt.Errorf("lpod: nonmonotonic or noncanonical accrual clock")
	}
	elapsed := uint64(r.Timestamp - parent.Header.Timestamp)
	if elapsed > LPoDMaxElapsedNS {
		elapsed = LPoDMaxElapsedNS
	}
	next := *before
	next.Positions = make(map[string]LPoDPosition, len(before.Positions))
	vaultIDs := map[string]bool{r.Proposer: true}
	totals := map[string]uint64{}
	entitlementVault := map[string]string{}
	for id, p := range before.Positions {
		// Fully closed positions no longer occupy capacity. Reopening their
		// deposit still requires its already-spent, cryptographically linked KI.
		if p.Returned && p.Due == 0 {
			continue
		}
		next.Positions[id] = p
		v := lpodPositionVaultID(p)
		entitlementVault[id] = v
		vaultIDs[v] = true
		if p.UnlockHeight == 0 {
			totals[v], err = lpodAdd(totals[v], p.Deposit.Amount-p.Withdrawn)
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}
	accrual := map[string]uint64{}
	arrears := map[string]uint64{}
	for id, p := range next.Positions {
		v := lpodPositionVaultID(p)
		arrears[v], err = lpodAdd(arrears[v], p.Due)
		if err != nil {
			return nil, nil, nil, err
		}
		stake := r.Stake[v]
		if prior := r.PreviousStake[v]; !stake.Active && prior.Active {
			stake = prior
		}
		if p.UnlockHeight != 0 || !stake.Active {
			continue
		}
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
		n := new(big.Int).SetUint64(p.Deposit.Amount - p.Withdrawn)
		n.Mul(n, new(big.Int).SetUint64(tier.APRPercent))
		n.Mul(n, new(big.Int).SetUint64(elapsed))
		n.Add(n, new(big.Int).SetUint64(p.APRCarry))
		q, rem := new(big.Int), new(big.Int)
		q.QuoRem(n, new(big.Int).SetUint64(lpodAPRDenominator), rem)
		if !q.IsUint64() {
			return nil, nil, nil, fmt.Errorf("lpod: accrued liability overflow")
		}
		p.APRCarry = rem.Uint64()
		p.Due, err = lpodAdd(p.Due, q.Uint64())
		if err != nil {
			return nil, nil, nil, err
		}
		accrual[v], err = lpodAdd(accrual[v], q.Uint64())
		if err != nil {
			return nil, nil, nil, err
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
			next.Positions[id] = LPoDPosition{Deposit: *a}
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
			p.Returned = p.Withdrawn == p.Deposit.Amount
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
		remaining := p.Deposit.Amount - p.Withdrawn
		best, err := lpodRouteDestination(source, sourceBefore, remaining, r.Stake, totals, eligible)
		if err != nil {
			return nil, nil, nil, err
		}
		totals[source] -= remaining
		if best == "" {
			p.Withdrawn = p.Deposit.Amount
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
			priorTotals[v], err = lpodAdd(priorTotals[v], p.Deposit.Amount-p.Withdrawn)
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

func (d *DB) previewPositions(before *LPoDCheckpoint, height uint64, r *LPoDSettlement) (*LPoDCheckpoint, []lpod.Payment, error) {
	next, vaults, entitlementVault, err := d.positionTransition(before, height, r)
	if err != nil {
		return nil, nil, err
	}
	state, carries, payments, err := lpod.Settle(before.State, before.Carries, height, vaults)
	if err != nil {
		return nil, nil, err
	}
	next.State, next.Carries = state, carries
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
	if err := next.validatePositions(); err != nil {
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
