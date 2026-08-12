package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidate_BadBlockKnobs(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(cfg *Config)
		wantError bool
	}{
		{
			name:      "negative threshold is rejected",
			mutate:    func(cfg *Config) { cfg.P2P.BadBlockBanThreshold = -1 },
			wantError: true,
		},
		{
			name:      "zero threshold is accepted (means use default)",
			mutate:    func(cfg *Config) { cfg.P2P.BadBlockBanThreshold = 0 },
			wantError: false,
		},
		{
			name:      "positive threshold is accepted",
			mutate:    func(cfg *Config) { cfg.P2P.BadBlockBanThreshold = 5 },
			wantError: false,
		},
		{
			name:      "negative ban duration is rejected",
			mutate:    func(cfg *Config) { cfg.P2P.BadBlockBanDuration = -time.Second },
			wantError: true,
		},
		{
			name:      "zero ban duration is accepted (means use default)",
			mutate:    func(cfg *Config) { cfg.P2P.BadBlockBanDuration = 0 },
			wantError: false,
		},
		{
			name:      "positive ban duration is accepted",
			mutate:    func(cfg *Config) { cfg.P2P.BadBlockBanDuration = 5 * time.Minute },
			wantError: false,
		},
		{
			name:      "height lead exceeding 1 billion is rejected (overflow risk)",
			mutate:    func(cfg *Config) { cfg.P2P.BadBlockHeightLead = 1_000_000_001 },
			wantError: true,
		},
		{
			name:      "height lead of exactly 1 billion is accepted",
			mutate:    func(cfg *Config) { cfg.P2P.BadBlockHeightLead = 1_000_000_000 },
			wantError: false,
		},
		{
			name:      "zero height lead is accepted (means use default)",
			mutate:    func(cfg *Config) { cfg.P2P.BadBlockHeightLead = 0 },
			wantError: false,
		},
		{
			name: "custom valid combination is accepted",
			mutate: func(cfg *Config) {
				cfg.P2P.BadBlockBanThreshold = 3
				cfg.P2P.BadBlockHeightLead = 500
				cfg.P2P.BadBlockBanDuration = 12 * time.Hour
			},
			wantError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantError && err == nil {
				t.Errorf("Validate() returned nil, want error")
			}
			if !tc.wantError && err != nil {
				t.Errorf("Validate() returned unexpected error: %v", err)
			}
		})
	}
}

func TestValidate_PeerWhitelist(t *testing.T) {
	tests := []struct {
		name      string
		list      []string
		wantError bool
	}{
		{
			name:      "empty whitelist is accepted (open network)",
			list:      nil,
			wantError: false,
		},
		{
			name:      "single valid IPv4 is accepted",
			list:      []string{"1.2.3.4"},
			wantError: false,
		},
		{
			name:      "single valid IPv6 is accepted",
			list:      []string{"2001:db8::1"},
			wantError: false,
		},
		{
			name:      "valid CIDR range is accepted",
			list:      []string{"10.0.0.0/8"},
			wantError: false,
		},
		{
			name:      "multiple valid entries are accepted",
			list:      []string{"1.2.3.4", "10.0.0.0/24", "192.168.1.100"},
			wantError: false,
		},
		{
			name:      "malformed entry is rejected",
			list:      []string{"not-an-ip"},
			wantError: true,
		},
		{
			name:      "hostname without port is rejected (not an IP)",
			list:      []string{"validator.example.com"},
			wantError: true,
		},
		{
			name:      "valid entry mixed with malformed entry is rejected",
			list:      []string{"1.2.3.4", "bad-entry"},
			wantError: true,
		},
		{
			name:      "IP with port is rejected (must be bare IP)",
			list:      []string{"1.2.3.4:30303"},
			wantError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.P2P.PeerWhitelist = tc.list
			err := cfg.Validate()
			if tc.wantError && err == nil {
				t.Errorf("Validate() returned nil, want error for peer_whitelist=%v", tc.list)
			}
			if !tc.wantError && err != nil {
				t.Errorf("Validate() returned unexpected error for peer_whitelist=%v: %v", tc.list, err)
			}
		})
	}
}

