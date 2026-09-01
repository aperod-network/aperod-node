package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
)

func TestValidate_RewardAuthorizationActivation(t *testing.T) {
	validRewardAddress := func(t *testing.T, network crypto.NetworkByte) string {
		t.Helper()
		keys, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatal(err)
		}
		return string(crypto.AddressFromKeys(network, keys))
	}

	t.Run("rejects activation before RingCT v4", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Consensus.RingCTV4ActivationHeight = 100
		cfg.Consensus.RewardAuthorizationActivationHeight = 99
		cfg.Consensus.RewardAddress = validRewardAddress(t, crypto.TestnetByte)
		if err := cfg.Validate(); err == nil {
			t.Fatal("activation before RingCT v4 was accepted")
		}
	})

	t.Run("requires reward address for validator", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Consensus.RingCTV4ActivationHeight = 100
		cfg.Consensus.RewardAuthorizationActivationHeight = 100
		cfg.Consensus.RewardAddress = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("validator activation without reward_address was accepted")
		}
	})

	t.Run("allows relay without reward address", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Consensus.NonValidator = true
		cfg.Consensus.RingCTV4ActivationHeight = 100
		cfg.Consensus.RewardAuthorizationActivationHeight = 100
		cfg.Consensus.RewardAddress = ""
		if err := cfg.Validate(); err != nil {
			t.Fatalf("relay configuration rejected: %v", err)
		}
	})

	t.Run("accepts coordinated validator activation", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Consensus.RingCTV4ActivationHeight = 100
		cfg.Consensus.RewardAuthorizationActivationHeight = 101
		cfg.Consensus.RewardAddress = validRewardAddress(t, crypto.TestnetByte)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid activation rejected: %v", err)
		}
	})

	t.Run("rejects malformed validator reward address", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Consensus.RingCTV4ActivationHeight = 100
		cfg.Consensus.RewardAuthorizationActivationHeight = 100
		cfg.Consensus.RewardAddress = "not-an-aperod-address"
		if err := cfg.Validate(); err == nil {
			t.Fatal("malformed reward_address was accepted")
		}
	})

	t.Run("rejects reward address from another network", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Consensus.RingCTV4ActivationHeight = 100
		cfg.Consensus.RewardAuthorizationActivationHeight = 100
		cfg.Consensus.RewardAddress = validRewardAddress(t, crypto.MainnetByte)
		if err := cfg.Validate(); err == nil {
			t.Fatal("mainnet reward_address was accepted for testnet")
		}
	})
}

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

