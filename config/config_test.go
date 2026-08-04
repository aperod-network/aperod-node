package config

import (
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
			name:      "custom valid combination is accepted",
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
