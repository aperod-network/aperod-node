package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

func TestPrepareAndSignOneValidatorWitness(t *testing.T) {
	opts, priv := makeFixture(t)
	if err := prepareAndSign(opts); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(opts.outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("witness output mode = %o, want 600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(opts.outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var migration store.LPoDMigration
	if err := json.Unmarshal(raw, &migration); err != nil {
		t.Fatal(err)
	}
	migration.TrustedValidators = []crypto.ValidatorPubKey{priv.Public()}
	if err := migration.VerifyAuthorization(); err != nil {
		t.Fatalf("written witness authorization is invalid: %v", err)
	}
	if migration.Version != 3 || migration.Height != opts.height ||
		migration.NominalSnapshot == nil ||
		migration.NominalSnapshot.NominalCirculatingBefore != 6_230_000_000*lpod.Unit {
		t.Fatalf("written witness does not preserve the explicit nominal values")
	}
	if bytes.Contains(raw, priv.Bytes()) {
		t.Fatal("witness unexpectedly contains private key bytes")
	}
}

func TestPrepareAndSignAcceptsModernIndexedGenesis(t *testing.T) {
	opts, _ := makeFixtureWithGenesisIndex(t, true)
	if err := prepareAndSign(opts); err != nil {
		t.Fatalf("modern-indexed genesis should pass the same path as the store verifier: %v", err)
	}
}

func TestPrepareAndSignRequiresSafetyAcknowledgements(t *testing.T) {
	opts, _ := makeFixture(t)
	opts.acceptOperatorRegistryAssumption = false
	if err := prepareAndSign(opts); err == nil {
		t.Fatal("missing operator-registry acknowledgement unexpectedly allowed signing")
	}
	if _, err := os.Lstat(opts.outputPath); !os.IsNotExist(err) {
		t.Fatalf("missing operator-registry acknowledgement created output: %v", err)
	}

	opts, _ = makeFixture(t)
	opts.confirmFrozenCopy = false
	if err := prepareAndSign(opts); err == nil {
		t.Fatal("missing frozen-copy confirmation unexpectedly allowed signing")
	}
	if _, err := os.Lstat(opts.outputPath); !os.IsNotExist(err) {
		t.Fatalf("missing frozen-copy confirmation created output: %v", err)
	}
}

func TestPrepareAndSignRejectsUnapprovedNominalAmountsBeforeReadingKey(t *testing.T) {
	tests := []struct {
		name        string
		circulating uint64
		eligible    uint64
	}{
		{
			name:        "plausible higher circulation",
			circulating: 6_317_000_000,
			eligible:    approvedEligibleNominalAPRO,
		},
		{
			name:        "plausible higher eligible allocation",
			circulating: approvedNominalCirculatingAPRO,
			eligible:    1_100_000_000,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts, _ := makeFixture(t)
			opts.nominalCirculatingAPRO = test.circulating
			opts.eligibleNominalAPRO = test.eligible
			if err := os.Remove(opts.keyPath); err != nil {
				t.Fatal(err)
			}
			err := prepareAndSign(opts)
			if err == nil || !strings.Contains(err.Error(), "approved operator declarations") {
				t.Fatalf("unapproved declaration should fail before key access, got: %v", err)
			}
			if _, err := os.Lstat(opts.outputPath); !os.IsNotExist(err) {
				t.Fatalf("unapproved declaration created output: %v", err)
			}
		})
	}
}

func TestPrepareAndSignRejectsInvalidFinalityCertificate(t *testing.T) {
	opts, _ := makeFixture(t)
	db, err := store.Open(opts.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := db.LoadFinalityCertificate()
	if err != nil {
		t.Fatal(err)
	}
	cert.Votes[0].Signature[0] ^= 0xff
	if err := db.SaveFinalityCertificate(*cert); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepareAndSign(opts); err == nil {
		t.Fatal("invalid finality vote unexpectedly authorized a witness")
	}
	if _, err := os.Lstat(opts.outputPath); !os.IsNotExist(err) {
		t.Fatalf("invalid certificate created output: %v", err)
	}
}

func TestPrepareAndSignRejectsMismatchedValidatorPool(t *testing.T) {
	opts, _ := makeFixture(t)
	opts.validatorRemainingNAPRO++
	if err := prepareAndSign(opts); err == nil {
		t.Fatal("mismatched validator pool unexpectedly authorized a witness")
	}
	if _, err := os.Lstat(opts.outputPath); !os.IsNotExist(err) {
		t.Fatalf("mismatched pool created output: %v", err)
	}
}

func TestPrepareAndSignRejectsUncertainValidatorSet(t *testing.T) {
	opts, _ := makeFixture(t)
	raw, err := os.ReadFile(opts.registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot anchoredRegistry
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	_, other, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Registry.Validators[other.Hex()] = &core.ValidatorEntry{
		PubKey:    other,
		StakeNAPR: core.MinStakeNAPR,
		Status:    core.ValidatorActive,
	}
	raw, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.registryPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareAndSign(opts); err == nil {
		t.Fatal("multiple-validator snapshot unexpectedly authorized a witness")
	}
	if _, err := os.Lstat(opts.outputPath); !os.IsNotExist(err) {
		t.Fatalf("uncertain validator set created output: %v", err)
	}
}

func TestRead0600RegularFileRejectsSymlinkAndLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := read0600RegularFile(key); err == nil {
		t.Fatal("key file with loose permissions was accepted")
	}
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "key-link")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if _, err := read0600RegularFile(link); err == nil {
		t.Fatal("symlinked validator key was accepted")
	}
}

func TestValidateNominalAmounts(t *testing.T) {
	tests := []struct {
		name        string
		circulating uint64
		eligible    uint64
	}{
		{name: "eligible exceeds circulating", circulating: 999, eligible: 1_000},
		{name: "circulating exceeds cap", circulating: store.LPoDV3NominalCirculationMaxNAPRO + 1, eligible: lpod.InitialNAPRO},
		{name: "eligible below one billion", circulating: 2 * lpod.InitialNAPRO, eligible: lpod.InitialNAPRO - 1},
		{name: "eligible exceeds public allocation maximum", circulating: 10_000_000_000 * lpod.Unit, eligible: 7_000_000_001 * lpod.Unit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateNominalAmounts(test.circulating, test.eligible); err == nil {
				t.Fatal("invalid nominal declaration was accepted")
			}
		})
	}
}

