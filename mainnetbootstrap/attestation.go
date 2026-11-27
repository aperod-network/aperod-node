// Package mainnetbootstrap defines the signed, operator-1-of-1 trust anchor
// used to distribute the existing Mainnet genesis and a closed chain snapshot.
package mainnetbootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aperod/aperod/core"
	chaincrypto "github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

const (
	SchemaVersion = "aperod-mainnet-bootstrap-v2"
	Network       = "mainnet"
	ChainID       = core.MainnetChainID
	TrustModel    = "operator_1_of_1"

	// GenesisSignerPublicKeyHex is the immutable historical genesis identity.
	GenesisSignerPublicKeyHex = "08fd38ba8857d32d2ac807f0246a0fd298c9edf336fbd0fea3bb7bd818c91137"
	// CanonicalMainnetGenesisHeaderHashHex and CanonicalMainnetGenesisIndexHashHex
	// pin the single preserved Mainnet genesis in addition to pinning its signer.
	CanonicalMainnetGenesisHeaderHashHex = "c8fde86c1764a5603eabd6d9c56c9e47b4cf06d88d299a55665694473e2c512f"
	CanonicalMainnetGenesisIndexHashHex  = "da452e4c9d3bd8a21d73289c88a3583960eb774d0fa312206b72020b5dc54de1"
	// BootstrapSignerPublicKeyHex is the dedicated off-chain 1-of-1 operator key.
	// It must never be used as a validator block-signing key.
	BootstrapSignerPublicKeyHex = "b4c0efdb8b08110d933461c61a24892a9eeb1eb810bd2c3e1d31012f448b50df"
	// HistoricalBootstrapSignerPublicKeyHex verifies only previously published
	// statements. It is not trusted for new bootstrap or release signatures.
	HistoricalBootstrapSignerPublicKeyHex = "8bf6b47c87e075007527993d3ea1c8f45424c1f382cb43eab8ab7f5c19cb650f"
	// HistoricalCommitteeOperatorPublicKeyHex pins the legacy recovery statement
	// to its original signer; it is not the key for new bootstrap signatures.
	HistoricalCommitteeOperatorPublicKeyHex = GenesisSignerPublicKeyHex
)

const statementDomain = "aperod/mainnet-bootstrap-attestation/v2\x00"
const snapshotDomain = "aperod/mainnet-snapshot-tree/v1\x00"

// Statement binds the operator's Mainnet identity claim to the exact genesis,
// checkpoint and snapshot bytes. Genesis header and index hashes are separate
// because the historical genesis index uses a legacy hash convention.
type Statement struct {
	SchemaVersion                   string `json:"schema_version"`
	Network                         string `json:"network"`
	ChainID                         string `json:"chain_id"`
	TrustModel                      string `json:"trust_model"`
	BootstrapSignerPublicKeyHex     string `json:"bootstrap_signer_public_key"`
	GenesisHeaderHash               string `json:"genesis_header_hash"`
	GenesisIndexHash                string `json:"genesis_index_hash"`
	GenesisSignatureHashKind        string `json:"genesis_signature_hash_kind"`
	GenesisSignerPublicKeyHex       string `json:"genesis_signer_public_key"`
	CheckpointHeight                uint64 `json:"checkpoint_height"`
	CheckpointHeaderHash            string `json:"checkpoint_header_hash"`
	CheckpointIndexHash             string `json:"checkpoint_index_hash"`
	CheckpointValidatorPublicKeyHex string `json:"checkpoint_validator_public_key"`
	CheckpointSignatureLength       int    `json:"checkpoint_signature_length"`
	CheckpointSignatureHashKind     string `json:"checkpoint_signature_hash_kind"`
	CheckpointOraclePrice           uint64 `json:"checkpoint_oracle_price"`
	CheckpointBaseFee               uint64 `json:"checkpoint_base_fee"`
	ValidatorAuthorityForkSHA256    string `json:"validator_authority_fork_sha256"`
	ProtocolSaleMigrationSHA256     string `json:"protocol_sale_migration_sha256"`
	LPoDMigrationWitnessSHA256      string `json:"lpod_migration_witness_sha256"`
	SnapshotTreeSHA256              string `json:"snapshot_tree_sha256"`
	// Optional explicit attestation to the operator's registry at this tip,
	// not a commitment made by the block header.
	RegistrySHA256 string `json:"registry_sha256,omitempty"`
	// Optional commitment to the exact key-image set at an earlier operator-
	// attested snapshot anchor. When present, all three fields are signed as
	// part of this statement and must match the node's local bootstrap config.
	KeyImageAnchorSHA256    string `json:"key_image_anchor_sha256,omitempty"`
	KeyImageAnchorHeight    uint64 `json:"key_image_anchor_height,omitempty"`
	KeyImageAnchorBlockHash string `json:"key_image_anchor_block_hash,omitempty"`
}

