package core

import (
	"math"
	"testing"
)

func TestVesting_ClockArithmeticCannotWrapUnlockBoundary(t *testing.T) {
	for _, tc := range []struct {
		name            string
		kind            VestingType
		genesis, now    int64
		cliff, duration int64
		want            uint64
	}{
		{"linear elapsed exceeds int64", VestingLinear, math.MinInt64, math.MaxInt64, 0, math.MaxInt64, 100},
		{"linear before genesis across extremes", VestingLinear, math.MaxInt64, math.MinInt64, 0, math.MaxInt64, 0},
		{"future cliff exceeds int64", VestingCliffLinear, math.MaxInt64 - 1, math.MaxInt64, 2, 10, 0},
		{"past cliff below int64 remains exact", VestingCliffLinear, math.MinInt64 + 1, math.MinInt64, -2, 2, 50},
		{"cliff elapsed exceeds int64", VestingCliffLinear, math.MinInt64, math.MaxInt64, 0, math.MaxInt64, 100},
		{"cliff before genesis across extremes", VestingCliffLinear, math.MaxInt64, math.MinInt64, 0, math.MaxInt64, 0},
		{"ordinary linear quarter", VestingLinear, 100, 125, 0, 100, 25},
		{"ordinary cliff quarter", VestingCliffLinear, 100, 175, 50, 100, 25},
		{"cliff exact boundary", VestingCliffLinear, 100, 150, 50, 100, 0},
		{"immediate invalid linear duration retained", VestingLinear, 100, 0, 0, 0, 100},
		{"nonpositive cliff duration still waits", VestingCliffLinear, 100, 149, 50, 0, 0},
		{"nonpositive cliff duration unlocks at cliff", VestingCliffLinear, 100, 150, 50, 0, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schedule := VestingSchedule{Type: tc.kind, CliffSeconds: tc.cliff, VestSeconds: tc.duration}
			if got := schedule.VestedAmount(100, tc.genesis, tc.now); got != tc.want {
				t.Fatalf("vested=%d, want %d", got, tc.want)
			}
			if got := schedule.LockedAmount(100, tc.genesis, tc.now); got != 100-tc.want {
				t.Fatalf("locked=%d, want %d", got, 100-tc.want)
			}
		})
	}
}
