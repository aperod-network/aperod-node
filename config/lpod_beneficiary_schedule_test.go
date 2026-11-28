package config

import (
	"strings"
	"testing"
)

func TestLPoDBeneficiaryMainnetScheduleCannotBeEnabledByOperator(t *testing.T) {
	c := DefaultConfig()
	c.Network = Mainnet
	if MainnetLPoDBeneficiaryActivationHeight != 0 {
		t.Fatal("candidate must not silently activate Mainnet")
	}
	for _, relay := range []bool{false, true} {
		c.Consensus.NonValidator = relay
		c.Consensus.LPoDBeneficiaryActivationHeight = 4_500_000
		if c.LPoDBeneficiaryValidationHeight() != 0 {
			t.Fatal("operator changed compiled Mainnet consensus schedule")
		}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "mainnet lpod_beneficiary_activation_height") {
			t.Fatalf("operator override was not explicitly rejected: %v", err)
		}
	}
}

func TestLPoDBeneficiaryDevelopmentScheduleAndRange(t *testing.T) {
	c := DefaultConfig()
	c.Consensus.NonValidator = true
	c.Consensus.LPoDBeneficiaryActivationHeight = 100
	if c.LPoDBeneficiaryValidationHeight() != 100 {
		t.Fatal("development schedule lost")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("development candidate invalid: %v", err)
	}
	c.Consensus.LPoDBeneficiaryActivationHeight = 1 << 63
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "lpod_beneficiary_activation_height exceeds") {
		t.Fatalf("overflowing activation accepted: %v", err)
	}
}