// SignedStatement is safe to distribute publicly; it contains no private key
// material. SignatureHex is an Ed25519 signature over the domain-separated,
// canonical JSON encoding of Statement.
type SignedStatement struct {
	Statement       Statement `json:"statement"`
	SignerPublicKey string    `json:"signer_public_key"`
	SignatureHex    string    `json:"signature"`
}

type signer interface {
	Public() chaincrypto.ValidatorPubKey
	Sign(chaincrypto.Hash32) ([]byte, error)
}

func (s Statement) Validate() error {
	return s.validateWithAuthority(BootstrapSignerPublicKeyHex)
}

func (s Statement) validateWithAuthority(authority string) error {
	if s.SchemaVersion != SchemaVersion || s.Network != Network ||
		s.ChainID != ChainID || s.TrustModel != TrustModel {
		return errors.New("unsupported Mainnet bootstrap identity")
	}
	for name, value := range map[string]string{
		"genesis_header_hash":             s.GenesisHeaderHash,
		"genesis_index_hash":              s.GenesisIndexHash,
		"genesis_signer_public_key":       s.GenesisSignerPublicKeyHex,
		"bootstrap_signer_public_key":     s.BootstrapSignerPublicKeyHex,
		"checkpoint_header_hash":          s.CheckpointHeaderHash,
		"checkpoint_index_hash":           s.CheckpointIndexHash,
		"checkpoint_validator_public_key": s.CheckpointValidatorPublicKeyHex,
		"validator_authority_fork_sha256": s.ValidatorAuthorityForkSHA256,
		"protocol_sale_migration_sha256":  s.ProtocolSaleMigrationSHA256,
		"lpod_migration_witness_sha256":   s.LPoDMigrationWitnessSHA256,
		"snapshot_tree_sha256":            s.SnapshotTreeSHA256,
	} {
		if !isLowerHex(value, 64) {
			return fmt.Errorf("%s must be 64 lowercase hex characters", name)
		}
	}
	if s.GenesisSignerPublicKeyHex != GenesisSignerPublicKeyHex {
		return errors.New("genesis signer does not match the pinned Mainnet genesis identity")
	}
	if s.GenesisHeaderHash != CanonicalMainnetGenesisHeaderHashHex ||
		s.GenesisIndexHash != CanonicalMainnetGenesisIndexHashHex {
		return errors.New("genesis hashes do not match the single pinned Mainnet genesis")
	}
	if s.BootstrapSignerPublicKeyHex != authority {
		return errors.New("bootstrap signer does not match the active Mainnet operator")
	}
	if s.GenesisSignatureHashKind != "header_hash" &&
		s.GenesisSignatureHashKind != "canonical_index_hash" {
		return errors.New("genesis signature hash kind is unsupported")
	}
	if s.CheckpointSignatureHashKind != "header_hash" &&
		s.CheckpointSignatureHashKind != "legacy_header_hash_v0" &&
		s.CheckpointSignatureHashKind != "legacy_header_hash_oracle_price_v1" &&
		s.CheckpointSignatureHashKind != "legacy_header_hash_base_fee_v1" {
		return errors.New("checkpoint signature hash kind is unsupported")
	}
	if s.CheckpointSignatureLength < 0 || s.CheckpointSignatureLength > 1024 {
		return errors.New("checkpoint signature length is outside the allowed range")
	}
	if s.CheckpointHeight == 0 {
		return errors.New("checkpoint height must be greater than zero")
	}
	if s.RegistrySHA256 != "" && !isLowerHex(s.RegistrySHA256, 64) {
		return errors.New("registry_sha256 must be 64 lowercase hex characters")
	}
	hasKeyImageDigest := s.KeyImageAnchorSHA256 != ""
	hasKeyImageHeight := s.KeyImageAnchorHeight != 0
	hasKeyImageHash := s.KeyImageAnchorBlockHash != ""
	if hasKeyImageDigest || hasKeyImageHeight || hasKeyImageHash {
		if !hasKeyImageDigest || !hasKeyImageHeight || !hasKeyImageHash {
			return errors.New("key-image anchor digest, height, and block hash must be provided together")
		}
		if !isLowerHex(s.KeyImageAnchorSHA256, 64) ||
			!isLowerHex(s.KeyImageAnchorBlockHash, 64) {
			return errors.New("key-image anchor digest and block hash must be 64 lowercase hex characters")
		}
		if s.KeyImageAnchorHeight >= s.CheckpointHeight {
			return errors.New("key-image anchor height must be below the signed checkpoint")
		}
	}
	return nil
}

func isLowerHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func statementHash(statement Statement) (chaincrypto.Hash32, error) {
	return statementHashWithAuthority(statement, BootstrapSignerPublicKeyHex)
}

func statementHashWithAuthority(statement Statement, authority string) (chaincrypto.Hash32, error) {
	if err := statement.validateWithAuthority(authority); err != nil {
		return chaincrypto.Hash32{}, err
	}
	raw, err := json.Marshal(statement)
	if err != nil {
		return chaincrypto.Hash32{}, fmt.Errorf("encode bootstrap statement: %w", err)
	}
	return chaincrypto.HashBytes([]byte(statementDomain), raw), nil
}

// SignOperator signs a validated statement only when the supplied key matches
// the one pinned for the operator_1_of_1 trust model.
func SignOperator(statement Statement, key signer) (SignedStatement, error) {
	return signStatementWithAuthority(statement, key, BootstrapSignerPublicKeyHex)
}

func signStatementWithAuthority(statement Statement, key signer, authority string) (SignedStatement, error) {
	if key == nil {
		return SignedStatement{}, errors.New("operator signing key is required")
	}
	if err := statement.validateWithAuthority(authority); err != nil {
		return SignedStatement{}, err
	}
	public := hex.EncodeToString(key.Public().Bytes())
	if public != authority {
		return SignedStatement{}, errors.New("signing key does not match the authorized Mainnet operator")
	}
	hash, err := statementHashWithAuthority(statement, authority)
	if err != nil {
		return SignedStatement{}, err
	}
	signature, err := key.Sign(hash)
	if err != nil {
		return SignedStatement{}, fmt.Errorf("sign Mainnet bootstrap statement: %w", err)
	}
	return SignedStatement{
		Statement:       statement,
		SignerPublicKey: public,
		SignatureHex:    hex.EncodeToString(signature),
	}, nil
}

// Verify checks the statement and signature against the pinned operator key.
func (signed SignedStatement) Verify() error {
	return signed.verifyWithAuthority(BootstrapSignerPublicKeyHex)
}

// VerifyHistorical validates a previously published statement under the former
// operator key. The historical key must never authorize new bootstrap files.
func (signed SignedStatement) VerifyHistorical() error {
	return signed.verifyWithAuthority(HistoricalBootstrapSignerPublicKeyHex)
}

func (signed SignedStatement) verifyWithAuthority(authority string) error {
	if err := signed.Statement.validateWithAuthority(authority); err != nil {
		return err
	}
	if signed.SignerPublicKey != authority {
		return errors.New("bootstrap signature is not from the authorized Mainnet operator")
	}
	signature, err := hex.DecodeString(signed.SignatureHex)
	if err != nil || len(signature) != 64 {
		return errors.New("bootstrap signature must be 128 lowercase hex characters")
	}
	if strings.ToLower(signed.SignatureHex) != signed.SignatureHex {
		return errors.New("bootstrap signature must use lowercase hex")
	}
	publicBytes, err := hex.DecodeString(authority)
	if err != nil {
		return errors.New("authorized operator public key is invalid")
	}
	publicKey, err := chaincrypto.ValidatorPubKeyFromBytes(publicBytes)
	if err != nil {
		return fmt.Errorf("load pinned operator public key: %w", err)
	}
	hash, err := statementHashWithAuthority(signed.Statement, authority)
	if err != nil {
		return err
	}
	if !publicKey.Verify(hash, signature) {
		return errors.New("Mainnet bootstrap operator signature is invalid")
	}
	return nil
}

