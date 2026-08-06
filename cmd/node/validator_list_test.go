package main

// validator_list_test.go — regression guard for the registry-seeding fix.
//
// The fix (main.go, NonValidator=true branch) seeds consensus.Config.Validators
// from the genesis-config public keys — NOT from the node's own P2P-identity key.
//
// If that branch regresses to:
//
//	validators = []crypto.ValidatorPubKey{myKey.Public()}
//
// a non-validator node's isKnownValidator() check will return false for every
// block from a real validator, silently breaking sync with no visible error.
//
// These tests call buildValidatorList() — the function that encapsulates exactly
// that branch — so any regression in the production wiring is caught here.

import (
	"encoding/hex"
	"testing"

	"github.com/aperod/aperod/crypto"
)

// TestBuildValidatorList_NonValidatorMode_UsesGenesisKeys verifies the correct
// path: non-validator mode must decode and return genesis-config validator keys,
// NOT the node's own identity key.
//
// This is the regression guard for main.go lines 252-266 (NonValidator=true branch).
func TestBuildValidatorList_NonValidatorMode_UsesGenesisKeys(t *testing.T) {
	// ── Genesis validator key (a real network validator) ──────────────────────
	_, genesisValidatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey (genesis validator):", err)
	}
	// Encode as hex string — the format used in genesis.yaml.
	genesisHex := hex.EncodeToString(genesisValidatorPub[:])

	// ── Node's own P2P identity key (a DIFFERENT key) ─────────────────────────
	// This simulates what loadOrGenerateValidatorKey() returns for this node.
	// It must NOT end up in the validators list in non-validator mode.
	nodePriv, nodePub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey (node identity):", err)
	}
	myKey, err := crypto.NewLockedValidatorKey(nodePriv.Bytes(), nil)
	if err != nil {
		t.Fatal("NewLockedValidatorKey:", err)
	}
	defer myKey.Destroy()

	// Sanity: the two keys must be distinct so the test is meaningful.
	if genesisValidatorPub.Equals(nodePub) {
		t.Fatal("test setup error: genesis validator pub and node pub are the same key")
	}

	// ── Call the production wiring function ───────────────────────────────────
	validators, err := buildValidatorList(
		true,                    // nonValidator = true
		[]string{genesisHex},   // genesis config validators (as hex)
		myKey,                   // node's own identity key
	)
	if err != nil {
		t.Fatalf("buildValidatorList: %v", err)
	}

	// ── Assertions ────────────────────────────────────────────────────────────
	if len(validators) != 1 {
		t.Fatalf("expected 1 validator in list, got %d", len(validators))
	}

	// Must contain the genesis validator key.
	if !validators[0].Equals(genesisValidatorPub) {
		t.Errorf("non-validator mode: expected genesis validator key in list, got %s", validators[0].ID())
	}

	// Must NOT contain the node's own key.
	// This is the core regression check: if the branch uses myKey.Public()
	// instead of the genesis keys, this assertion fails.
	if validators[0].Equals(nodePub) {
		t.Errorf("non-validator mode: list must NOT contain the node's own identity key "+
			"(got %s == node pub %s); "+
			"the NonValidator=true branch in main.go may have regressed to using myKey.Public()",
			validators[0].ID(), nodePub.ID())
	}
}

// TestBuildValidatorList_ValidatorMode_UsesOwnKey verifies the validator path:
// when NonValidator=false the list must contain exactly the node's own key.
func TestBuildValidatorList_ValidatorMode_UsesOwnKey(t *testing.T) {
	nodePriv, nodePub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey:", err)
	}
	myKey, err := crypto.NewLockedValidatorKey(nodePriv.Bytes(), nil)
	if err != nil {
		t.Fatal("NewLockedValidatorKey:", err)
	}
	defer myKey.Destroy()

	validators, err := buildValidatorList(
		false,         // nonValidator = false (validator mode)
		[]string{},    // genesis config validators unused in this mode
		myKey,
	)
	if err != nil {
		t.Fatalf("buildValidatorList: %v", err)
	}

	if len(validators) != 1 {
		t.Fatalf("expected 1 validator in list, got %d", len(validators))
	}
	if !validators[0].Equals(nodePub) {
		t.Errorf("validator mode: list must contain node's own key")
	}
}

// TestBuildValidatorList_NonValidatorMode_MultipleKeys verifies that all genesis
// validator keys are decoded and returned when the genesis config lists several.
func TestBuildValidatorList_NonValidatorMode_MultipleKeys(t *testing.T) {
	const n = 3
	genesisHexKeys := make([]string, n)
	genesisPubs := make([]crypto.ValidatorPubKey, n)
	for i := range genesisPubs {
		_, pub, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal("GenerateValidatorKey:", err)
		}
		genesisPubs[i] = pub
		genesisHexKeys[i] = hex.EncodeToString(pub[:])
	}

	nodePriv, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey (node):", err)
	}
	myKey, err := crypto.NewLockedValidatorKey(nodePriv.Bytes(), nil)
	if err != nil {
		t.Fatal("NewLockedValidatorKey:", err)
	}
	defer myKey.Destroy()

	validators, err := buildValidatorList(true, genesisHexKeys, myKey)
	if err != nil {
		t.Fatalf("buildValidatorList: %v", err)
	}
	if len(validators) != n {
		t.Fatalf("expected %d validators, got %d", n, len(validators))
	}
	for i, want := range genesisPubs {
		if !validators[i].Equals(want) {
			t.Errorf("validator[%d]: expected genesis key %s, got %s", i, want.ID(), validators[i].ID())
		}
	}
}
