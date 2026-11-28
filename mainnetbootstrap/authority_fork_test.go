package mainnetbootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	chaincrypto "github.com/aperod/aperod/crypto"
)

func TestBuildSuccessorAuthorityForkPreservesHistoryAndBindsRegistry(t *testing.T) {
	identityBytes, err := hex.DecodeString(GenesisSignerPublicKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := chaincrypto.ValidatorPubKeyFromBytes(identityBytes)
	if err != nil {
		t.Fatal(err)
	}
	oldPrivate, oldPublic, err := chaincrypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	newPrivate, newPublic, err := chaincrypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	previous := core.ValidatorAuthorityFork{
		Version:             1,
		Genesis:             mustHash(CanonicalMainnetGenesisIndexHashHex),
		Height:              100,
		Parent:              chaincrypto.HashBytes([]byte("previous parent")),
		Identity:            identity,
		Replacement:         oldPublic,
		RegistrySHA256:      chaincrypto.HashBytes([]byte("previous registry")),
		LegacyWitnessSHA256: chaincrypto.HashBytes([]byte("legacy witness")),
	}
	previous.Possession, err = oldPrivate.Sign(previous.Message())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := core.RegistrySnapshot{
		Validators: map[string]*core.ValidatorEntry{
			identity.Hex(): {
				PubKey:    identity,
				StakeNAPR: core.MinStakeNAPR,
				Status:    core.ValidatorActive,
			},
		},
	}
	parent := chaincrypto.HashBytes([]byte("canonical checkpoint parent"))
	fork, registryDigest, err := BuildSuccessorAuthorityFork(
		&previous, 200, parent, snapshot, newPrivate, oldPublic.Hex(), newPublic.Hex(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := fork.Validate(); err != nil {
		t.Fatalf("new fork failed validation: %v", err)
	}
	if fork.Version != 2 || fork.Height != 201 || fork.Parent != parent ||
		fork.Previous == nil || fork.Previous.Height != previous.Height ||
		!fork.Identity.Equals(identity) || !fork.Replacement.Equals(newPublic) {
		t.Fatalf("unexpected successor transition: %+v", fork)
	}
	if got := fork.Authority(identity, 200); !got.Equals(oldPublic) {
		t.Fatalf("historical signer at checkpoint = %s, want %s", got.Hex(), oldPublic.Hex())
	}
	if got := fork.Authority(identity, 201); !got.Equals(newPublic) {
		t.Fatalf("successor signer at activation = %s, want %s", got.Hex(), newPublic.Hex())
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	expectedDigest := sha256.Sum256(raw)
	if registryDigest != hex.EncodeToString(expectedDigest[:]) ||
		fork.RegistrySHA256 != expectedDigest {
		t.Fatal("successor fork did not bind the exact serialized registry snapshot")
	}
}

func TestBuildSuccessorAuthorityForkRejectsUnprovenTransitionInputs(t *testing.T) {
	identityBytes, err := hex.DecodeString(GenesisSignerPublicKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := chaincrypto.ValidatorPubKeyFromBytes(identityBytes)
	if err != nil {
		t.Fatal(err)
	}
	oldPrivate, oldPublic, err := chaincrypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	newPrivate, newPublic, err := chaincrypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_, otherPublic, err := chaincrypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	previous := core.ValidatorAuthorityFork{
		Version:             1,
		Genesis:             mustHash(CanonicalMainnetGenesisIndexHashHex),
		Height:              100,
		Parent:              chaincrypto.HashBytes([]byte("previous parent")),
		Identity:            identity,
		Replacement:         oldPublic,
		RegistrySHA256:      chaincrypto.HashBytes([]byte("previous registry")),
		LegacyWitnessSHA256: chaincrypto.HashBytes([]byte("legacy witness")),
	}
	previous.Possession, err = oldPrivate.Sign(previous.Message())
	if err != nil {
		t.Fatal(err)
	}
	active := core.RegistrySnapshot{
		Validators: map[string]*core.ValidatorEntry{
			identity.Hex(): {
				PubKey:    identity,
				StakeNAPR: core.MinStakeNAPR,
				Status:    core.ValidatorActive,
			},
		},
	}
	parent := chaincrypto.HashBytes([]byte("canonical parent"))
	tests := []struct {
		name        string
		checkpoint  uint64
		parent      chaincrypto.Hash32
		registry    core.RegistrySnapshot
		key         chaincrypto.ValidatorPrivKey
		expectedOld string
		expectedNew string
	}{
		{"wrong current signer", 200, parent, active, newPrivate, otherPublic.Hex(), newPublic.Hex()},
		{"wrong replacement pin", 200, parent, active, newPrivate, oldPublic.Hex(), otherPublic.Hex()},
		{"no funded active identity", 200, parent, core.RegistrySnapshot{}, newPrivate, oldPublic.Hex(), newPublic.Hex()},
		{"below minimum stake", 200, parent, core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{
			identity.Hex(): {PubKey: identity, StakeNAPR: core.MinStakeNAPR - 1, Status: core.ValidatorActive},
		}}, newPrivate, oldPublic.Hex(), newPublic.Hex()},
		{"additional active validator", 200, parent, core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{
			identity.Hex():    {PubKey: identity, StakeNAPR: core.MinStakeNAPR, Status: core.ValidatorActive},
			otherPublic.Hex(): {PubKey: otherPublic, StakeNAPR: core.MinStakeNAPR, Status: core.ValidatorActive},
		}}, newPrivate, oldPublic.Hex(), newPublic.Hex()},
		{"synthetic registry entry", 200, parent, core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{
			identity.Hex(): {PubKey: identity, StakeNAPR: core.MinStakeNAPR, Status: core.ValidatorActive, Seeded: true},
		}}, newPrivate, oldPublic.Hex(), newPublic.Hex()},
		{"zero parent", 200, chaincrypto.Hash32{}, active, newPrivate, oldPublic.Hex(), newPublic.Hex()},
		{"same key as current signer", 200, parent, active, oldPrivate, oldPublic.Hex(), oldPublic.Hex()},
		{"checkpoint precedes previous fork", 99, parent, active, newPrivate, oldPublic.Hex(), newPublic.Hex()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := BuildSuccessorAuthorityFork(
				&previous, test.checkpoint, test.parent, test.registry,
				test.key, test.expectedOld, test.expectedNew,
			); err == nil {
				t.Fatal("unproven authority transition input was accepted")
			}
		})
	}
}
