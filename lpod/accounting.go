// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

// Package lpod contains the deterministic accounting component of a future
// coordinated LPoD fork. It does not authorize funding, deposits or payouts.
// In particular there is deliberately no initial-allocation/mint constructor.
package lpod

import (
	"fmt"
	"math"
	"math/big"
)

const InitialNAPRO uint64 = 100_000_000_000_000_000
const Unit uint64 = 100_000_000
const YearSeconds uint64 = 365 * 24 * 60 * 60
const denominator uint64 = 100 * YearSeconds

// Tier percentages mirror artifacts/landing/src/pages/vaults.tsx TIERS.
// No post-2088 rate change is inferred from marketing projections.
type Tier struct {
	Threshold, MinimumDeposit uint64
	LeaderPercent, APRPercent uint64
}

var tiers = [...]Tier{
	{100_000 * Unit, 100 * Unit, 8, 3},
	{500_000 * Unit, 1_000 * Unit, 10, 5},
	{1_000_000 * Unit, 10_000 * Unit, 12, 6},
	{5_000_000 * Unit, 20_000 * Unit, 15, 8},
	{10_000_000 * Unit, 50_000 * Unit, 16, 9},
	{30_000_000 * Unit, 80_000 * Unit, 16, 9},
	{50_000_000 * Unit, 100_000 * Unit, 16, 9},
	{80_000_000 * Unit, 150_000 * Unit, 16, 9},
	{100_000_000 * Unit, 200_000 * Unit, 20, 10},
}

func TierFor(total uint64) (Tier, error) {
	if total < tiers[0].Threshold || total > tiers[len(tiers)-1].Threshold {
		return Tier{}, fmt.Errorf("lpod: vault stake outside 100K–100M APRO")
	}
	for i := len(tiers)-1; i >= 0; i-- {
		if total >= tiers[i].Threshold { return tiers[i], nil }
	}
	panic("unreachable")
}

// All externally serialized monetary values are decimal strings. A State is an
// accounting checkpoint, NOT evidence of an authorized allocation or payment.
// FundingDebit may only originate from a future audited available-allocation
// ledger debit, never validator reserves, arbitrary wallets, or issuance.
type State struct {
	FundingDebit uint64 `json:"funding_debit_napro,string"`
	Balance uint64 `json:"balance_napro,string"`
	RewardInflow uint64 `json:"reward_inflow_napro,string"`
	LeaderPaid uint64 `json:"leader_paid_napro,string"`
	AngelPaid uint64 `json:"angel_paid_napro,string"`
	SurplusInflow uint64 `json:"surplus_inflow_napro,string"`
	DeficitOutflow uint64 `json:"deficit_outflow_napro,string"`
	UnfundedLiability uint64 `json:"unfunded_liability_napro,string"`
	AccruedLiability uint64 `json:"accrued_liability_napro,string"`
	LastHeight uint64 `json:"last_settled_height"`
}

// Carry belongs to a stable vault/principal accounting stream and must survive
// tier changes, restart and rollback. Do not reset it at a block or year boundary.
// A future depositor ledger must apportion funded AngelPaid to actual outputs.
type Carry struct {
	APR uint64 `json:"apr_remainder,string"`
	Leader uint64 `json:"leader_remainder,string"`
}

type Vault struct {
	ID string `json:"id"`
	TotalStake uint64 `json:"total_stake_napro,string"`
	GuardianStake uint64 `json:"guardian_stake_napro,string"`
	ActualIncome uint64 `json:"actual_income_napro,string"`
	ElapsedSeconds uint64 `json:"elapsed_seconds"`
ExactAccrual *uint64 `json:"exact_accrual_napro,omitempty"`
Arrears uint64 `json:"arrears_napro,string"`
}

type Payment struct {
	VaultID string `json:"vault_id"`
	Leader uint64 `json:"leader_napro,string"`
	Angels uint64 `json:"angels_napro,string"`
	Unfunded uint64 `json:"unfunded_napro,string"`
}

func add(a, b uint64) (uint64, error) {
	if math.MaxUint64-a < b { return 0, fmt.Errorf("lpod: uint64 overflow") }
	return a+b, nil
}

func rational(a, b, c, carry, divisor uint64) (uint64, uint64, error) {
	n := new(big.Int).SetUint64(a)
	n.Mul(n, new(big.Int).SetUint64(b))
	n.Mul(n, new(big.Int).SetUint64(c))
	n.Add(n, new(big.Int).SetUint64(carry))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, new(big.Int).SetUint64(divisor), r)
	if !q.IsUint64() { return 0, 0, fmt.Errorf("lpod: rational result overflow") }
	return q.Uint64(), r.Uint64(), nil
}