func TestAPROToNAPROUsesExactIntegerUnits(t *testing.T) {
	got, err := aproToNAPRO(6_230_000_000)
	if err != nil {
		t.Fatal(err)
	}
	want := uint64(623_000_000_000_000_000)
	if got != want {
		t.Fatalf("6.23B APRO converted to %d nAPRO, want %d", got, want)
	}
}

func TestRunRequiresExplicitFlags(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("missing operator-supplied witness values were accepted")
	}
}

func makeFixture(t *testing.T) (options, crypto.ValidatorPrivKey) {
	return makeFixtureWithGenesisIndex(t, false)
}

func makeFixtureWithGenesisIndex(t *testing.T, modern bool) (options, crypto.ValidatorPrivKey) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "copied-chain")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}

	genesis := core.Block{Header: core.BlockHeader{
		Height:       0,
		Timestamp:    1,
		MerkleRoot:   core.MerkleRoot(nil),
		ValidatorPub: pub,
	}}
	genesisIndexHash := legacyGenesisID(&genesis.Header)
	if modern {
		if err := genesis.Header.Sign(priv); err != nil {
			t.Fatal(err)
		}
		genesisIndexHash = genesis.Hash()
	} else {
		genesisSig, err := priv.Sign(genesisIndexHash)
		if err != nil {
			t.Fatal(err)
		}
		genesis.Header.Signature = genesisSig
	}
	genesisRaw, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(genesisIndexHash, 0, genesisRaw); err != nil {
		t.Fatal(err)
	}

	parent := core.Block{Header: core.BlockHeader{
		Height:       1,
		PrevHash:     genesisIndexHash,
		Timestamp:    2,
		MerkleRoot:   core.MerkleRoot(nil),
		ValidatorPub: pub,
	}}
	if err := parent.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	parentHash := parent.Hash()
	parentRaw, err := json.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(parentHash, 1, parentRaw); err != nil {
		t.Fatal(err)
	}
	heightBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(heightBytes, 1)
	if err := db.PutMeta("tip/hash", parentHash[:]); err != nil {
		t.Fatal(err)
	}
	if err := db.PutMeta("tip/height", heightBytes); err != nil {
		t.Fatal(err)
	}
	pool := 2_000_000_000 * lpod.Unit
	if err := db.StoreStakingPoolRemaining(pool); err != nil {
		t.Fatal(err)
	}
	finalityMessage := crypto.HashBytes([]byte("aperod/finalize/v1"), parentHash[:])
	finalitySignature, err := priv.Sign(finalityMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{
		Version:   1,
		Height:    1,
		BlockHash: parentHash,
		Votes: []store.FinalityVote{{
			Validator: pub,
			Signature: finalitySignature,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	registry := anchoredRegistry{
		Height: 1,
		Hash:   parentHash,
		Registry: core.RegistrySnapshot{
			Validators: map[string]*core.ValidatorEntry{
				pub.Hex(): {
					PubKey:    pub,
					StakeNAPR: core.MinStakeNAPR,
					Status:    core.ValidatorActive,
				},
			},
		},
	}
	registryPath := filepath.Join(root, "validator-set.json")
	registryJSON, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, registryJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	keyPath := filepath.Join(root, "validator.key")
	if err := os.WriteFile(keyPath, priv.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	return options{
		dbPath:                           dbPath,
		outputPath:                       filepath.Join(root, "witness.json"),
		keyPath:                          keyPath,
		registryPath:                     registryPath,
		height:                           2,
		parentHash:                       parentHash,
		genesis:                          genesis.Hash(),
		nominalCirculatingAPRO:           approvedNominalCirculatingAPRO,
		eligibleNominalAPRO:              approvedEligibleNominalAPRO,
		validatorRemainingNAPRO:          pool,
		expectedValidator:                pub,
		registryHeight:                   1,
		registryHash:                     parentHash,
		acceptOperatorRegistryAssumption: true,
		confirmFrozenCopy:                true,
	}, priv
}
