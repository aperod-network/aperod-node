package config

import "testing"

func TestGuardianFundConfigRequiresPairedValidAnchor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Consensus.GuardianFundActivationHeight = 100
	if err := cfg.Validate(); err == nil {
		t.Fatal("activation without anchor accepted")
	}
	cfg.Consensus.GuardianFundChainAnchor = "not-hex"
	if err := cfg.Validate(); err == nil {
		t.Fatal("malformed anchor accepted")
	}
	cfg.Consensus.GuardianFundChainAnchor = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := cfg.Validate(); err == nil {
		t.Fatal("zero anchor accepted")
	}
	cfg.Consensus.GuardianFundChainAnchor = "0100000000000000000000000000000000000000000000000000000000000000"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid paired Guardian configuration rejected: %v", err)
	}
	cfg.Consensus.GuardianFundActivationHeight = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("anchor without activation accepted")
	}
}
