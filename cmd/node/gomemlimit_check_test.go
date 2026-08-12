package main

// gomemlimit_check_test.go — coverage for checkGOMLEMLIMIT's startup memory-limit
// guard (queue task #1710).
//
// The behaviour under test: when neither the GOMEMLIMIT environment variable nor
// node.yaml's memory_limit_bytes is set, the node warns operators that it may OOM
// under load.  Operators who intentionally run uncapped can set
// memory_limit_disabled: true in node.yaml to silence the per-startup WARN;
// the guard then downgrades the message to DEBUG.
//
// This test reuses the captureHandler defined in startup_integrity_test.go
// (same package) to assert on the emitted record's level.

import (
	"log/slog"
	"testing"
)

const gomemlimitDropinPath = "/etc/systemd/system/aperod-node.service.d/gomemlimit.conf"

// TestCheckGOMEMLIMIT_UnsetNotDisabled_LogsWARN asserts that when the limit is
// unset and memory_limit_disabled is false, the guard emits the warning at WARN.
func TestCheckGOMEMLIMIT_UnsetNotDisabled_LogsWARN(t *testing.T) {
	cap := &captureHandler{}
	log := slog.New(cap)

	err := checkGOMLEMLIMIT(
		"",    // gomlimitEnv: unset
		false, // configLimitApplied: no memory_limit_bytes
		false, // strictMode
		false, // memLimitDisabled
		gomemlimitDropinPath,
		log,
	)
	if err != nil {
		t.Fatalf("checkGOMLEMLIMIT returned error in non-strict mode: %v", err)
	}

	rec, ok := cap.find("GOMEMLIMIT is not set")
	if !ok {
		t.Fatalf("expected a GOMEMLIMIT warning record, got none")
	}
	if rec.Level != slog.LevelWarn {
		t.Fatalf("expected warning at WARN level, got %v", rec.Level)
	}
}

// TestCheckGOMEMLIMIT_Disabled_SuppressesWARN asserts that when the operator has
// set memory_limit_disabled: true, the warning is downgraded to DEBUG so the
// journal is not spammed with a WARN on every startup — no WARN record is
// emitted.
func TestCheckGOMEMLIMIT_Disabled_SuppressesWARN(t *testing.T) {
	cap := &captureHandler{}
	log := slog.New(cap)

	err := checkGOMLEMLIMIT(
		"",    // gomlimitEnv: unset
		false, // configLimitApplied: no memory_limit_bytes
		false, // strictMode
		true,  // memLimitDisabled: operator opted out
		gomemlimitDropinPath,
		log,
	)
	if err != nil {
		t.Fatalf("checkGOMLEMLIMIT returned error: %v", err)
	}

	rec, ok := cap.find("GOMEMLIMIT is not set")
	if !ok {
		t.Fatalf("expected a GOMEMLIMIT record at DEBUG, got none")
	}
	if rec.Level == slog.LevelWarn {
		t.Fatalf("expected warning to be suppressed (not WARN) when memory_limit_disabled=true, got WARN")
	}
	if rec.Level != slog.LevelDebug {
		t.Fatalf("expected message at DEBUG level, got %v", rec.Level)
	}
}