// SnapshotTreeSHA256 hashes the relative path, size and SHA-256 of every
// regular file under root in sorted order. Symlinks and special files fail
// closed. The directory must be a closed snapshot and must not change while it
// is measured.
func SnapshotTreeSHA256(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("inspect snapshot root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("snapshot root must be a real directory")
	}

	type fileDigest struct {
		path string
		size int64
		hash [32]byte
	}
	var files []fileDigest
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshot contains symlink %q", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("snapshot contains unsupported file %q", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || strings.ContainsAny(rel, "\r\n") {
			return fmt.Errorf("snapshot has an invalid relative path %q", rel)
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		before, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return err
		}
		h := sha256.New()
		n, copyErr := io.Copy(h, f)
		after, statErr := f.Stat()
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if statErr != nil {
			return statErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != before.Size() || before.Size() != after.Size() ||
			!before.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("snapshot file changed while hashing: %q", rel)
		}
		var digest [32]byte
		copy(digest[:], h.Sum(nil))
		files = append(files, fileDigest{path: rel, size: n, hash: digest})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk snapshot: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	tree := sha256.New()
	_, _ = tree.Write([]byte(snapshotDomain))
	for _, file := range files {
		_, _ = fmt.Fprintf(tree, "%s\x00%d\x00%x\n", file.path, file.size, file.hash)
	}
	return hex.EncodeToString(tree.Sum(nil)), nil
}

// ReadStrict decodes exactly one signed statement and rejects unknown fields
// and trailing data.
func ReadStrict(r io.Reader) (SignedStatement, error) {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	var signed SignedStatement
	if err := decoder.Decode(&signed); err != nil {
		return SignedStatement{}, fmt.Errorf("decode signed bootstrap: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return SignedStatement{}, errors.New("trailing JSON value")
		}
		return SignedStatement{}, fmt.Errorf("trailing bootstrap data: %w", err)
	}
	return signed, nil
}

