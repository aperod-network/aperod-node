package core

import (
	"path/filepath"
	"testing"
)

func TestBundledMainnetGenesisProfileMatchesPinnedMainnetIdentity(t *testing.T) {
	genesis, err := LoadGenesis(filepath.Join("..", "config", "genesis-mainnet.yaml"))
	if err != nil {
		t.Fatalf("load Mainnet genesis profile: %v", err)
	}
	if err := ValidateNetworkGenesis("mainnet", genesis); err != nil {
		t.Fatalf("validate Mainnet genesis profile: %v", err)
	}

	if genesis.ChainID != MainnetChainID {
		t.Fatalf("chain ID = %q, want %q", genesis.ChainID, MainnetChainID)
	}
	if genesis.Timestamp != 1_782_932_857 {
		t.Fatalf("genesis timestamp = %d, want 1782932857", genesis.Timestamp)
	}
	if genesis.InitialSupply != 10_000_000_000 {
		t.Fatalf("initial supply = %d APRO, want 10000000000", genesis.InitialSupply)
	}
	if genesis.BlockTimeMs != 3000 || genesis.RingSize != 16 {
		t.Fatalf("Mainnet block/ring profile = %d ms / %d, want 3000 ms / 16",
			genesis.BlockTimeMs, genesis.RingSize)
	}
	if len(genesis.Validators) != 1 ||
		genesis.Validators[0] != "08fd38ba8857d32d2ac807f0246a0fd298c9edf336fbd0fea3bb7bd818c91137" {
		t.Fatalf("genesis validators do not match the authenticated genesis signer: %v", genesis.Validators)
	}
	if len(genesis.Allocations) != 0 {
		t.Fatalf("Mainnet genesis profile unexpectedly contains allocations: %d", len(genesis.Allocations))
	}
}
