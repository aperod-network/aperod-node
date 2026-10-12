package api

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

// Auxiliary report data, accumulated only after the existing block, payout,
// checkpoint and historical registry verification has succeeded.
type dailyRewardRecipient struct {
	Address string `json:"address"`
	Amount  uint64 `json:"amount_napro,string"`
	First   uint64 `json:"first_height"`
	Last    uint64 `json:"last_height"`
	Blocks  uint64 `json:"payout_blocks"`
}

type dailyGuardianTier struct {
	Tier   int    `json:"tier"`
	APR    uint64 `json:"apr_percent"`
	Amount uint64 `json:"amount_napro,string"`
}

type dailyRewardProgress struct {
	Validators map[string]dailyRewardRecipient
	Tiers      [9]dailyGuardianTier
	People     [9]map[string]bool
}

var dailyTierThresholds = [...]uint64{100000, 500000, 1000000, 5000000, 10000000, 30000000, 50000000, 80000000, 100000000}
var dailyTierAPRs = [...]uint64{3, 5, 6, 8, 9, 9, 9, 9, 10}

func newDailyRewardProgress() *dailyRewardProgress {
	p := &dailyRewardProgress{Validators: make(map[string]dailyRewardRecipient)}
	for i := range p.Tiers {
		p.Tiers[i] = dailyGuardianTier{Tier: i + 1, APR: dailyTierAPRs[i]}
		p.People[i] = make(map[string]bool)
	}
	return p
}

func (p *dailyRewardProgress) clone() *dailyRewardProgress {
	if p == nil {
		return nil
	}
	q := newDailyRewardProgress()
	q.Tiers = p.Tiers
	for address, row := range p.Validators {
		q.Validators[address] = row
	}
	for i, people := range p.People {
		for address, present := range people {
			q.People[i][address] = present
		}
	}
	return q
}

func (p *dailyRewardProgress) accumulate(block *core.Block, record *store.LPoDBlockAudit) error {
	var leader uint64
	tiers := make(map[string]int, len(record.Vaults))
	for _, v := range record.Vaults {
		if !sumDaily(&leader, v.ActualLeaderPaid) {
			return fmt.Errorf("validator reward overflow")
		}
		tier := -1
		if v.TierStake == 0 && v.GuardianPaid == 0 {
			continue
		}
		for i, threshold := range dailyTierThresholds {
			if v.TierStake >= threshold*100000000 {
				tier = i
			}
		}
		if tier < 0 || v.TierStake > 100000000*100000000 || v.APRPercent != dailyTierAPRs[tier] {
			return fmt.Errorf("guardian tier differs from verified stake")
		}
		tiers[v.ID] = tier
	}
	if leader > 0 {
		auth, err := block.Txs[0].LPoDPayoutAuthorization()
		if err != nil {
			return err
		}
		address := string(auth.Leader)
		row := p.Validators[address]
		if row.Blocks == 0 {
			row.Address, row.First = address, record.Height
		}
		if !sumDaily(&row.Amount, leader) {
			return fmt.Errorf("validator daily reward overflow")
		}
		row.Last, row.Blocks = record.Height, row.Blocks+1
		p.Validators[address] = row
	}
	for _, position := range record.Positions {
		if position.Accrued == 0 && position.Paid == 0 {
			continue
		}
		tier, ok := tiers[position.EffectiveVault]
		if !ok || position.Beneficiary == "" {
			return fmt.Errorf("guardian reward tier or beneficiary missing")
		}
		if !sumDaily(&p.Tiers[tier].Amount, position.Paid) {
			return fmt.Errorf("guardian daily reward overflow")
		}
		p.People[tier][position.Beneficiary] = true
	}
	return nil
}

func (p *dailyRewardProgress) result(opening, closing *store.LPoDCheckpoint) (map[string]interface{}, error) {
	if opening == nil || closing == nil || opening.Allocation == nil || closing.Allocation == nil ||
		closing.State.RewardInflow < opening.State.RewardInflow {
		return nil, fmt.Errorf("daily opening/closing funds unavailable")
	}
	validators := make([]dailyRewardRecipient, 0, len(p.Validators))
	for _, row := range p.Validators {
		validators = append(validators, row)
	}
	sort.Slice(validators, func(i, j int) bool { return validators[i].Address < validators[j].Address })
	tiers := make([]map[string]interface{}, 0, 9)
	for i, row := range p.Tiers {
		tiers = append(tiers, map[string]interface{}{
			"tier": i + 1, "apr_percent": row.APR, "people": len(p.People[i]),
			"amount_napro": strconv.FormatUint(row.Amount, 10),
		})
	}
	return map[string]interface{}{
		"version": 1, "validators": validators, "guardian_tiers": tiers,
		"validator_to_guardian_napro":   strconv.FormatUint(closing.State.RewardInflow-opening.State.RewardInflow, 10),
		"validator_remaining_napro":     strconv.FormatUint(closing.Allocation.ValidatorRemaining, 10),
		"guardian_reward_balance_napro": strconv.FormatUint(closing.State.Balance, 10),
		"guardian_principal_napro":      strconv.FormatUint(closing.PrincipalLocked, 10),
	}, nil
}
