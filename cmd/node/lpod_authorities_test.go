package main

import (
	"encoding/hex"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

func newLPoDAuthority(t *testing.T) crypto.ValidatorPubKey {
	t.Helper()
	_, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("generate validator authority:", err)
	}
	return pub
}

func TestLPoDV2WitnessRejectsDuplicateJSONFields(t *testing.T) {
	for _, raw := range []string{
		`{"version":2,"version":1}`,
		`{"version":2,"attestations":[{"validator":"a","validator":"b"}]}`,
	} {
		if err := rejectDuplicateLPoDJSONFields([]byte(raw)); err == nil {
			t.Fatalf("duplicate version-2 witness field accepted: %s", raw)
		}
	}
	if err := rejectDuplicateLPoDJSONFields([]byte(`{"version":2,"attestations":[]}`)); err != nil {
		t.Fatalf("valid version-2 witness shape rejected: %v", err)
	}
	for _, raw := range []string{
		`{"version":2,"unrecognized_authority_override":true}`,
		`{"version":2,"version":1}`,
	} {
		if _, err := decodeLPoDMigrationWitness([]byte(raw)); err == nil {
			t.Fatalf("unknown or colliding version-2 field accepted: %s", raw)
		}
	}
}

func TestGuardLPoDMigrationAuthoritiesMatchingSingleton(t *testing.T) {
	pub := newLPoDAuthority(t)
	if err := guardLPoDMigrationAuthorities(
		[]crypto.ValidatorPubKey{pub},
		[]string{hex.EncodeToString(pub)},
	); err != nil {
		t.Fatalf("matching singleton authority set rejected: %v", err)
	}
}

func TestGuardLPoDMigrationAuthoritiesRejectsMismatch(t *testing.T) {
	effective := newLPoDAuthority(t)
	configured := newLPoDAuthority(t)
	if err := guardLPoDMigrationAuthorities(
		[]crypto.ValidatorPubKey{effective},
		[]string{hex.EncodeToString(configured)},
	); err == nil {
		t.Fatal("mismatched authority sets accepted")
	}
}

func TestGuardLPoDMigrationAuthoritiesRejectsZeroGenesisAuthority(t *testing.T) {
	zero := make([]byte, 32)
	if err := guardLPoDMigrationAuthorities(
		[]crypto.ValidatorPubKey{crypto.ValidatorPubKey(zero)},
		[]string{hex.EncodeToString(zero)},
	); err == nil {
		t.Fatal("all-zero genesis authority accepted")
	}
}

func TestGuardLPoDMigrationAuthoritiesRejectsDuplicateGenesisAuthority(t *testing.T) {
	pub := newLPoDAuthority(t)
	encoded := hex.EncodeToString(pub)
	if err := guardLPoDMigrationAuthorities(
		[]crypto.ValidatorPubKey{pub},
		[]string{encoded, encoded},
	); err == nil {
		t.Fatal("duplicate genesis authority accepted")
	}
}

func TestGuardLPoDMigrationAuthoritiesRejectsEmptyAndMalformedGenesis(t *testing.T) {
	pub := newLPoDAuthority(t)
	tests := []struct {
		name        string
		authorities []string
	}{
		{name: "empty"},
		{name: "invalid hex", authorities: []string{"not-hex"}},
		{name: "invalid key length", authorities: []string{hex.EncodeToString([]byte{1, 2, 3})}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := guardLPoDMigrationAuthorities([]crypto.ValidatorPubKey{pub}, tc.authorities); err == nil {
				t.Fatal("empty or malformed genesis authorities accepted")
			}
		})
	}
}

func TestGuardLPoDMigrationAuthoritiesAllowsPermutation(t *testing.T) {
	first := newLPoDAuthority(t)
	second := newLPoDAuthority(t)
	third := newLPoDAuthority(t)
	if err := guardLPoDMigrationAuthorities(
		[]crypto.ValidatorPubKey{first, second, third},
		[]string{
			hex.EncodeToString(third),
			hex.EncodeToString(first),
			hex.EncodeToString(second),
		},
	); err != nil {
		t.Fatalf("permuted matching authority set rejected: %v", err)
	}
}

