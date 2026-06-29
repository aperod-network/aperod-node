// Package core — vesting schedule for genesis allocations.
//
// Each GenesisAlloc may carry a VestingSchedule that controls how many of the
// allocated tokens are unlocked at any given point in time.  Three schemes are
// supported:
//
//   - immediate      — all tokens unlocked at genesis (no lock-up)
//   - linear         — tokens unlock gradually from genesis over VestSeconds
//   - cliff_linear   — tokens are fully locked for CliffSeconds, then unlock
//                      linearly over VestSeconds
//
// All timestamps are Unix seconds.  VestedAmount is monotone-increasing and
// always returns a value in [0, totalAmount].
package core

import "math/big"

// VestingType identifies the unlock curve for a genesis allocation.
type VestingType string

const (
	VestingImmediate   VestingType = "immediate"    // unlock everything at genesis
	VestingLinear      VestingType = "linear"       // linear from genesis, no cliff
	VestingCliffLinear VestingType = "cliff_linear" // cliff, then linear
)

// SecondsPerMonth is the canonical number of seconds in a 30-day month used
// for all vesting calculations.
const SecondsPerMonth = 30 * 86400 // 2,592,000

// VestingSchedule holds the vesting parameters for a single genesis allocation.
// All duration fields are in seconds.
type VestingSchedule struct {
	// Type selects the unlock curve.  An empty string is treated as "immediate".
	Type VestingType `yaml:"type" json:"type"`

	// CliffSeconds is the number of seconds after genesis before any tokens are
	// released.  Only meaningful for cliff_linear; ignored otherwise.
	CliffSeconds int64 `yaml:"cliff_seconds" json:"cliff_seconds"`

	// VestSeconds is the length of the linear-vesting window, starting after
	// the cliff (for cliff_linear) or from genesis (for linear).
	VestSeconds int64 `yaml:"vest_seconds" json:"vest_seconds"`
}

// VestedAmount returns the number of base-unit tokens that are unlocked at
// `now` (Unix seconds), given that genesis occurred at `genesisTime`.
//
// The calculation is performed in big.Int arithmetic to avoid overflow for
// large totalAmount values (uint64 × int64 can exceed uint64 range in the
// intermediate product).
func (v *VestingSchedule) VestedAmount(totalAmount uint64, genesisTime, now int64) uint64 {
	switch v.Type {
	case VestingImmediate, "":
		return totalAmount

	case VestingLinear:
		if v.VestSeconds <= 0 {
			return totalAmount
		}
		elapsed := now - genesisTime
		if elapsed <= 0 {
			return 0
		}
		if elapsed >= v.VestSeconds {
			return totalAmount
		}
		return mulDiv(totalAmount, uint64(elapsed), uint64(v.VestSeconds))

	case VestingCliffLinear:
		cliffEnd := genesisTime + v.CliffSeconds
		if now < cliffEnd {
			return 0
		}
		if v.VestSeconds <= 0 {
			return totalAmount
		}
		elapsed := now - cliffEnd
		if elapsed >= v.VestSeconds {
			return totalAmount
		}
		return mulDiv(totalAmount, uint64(elapsed), uint64(v.VestSeconds))

	default:
		return totalAmount
	}
}

// LockedAmount returns how many base-unit tokens are still locked at `now`.
func (v *VestingSchedule) LockedAmount(totalAmount uint64, genesisTime, now int64) uint64 {
	vested := v.VestedAmount(totalAmount, genesisTime, now)
	if vested >= totalAmount {
		return 0
	}
	return totalAmount - vested
}

// UnlockPercent returns the percentage [0.0, 100.0] of tokens unlocked at `now`.
func (v *VestingSchedule) UnlockPercent(totalAmount uint64, genesisTime, now int64) float64 {
	if totalAmount == 0 {
		return 100.0
	}
	vested := v.VestedAmount(totalAmount, genesisTime, now)
	return float64(vested) / float64(totalAmount) * 100.0
}

// CliffAt returns the Unix timestamp when tokens first begin to unlock.
// For immediate and linear schedules this equals genesisTime.
func (v *VestingSchedule) CliffAt(genesisTime int64) int64 {
	if v.Type == VestingCliffLinear {
		return genesisTime + v.CliffSeconds
	}
	return genesisTime
}

// FullUnlockAt returns the Unix timestamp when all tokens become unlocked.
// Returns genesisTime for immediate vesting.
func (v *VestingSchedule) FullUnlockAt(genesisTime int64) int64 {
	switch v.Type {
	case VestingImmediate, "":
		return genesisTime
	case VestingLinear:
		return genesisTime + v.VestSeconds
	case VestingCliffLinear:
		return genesisTime + v.CliffSeconds + v.VestSeconds
	default:
		return genesisTime
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────────

// mulDiv computes floor(a * b / c) using big.Int to avoid overflow.
func mulDiv(a, b, c uint64) uint64 {
	ab := new(big.Int).Mul(new(big.Int).SetUint64(a), new(big.Int).SetUint64(b))
	result := new(big.Int).Div(ab, new(big.Int).SetUint64(c))
	if !result.IsUint64() {
		return a // saturate — should not happen in practice
	}
	return result.Uint64()
}
