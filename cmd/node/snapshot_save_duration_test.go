package main

// snapshot_save_duration_test.go — unit tests for checkStartupSnapshotTiming.
//
// checkStartupSnapshotTiming reads the effective TimeoutStopSec via
// readEffectiveTimeoutStopSec and then emits:
//   - no log record   when ratio  ≤ 50 %
//   - Warn            when ratio  > 50 % and ≤ 80 %
//   - Error           when ratio  > 80 %
//
// The test drives the function using a real temp-dir drop-in conf file so that
// readEffectiveTimeoutStopSec actually returns the value we planted, avoiding
// any dependency on the host's systemd configuration.
//
// captureHandler is defined in startup_integrity_test.go (same package).

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// writeDropinConf writes a minimal systemd drop-in file containing
// TimeoutStopSec=<secs> into dir/timeout.conf.
func writeDropinConf(t *testing.T, dir string, secs int) {
	t.Helper()
	content := fmt.Sprintf("[Service]\nTimeoutStopSec=%d\n", secs)
	if err := os.WriteFile(filepath.Join(dir, "timeout.conf"), []byte(content), 0644); err != nil {
		t.Fatalf("write drop-in conf: %v", err)
	}
}

// TestCheckStartupSnapshotTiming_BelowWarnThreshold verifies that no log record
// is emitted when the snapshot save duration is well below 50 % of the timeout.
// (50 000 ms / (300 s * 1000) = 16.7 %)
func TestCheckStartupSnapshotTiming_BelowWarnThreshold(t *testing.T) {
	dropinDir := t.TempDir()
	writeDropinConf(t, dropinDir, 300) // 300 s timeout

	cap := &captureHandler{}
	log := slog.New(cap)

	checkStartupSnapshotTiming(50_000, dropinDir, "", log) // 50 s → 16.7 %

	if _, found := cap.find("snapshot save duration"); found {
		t.Error("expected no snapshot-timing log record below 50 % threshold, but got one")
	}
}

// TestCheckStartupSnapshotTiming_AboveWarnThreshold verifies that a Warn record
// is emitted when ratio is > 50 % but ≤ 80 %.
// (180 000 ms / (300 s * 1000) = 60 %)
func TestCheckStartupSnapshotTiming_AboveWarnThreshold(t *testing.T) {
	dropinDir := t.TempDir()
	writeDropinConf(t, dropinDir, 300)

	cap := &captureHandler{}
	log := slog.New(cap)

	checkStartupSnapshotTiming(180_000, dropinDir, "", log) // 180 s → 60 %

	rec, found := cap.find("snapshot save duration")
	if !found {
		t.Fatal("expected a Warn record at 60 % ratio, got none")
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("expected Warn level at 60 %% ratio, got %v", rec.Level)
	}
}

// TestCheckStartupSnapshotTiming_AboveErrorThreshold verifies that an Error
// record is emitted when ratio is > 80 %.
// (270 000 ms / (300 s * 1000) = 90 %)
func TestCheckStartupSnapshotTiming_AboveErrorThreshold(t *testing.T) {
	dropinDir := t.TempDir()
	writeDropinConf(t, dropinDir, 300)

	cap := &captureHandler{}
	log := slog.New(cap)

	checkStartupSnapshotTiming(270_000, dropinDir, "", log) // 270 s → 90 %

	rec, found := cap.find("snapshot save duration")
	if !found {
		t.Fatal("expected an Error record at 90 % ratio, got none")
	}
	if rec.Level != slog.LevelError {
		t.Errorf("expected Error level at 90 %% ratio, got %v", rec.Level)
	}
}

// TestCheckStartupSnapshotTiming_ExactlyAtWarnBoundary verifies the threshold
// is strictly greater-than, so a ratio of exactly 50 % produces no record.
// (150 000 ms / (300 s * 1000) = 50.0 % exactly → no-op)
func TestCheckStartupSnapshotTiming_ExactlyAtWarnBoundary(t *testing.T) {
	dropinDir := t.TempDir()
	writeDropinConf(t, dropinDir, 300)

	cap := &captureHandler{}
	log := slog.New(cap)

	checkStartupSnapshotTiming(150_000, dropinDir, "", log) // exactly 50 %

	if _, found := cap.find("snapshot save duration"); found {
		t.Error("expected no log record at exactly 50 % (threshold is strictly >), but got one")
	}
}

// TestCheckStartupSnapshotTiming_NoTimeoutConfigured verifies that the function
// is a no-op when readEffectiveTimeoutStopSec cannot find a configured timeout
// (empty drop-in dir, no service file) — we must not emit spurious alerts on
// non-systemd hosts or fresh installs.
func TestCheckStartupSnapshotTiming_NoTimeoutConfigured(t *testing.T) {
	emptyDir := t.TempDir() // no .conf file written

	cap := &captureHandler{}
	log := slog.New(cap)

	// Even with an enormous saved duration, without a timeout the function
	// should be silent.
	checkStartupSnapshotTiming(999_999_999, emptyDir, "", log)

	if _, found := cap.find("snapshot"); found {
		t.Error("expected no log record when timeout is unconfigured, but got one")
	}
}
