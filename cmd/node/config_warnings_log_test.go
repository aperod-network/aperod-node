package main

// config_warnings_log_test.go — confirms that emitConfigWarnings() bridges
// Config.Warnings() to the structured logger at WARN level.
//
// Why this matters: Config.Warnings() returns a slice of strings; a separate
// loop in main.go (now emitConfigWarnings) fans those strings out as slog
// records.  If that loop were accidentally removed or the log level changed,
// operators would receive no warning at startup even though the underlying
// Warnings() method still returns the right strings.  This test locks the
// full path — config method → logger call → WARN record — in one assertion.
//
// The test reuses the captureHandler defined in startup_integrity_test.go
// (same package) to intercept slog records without any I/O.

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/aperod/aperod/config"
)

// TestEmitConfigWarnings_UnsafeBanThreshold confirms that a
// bad_block_ban_threshold above the safe limit produces a WARN-level slog
// record whose message is "config warning" and whose "msg" attribute contains
// "bad_block_ban_threshold".
func TestEmitConfigWarnings_UnsafeBanThreshold(t *testing.T) {
	cap := &captureHandler{}
	log := slog.New(cap)

	cfg := config.DefaultConfig()
	cfg.P2P.BadBlockBanThreshold = 200 // well above safeBanThreshold (50)

	emitConfigWarnings(log, cfg)

	rec, ok := cap.find("config warning")
	if !ok {
		t.Fatal("expected a 'config warning' log record but got none")
	}
	if rec.Level != slog.LevelWarn {
		t.Fatalf("expected log record at WARN level, got %v", rec.Level)
	}

	// The "msg" attribute must contain the field name so operators can act.
	var msgAttr string
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "msg" {
			msgAttr = a.Value.String()
			return false
		}
		return true
	})
	if !strings.Contains(msgAttr, "bad_block_ban_threshold") {
		t.Errorf("expected 'msg' attr to contain %q, got %q",
			"bad_block_ban_threshold", msgAttr)
	}
}

// TestEmitConfigWarnings_UnsafeHeightLead confirms that a
// bad_block_height_lead above the safe limit produces a WARN-level slog
// record whose "msg" attribute contains "bad_block_height_lead".
func TestEmitConfigWarnings_UnsafeHeightLead(t *testing.T) {
	cap := &captureHandler{}
	log := slog.New(cap)

	cfg := config.DefaultConfig()
	cfg.P2P.BadBlockHeightLead = 50_000 // well above safeHeightLead (10 000)

	emitConfigWarnings(log, cfg)

	rec, ok := cap.find("config warning")
	if !ok {
		t.Fatal("expected a 'config warning' log record but got none")
	}
	if rec.Level != slog.LevelWarn {
		t.Fatalf("expected log record at WARN level, got %v", rec.Level)
	}

	var msgAttr string
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "msg" {
			msgAttr = a.Value.String()
			return false
		}
		return true
	})
	if !strings.Contains(msgAttr, "bad_block_height_lead") {
		t.Errorf("expected 'msg' attr to contain %q, got %q",
			"bad_block_height_lead", msgAttr)
	}
}

// TestEmitConfigWarnings_BothUnsafe confirms that when both fields exceed their
// safe limits, two separate WARN records are emitted — one for each field.
func TestEmitConfigWarnings_BothUnsafe(t *testing.T) {
	cap := &captureHandler{}
	log := slog.New(cap)

	cfg := config.DefaultConfig()
	cfg.P2P.BadBlockBanThreshold = 100
	cfg.P2P.BadBlockHeightLead = 20_000

	emitConfigWarnings(log, cfg)

	var banFound, heightFound bool
	cap.mu.Lock()
	for _, r := range cap.records {
		if r.Message != "config warning" || r.Level != slog.LevelWarn {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "msg" {
				v := a.Value.String()
				if strings.Contains(v, "bad_block_ban_threshold") {
					banFound = true
				}
				if strings.Contains(v, "bad_block_height_lead") {
					heightFound = true
				}
			}
			return true
		})
	}
	cap.mu.Unlock()

	if !banFound {
		t.Error("expected a WARN record for bad_block_ban_threshold but got none")
	}
	if !heightFound {
		t.Error("expected a WARN record for bad_block_height_lead but got none")
	}
}

// TestEmitConfigWarnings_SafeValues confirms that the default config (all
// values within safe limits) produces no "config warning" WARN records.
func TestEmitConfigWarnings_SafeValues(t *testing.T) {
	cap := &captureHandler{}
	log := slog.New(cap)

	cfg := config.DefaultConfig()
	// DefaultConfig() values are within safe limits — no warnings expected.

	emitConfigWarnings(log, cfg)

	if _, ok := cap.find("config warning"); ok {
		t.Error("expected no 'config warning' records for a default config, but got one")
	}
}