// InspectSnapshotDB derives the exact identity fields from a closed chain.db.
// It deliberately does not claim that pruned history between genesis and the
// checkpoint was replayed; operator_1_of_1 is the explicit trust anchor for
// that gap.
func InspectSnapshotDB(
	dbPath, snapshotDigest string,
	authorityFork *core.ValidatorAuthorityFork, authorityForkSHA256 string,
	saleMigrationSHA256, lpodWitnessSHA256 string,
) (Statement, error) {
	if !isLowerHex(snapshotDigest, 64) {
		return Statement{}, errors.New("snapshot tree digest must be 64 lowercase hex characters")
	}
	if authorityFork == nil || !isLowerHex(authorityForkSHA256, 64) {
		return Statement{}, errors.New("pinned validator authority fork is required")
	}
	if !isLowerHex(saleMigrationSHA256, 64) || !isLowerHex(lpodWitnessSHA256, 64) {
		return Statement{}, errors.New("pinned sale and LPoD migration file digests are required")
	}
	if err := authorityFork.Validate(); err != nil {
		return Statement{}, fmt.Errorf("validate pinned authority fork: %w", err)
	}
	db, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return Statement{}, fmt.Errorf("open snapshot database read-only: %w", err)
	}
	defer db.Close()

	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		return Statement{}, fmt.Errorf("read snapshot tip: %w", err)
	}

	genesisIndexHash, found, err := db.GetCanonicalHash(0)
	if err != nil || !found {
		return Statement{}, errors.New("canonical genesis index is unavailable")
	}
	genesis, err := readBlockAt(db, genesisIndexHash)
	if err != nil {
		return Statement{}, fmt.Errorf("read canonical genesis block: %w", err)
	}
	genesisHeaderHash := genesis.Hash()
	if genesis.Header.Height != 0 || !genesis.IsGenesis() ||
		genesis.Header.MerkleRoot != core.MerkleRoot(genesis.Txs) {
		return Statement{}, errors.New("canonical genesis block structure is invalid")
	}
	if fmt.Sprintf("%x", genesisHeaderHash[:]) != CanonicalMainnetGenesisHeaderHashHex ||
		fmt.Sprintf("%x", genesisIndexHash[:]) != CanonicalMainnetGenesisIndexHashHex {
		return Statement{}, errors.New("canonical genesis hashes do not match the single pinned Mainnet genesis")
	}
	if genesis.Header.ValidatorPub.Hex() != GenesisSignerPublicKeyHex {
		return Statement{}, errors.New("canonical genesis validator does not match the pinned Mainnet genesis identity")
	}
	if authorityFork.Genesis != genesisIndexHash ||
		!authorityFork.Identity.Equals(genesis.Header.ValidatorPub) {
		return Statement{}, errors.New("authority fork is not anchored to the canonical genesis identity")
	}
	signatureHashKind := ""
	switch {
	case genesis.Header.ValidatorPub.Verify(genesisHeaderHash, genesis.Header.Signature):
		signatureHashKind = "header_hash"
	case genesis.Header.ValidatorPub.Verify(genesisIndexHash, genesis.Header.Signature):
		signatureHashKind = "canonical_index_hash"
	default:
		return Statement{}, errors.New("canonical genesis signature is invalid for its header and index hashes")
	}

	checkpointIndexHash, found, err := db.GetCanonicalHash(tipHeight)
	if err != nil || !found {
		return Statement{}, errors.New("canonical checkpoint index is unavailable")
	}
	if checkpointIndexHash != tipHash {
		return Statement{}, errors.New("snapshot tip does not match the canonical checkpoint index")
	}
	for _, transition := range authorityFork.Transitions() {
		if transition.Height <= tipHeight {
			parentHash, found, err := db.GetCanonicalHash(transition.Height - 1)
			if err != nil || !found || parentHash != transition.Parent {
				return Statement{}, errors.New("authority fork activation parent does not match the canonical chain")
			}
		}
	}
	checkpoint, err := readBlockAt(db, checkpointIndexHash)
	if err != nil {
		return Statement{}, fmt.Errorf("read canonical checkpoint block: %w", err)
	}
	checkpointHeaderHash := checkpoint.Hash()
	if checkpoint.Header.Height != tipHeight ||
		checkpoint.Header.MerkleRoot != core.MerkleRoot(checkpoint.Txs) {
		return Statement{}, errors.New("checkpoint block structure is invalid")
	}
	checkpointAuthority := authorityFork.Authority(checkpoint.Header.ValidatorPub, tipHeight)
	checkpointSignatureHashKind := checkpointSignatureHashKind(&checkpoint.Header, checkpointAuthority)
	if checkpointSignatureHashKind == "operator_attested_only" {
		return Statement{}, errors.New("checkpoint block signature does not verify under the pinned authority fork")
	}
	statement := Statement{
		SchemaVersion:                   SchemaVersion,
		Network:                         Network,
		ChainID:                         ChainID,
		TrustModel:                      TrustModel,
		BootstrapSignerPublicKeyHex:     BootstrapSignerPublicKeyHex,
		GenesisHeaderHash:               fmt.Sprintf("%x", genesisHeaderHash[:]),
		GenesisIndexHash:                fmt.Sprintf("%x", genesisIndexHash[:]),
		GenesisSignatureHashKind:        signatureHashKind,
		GenesisSignerPublicKeyHex:       genesis.Header.ValidatorPub.Hex(),
		CheckpointHeight:                tipHeight,
		CheckpointHeaderHash:            fmt.Sprintf("%x", checkpointHeaderHash[:]),
		CheckpointIndexHash:             fmt.Sprintf("%x", checkpointIndexHash[:]),
		CheckpointValidatorPublicKeyHex: checkpoint.Header.ValidatorPub.Hex(),
		CheckpointSignatureLength:       len(checkpoint.Header.Signature),
		CheckpointSignatureHashKind:     checkpointSignatureHashKind,
		CheckpointOraclePrice:           checkpoint.Header.OraclePrice,
		CheckpointBaseFee:               checkpoint.Header.BaseFee,
		ValidatorAuthorityForkSHA256:    authorityForkSHA256,
		ProtocolSaleMigrationSHA256:     saleMigrationSHA256,
		LPoDMigrationWitnessSHA256:      lpodWitnessSHA256,
		SnapshotTreeSHA256:              snapshotDigest,
	}
	if err := statement.Validate(); err != nil {
		return Statement{}, err
	}
	return statement, nil
}

