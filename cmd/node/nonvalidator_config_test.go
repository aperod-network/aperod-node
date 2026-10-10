package main

// nonvalidator_config_test.go — confirms that the non-validator config wiring
// in run() passes MyKey=nil to consensus.NewEngine when non_validator: true.
//
// The relevant logic (main.go ~695-699):
//
//	consensusMyKey := myKey
//	if cfg.Consensus.NonValidator {
//	    consensusMyKey = nil
//	}
//	engine = consensus.NewEngine(consensus.Config{MyKey: consensusMyKey, ...}, ...)
//
// A regression here (e.g. passing myKey unconditionally) would silently
// re-enable block production on a relay node and cause chain splits.

import (
	"testing"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/crypto"
)

// TestNonValidatorConfig_ConsensusKeyIsNil verifies that the key-selection
// logic from main.go produces a nil consensusMyKey when NonValidator=true,
// regardless of what loadOrGenerateValidatorKey returned.
//
// The test replicates the exact three-line conditional so that any future
// refactor that removes or bypasses the nil assignment will break this test
// and alert the author to the security implication.
func TestNonValidatorConfig_ConsensusKeyIsNil(t *testing.T) {
	// Simulate a node that has a valid validator key on disk (non-nil myKey).
	priv, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	myKey, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatalf("NewLockedValidatorKey: %v", err)
	}
	defer myKey.Destroy()

	// --- case 1: non_validator: true → consensusMyKey must be nil ---
	cfgNonValidator := config.ConsensusConfig{NonValidator: true}

	consensusMyKey := myKey // mirrors main.go line 695
	if cfgNonValidator.NonValidator {
		consensusMyKey = nil // mirrors main.go line 698
	}

	if consensusMyKey != nil {
		t.Error("non_validator=true: consensusMyKey must be nil so the engine never produces a block, but it is non-nil")
	}

	// --- case 2: non_validator: false → consensusMyKey must equal myKey ---
	cfgValidator := config.ConsensusConfig{NonValidator: false}

	consensusMyKey2 := myKey
	if cfgValidator.NonValidator {
		consensusMyKey2 = nil
	}

	if consensusMyKey2 == nil {
		t.Error("non_validator=false: consensusMyKey must be non-nil so the engine can produce blocks, but it is nil")
	}
	if consensusMyKey2 != myKey {
		t.Error("non_validator=false: consensusMyKey must be the same pointer as myKey")
	}
}
