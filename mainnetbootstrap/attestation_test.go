package mainnetbootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	chaincrypto "github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type testSigner struct {
	private chaincrypto.ValidatorPrivKey
}

func TestCheckpointRegistryEvidenceBindsStakeAndRejectsInvalidRecords(t *testing.T) {
	_, pub, err := chaincrypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	s := validStatement()
	genesisBytes, err := hex.DecodeString(CanonicalMainnetGenesisHeaderHashHex)
	if err != nil {
		t.Fatal(err)
	}
	var genesis chaincrypto.Hash32
	copy(genesis[:], genesisBytes)
	tip := chaincrypto.HashBytes([]byte("canonical tip"))
	entry := &core.ValidatorEntry{PubKey: pub, Status: core.ValidatorActive, StakeNAPR: core.MinStakeNAPR}
	audit := store.LPoDBlockAudit{
		Available: true, Height: s.CheckpointHeight, BlockHash: tip, FundingGenesis: genesis,
		RegistryAfter: &core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{pub.Hex(): entry}},
	}
	first, err := checkpointRegistryDigest(s, tip, &audit)
	if err != nil || !isLowerHash(first) {
		t.Fatalf("valid registry rejected: %q %v", first, err)
	}
	s.CheckpointIndexHash = hex.EncodeToString(tip[:])
	s.RegistrySHA256 = first
	if err := VerifyRegistrySnapshot(s, tip, *audit.RegistryAfter); err != nil {
		t.Fatalf("matching restored registry rejected: %v", err)
	}
	if err := VerifyRegistrySnapshot(s, chaincrypto.HashBytes([]byte("other tip")), *audit.RegistryAfter); err == nil {
		t.Fatal("snapshot accepted against another tip")
	}
	entry.StakeNAPR++
	if err := VerifyRegistrySnapshot(s, tip, *audit.RegistryAfter); err == nil {
		t.Fatal("restored registry with changed stake accepted")
	}
	second, err := checkpointRegistryDigest(s, tip, &audit)
	if err != nil || first == second {
		t.Fatalf("stake change not bound: %q %v", second, err)
	}
	entry.Seeded = true
	if _, err := checkpointRegistryDigest(s, tip, &audit); err == nil {
		t.Fatal("synthetic registry accepted")
	}
	entry.Seeded = false
	audit.Height++
	if _, err := checkpointRegistryDigest(s, tip, &audit); err == nil {
		t.Fatal("wrong-height registry accepted")
	}
}

func (s testSigner) Public() chaincrypto.ValidatorPubKey {
	return s.private.Public()
}

func (s testSigner) Sign(hash chaincrypto.Hash32) ([]byte, error) {
	return s.private.Sign(hash)
}

func validStatement() Statement {
	return Statement{
		SchemaVersion:                   SchemaVersion,
		Network:                         Network,
		ChainID:                         ChainID,
		TrustModel:                      TrustModel,
		BootstrapSignerPublicKeyHex:     BootstrapSignerPublicKeyHex,
		GenesisHeaderHash:               CanonicalMainnetGenesisHeaderHashHex,
		GenesisIndexHash:                CanonicalMainnetGenesisIndexHashHex,
		GenesisSignatureHashKind:        "header_hash",
		GenesisSignerPublicKeyHex:       GenesisSignerPublicKeyHex,
		CheckpointHeight:                123,
		CheckpointHeaderHash:            strings.Repeat("3", 64),
		CheckpointIndexHash:             strings.Repeat("4", 64),
		CheckpointValidatorPublicKeyHex: GenesisSignerPublicKeyHex,
		CheckpointSignatureLength:       64,
		CheckpointSignatureHashKind:     "header_hash",
		ValidatorAuthorityForkSHA256:    strings.Repeat("6", 64),
		ProtocolSaleMigrationSHA256:     strings.Repeat("7", 64),
		LPoDMigrationWitnessSHA256:      strings.Repeat("8", 64),
		SnapshotTreeSHA256:              strings.Repeat("5", 64),
	}
}

func TestStatementHashIsStable(t *testing.T) {
	statement := validStatement()
	first, err := statementHash(statement)
	if err != nil {
		t.Fatal(err)
	}
	second, err := statementHash(statement)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same statement produced different hashes")
	}
}

