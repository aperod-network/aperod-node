package main

// nightly_restart_test.go — tests for the configurable nightly restart scheduler.
//
// Covers three requirements from task #2314:
//  1. durationUntilRestartAt computes the correct sleep duration for various
//     (now, restart_at) combinations, including the next-day wrap-around.
//  2. runNightlyRestartScheduler calls actionFn after the computed delay,
//     using an injectable timerFn so the test runs in milliseconds rather
//     than waiting until 04:00 UTC.
//  3. Closing stop before the timer fires suppresses actionFn (no spurious
//     restart during a concurrent graceful shutdown).

import (
	"testing"
	"time"

	"github.com/aperod/aperod/config"
)

// ─── durationUntilRestartAt ───────────────────────────────────────────────────

func TestDurationUntilRestartAt(t *testing.T) {
	tests := []struct {
		name         string
		now          time.Time
		hour, minute int
		wantMin      time.Duration
		wantMax      time.Duration
	}{
		{
			name:    "10 min before restart window",
			now:     time.Date(2026, 8, 14, 3, 50, 0, 0, time.UTC),
			hour:    4, minute: 0,
			wantMin: 9*time.Minute + 59*time.Second,
			wantMax: 10*time.Minute + 1*time.Second,
		},
		{
			name:    "exactly at restart time → next day (24h)",
			now:     time.Date(2026, 8, 14, 4, 0, 0, 0, time.UTC),
			hour:    4, minute: 0,
			wantMin: 23*time.Hour + 59*time.Minute + 59*time.Second,
			wantMax: 24 * time.Hour,
		},
		{
			name:    "1 s after restart time → ~24h from now",
			now:     time.Date(2026, 8, 14, 4, 0, 1, 0, time.UTC),
			hour:    4, minute: 0,
			wantMin: 23*time.Hour + 59*time.Minute,
			wantMax: 23*time.Hour + 59*time.Minute + 59*time.Second,
		},
		{
			name:    "midnight restart from 23:50",
			now:     time.Date(2026, 8, 14, 23, 50, 0, 0, time.UTC),
			hour:    0, minute: 0,
			wantMin: 9*time.Minute + 59*time.Second,
			wantMax: 10*time.Minute + 1*time.Second,
		},
		{
			name:    "14 h 30 min ahead",
			now:     time.Date(2026, 8, 14, 13, 30, 0, 0, time.UTC),
			hour:    4, minute: 0,
			wantMin: 14*time.Hour + 29*time.Minute,
			wantMax: 14*time.Hour + 31*time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := durationUntilRestartAt(tc.now, tc.hour, tc.minute)
			if got < tc.wantMin || got > tc.wantMax {
				t.Errorf("durationUntilRestartAt(%v, %d:%02d) = %v, want [%v, %v]",
					tc.now.Format("15:04:05"), tc.hour, tc.minute, got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// ─── ParseRestartAt (config package) ─────────────────────────────────────────

func TestParseRestartAt(t *testing.T) {
	valid := []struct {
		s    string
		h, m int
	}{
		{"04:00", 4, 0},
		{"00:00", 0, 0},
		{"23:59", 23, 59},
		{"12:30", 12, 30},
		{"0:05", 0, 5},
	}
	for _, tc := range valid {
		h, m, err := config.ParseRestartAt(tc.s)
		if err != nil {
			t.Errorf("ParseRestartAt(%q): unexpected error: %v", tc.s, err)
			continue
		}
		if h != tc.h || m != tc.m {
			t.Errorf("ParseRestartAt(%q) = (%d, %d), want (%d, %d)", tc.s, h, m, tc.h, tc.m)
		}
	}

	invalid := []string{
		"",
		"4",
		"4:0:0",
		"24:00",
		"23:60",
		"-1:00",
		"04:xx",
		"ab:cd",
		":00",
		"04:",
	}
	for _, s := range invalid {
		if _, _, err := config.ParseRestartAt(s); err == nil {
			t.Errorf("ParseRestartAt(%q): want error, got nil", s)
		}
	}
}

// ─── runNightlyRestartScheduler ───────────────────────────────────────────────

// TestNightlyRestartScheduler_FiresAtCorrectDelay verifies that the scheduler
// calls actionFn after the delay returned by durationUntilRestartAt.
//
// The fake clock is set to 03:50 UTC so the computed delay is ~10 minutes.
// timerFn captures the delay and fires immediately, so the test completes
// in milliseconds rather than waiting until 04:00 UTC.
func TestNightlyRestartScheduler_FiresAtCorrectDelay(t *testing.T) {
	fakeNow := time.Date(2026, 8, 14, 3, 50, 0, 0, time.UTC) // 10 min before 04:00
	nowFn := func() time.Time { return fakeNow }

	var capturedDelay time.Duration
	timerFn := func(d time.Duration) <-chan time.Time {
		capturedDelay = d
		ch := make(chan time.Time, 1)
		ch <- time.Now() // fire immediately
		return ch
	}

	stop := make(chan struct{})
	defer close(stop)
	actionFired := make(chan struct{}, 1)
	actionFn := func() { actionFired <- struct{}{} }

	go runNightlyRestartScheduler(stop, 4, 0, nowFn, timerFn, actionFn, discardLogger())

	select {
	case <-actionFired:
		// expected
	case <-time.After(time.Second):
		t.Fatal("actionFn was not called within 1 s")
	}

	// The delay captured by timerFn must be close to 10 minutes.
	const wantDelay = 10 * time.Minute
	const tolerance = time.Second
	if capturedDelay < wantDelay-tolerance || capturedDelay > wantDelay+tolerance {
		t.Errorf("timerFn received delay %v, want ~%v (±%v)", capturedDelay, wantDelay, tolerance)
	}
}

// TestNightlyRestartScheduler_StopCancels verifies that closing stop before
// the timer fires prevents actionFn from being called.  This models the case
// where an operator sends SIGTERM manually while the scheduler is waiting.
func TestNightlyRestartScheduler_StopCancels(t *testing.T) {
	fakeNow := time.Date(2026, 8, 14, 3, 50, 0, 0, time.UTC)
	nowFn := func() time.Time { return fakeNow }

	blockForever := make(chan time.Time) // never fires
	timerFn := func(d time.Duration) <-chan time.Time { return blockForever }

	stop := make(chan struct{})
	actionFired := false
	actionFn := func() { actionFired = true }

	done := make(chan struct{})
	go func() {
		defer close(done)
		runNightlyRestartScheduler(stop, 4, 0, nowFn, timerFn, actionFn, discardLogger())
	}()

	close(stop)
	select {
	case <-done:
		// expected
	case <-time.After(time.Second):
		t.Fatal("scheduler did not exit within 1 s of stop being closed")
	}
	if actionFired {
		t.Error("actionFn must not be called when stop fires before the timer")
	}
}

// TestNightlyRestartScheduler_RestartAtMidnight verifies that midnight (00:00)
// is scheduled correctly from 23:50 — the day boundary must not produce a
// negative or zero delay.
func TestNightlyRestartScheduler_RestartAtMidnight(t *testing.T) {
	fakeNow := time.Date(2026, 8, 14, 23, 50, 0, 0, time.UTC) // 10 min before midnight
	nowFn := func() time.Time { return fakeNow }

	var capturedDelay time.Duration
	timerFn := func(d time.Duration) <-chan time.Time {
		capturedDelay = d
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	stop := make(chan struct{})
	defer close(stop)
	actionFired := make(chan struct{}, 1)
	actionFn := func() { actionFired <- struct{}{} }

	go runNightlyRestartScheduler(stop, 0, 0, nowFn, timerFn, actionFn, discardLogger())

	select {
	case <-actionFired:
	case <-time.After(time.Second):
		t.Fatal("actionFn not called for midnight restart")
	}

	if capturedDelay <= 0 {
		t.Errorf("delay must be positive, got %v", capturedDelay)
	}
	const wantDelay = 10 * time.Minute
	const tolerance = time.Second
	if capturedDelay < wantDelay-tolerance || capturedDelay > wantDelay+tolerance {
		t.Errorf("delay = %v, want ~%v for midnight restart from 23:50", capturedDelay, wantDelay)
	}
}
