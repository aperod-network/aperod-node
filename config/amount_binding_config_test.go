package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRingCTAmountBindingActivationValidationAndLoad(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Consensus.RingCTAmountBindingActivationHeight = 100
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "requires ring_ct_clsag_activation_height") {
		t.Fatalf("amount-binding activation without CLSAG should fail, got %v", err)
	}

	cfg.Consensus.RingCTCLSAGActivationHeight = 200
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must be >=") {
		t.Fatalf("amount-binding activation before CLSAG should fail, got %v", err)
	}
	cfg.Consensus.RingCTAmountBindingActivationHeight = 300
	if err := cfg.Validate(); err != nil {
		t.Fatalf("coordinated amount-binding activation should validate: %v", err)
	}

	path := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(path, []byte("consensus:\n  ring_ct_clsag_activation_height: 200\n  ring_ct_amount_binding_activation_height: 300\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Consensus.RingCTAmountBindingActivationHeight; got != 300 {
		t.Fatalf("loaded amount-binding activation height = %d, want 300", got)
	}
}
