package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/crypto"
)

func TestMainnetBootstrapRefusalUsesPermanentConfigurationExit(t *testing.T) {
	if got := startupExitCode(config.ErrMainnetBootstrapUnavailable); got != exitCodeConfigurationError {
		t.Fatalf("mainnet identity refusal exit code = %d, want %d", got, exitCodeConfigurationError)
	}
	wrapped := fmt.Errorf("invalid config: %w", config.ErrMainnetBootstrapUnavailable)
	if got := startupExitCode(wrapped); got != exitCodeConfigurationError {
		t.Fatalf("wrapped mainnet identity refusal exit code = %d, want %d", got, exitCodeConfigurationError)
	}
	if got := startupExitCode(errors.New("temporary peer timeout")); got != 1 {
		t.Fatalf("ordinary startup failure exit code = %d, want 1", got)
	}
}

func TestRunRefusesUnverifiedMainnetBeforeCreatingData(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Network = config.Mainnet
	cfg.Consensus.NonValidator = true
	cfg.DataDir = filepath.Join(t.TempDir(), "must-not-be-created")
	cfg.Consensus.SecurityHardeningActivationHeight = config.MainnetSecurityHardeningActivationHeight
	cfg.Consensus.OracleMaxDeviation = 0.05
	cfg.Consensus.OracleURL = "https://oracle.example.invalid"
	cfg.Consensus.RingCTV4ActivationHeight = 100
	cfg.Consensus.RingCTCLSAGActivationHeight = 101
	cfg.Consensus.StakedCollateralRingExclusionActivationHeight = 102

	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Consensus.RewardAddress = string(crypto.AddressFromKeys(crypto.MainnetByte, keys))

	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	os.Args = []string{"aperod-node", "--config", configPath, "--validate-config"}
	t.Cleanup(func() { os.Args = oldArgs })

	err = run()
	if err == nil || !strings.Contains(err.Error(), "no valid signed operator_1_of_1 Mainnet bootstrap manifest") {
		t.Fatalf("mainnet config should be refused before startup, got %v", err)
	}
	if _, statErr := os.Stat(cfg.DataDir); !os.IsNotExist(statErr) {
		t.Fatalf("mainnet refusal created data directory: stat error = %v", statErr)
	}
}

func TestRunRefusesConfiglessStartupWithoutCreatingData(t *testing.T) {
	oldArgs := os.Args
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tempWD := t.TempDir()
	if err := os.Chdir(tempWD); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
		os.Args = oldArgs
	})

	os.Args = []string{"aperod-node", "--help"}
	if err := run(); err != nil {
		t.Fatalf("help should exit without starting a node: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(tempWD, "data", "testnet")); !os.IsNotExist(statErr) {
		t.Fatalf("help created a legacy testnet data directory: stat error = %v", statErr)
	}

	os.Args = []string{"aperod-node"}
	err = run()
	if err == nil || !strings.Contains(err.Error(), "config-free startup is disabled") {
		t.Fatalf("config-free node startup should be refused, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(tempWD, "data", "testnet")); !os.IsNotExist(statErr) {
		t.Fatalf("config-free startup created a legacy testnet data directory: stat error = %v", statErr)
	}
}