func TestParseLPoDV2TrustAuthoritiesFailsClosed(t *testing.T) {
	pub := newLPoDAuthority(t)
	encoded := hex.EncodeToString(pub)
	tests := []struct {
		name  string
		input []string
	}{
		{name: "missing"},
		{name: "malformed", input: []string{"xyz"}},
		{name: "wrong length", input: []string{hex.EncodeToString([]byte{1, 2})}},
		{name: "zero key", input: []string{hex.EncodeToString(make([]byte, 32))}},
		{name: "duplicate", input: []string{encoded, encoded}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseLPoDV2TrustAuthorities(tc.input); err == nil {
				t.Fatal("invalid explicit v2 trust set accepted")
			}
		})
	}
	parsed, err := parseLPoDV2TrustAuthorities([]string{encoded})
	if err != nil || len(parsed) != 1 || !parsed[0].Equals(pub) {
		t.Fatalf("valid exact public key rejected: %v", err)
	}
}

func TestLPoDV2WitnessRejectsDifferentPeerTrustAnchor(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	configured, err := parseLPoDV2TrustAuthorities([]string{hex.EncodeToString(pub)})
	if err != nil {
		t.Fatal(err)
	}
	m := &store.LPoDMigration{
		Version: 2, PositionLifecycleVersion: 1, Height: store.LPoDV2ActivationHeight,
		Genesis: crypto.HashBytes([]byte("genesis")), ParentHash: crypto.HashBytes([]byte("parent")),
		SnapshotRoot:         crypto.HashBytes([]byte("snapshot")),
		TrustAssumption:      store.LPoDV2TrustAssumption,
		HistoricalIssued:     1_000_000_000 * lpod.Unit,
		HistoricalSaleRemain: 3_000_000_000 * lpod.Unit,
		ValidatorRemaining:   2_000_000_000 * lpod.Unit,
		TrustedValidators:    configured,
	}
	m.ReconciliationRoot = m.Root()
	sig, err := priv.Sign(m.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	m.Attestations = []store.LPoDAttestation{{Validator: pub, Signature: sig}}
	if err := m.VerifyAuthorization(); err != nil {
		t.Fatalf("matching explicit authority rejected: %v", err)
	}

	peerAuthority := newLPoDAuthority(t)
	peerConfig, err := parseLPoDV2TrustAuthorities([]string{hex.EncodeToString(peerAuthority)})
	if err != nil {
		t.Fatal(err)
	}
	m.TrustedValidators = peerConfig
	if err := m.VerifyAuthorization(); err == nil {
		t.Fatal("peer with a different configured trust anchor accepted witness")
	}
}

func TestLPoDV2DoesNotUseInvalidConsensusValidatorSet(t *testing.T) {
	if err := guardLPoDConsensusAuthorities([]crypto.ValidatorPubKey{make(crypto.ValidatorPubKey, 32)}); err == nil {
		t.Fatal("zero genesis consensus validator accepted for v2 operation")
	}
	if err := guardLPoDConsensusAuthorities(nil); err == nil {
		t.Fatal("missing consensus validator set accepted for v2 operation")
	}
	pub := newLPoDAuthority(t)
	if err := guardLPoDConsensusAuthorities([]crypto.ValidatorPubKey{pub, pub}); err == nil {
		t.Fatal("duplicate consensus validator accepted for v2 operation")
	}
}

func TestLPoDV2TrustOptInRequiresWitness(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, authority, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain := core.NewChain()
	if _, err := loadLPoDMigration("", db, chain, []crypto.ValidatorPubKey{authority}, nil,
		[]string{hex.EncodeToString(authority)}, nil); err == nil {
		t.Fatal("configured v2 trust authority silently opted in without a witness")
	}
}

func TestLPoDV3TrustOptInRequiresWitness(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, authority, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain := core.NewChain()
	if _, err := loadLPoDMigration("", db, chain, []crypto.ValidatorPubKey{authority}, nil,
		nil, []string{hex.EncodeToString(authority)}); err == nil {
		t.Fatal("configured v3 trust authority silently opted in without a witness")
	}
	if _, err := parseLPoDV3TrustAuthorities([]string{hex.EncodeToString(make([]byte, 32))}); err == nil {
		t.Fatal("zero v3 authority accepted")
	}
}

func TestLPoDPastUnfundedWitnessFailsClosed(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := &store.LPoDMigration{Version: 2, Height: 20}
	for _, tip := range []uint64{20, 21} {
		hash := crypto.HashBytes([]byte{byte(tip)})
		if err := db.PutTip(hash, tip); err != nil {
			t.Fatal(err)
		}
		if err := requireLPoDFundedTip(db, m, tip); err == nil {
			t.Fatalf("unfunded v2 witness accepted at tip %d", tip)
		}
	}
	if err := requireLPoDFundedTip(db, m, 19); err != nil {
		t.Fatalf("future v2 witness rejected before activation height: %v", err)
	}
}