// TestWarnings_UnsafeBanValues verifies that Config.Warnings() emits a warning
// when bad_block_ban_threshold or bad_block_height_lead exceed their safe
// maximums (50 and 10 000 respectively), and stays silent when values are at
// or below those limits.
func TestWarnings_UnsafeBanValues(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(cfg *Config)
		wantBanWarn    bool // expect bad_block_ban_threshold warning
		wantHeightWarn bool // expect bad_block_height_lead warning
	}{
		// --- ban threshold ---
		{
			name:        "threshold at safe maximum (50) produces no warning",
			mutate:      func(cfg *Config) { cfg.P2P.BadBlockBanThreshold = 50 },
			wantBanWarn: false,
		},
		{
			name:        "threshold below safe maximum produces no warning",
			mutate:      func(cfg *Config) { cfg.P2P.BadBlockBanThreshold = 10 },
			wantBanWarn: false,
		},
		{
			name:        "threshold at default (5) produces no warning",
			mutate:      func(cfg *Config) { cfg.P2P.BadBlockBanThreshold = 5 },
			wantBanWarn: false,
		},
		{
			name:        "threshold just above safe maximum (51) produces warning",
			mutate:      func(cfg *Config) { cfg.P2P.BadBlockBanThreshold = 51 },
			wantBanWarn: true,
		},
		{
			name:        "very large threshold produces warning",
			mutate:      func(cfg *Config) { cfg.P2P.BadBlockBanThreshold = 10_000 },
			wantBanWarn: true,
		},
		// --- height lead ---
		{
			name:           "height lead at safe maximum (10000) produces no warning",
			mutate:         func(cfg *Config) { cfg.P2P.BadBlockHeightLead = 10_000 },
			wantHeightWarn: false,
		},
		{
			name:           "height lead below safe maximum produces no warning",
			mutate:         func(cfg *Config) { cfg.P2P.BadBlockHeightLead = 1_000 },
			wantHeightWarn: false,
		},
		{
			name:           "height lead just above safe maximum (10001) produces warning",
			mutate:         func(cfg *Config) { cfg.P2P.BadBlockHeightLead = 10_001 },
			wantHeightWarn: true,
		},
		{
			name:           "very large height lead produces warning",
			mutate:         func(cfg *Config) { cfg.P2P.BadBlockHeightLead = 1_000_000 },
			wantHeightWarn: true,
		},
		// --- both fields unsafe ---
		{
			name: "both fields above safe limits produce two warnings",
			mutate: func(cfg *Config) {
				cfg.P2P.BadBlockBanThreshold = 100
				cfg.P2P.BadBlockHeightLead = 20_000
			},
			wantBanWarn:    true,
			wantHeightWarn: true,
		},
		// --- both fields within limits ---
		{
			name: "default config produces no unsafe-ban warnings",
			mutate: func(_ *Config) {
				// no mutations; DefaultConfig() values are within safe limits
			},
			wantBanWarn:    false,
			wantHeightWarn: false,
		},
	}

	const banSubstr = "bad_block_ban_threshold"
	const heightSubstr = "bad_block_height_lead"

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(cfg)

			warnings := cfg.Warnings()

			hasBanWarn := false
			hasHeightWarn := false
			for _, w := range warnings {
				if contains(w, banSubstr) {
					hasBanWarn = true
				}
				if contains(w, heightSubstr) {
					hasHeightWarn = true
				}
			}

			if tc.wantBanWarn && !hasBanWarn {
				t.Errorf("expected a warning containing %q but got none; warnings=%v",
					banSubstr, warnings)
			}
			if !tc.wantBanWarn && hasBanWarn {
				t.Errorf("unexpected warning containing %q; warnings=%v",
					banSubstr, warnings)
			}
			if tc.wantHeightWarn && !hasHeightWarn {
				t.Errorf("expected a warning containing %q but got none; warnings=%v",
					heightSubstr, warnings)
			}
			if !tc.wantHeightWarn && hasHeightWarn {
				t.Errorf("unexpected warning containing %q; warnings=%v",
					heightSubstr, warnings)
			}
		})
	}
}

// contains reports whether substr appears anywhere in s.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}

// TestWarnings_UnsafeBanValues_MessageText verifies that the warning messages
// include both the configured value and the safe-maximum constant so that
// operators can understand the message without reading the source code.
func TestWarnings_UnsafeBanValues_MessageText(t *testing.T) {
	cfg := DefaultConfig()
	cfg.P2P.BadBlockBanThreshold = 200
	cfg.P2P.BadBlockHeightLead = 50_000

	warnings := cfg.Warnings()

	var banWarn, heightWarn string
	for _, w := range warnings {
		if contains(w, "bad_block_ban_threshold") {
			banWarn = w
		}
		if contains(w, "bad_block_height_lead") {
			heightWarn = w
		}
	}

	if banWarn == "" {
		t.Fatal("expected ban-threshold warning, got none")
	}
	if !contains(banWarn, "200") {
		t.Errorf("ban-threshold warning should contain configured value 200; got: %q", banWarn)
	}
	if !contains(banWarn, "50") {
		t.Errorf("ban-threshold warning should contain safe maximum 50; got: %q", banWarn)
	}

	if heightWarn == "" {
		t.Fatal("expected height-lead warning, got none")
	}
	if !contains(heightWarn, "50000") {
		t.Errorf("height-lead warning should contain configured value 50000; got: %q", heightWarn)
	}
	if !contains(heightWarn, "10000") {
		t.Errorf("height-lead warning should contain safe maximum 10000; got: %q", heightWarn)
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

// TestResolveMempoolEvictInterval verifies that the resolved ticker duration
// matches the operator-configured value, falls back to 5 minutes when the
// field is absent (YAML zero-value), and falls back to 5 minutes when the
// operator explicitly writes mempool_evict_interval_sec: 0.
func TestResolveMempoolEvictInterval(t *testing.T) {
	// minimalYAML provides the fields that Load() needs to succeed without
	// relying on defaults for required settings (network, data_dir, block_time).
	const minimalYAML = "network: testnet\ndata_dir: ./data\nconsensus:\n  block_time: 1s\n"

	tests := []struct {
		name    string
		extra   string // appended to minimalYAML
		wantDur time.Duration
	}{
		{
			name:    "explicit 60s is respected",
			extra:   "mempool_evict_interval_sec: 60\n",
			wantDur: 60 * time.Second,
		},
		{
			name:    "absent field resolves to 300s default",
			extra:   "",
			wantDur: 300 * time.Second,
		},
		{
			name:    "explicit 0 falls back to 300s default",
			extra:   "mempool_evict_interval_sec: 0\n",
			wantDur: 300 * time.Second,
		},
	}

	dir := t.TempDir()
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name+".yaml")
			content := minimalYAML + tc.extra
			if err := os.WriteFile(p, []byte(content), 0644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := Load(p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := ResolveMempoolEvictInterval(cfg.MempoolEvictIntervalSec)
			if got != tc.wantDur {
				t.Errorf("ResolveMempoolEvictInterval(%d) = %v, want %v",
					cfg.MempoolEvictIntervalSec, got, tc.wantDur)
			}
		})
	}
}

