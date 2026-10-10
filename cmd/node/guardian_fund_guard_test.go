package main

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/config"
)

func TestGuardianFundProductionGuard(t *testing.T) {
	cfg := config.DefaultConfig()
	if err := guardGuardianFundActivation(cfg); err != nil {
		t.Fatalf("disabled Guardian configuration rejected: %v", err)
	}

	cfg.Consensus.GuardianFundActivationHeight = 1
	err := guardGuardianFundActivation(cfg)
	if err == nil {
		t.Fatal("nonzero Guardian activation passed the production startup guard")
	}
	if !strings.Contains(err.Error(), "economic reconciliation not approved") {
		t.Fatalf("startup refusal lacks explicit reconciliation reason: %v", err)
	}
}
