package main

import (
	"fmt"
	"log/slog"
	"time"
)

// durationUntilRestartAt returns how long until the next occurrence of
// hour:minute UTC, given now.  The result is always in the range (0, 24h]:
// if now is exactly at hour:minute the next occurrence is 24 h from now.
func durationUntilRestartAt(now time.Time, hour, minute int) time.Duration {
	now = now.UTC()
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// runNightlyRestartScheduler sleeps until the configured restart window and
// then calls actionFn.  In production actionFn sends SIGTERM to os.Getpid();
// in tests it is replaced with a recorder so the test process is not killed.
//
// timerFn is time.After in production and a fake channel provider in tests,
// allowing the delay duration to be verified without wall-clock sleeps.
//
// The goroutine exits immediately if stop is closed while the timer is pending.
func runNightlyRestartScheduler(
	stop <-chan struct{},
	hour, minute int,
	nowFn func() time.Time,
	timerFn func(time.Duration) <-chan time.Time,
	actionFn func(),
	log *slog.Logger,
) {
	delay := durationUntilRestartAt(nowFn(), hour, minute)
	log.Info("nightly restart: scheduled",
		"restart_at", fmt.Sprintf("%02d:%02d UTC", hour, minute),
		"delay", delay.Round(time.Second).String(),
	)
	select {
	case <-stop:
		return
	case <-timerFn(delay):
	}
	// Re-check stop: if it fired concurrently with the timer we bail rather
	// than sending SIGTERM during a shutdown that is already in progress.
	select {
	case <-stop:
		return
	default:
	}
	log.Info("nightly restart: triggering graceful restart for RAM reclaim",
		"restart_at", fmt.Sprintf("%02d:%02d UTC", hour, minute),
	)
	actionFn()
}
