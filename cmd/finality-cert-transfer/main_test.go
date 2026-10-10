package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type transferFixture struct {
	dbPath       string
	certPath     string
	registryPath string
	expected     crypto.ValidatorPubKey
	genesis      crypto.Hash32
	cert         store.FinalityCertificate
}

func makeTransferFixture(t *testing.T) transferFixture {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chain.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	genesis := signedTransferBlock(t, priv, pub, 0, crypto.Hash32{})
	parent := signedTransferBlock(t, priv, pub, 1, genesis.Hash())
	for _, block := range []*core.Block{genesis, parent} {
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.PutRawBlock(block.Hash(), block.Header.Height, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PutTip(parent.Hash(), 1); err != nil {
		t.Fatal(err)
	}
	parentHash := parent.Hash()
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), parentHash[:])
	signature, err := priv.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	cert := store.FinalityCertificate{
		Version: 1, Height: 1, BlockHash: parent.Hash(),
		Votes: []store.FinalityVote{{Validator: pub, Signature: signature}},
	}
	if err := db.SaveFinalityCertificate(cert); err != nil {
		t.Fatal(err)
	}
	registry := anchoredRegistry{
		Height: 0, Hash: genesis.Hash(),
		Registry: core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{
			pub.Hex(): {PubKey: pub, StakeNAPR: core.MinStakeNAPR, Status: core.ValidatorActive},
		}},
	}
	certPath := filepath.Join(dir, "cert.json")
	registryPath := filepath.Join(dir, "registry.json")
	writeJSONFixture(t, certPath, cert)
	writeJSONFixture(t, registryPath, registry)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return transferFixture{dbPath: dbPath, certPath: certPath, registryPath: registryPath, expected: pub, genesis: genesis.Hash(), cert: cert}
}

func signedTransferBlock(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey, height uint64, previous crypto.Hash32) *core.Block {
	t.Helper()
	block := &core.Block{Header: core.BlockHeader{
		Height: height, PrevHash: previous, Timestamp: int64(height + 1),
		ValidatorPub: pub, MerkleRoot: core.MerkleRoot(nil),
	}}
	if err := block.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	return block
}

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestValidCertificateTransfer(t *testing.T) {
	fixture := makeTransferFixture(t)
	exportPath := filepath.Join(t.TempDir(), "export.json")
	if err := exportCertificate(fixture.dbPath, exportPath, fixture.expected, fixture.genesis); err != nil {
		t.Fatalf("export: %v", err)
	}
	info, err := os.Stat(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("export mode = %04o, want 0600", info.Mode().Perm())
	}
	relayPath := filepath.Join(t.TempDir(), "relay.db")
	copyCanonicalChain(t, fixture.dbPath, relayPath, 1)
	fixture.certPath = exportPath
	if err := importCertificate(relayPath, fixture.certPath, fixture.registryPath, fixture.expected, fixture.genesis, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	db, err := store.OpenReadOnly(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.LoadFinalityCertificate()
	if err != nil || got == nil || got.Height != fixture.cert.Height || got.BlockHash != fixture.cert.BlockHash {
		t.Fatalf("imported certificate = %#v, error %v", got, err)
	}
}

func copyCanonicalChain(t *testing.T, sourcePath, targetPath string, tipHeight uint64) {
	t.Helper()
	source, err := store.OpenReadOnly(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	type blockData struct {
		hash crypto.Hash32
		raw  []byte
	}
	blocks := make([]blockData, 0, tipHeight+1)
	for height := uint64(0); height <= tipHeight; height++ {
		hash, found, err := source.GetCanonicalHash(height)
		if err != nil || !found {
			t.Fatalf("source index at %d: found=%v err=%v", height, found, err)
		}
		raw, err := source.GetRawBlock(hash)
		if err != nil || raw == nil {
			t.Fatalf("source block at %d: %v", height, err)
		}
		blocks = append(blocks, blockData{hash: hash, raw: raw})
	}
	tipHash, height, err := source.GetTip()
	if err != nil || height != tipHeight {
		t.Fatalf("source tip: height=%d err=%v", height, err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	target, err := store.Open(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	for height, block := range blocks {
		if err := target.PutRawBlock(block.hash, uint64(height), block.raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := target.PutTip(tipHash, tipHeight); err != nil {
		t.Fatal(err)
	}
}

func TestImportRejectsTamperedSignature(t *testing.T) {
	fixture := makeTransferFixture(t)
	fixture.cert.Votes[0].Signature[0] ^= 0xff
	writeJSONFixture(t, fixture.certPath, fixture.cert)
	assertImportFailureContains(t, fixture, "invalid expected-validator signature")
}

func TestImportRejectsWrongCertificateHash(t *testing.T) {
	fixture := makeTransferFixture(t)
	fixture.cert.BlockHash[0] ^= 0xff
	writeJSONFixture(t, fixture.certPath, fixture.cert)
	assertImportFailureContains(t, fixture, "exact database tip")
}

func TestImportRejectsRegistryMismatch(t *testing.T) {
	fixture := makeTransferFixture(t)
	var registry anchoredRegistry
	raw, err := os.ReadFile(fixture.registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		t.Fatal(err)
	}
	registry.Registry.Validators[fixture.expected.Hex()].Status = core.ValidatorUnbonding
	writeJSONFixture(t, fixture.registryPath, registry)
	assertImportFailureContains(t, fixture, "expected active validator")
}

func TestImportRejectsExistingNewerCertificate(t *testing.T) {
	fixture := makeTransferFixture(t)
	db, err := store.Open(fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	newer := fixture.cert
	newer.Height++
	if err := db.SaveFinalityCertificate(newer); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	assertImportFailureContains(t, fixture, "newer finality certificate")
}

func TestVerifyLegacyIndexedGenesis(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	block := &core.Block{Header: core.BlockHeader{
		Height: 0, Timestamp: 1, ValidatorPub: pub, MerkleRoot: core.MerkleRoot(nil),
	}}
	legacyHash := legacyGenesisID(&block.Header)
	block.Header.Signature, err = priv.Sign(legacyHash)
	if err != nil {
		t.Fatal(err)
	}
	modernHash := block.Hash()
	if modernHash == legacyHash {
		t.Fatal("fixture unexpectedly has identical modern and legacy hashes")
	}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "legacy-genesis.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PutRawBlock(legacyHash, 0, raw); err != nil {
		t.Fatal(err)
	}
	if err := verifyExpectedGenesis(db, modernHash); err != nil {
		t.Fatalf("legacy-indexed genesis was rejected: %v", err)
	}
	if err := verifyExpectedGenesis(db, legacyHash); err == nil {
		t.Fatal("legacy-indexed genesis accepted the indexed legacy hash as expected modern hash")
	}
}

func assertImportFailureContains(t *testing.T, fixture transferFixture, want string) {
	t.Helper()
	err := importCertificate(fixture.dbPath, fixture.certPath, fixture.registryPath, fixture.expected, fixture.genesis, false)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("import error = %v, want containing %q", err, want)
	}
}