func TestStatementBindsKeyImageAnchorAsAnAllOrNothingTuple(t *testing.T) {
	statement := validStatement()
	statement.KeyImageAnchorSHA256 = strings.Repeat("a", 64)
	statement.KeyImageAnchorHeight = 100
	statement.KeyImageAnchorBlockHash = strings.Repeat("b", 64)
	if err := statement.Validate(); err != nil {
		t.Fatalf("valid earlier key-image anchor rejected: %v", err)
	}

	statement.KeyImageAnchorBlockHash = ""
	if err := statement.Validate(); err == nil {
		t.Fatal("partial key-image anchor tuple accepted")
	}

	statement = validStatement()
	statement.KeyImageAnchorSHA256 = strings.Repeat("A", 64)
	statement.KeyImageAnchorHeight = 100
	statement.KeyImageAnchorBlockHash = strings.Repeat("b", 64)
	if err := statement.Validate(); err == nil {
		t.Fatal("malformed key-image anchor digest accepted")
	}

	statement = validStatement()
	statement.KeyImageAnchorSHA256 = strings.Repeat("a", 64)
	statement.KeyImageAnchorHeight = statement.CheckpointHeight
	statement.KeyImageAnchorBlockHash = strings.Repeat("b", 64)
	if err := statement.Validate(); err == nil {
		t.Fatal("key-image anchor at/after checkpoint accepted")
	}
}

func TestStatementRejectsWrongNetworkAndMalformedHashes(t *testing.T) {
	statement := validStatement()
	statement.Network = "testnet"
	if err := statement.Validate(); err == nil {
		t.Fatal("test network identity accepted")
	}

	statement = validStatement()
	statement.CheckpointHeaderHash = strings.Repeat("A", 64)
	if err := statement.Validate(); err == nil {
		t.Fatal("uppercase hash accepted")
	}

	statement = validStatement()
	statement.CheckpointHeight = 0
	if err := statement.Validate(); err == nil {
		t.Fatal("zero checkpoint height accepted")
	}

	statement = validStatement()
	statement.GenesisHeaderHash = strings.Repeat("9", 64)
	if err := statement.Validate(); err == nil {
		t.Fatal("alternate Mainnet genesis header accepted")
	}

	statement = validStatement()
	statement.GenesisIndexHash = strings.Repeat("9", 64)
	if err := statement.Validate(); err == nil {
		t.Fatal("alternate Mainnet genesis index accepted")
	}
}

func TestStatementV2SeparatesOperatorAndBlockSigner(t *testing.T) {
	statement := validStatement()
	statement.SchemaVersion = "aperod-mainnet-bootstrap-v1"
	if err := statement.Validate(); err == nil {
		t.Fatal("legacy bootstrap schema accepted after signer rotation")
	}

	statement = validStatement()
	statement.CheckpointValidatorPublicKeyHex = strings.Repeat("a", 64)
	if err := statement.Validate(); err != nil {
		t.Fatalf("separate operator and block-signing keys rejected: %v", err)
	}
}

func TestSignedStatementVerifiesAndDetectsTampering(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := testSigner{private: chaincrypto.ValidatorPrivKey(private)}
	authority := hex.EncodeToString(public)
	statement := validStatement()
	statement.BootstrapSignerPublicKeyHex = authority
	signed, err := signStatementWithAuthority(statement, key, authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := signed.verifyWithAuthority(authority); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	signed.Statement.CheckpointHeight++
	if err := signed.verifyWithAuthority(authority); err == nil {
		t.Fatal("tampered statement signature accepted")
	}
	signed.Statement.CheckpointHeight--
	signed.Statement.RegistrySHA256 = strings.Repeat("a", 64)
	if err := signed.verifyWithAuthority(authority); err == nil {
		t.Fatal("unsigned registry digest accepted")
	}
}

func TestSignOperatorRejectsUnpinnedKey(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := testSigner{private: chaincrypto.ValidatorPrivKey(private)}
	if _, err := SignOperator(validStatement(), key); err == nil {
		t.Fatal("untrusted signer was accepted")
	}
}

func TestPublishedBootstrapRemainsHistoricalOnlyAfterOperatorRotation(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "deploy", "mainnet-bootstrap-4076315.json"))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := ReadStrict(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := signed.VerifyHistorical(); err != nil {
		t.Fatalf("published historical statement no longer verifies: %v", err)
	}
	if err := signed.Verify(); err == nil {
		t.Fatal("historical operator was accepted for a new Mainnet trust statement")
	}
}

