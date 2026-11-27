// Package core — vesting schedule for genesis allocations.
//
// Each GenesisAlloc may carry a VestingSchedule that controls how many of the
// allocated tokens are unlocked at any given point in time.  Three schemes are
// supported:
//
//   - immediate      — all tokens unlocked at genesis (no lock-up)
//   - linear         — tokens unlock gradually from genesis over VestSeconds
//   - cliff_linear   — tokens are fully locked for CliffSeconds, then unlock
//     linearly over VestSeconds
//
// All timestamps are Unix seconds.  VestedAmount is monotone-increasing and
// always returns a value in [0, totalAmount].
package core

import "math/big"

// VestingType identifies the unlock curve for a genesis allocation.
type VestingType string

// Vesting types select immediate, linear or cliff-then-linear unlocking.
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
		elapsed := new(big.Int).Sub(big.NewInt(now), big.NewInt(genesisTime))
		return linearVestedAfterElapsed(totalAmount, elapsed, v.VestSeconds)

	case VestingCliffLinear:
		// Clock arithmetic needs the same overflow protection as the amount:
		// a wrapped cliff must not turn a future unlock into an early payout.
		cliffEnd := new(big.Int).Add(big.NewInt(genesisTime), big.NewInt(v.CliffSeconds))
		elapsed := new(big.Int).Sub(big.NewInt(now), cliffEnd)
		if elapsed.Sign() < 0 {
			return 0
		}
		if v.VestSeconds <= 0 {
			return totalAmount
		}
		return linearVestedAfterElapsed(totalAmount, elapsed, v.VestSeconds)

	default:
		// F-17 fix: unknown vesting type → fail-closed (0 tokens unlocked).
		// Previously this returned totalAmount (full immediate unlock), allowing
		// a typo or unknown future type to bypass all vesting restrictions.
		return 0
	}
}

// linearVestedAfterElapsed is called only for a strictly positive duration.
// Keep both clock differences and the multiplication exact before narrowing a
// result already bounded by totalAmount.
func linearVestedAfterElapsed(totalAmount uint64, elapsed *big.Int, duration int64) uint64 {
	if elapsed.Sign() <= 0 {
		return 0
	}
	window := big.NewInt(duration)
	if elapsed.Cmp(window) >= 0 {
		return totalAmount
	}
	unlocked := new(big.Int).Mul(new(big.Int).SetUint64(totalAmount), elapsed)
	return unlocked.Div(unlocked, window).Uint64()
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
		// F-17 fix: unknown type → never unlocks (max int64 timestamp).
		return 1<<62 - 1
	}
}
