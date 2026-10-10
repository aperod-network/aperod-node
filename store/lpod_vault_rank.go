// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"sort"

	"github.com/aperod/aperod/lpod"
)

const lpodMaxVaultNAPRO uint64 = 100_000_000 * lpod.Unit

// LPoDVaultRank is the deterministic public LPoD top-21 view. It is deliberately
// separate from the validator signing/quorum set: ranking a vault never grants
// block-signing authority.
type LPoDVaultRank struct {
	ID             string `json:"id"`
	ValidatorStake uint64 `json:"validator_stake_napro,string"`
	GuardianStake  uint64 `json:"guardian_stake_napro,string"`
	TotalStake     uint64 `json:"total_stake_napro,string"`
}

// LPoDEligibleVaults ranks canonically active, protocol-minimum validators by
// combined validator and effective Guardian stake. Equal totals use validator
// public-key bytes (their lowercase hex ID) as the deterministic tie-break.
func LPoDEligibleVaults(stakes map[string]LPoDValidatorStake, guardians map[string]uint64) []LPoDVaultRank {
	out := make([]LPoDVaultRank, 0, len(stakes))
	for id, stake := range stakes {
		if id == "" || !stake.Active || stake.Amount < 100_000*lpod.Unit {
			continue
		}
		total, err := lpodAdd(stake.Amount, guardians[id])
		if err != nil {
			continue
		}
		out = append(out, LPoDVaultRank{ID: id, ValidatorStake: stake.Amount, GuardianStake: guardians[id], TotalStake: total})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalStake != out[j].TotalStake {
			return out[i].TotalStake > out[j].TotalStake
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > 21 {
		out = out[:21]
	}
	return out
}

func lpodRouteDestination(source string, sourceBefore, principal uint64, stakes map[string]LPoDValidatorStake, totals map[string]uint64, eligible map[string]bool) (string, error) {
	ids := make([]string, 0, len(stakes))
	for id := range stakes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	best := ""
	var bestTotal uint64
	for _, id := range ids {
		stake := stakes[id]
		if id == source || !eligible[id] || !stake.Active || stake.Amount < 100_000*lpod.Unit {
			continue
		}
		total, err := lpodAdd(stake.Amount, totals[id])
		if err != nil {
			return "", err
		}
		if total > sourceBefore || total > lpodMaxVaultNAPRO || principal > lpodMaxVaultNAPRO-total {
			continue
		}
		if best == "" || total > bestTotal {
			best, bestTotal = id, total
		}
	}
	return best, nil
}

// LPoDEffectiveGuardianTotals builds the API/storage view by current effective
// vault. Closed principal is excluded; original signed deposit identities remain
// available in Positions for audit and owner-authorized withdrawals.
func (c *LPoDCheckpoint) LPoDEffectiveGuardianTotals() (map[string]uint64, error) {
	totals := make(map[string]uint64)
	if c == nil {
		return totals, nil
	}
	for _, p := range c.Positions {
		if p.Returned {
			continue
		}
		id := lpodPositionVaultID(p)
		n, err := lpodAdd(totals[id], p.Deposit.Amount-p.Withdrawn)
		if err != nil {
			return nil, err
		}
		totals[id] = n
	}
	return totals, nil
}