func TestCheckpointSignatureHashKind(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := chaincrypto.ValidatorPrivKey(private)
	header := &core.BlockHeader{
		Height:       42,
		ValidatorPub: chaincrypto.ValidatorPubKey(public),
		OraclePrice:  123,
		BaseFee:      456,
	}
	legacyHash := legacyHeaderHashV0(header)
	legacySignature, err := key.Sign(legacyHash)
	if err != nil {
		t.Fatal(err)
	}
	header.Signature = legacySignature
	if got := checkpointSignatureHashKind(header, key.Public()); got != "legacy_header_hash_v0" {
		t.Fatalf("legacy signature kind = %q", got)
	}

	oraclePriceHash := historicalHeaderHash(header, true, false)
	oraclePriceSignature, err := key.Sign(oraclePriceHash)
	if err != nil {
		t.Fatal(err)
	}
	header.Signature = oraclePriceSignature
	if got := checkpointSignatureHashKind(header, key.Public()); got != "legacy_header_hash_oracle_price_v1" {
		t.Fatalf("legacy OraclePrice signature kind = %q", got)
	}

	baseFeeHash := historicalHeaderHash(header, false, true)
	baseFeeSignature, err := key.Sign(baseFeeHash)
	if err != nil {
		t.Fatal(err)
	}
	header.Signature = baseFeeSignature
	if got := checkpointSignatureHashKind(header, key.Public()); got != "legacy_header_hash_base_fee_v1" {
		t.Fatalf("legacy BaseFee signature kind = %q", got)
	}

	if err := header.Sign(key); err != nil {
		t.Fatal(err)
	}
	if got := checkpointSignatureHashKind(header, key.Public()); got != "header_hash" {
		t.Fatalf("current signature kind = %q", got)
	}

	header.Signature[0] ^= 0xff
	if got := checkpointSignatureHashKind(header, key.Public()); got != "operator_attested_only" {
		t.Fatalf("invalid signature kind = %q", got)
	}
}

func TestLoadPinnedAuthorityForkRequiresExactHashAndValidPossession(t *testing.T) {
	identityPrivate, identityPublic, err := chaincrypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	replacementPrivate, replacementPublic, err := chaincrypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	fork := core.ValidatorAuthorityFork{
		Version:             1,
		Genesis:             chaincrypto.HashBytes([]byte("genesis")),
		Height:              10,
		Parent:              chaincrypto.HashBytes([]byte("parent")),
		Identity:            identityPublic,
		Replacement:         replacementPublic,
		RegistrySHA256:      chaincrypto.HashBytes([]byte("registry")),
		LegacyWitnessSHA256: chaincrypto.HashBytes([]byte("witness")),
	}
	fork.Possession, err = replacementPrivate.Sign(fork.Message())
	if err != nil {
		t.Fatal(err)
	}
	if !identityPrivate.Public().Equals(fork.Identity) {
		t.Fatal("test identity key did not match the authority fork")
	}
	raw, err := json.Marshal(fork)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "authority-fork.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	pin := hex.EncodeToString(digest[:])
	if _, err := LoadPinnedAuthorityFork(path, pin); err != nil {
		t.Fatalf("valid pinned fork rejected: %v", err)
	}
	if _, err := LoadPinnedAuthorityFork(path, strings.Repeat("0", 64)); err == nil {
		t.Fatal("fork with mismatched SHA-256 accepted")
	}
}

func TestFileSHA256RejectsSymlinksAndEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "artifact.json")
	if err := os.WriteFile(regular, []byte("signed protocol artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := FileSHA256(regular)
	if err != nil || !isLowerHash(digest) {
		t.Fatalf("regular file digest=%q err=%v", digest, err)
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileSHA256(empty); err == nil {
		t.Fatal("empty artifact accepted")
	}
	link := filepath.Join(dir, "artifact-link.json")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if _, err := FileSHA256(link); err == nil {
		t.Fatal("artifact symlink accepted")
	}
}

func TestSnapshotTreeSHA256BindsContentsAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "chain.db"), 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "chain.db")
	if err := os.WriteFile(filepath.Join(db, "CURRENT"), []byte("MANIFEST-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db, "000001.ldb"), []byte("immutable bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := SnapshotTreeSHA256(db)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SnapshotTreeSHA256(db)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("unstable tree digest: %q and %q", first, second)
	}
	if err := os.WriteFile(filepath.Join(db, "000001.ldb"), []byte("changed bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := SnapshotTreeSHA256(db)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("tree digest did not change after file contents changed")
	}
	if err := os.Symlink("CURRENT", filepath.Join(db, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotTreeSHA256(db); err == nil {
		t.Fatal("snapshot with symlink accepted")
	}
}

func TestReadStrictRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	if _, err := ReadStrict(strings.NewReader(`{"statement":{},"signer_public_key":"","signature":"","unexpected":true}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := ReadStrict(strings.NewReader(`{"statement":{},"signer_public_key":"","signature":{}}`)); err == nil {
		t.Fatal("invalid signature type accepted")
	}
	if _, err := ReadStrict(strings.NewReader(`{"statement":{},"signer_public_key":"","signature":""} {}`)); err == nil {
		t.Fatal("trailing JSON value accepted")
	}
}