// VerifySnapshotDB verifies a signed statement against the exact closed
// snapshot database and its complete file-tree digest.
func VerifySnapshotDB(
	dbPath string, statement Statement,
	authorityFork *core.ValidatorAuthorityFork, authorityForkSHA256 string,
	saleMigrationSHA256, lpodWitnessSHA256 string,
) error {
	return VerifySnapshotDBWithKeyImageAnchor(
		dbPath, statement, authorityFork, authorityForkSHA256,
		saleMigrationSHA256, lpodWitnessSHA256, "",
	)
}

// VerifySnapshotDBWithKeyImageAnchor verifies the signed snapshot and, when
// the statement includes one, the exact earlier key-image anchor file. The
// optional anchor is separate from the checkpoint database tree, so it must
// be supplied and checked explicitly rather than silently copied into the
// inspected statement before comparison.
func VerifySnapshotDBWithKeyImageAnchor(
	dbPath string, statement Statement,
	authorityFork *core.ValidatorAuthorityFork, authorityForkSHA256 string,
	saleMigrationSHA256, lpodWitnessSHA256, keyImageAnchorPath string,
) error {
	if err := statement.Validate(); err != nil {
		return err
	}
	if authorityForkSHA256 != statement.ValidatorAuthorityForkSHA256 {
		return errors.New("authority fork SHA-256 does not match the signed statement")
	}
	if saleMigrationSHA256 != statement.ProtocolSaleMigrationSHA256 ||
		lpodWitnessSHA256 != statement.LPoDMigrationWitnessSHA256 {
		return errors.New("protocol migration artifact digests do not match the signed statement")
	}
	actual, err := InspectSnapshotDB(
		dbPath, statement.SnapshotTreeSHA256, authorityFork, authorityForkSHA256,
		saleMigrationSHA256, lpodWitnessSHA256,
	)
	if err != nil {
		return err
	}
	if statement.RegistrySHA256 != "" {
		actual.RegistrySHA256, err = InspectRegistryEvidence(dbPath, actual)
		if err != nil {
			return err
		}
		if actual.RegistrySHA256 != statement.RegistrySHA256 {
			return errors.New("checkpoint registry digest does not match the signed statement")
		}
	}
	if err := VerifyKeyImageAnchor(dbPath, statement, keyImageAnchorPath); err != nil {
		return err
	}
	actual.KeyImageAnchorSHA256 = statement.KeyImageAnchorSHA256
	actual.KeyImageAnchorHeight = statement.KeyImageAnchorHeight
	actual.KeyImageAnchorBlockHash = statement.KeyImageAnchorBlockHash
	if actual != statement {
		return errors.New("snapshot genesis or checkpoint identity does not match the signed statement")
	}
	return nil
}

// VerifyKeyImageAnchor verifies the optional, earlier key-image export
// against both its signed digest/height/hash and the chain database's
// canonical height index. A signed anchor cannot be accepted without its
// corresponding file.
func VerifyKeyImageAnchor(dbPath string, statement Statement, anchorPath string) error {
	if err := statement.Validate(); err != nil {
		return err
	}
	hasSignedAnchor := statement.KeyImageAnchorSHA256 != "" ||
		statement.KeyImageAnchorHeight != 0 ||
		statement.KeyImageAnchorBlockHash != ""
	if !hasSignedAnchor {
		if anchorPath != "" {
			return errors.New("key-image anchor file supplied but the signed statement has no key-image anchor")
		}
		return nil
	}
	if anchorPath == "" {
		return errors.New("signed key-image anchor requires its key-image anchor file")
	}
	digest, blockHash, height, err := InspectKeyImageAnchor(
		dbPath, anchorPath, statement.CheckpointHeight,
	)
	if err != nil {
		return err
	}
	if digest != statement.KeyImageAnchorSHA256 ||
		blockHash != statement.KeyImageAnchorBlockHash ||
		height != statement.KeyImageAnchorHeight {
		return errors.New("key-image anchor file does not match the signed Mainnet statement")
	}
	return nil
}