// Validate enforces conservation, not funding authority or canonical finality.
func (s State) Validate() error {
	if s.FundingDebit != InitialNAPRO { return fmt.Errorf("lpod: missing conserved 1B allocation debit") }
	left, err := add(s.FundingDebit, s.RewardInflow)
	if err != nil { return err }
	right, err := add(s.Balance, s.LeaderPaid)
	if err != nil { return err }
	right, err = add(right, s.AngelPaid)
	if err != nil { return err }
	if left != right { return fmt.Errorf("lpod: income conservation violated") }
	available, err := add(s.FundingDebit, s.SurplusInflow)
	if err != nil { return err }
	if available < s.DeficitOutflow || available-s.DeficitOutflow != s.Balance {
		return fmt.Errorf("lpod: reserve conservation violated")
	}
	liability, err := add(s.AngelPaid, s.UnfundedLiability)
	if err != nil { return err }
	if liability != s.AccruedLiability { return fmt.Errorf("lpod: liability conservation violated") }
	return nil
}

// Settle computes a complete block checkpoint without mutating any input.
// Vaults must be unique and sorted by canonical ID to fix scarce-reserve payout
// ordering. Unfunded amounts are explicit outstanding liabilities, not paid or
// silently minted; a later arrears-payment rule still requires a coordinated fork.
// This is a payout PLAN, never proof that payout outputs were actually committed.
func Settle(before State, carries map[string]Carry, height uint64, vaults []Vault) (State, map[string]Carry, []Payment, error) {
	fail := func(err error) (State, map[string]Carry, []Payment, error) { return before, nil, nil, err }
	if err := before.Validate(); err != nil { return fail(err) }
	if before.LastHeight == math.MaxUint64 || height != before.LastHeight+1 {
		return fail(fmt.Errorf("lpod: duplicate or nonsequential settlement"))
	}
	next := before
	out := make(map[string]Carry, len(carries))
	for id, c := range carries {
		if id == "" || c.APR >= denominator || c.Leader >= 100 { return fail(fmt.Errorf("lpod: invalid carry")) }
		out[id] = c
	}
	payments := make([]Payment, 0, len(vaults))
	for i, v := range vaults {
		if v.ID == "" || (i > 0 && vaults[i-1].ID >= v.ID) { return fail(fmt.Errorf("lpod: vault IDs must be sorted and unique")) }
		t, err := TierFor(v.TotalStake)
		if err != nil { return fail(err) }
		if v.GuardianStake > v.TotalStake { return fail(fmt.Errorf("lpod: guardian stake exceeds vault stake")) }
		c := out[v.ID]
		leader, rem, err := rational(v.ActualIncome, t.LeaderPercent, 1, c.Leader, 100)
		if err != nil { return fail(err) }
		c.Leader = rem
		due, rem, err := rational(v.GuardianStake, t.APRPercent, v.ElapsedSeconds, c.APR, denominator)
		if err != nil { return fail(err) }
newDue:=due
if v.ExactAccrual!=nil {newDue=*v.ExactAccrual;rem=c.APR}
if v.Arrears>next.UnfundedLiability {return fail(fmt.Errorf("lpod: arrears exceed outstanding liability"))}
next.UnfundedLiability-=v.Arrears
due,err=add(newDue,v.Arrears)
if err!=nil{return fail(err)}
		c.APR = rem
		residual := v.ActualIncome-leader
		paid, missing := due, uint64(0)
		surplus, deficit := uint64(0), uint64(0)
		if residual >= due {
			surplus = residual-due
			next.Balance, err = add(next.Balance, surplus)
			if err != nil { return fail(err) }
		} else {
			deficit = due-residual
			if deficit > next.Balance { deficit = next.Balance }
			next.Balance -= deficit
			paid = residual+deficit
			missing = due-paid
		}
		for _, inc := range []struct{ dst *uint64; amount uint64 }{
			{&next.RewardInflow, v.ActualIncome}, {&next.LeaderPaid, leader},
			{&next.AngelPaid, paid}, {&next.UnfundedLiability, missing},
{&next.AccruedLiability, newDue}, {&next.SurplusInflow, surplus},
			{&next.DeficitOutflow, deficit},
		} {
			*inc.dst, err = add(*inc.dst, inc.amount)
			if err != nil { return fail(err) }
		}
		out[v.ID] = c
		payments = append(payments, Payment{v.ID, leader, paid, missing})
	}
	next.LastHeight = height
	if err := next.Validate(); err != nil { return fail(err) }
	return next, out, payments, nil
}