// TestWarnings_BootnodeNotWhitelisted verifies that Warnings() emits a warning
// when a bootnode IP is not covered by peer_whitelist and bad_block_ban_threshold
// is non-zero, and stays silent when the bootnode IP is whitelisted or when
// the threshold is zero (banning disabled).
func TestWarnings_BootnodeNotWhitelisted(t *testing.T) {
	tests := []struct {
		name        string
		bootnodes   []string
		whitelist   []string
		threshold   int
		wantWarning bool
	}{
		{
			name:        "bootnode not in whitelist — warn",
			bootnodes:   []string{"89.169.53.128:30303"},
			whitelist:   []string{},
			threshold:   5,
			wantWarning: true,
		},
		{
			name:        "bootnode in whitelist by IP — no warn",
			bootnodes:   []string{"89.169.53.128:30303"},
			whitelist:   []string{"89.169.53.128"},
			threshold:   5,
			wantWarning: false,
		},
		{
			name:        "bootnode covered by CIDR — no warn",
			bootnodes:   []string{"89.169.53.128:30303"},
			whitelist:   []string{"89.169.53.0/24"},
			threshold:   5,
			wantWarning: false,
		},
		{
			name:        "threshold zero (banning disabled) — no warn even without whitelist",
			bootnodes:   []string{"89.169.53.128:30303"},
			whitelist:   []string{},
			threshold:   0,
			wantWarning: false,
		},
		{
			name:        "no bootnodes — no warn",
			bootnodes:   []string{},
			whitelist:   []string{},
			threshold:   5,
			wantWarning: false,
		},
		{
			name:        "DNS bootnode skipped — no warn",
			bootnodes:   []string{"bootnode.aperod.com:30303"},
			whitelist:   []string{},
			threshold:   5,
			wantWarning: false,
		},
		{
			name:        "multiple bootnodes — warns only for uncovered IP",
			bootnodes:   []string{"89.169.53.128:30303", "10.0.0.1:30303"},
			whitelist:   []string{"89.169.53.128"},
			threshold:   5,
			wantWarning: true, // 10.0.0.1 not covered
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.P2P.Bootnodes = tc.bootnodes
			cfg.P2P.PeerWhitelist = tc.whitelist
			cfg.P2P.BadBlockBanThreshold = tc.threshold

			warnings := cfg.Warnings()
			hasBootnodeWarn := false
			for _, w := range warnings {
				if contains(w, "peer_whitelist") && contains(w, "bootnode") {
					hasBootnodeWarn = true
				}
			}
			if hasBootnodeWarn != tc.wantWarning {
				t.Errorf("Warnings() bootnode warning = %v, want %v\nwarnings: %v",
					hasBootnodeWarn, tc.wantWarning, warnings)
			}
		})
	}
}
