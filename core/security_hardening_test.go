package core

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
)

const testSecurityHardeningHeight = 3_800_000

func TestSecurityHardeningRejectsOversizedBlocksAtActivation(t *testing.T) {
	verifier := NewTxVerifier(nil)
	verifier.SetSecurityHardeningActivationHeight(testSecurityHardeningHeight)
	block := &Block{
		Header: BlockHeader{Height: testSecurityHardeningHeight},
		Txs:    make([]Transaction, MaxTransactionsPerBlock+1),
	}
	if err := verifier.VerifyBlock(block); err == nil ||
		!strings.Contains(err.Error(), "too many txs") {
		t.Fatalf("VerifyBlock error = %v, want transaction-count rejection", err)
	}

	block.Header.Height--
	if err := verifier.VerifyBlock(block); err != nil {
		t.Fatalf("historical block just before activation was rejected: %v", err)
	}
}

func TestSecurityHardeningRejectsOversizedSerializedBlock(t *testing.T) {
	verifier := NewTxVerifier(nil)
	verifier.SetSecurityHardeningActivationHeight(testSecurityHardeningHeight)
	block := &Block{
		Header: BlockHeader{Height: testSecurityHardeningHeight},
		Txs: []Transaction{{
			Extra: make([]byte, MaxBlockBytes),
		}},
	}
	if err := verifier.VerifyBlock(block); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("VerifyBlock error = %v, want block-size rejection", err)
	}
}

func TestSecurityHardeningRejectsDuplicateOutputKeys(t *testing.T) {
	var pub crypto.Point32
	pub[0] = 1
	block := &Block{Header: BlockHeader{Height: testSecurityHardeningHeight}}
	block.Txs = []Transaction{
		{Outputs: []Output{{OneTimePub: pub}}},
		{Outputs: []Output{{OneTimePub: pub}}},
	}
	verifier := NewTxVerifier(nil)
	if err := verifier.verifySecurityHardeningRules(block); err == nil ||
		!strings.Contains(err.Error(), "duplicate one-time public key") {
		t.Fatalf("hardening-rule error = %v, want duplicate-key rejection", err)
	}
}

func TestSecurityHardeningRejectsExistingOutputKeyCollision(t *testing.T) {
	var pub crypto.Point32
	pub[0] = 9
	utxos := NewUTXOSet()
	utxos.Add(&UTXO{TxHash: crypto.Hash32{1}, OneTimePub: pub})

	block := &Block{
		Header: BlockHeader{Height: testSecurityHardeningHeight},
		Txs:    []Transaction{{Outputs: []Output{{OneTimePub: pub}}}},
	}
	verifier := NewTxVerifier(utxos)
	if err := verifier.verifySecurityHardeningRules(block); err == nil ||
		!strings.Contains(err.Error(), "one-time public key already exists") {
		t.Fatalf("hardening-rule error = %v, want existing-key collision rejection", err)
	}
}

func TestSecurityHardeningFailsClosedWhenRingIndexCannotBeRead(t *testing.T) {
	utxos := NewUTXOSet()
	utxos.ringMembers = hardeningFailingRingMemberStore{}
	var pub crypto.Point32
	pub[0] = 12
	block := &Block{
		Header: BlockHeader{Height: testSecurityHardeningHeight},
		Txs:    []Transaction{{Outputs: []Output{{OneTimePub: pub}}}},
	}
	verifier := NewTxVerifier(utxos)
	if err := verifier.verifySecurityHardeningRules(block); err == nil ||
		!strings.Contains(err.Error(), "could not check one-time public key uniqueness") {
		t.Fatalf("ring-index lookup error = %v, want fail-closed error", err)
	}
}

func TestUpdateEpochBreaksStakeTiesDeterministicallyAtActivation(t *testing.T) {
	pubs := make([]crypto.ValidatorPubKey, MaxValidators+1)
	for i := range pubs {
		raw := make([]byte, 32)
		raw[30], raw[31] = byte(i>>8), byte(i+1)
		pub, err := crypto.ValidatorPubKeyFromBytes(raw)
		if err != nil {
			t.Fatalf("ValidatorPubKeyFromBytes(%d): %v", i, err)
		}
		pubs[i] = pub
	}

	var want []crypto.ValidatorPubKey
	for _, pub := range pubs {
		want = append(want, pub)
	}
	sortValidatorKeys(want)
	want = want[:MaxValidators]

	for _, reverse := range []bool{false, true} {
		registry := NewValidatorRegistry()
		registry.SetSecurityHardeningActivationHeight(testSecurityHardeningHeight)
		for step := range pubs {
			i := step
			if reverse {
				i = len(pubs) - 1 - step
			}
			pub := pubs[i]
			registry.validators[pub.Hex()] = &ValidatorEntry{
				PubKey:    pub,
				StakeNAPR: 100,
				Status:    ValidatorActive,
			}
		}

		got := registry.UpdateEpoch(testSecurityHardeningHeight)
		if len(got) != MaxValidators {
			t.Fatalf("active validator count = %d, want %d", len(got), MaxValidators)
		}
		for _, wantPub := range want {
			if !containsValidatorKey(got, wantPub) {
				t.Fatalf("deterministic active set omitted expected key %s", wantPub.Hex())
			}
		}
	}
}

func TestUpdateEpochUsesDeterministicPendingTieBreakAtActivation(t *testing.T) {
	pubs := make([]crypto.ValidatorPubKey, ChurnLimit+1)
	for i := range pubs {
		raw := make([]byte, 32)
		raw[30], raw[31] = byte(i>>8), byte(i+33)
		pub, err := crypto.ValidatorPubKeyFromBytes(raw)
		if err != nil {
			t.Fatalf("ValidatorPubKeyFromBytes(%d): %v", i, err)
		}
		pubs[i] = pub
	}
	want := append([]crypto.ValidatorPubKey(nil), pubs...)
	sortValidatorKeys(want)
	want = want[:ChurnLimit]

	registry := NewValidatorRegistry()
	registry.SetSecurityHardeningActivationHeight(testSecurityHardeningHeight)
	for _, pub := range pubs {
		registry.validators[pub.Hex()] = &ValidatorEntry{
			PubKey:    pub,
			StakeNAPR: 100,
			Status:    ValidatorPending,
		}
	}
	got := registry.UpdateEpoch(testSecurityHardeningHeight)
	if len(got) != ChurnLimit {
		t.Fatalf("activated validator count = %d, want churn limit %d", len(got), ChurnLimit)
	}
	for _, wantPub := range want {
		if !containsValidatorKey(got, wantPub) {
			t.Fatalf("deterministic pending selection omitted expected key %s", wantPub.Hex())
		}
	}
}

func sortValidatorKeys(keys []crypto.ValidatorPubKey) {
	sort.Slice(keys, func(i, j int) bool { return keys[i].Hex() < keys[j].Hex() })
}

func containsValidatorKey(keys []crypto.ValidatorPubKey, target crypto.ValidatorPubKey) bool {
	for _, key := range keys {
		if key.Hex() == target.Hex() {
			return true
		}
	}
	return false
}

type hardeningFailingRingMemberStore struct{}

func (hardeningFailingRingMemberStore) LookupRingMember(crypto.Point32) (*UTXO, error) {
	return nil, errors.New("simulated ring-member index read failure")
}

func (hardeningFailingRingMemberStore) SampleRingMembers(int, map[crypto.Point32]bool) ([]DecoyUTXO, error) {
	return nil, nil
}
