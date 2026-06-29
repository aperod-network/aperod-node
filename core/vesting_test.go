package core

import (
	"testing"
	"time"
)

func TestVesting_Immediate(t *testing.T) {
	v := &VestingSchedule{Type: VestingImmediate}
	total := uint64(1_000_000_000_000)
	genesis := int64(1_700_000_000)
	now := genesis + 10

	if got := v.VestedAmount(total, genesis, now); got != total {
		t.Errorf("immediate: expected %d, got %d", total, got)
	}
	if got := v.LockedAmount(total, genesis, now); got != 0 {
		t.Errorf("immediate: locked should be 0, got %d", got)
	}
	if got := v.UnlockPercent(total, genesis, now); got != 100.0 {
		t.Errorf("immediate: unlock%% should be 100, got %f", got)
	}
}

func TestVesting_EmptyTypeIsImmediate(t *testing.T) {
	v := &VestingSchedule{} // zero value → immediate
	total := uint64(500_000_000_000)
	genesis := int64(1_700_000_000)
	if got := v.VestedAmount(total, genesis, genesis-1); got != total {
		t.Errorf("empty type: expected total even before genesis, got %d", got)
	}
}

func TestVesting_Linear(t *testing.T) {
	const month = int64(SecondsPerMonth)
	v := &VestingSchedule{Type: VestingLinear, VestSeconds: 12 * month}
	total := uint64(1_000_000_000_000) // 10 000 APR
	genesis := int64(1_700_000_000)

	tests := []struct {
		elapsed  int64
		wantMin  uint64
		wantMax  uint64
	}{
		{0, 0, 0},
		{6 * month, total/2 - 1, total/2 + 1}, // ~50%
		{12 * month, total, total},
		{24 * month, total, total},
	}
	for _, tc := range tests {
		got := v.VestedAmount(total, genesis, genesis+tc.elapsed)
		if got < tc.wantMin || got > tc.wantMax {
			t.Errorf("linear elapsed=%d: got %d, want [%d, %d]", tc.elapsed, got, tc.wantMin, tc.wantMax)
		}
	}

	// locked + vested == total at midpoint
	mid := genesis + 6*month
	vested := v.VestedAmount(total, genesis, mid)
	locked := v.LockedAmount(total, genesis, mid)
	if vested+locked != total {
		t.Errorf("linear: vested(%d)+locked(%d) != total(%d)", vested, locked, total)
	}
}

func TestVesting_CliffLinear(t *testing.T) {
	const month = int64(SecondsPerMonth)
	cliff := 12 * month
	vest := 24 * month
	v := &VestingSchedule{Type: VestingCliffLinear, CliffSeconds: cliff, VestSeconds: vest}
	total := uint64(4_200_000_000_000_000) // 42 M APR worth in base units
	genesis := int64(1_700_000_000)

	// Before cliff: nothing unlocked
	if got := v.VestedAmount(total, genesis, genesis+cliff-1); got != 0 {
		t.Errorf("before cliff: expected 0, got %d", got)
	}
	// At cliff: 0 (cliff just ended, linear not started)
	if got := v.VestedAmount(total, genesis, genesis+cliff); got != 0 {
		t.Errorf("at cliff start: expected 0, got %d", got)
	}
	// Midpoint of vesting (cliff + 12 months): ~50%
	mid := genesis + cliff + vest/2
	vested := v.VestedAmount(total, genesis, mid)
	halfTotal := total / 2
	delta := int64(vested) - int64(halfTotal)
	if delta < 0 {
		delta = -delta
	}
	if delta > int64(halfTotal/1000) { // within 0.1%
		t.Errorf("cliff_linear midpoint: got %d, want ~%d (delta %d)", vested, halfTotal, delta)
	}
	// After full vest
	if got := v.VestedAmount(total, genesis, genesis+cliff+vest+1); got != total {
		t.Errorf("after full vest: expected %d, got %d", total, got)
	}
}

func TestVesting_CliffAt_FullUnlockAt(t *testing.T) {
	const month = int64(SecondsPerMonth)
	genesis := int64(1_700_000_000)

	imm := &VestingSchedule{Type: VestingImmediate}
	if imm.CliffAt(genesis) != genesis {
		t.Error("immediate CliffAt should equal genesis")
	}
	if imm.FullUnlockAt(genesis) != genesis {
		t.Error("immediate FullUnlockAt should equal genesis")
	}

	lin := &VestingSchedule{Type: VestingLinear, VestSeconds: 12 * month}
	if lin.CliffAt(genesis) != genesis {
		t.Error("linear CliffAt should equal genesis")
	}
	if lin.FullUnlockAt(genesis) != genesis+12*month {
		t.Errorf("linear FullUnlockAt: got %d, want %d", lin.FullUnlockAt(genesis), genesis+12*month)
	}

	cl := &VestingSchedule{Type: VestingCliffLinear, CliffSeconds: 6 * month, VestSeconds: 18 * month}
	if cl.CliffAt(genesis) != genesis+6*month {
		t.Errorf("cliff_linear CliffAt: got %d, want %d", cl.CliffAt(genesis), genesis+6*month)
	}
	if cl.FullUnlockAt(genesis) != genesis+24*month {
		t.Errorf("cliff_linear FullUnlockAt: got %d, want %d", cl.FullUnlockAt(genesis), genesis+24*month)
	}
}

func TestVesting_MonotoneIncreasing(t *testing.T) {
	const month = int64(SecondsPerMonth)
	v := &VestingSchedule{Type: VestingCliffLinear, CliffSeconds: 6 * month, VestSeconds: 24 * month}
	total := uint64(3_150_000_000_000_000)
	genesis := int64(1_700_000_000)

	var prev uint64
	for _, d := range []int64{-1, 0, 3 * month, 6 * month, 12 * month, 18 * month, 30 * month, 36 * month, 48 * month} {
		cur := v.VestedAmount(total, genesis, genesis+d)
		if cur < prev {
			t.Errorf("not monotone: at %v months vested=%d < prev=%d", time.Duration(d)*time.Second, cur, prev)
		}
		prev = cur
	}
}

func TestVesting_UnlockPercent(t *testing.T) {
	v := &VestingSchedule{Type: VestingLinear, VestSeconds: 12 * int64(SecondsPerMonth)}
	total := uint64(1_000_000_000_000)
	genesis := int64(1_700_000_000)

	pct := v.UnlockPercent(total, genesis, genesis+6*int64(SecondsPerMonth))
	if pct < 49.0 || pct > 51.0 {
		t.Errorf("expected ~50%%, got %f", pct)
	}
	if v.UnlockPercent(0, genesis, genesis+1) != 100.0 {
		t.Error("zero total should return 100%%")
	}
}
