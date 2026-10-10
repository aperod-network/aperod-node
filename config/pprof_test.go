package config

import (
	"strings"
	"testing"
)

// TestPprofDisabledByDefault confirms that the default config has pprof disabled
// and does not require a valid listen_addr to pass validation.
func TestPprofDisabledByDefault(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Pprof.Enabled {
		t.Fatal("pprof must be disabled in the default config")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must pass validation: %v", err)
	}
}

// TestPprofValidLoopbackAddresses confirms that common loopback addresses are accepted.
func TestPprofValidLoopbackAddresses(t *testing.T) {
	cases := []string{
		"127.0.0.1:8546",
		"127.0.0.1:9000",
		"127.1.2.3:8546",
		"[::1]:8546",
	}
	for _, addr := range cases {
		cfg := DefaultConfig()
		cfg.Pprof.Enabled = true
		cfg.Pprof.ListenAddr = addr
		if err := cfg.Validate(); err != nil {
			t.Errorf("loopback addr %q should be accepted, got: %v", addr, err)
		}
	}
}

// TestPprofNonLoopbackRejected confirms that non-loopback addresses are rejected
// when pprof is enabled, preventing accidental public exposure.
func TestPprofNonLoopbackRejected(t *testing.T) {
	cases := []string{
		"0.0.0.0:8546",
		"[::]:8546",
		"192.168.1.10:8546",
		"10.0.0.1:8546",
	}
	for _, addr := range cases {
		cfg := DefaultConfig()
		cfg.Pprof.Enabled = true
		cfg.Pprof.ListenAddr = addr
		err := cfg.Validate()
		if err == nil {
			t.Errorf("non-loopback addr %q should be rejected but was accepted", addr)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("error for %q should mention loopback, got: %v", addr, err)
		}
	}
}

// TestPprofInvalidAddrRejected confirms that a malformed listen_addr (no port)
// is rejected rather than silently ignored.
func TestPprofInvalidAddrRejected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Pprof.Enabled = true
	cfg.Pprof.ListenAddr = "notanaddr"
	if err := cfg.Validate(); err == nil {
		t.Fatal("malformed pprof.listen_addr should fail validation")
	}
}

// TestPprofEmptyAddrDefaultsToLoopback confirms that leaving listen_addr empty
// while enabling pprof passes validation (the default 127.0.0.1:8546 is used).
func TestPprofEmptyAddrDefaultsToLoopback(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Pprof.Enabled = true
	cfg.Pprof.ListenAddr = "" // should fall back to 127.0.0.1:8546 in Validate
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled pprof with empty listen_addr should pass validation: %v", err)
	}
}

// TestPprofNonLoopbackIgnoredWhenDisabled confirms that a bad listen_addr is
// not flagged when pprof is disabled — operators can leave a stale value in the
// config file without being blocked from starting the node.
func TestPprofNonLoopbackIgnoredWhenDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Pprof.Enabled = false
	cfg.Pprof.ListenAddr = "0.0.0.0:8546" // would be invalid if enabled
	if err := cfg.Validate(); err != nil {
		t.Fatalf("non-loopback addr in disabled pprof config should not fail validation: %v", err)
	}
}
