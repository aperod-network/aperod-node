package core

import (
	"strings"
	"testing"
)

func validNetworkGenesis(chainID string) *GenesisConfig {
	return &GenesisConfig{
		ChainID:       chainID,
		InitialSupply: 1,
		RingSize:      2,
		MinValidators: 1,
		BFTThreshold:  0.667,
		Validators:    []string{"fixture-validator"},
	}
}

func TestValidateNetworkGenesis(t *testing.T) {
	t.Run("allows valid testnet genesis", func(t *testing.T) {
		if err := ValidateNetworkGenesis("testnet", validNetworkGenesis(TestnetChainID)); err != nil {
			t.Fatalf("testnet genesis rejected: %v", err)
		}
	})

	t.Run("rejects bundled testnet genesis as mainnet", func(t *testing.T) {
		err := ValidateNetworkGenesis("mainnet", validNetworkGenesis(TestnetChainID))
		if err == nil || !strings.Contains(err.Error(), "cannot be used as mainnet") {
			t.Fatalf("testnet genesis/mainnet mismatch error = %v", err)
		}
	})

	t.Run("allows canonical mainnet chain id", func(t *testing.T) {
		if err := ValidateNetworkGenesis("mainnet", validNetworkGenesis(MainnetChainID)); err != nil {
			t.Fatalf("canonical mainnet genesis rejected: %v", err)
		}
	})

	t.Run("mainnet aliases fail closed", func(t *testing.T) {
		err := ValidateNetworkGenesis(" MAINNET ", validNetworkGenesis(MainnetChainID))
		if err == nil || !strings.Contains(err.Error(), "unsupported network") {
			t.Fatalf("mainnet alias error = %v", err)
		}
	})

	t.Run("rejects unsupported network", func(t *testing.T) {
		err := ValidateNetworkGenesis("production", validNetworkGenesis("aperod-testnet-1"))
		if err == nil || !strings.Contains(err.Error(), "unsupported network") {
			t.Fatalf("unsupported network error = %v", err)
		}
	})
}