func TestValidate_KeepaliveInterval(t *testing.T) {
	tests := []struct {
		name      string
		interval  time.Duration
		wantError bool
	}{
		// Zero means "use the built-in default (10 s)" — must be accepted.
		{
			name:      "zero means use default and is accepted",
			interval:  0,
			wantError: false,
		},
		// Values below the 1 s floor would flood slow peers with pings.
		{
			name:      "negative value is rejected",
			interval:  -time.Second,
			wantError: true,
		},
		{
			name:      "500ms is below 1s floor and is rejected",
			interval:  500 * time.Millisecond,
			wantError: true,
		},
		{
			name:      "999ms is just below 1s floor and is rejected",
			interval:  999 * time.Millisecond,
			wantError: true,
		},
		// Values above the 15 s ceiling risk the peer's 30 s ReadTimeout firing.
		{
			name:      "16s exceeds 15s ceiling and is rejected",
			interval:  16 * time.Second,
			wantError: true,
		},
		{
			name:      "1h is far above ceiling and is rejected",
			interval:  time.Hour,
			wantError: true,
		},
		// Values inside [1s, 15s] must be accepted.
		{
			name:      "1s (floor) is accepted",
			interval:  time.Second,
			wantError: false,
		},
		{
			name:      "5s is accepted",
			interval:  5 * time.Second,
			wantError: false,
		},
		{
			name:      "10s (production default) is accepted",
			interval:  10 * time.Second,
			wantError: false,
		},
		{
			name:      "15s (ceiling) is accepted",
			interval:  15 * time.Second,
			wantError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.P2P.KeepaliveInterval = tc.interval
			err := cfg.Validate()
			if tc.wantError && err == nil {
				t.Errorf("Validate() returned nil, want error for keepalive_interval=%v", tc.interval)
			}
			if !tc.wantError && err != nil {
				t.Errorf("Validate() returned unexpected error for keepalive_interval=%v: %v", tc.interval, err)
			}
		})
	}
}

func TestValidate_MemoryLimitBytes(t *testing.T) {
	const mib512 = 512 * 1024 * 1024 // 536870912

	tests := []struct {
		name      string
		value     int64
		wantError bool
	}{
		{
			name:      "negative value is rejected",
			value:     -1,
			wantError: true,
		},
		{
			name:      "below 512 MiB floor is rejected",
			value:     1,
			wantError: true,
		},
		{
			name:      "zero disables the limit and is accepted",
			value:     0,
			wantError: false,
		},
		{
			name:      "exactly 512 MiB is accepted",
			value:     mib512,
			wantError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.MemoryLimitBytes = tc.value

			err := cfg.Validate()
			if tc.wantError && err == nil {
				t.Errorf("Validate() returned nil, want error for memory_limit_bytes=%d", tc.value)
			}
			if !tc.wantError && err != nil {
				t.Errorf("Validate() returned unexpected error for memory_limit_bytes=%d: %v", tc.value, err)
			}
		})
	}
}

// TestValidate_MemoryLimitDisabled covers task #1710: memory_limit_disabled is
// intended to silence the startup GOMEMLIMIT warning when running uncapped is
// deliberate.  Setting it together with a positive memory_limit_bytes is
// contradictory and must be rejected; every other combination is accepted.
func TestValidate_MemoryLimitDisabled(t *testing.T) {
	const mib512 = 512 * 1024 * 1024

	tests := []struct {
		name       string
		disabled   bool
		limitBytes int64
		wantError  bool
	}{
		{
			name:      "disabled true with zero limit is accepted",
			disabled:  true,
			wantError: false,
		},
		{
			name:      "disabled false with zero limit is accepted",
			disabled:  false,
			wantError: false,
		},
		{
			name:       "disabled false with positive limit is accepted",
			disabled:   false,
			limitBytes: mib512,
			wantError:  false,
		},
		{
			name:       "disabled true with positive limit is rejected (contradictory)",
			disabled:   true,
			limitBytes: mib512,
			wantError:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.MemoryLimitDisabled = tc.disabled
			cfg.MemoryLimitBytes = tc.limitBytes

			err := cfg.Validate()
			if tc.wantError && err == nil {
				t.Errorf("Validate() returned nil, want error for disabled=%v limit=%d", tc.disabled, tc.limitBytes)
			}
			if !tc.wantError && err != nil {
				t.Errorf("Validate() returned unexpected error for disabled=%v limit=%d: %v", tc.disabled, tc.limitBytes, err)
			}
		})
	}
}

// TestMemoryLimitDisabledParsesFromYAML verifies the memory_limit_disabled
// field round-trips through the YAML loader (task #1710).
func TestMemoryLimitDisabledParsesFromYAML(t *testing.T) {
	dir := t.TempDir()

	// Explicit true must be parsed as true.
	pTrue := filepath.Join(dir, "node_true.yaml")
	if err := os.WriteFile(pTrue, []byte("memory_limit_disabled: true\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cTrue, err := Load(pTrue)
	if err != nil {
		t.Fatalf("Load(true): %v", err)
	}
	if !cTrue.MemoryLimitDisabled {
		t.Errorf("memory_limit_disabled = false, want true")
	}

	// Explicit false must be parsed as false.
	pFalse := filepath.Join(dir, "node_false.yaml")
	if err := os.WriteFile(pFalse, []byte("memory_limit_disabled: false\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cFalse, err := Load(pFalse)
	if err != nil {
		t.Fatalf("Load(false): %v", err)
	}
	if cFalse.MemoryLimitDisabled {
		t.Errorf("memory_limit_disabled = true, want false")
	}

	// Omitted field must default to false.
	pOmit := filepath.Join(dir, "node_omit.yaml")
	if err := os.WriteFile(pOmit, []byte("network: testnet\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cOmit, err := Load(pOmit)
	if err != nil {
		t.Fatalf("Load(omit): %v", err)
	}
	if cOmit.MemoryLimitDisabled {
		t.Errorf("memory_limit_disabled default = true, want false")
	}
}