// FileSHA256 returns a digest only for a regular, non-symlink public artifact.
func FileSHA256(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect artifact %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() < 1 || info.Size() > 16<<20 {
		return "", fmt.Errorf("artifact %q is not a regular file within size limits", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read artifact %q: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// LoadPinnedAuthorityFork reads the exact authority rule approved by the
// manifest. The fork's proof-of-possession alone is not treated as governance
// authorization; its complete file hash must match the independently pinned
// digest.
func LoadPinnedAuthorityFork(path, expectedSHA256 string) (*core.ValidatorAuthorityFork, error) {
	if !isLowerHex(expectedSHA256, 64) {
		return nil, errors.New("authority fork SHA-256 pin must be 64 lowercase hex characters")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect authority fork: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("authority fork must be a regular, non-symlink file")
	}
	if info.Size() < 1 || info.Size() > 65536 {
		return nil, errors.New("authority fork file size is outside the allowed range")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read authority fork: %w", err)
	}
	actualSHA256 := sha256.Sum256(raw)
	if hex.EncodeToString(actualSHA256[:]) != expectedSHA256 {
		return nil, errors.New("authority fork file SHA-256 does not match the pinned digest")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var fork core.ValidatorAuthorityFork
	if err := decoder.Decode(&fork); err != nil {
		return nil, fmt.Errorf("decode authority fork: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, errors.New("authority fork contains trailing JSON")
		}
		return nil, fmt.Errorf("decode authority fork trailing data: %w", err)
	}
	if err := fork.Validate(); err != nil {
		return nil, fmt.Errorf("validate authority fork: %w", err)
	}
	return &fork, nil
}

func checkpointSignatureHashKind(header *core.BlockHeader, authority chaincrypto.ValidatorPubKey) string {
	if authority.Verify(header.Hash(), header.Signature) {
		return "header_hash"
	}
	candidates := []struct {
		name               string
		includeOraclePrice bool
		includeBaseFee     bool
	}{
		{name: "legacy_header_hash_v0"},
		{name: "legacy_header_hash_oracle_price_v1", includeOraclePrice: true},
		{name: "legacy_header_hash_base_fee_v1", includeBaseFee: true},
	}
	for _, candidate := range candidates {
		hash := historicalHeaderHash(header, candidate.includeOraclePrice, candidate.includeBaseFee)
		if authority.Verify(hash, header.Signature) {
			return candidate.name
		}
	}
	return "operator_attested_only"
}

func legacyHeaderHashV0(header *core.BlockHeader) chaincrypto.Hash32 {
	return historicalHeaderHash(header, false, false)
}

func historicalHeaderHash(header *core.BlockHeader, includeOraclePrice, includeBaseFee bool) chaincrypto.Hash32 {
	var encoded bytes.Buffer
	var u64 [8]byte
	binary.LittleEndian.PutUint64(u64[:], header.Height)
	encoded.Write(u64[:])
	encoded.Write(header.PrevHash[:])
	encoded.Write(header.MerkleRoot[:])
	binary.LittleEndian.PutUint64(u64[:], uint64(header.Timestamp))
	encoded.Write(u64[:])
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], header.Round)
	encoded.Write(u32[:])
	encoded.Write(header.ValidatorPub)
	if includeOraclePrice {
		binary.LittleEndian.PutUint64(u64[:], header.OraclePrice)
		encoded.Write(u64[:])
	}
	if includeBaseFee {
		binary.LittleEndian.PutUint64(u64[:], header.BaseFee)
		encoded.Write(u64[:])
	}
	sum := sha256.Sum256(encoded.Bytes())
	return chaincrypto.Hash32(sum)
}

func readBlockAt(db *store.DB, hash chaincrypto.Hash32) (*core.Block, error) {
	raw, err := db.GetRawBlock(hash)
	if err != nil {
		return nil, err
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, err
	}
	return &block, nil
}
