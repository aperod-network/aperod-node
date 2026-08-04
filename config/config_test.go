package config

import (
	"testing"
)

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
