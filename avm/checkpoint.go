package avm

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/aperod/aperod/crypto"
)

const (
	AVMCheckpointVersion       uint32 = 1
	AVMCheckpointQuorumVersion uint32 = 2
	MaxAVMCheckpointReplay     uint64 = 1_000_000
	maxAVMCheckpointFileBytes         = 512 << 20
	maxAVMCheckpointEntries           = 10_000_000
)

const avmCheckpointDomain = "APRO/AVM/CHECKPOINT/V1\x00"
const avmQuorumCheckpointDomain = "APRO/AVM/CHECKPOINT/V2\x00"
const avmStateSnapshotDomain = "APRO/AVM/SNAPSHOT-STATE/V1\x00"

// CheckpointEntry is one key/value pair in a complete AVM state snapshot.
// Entries in a signed checkpoint must be strictly ordered by Key.
type CheckpointEntry struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

// AVMCheckpoint is a complete state snapshot. V1 uses a single
// operator-attested signature; v2 uses detached signatures from a configured
// witness quorum. It is not a consensus rule or a substitute for block
// validation: it only provides a trusted starting state for bounded replay.
type AVMCheckpoint struct {
	Version            uint32
	ActivationHeight   uint64
	Height             uint64
	GenesisHash        crypto.Hash32
	BlockHash          crypto.Hash32
	StateCommitment    crypto.Hash32
	WriteSetCommitment crypto.Hash32
	Entries            []CheckpointEntry
	WitnessPublicKey   crypto.ValidatorPubKey
	WitnessSignature   []byte
	WitnessSignatures  []CheckpointWitnessSignature
}

// CheckpointWitnessSignature is a detached v2 signature over the complete
// checkpoint payload. Signatures are kept in public-key order.
type CheckpointWitnessSignature struct {
	WitnessPublicKey crypto.ValidatorPubKey
	Signature        []byte
}

// CheckpointSigner is implemented by both ValidatorPrivKey and the node's
// locked validator key, avoiding copies of private key material.
type CheckpointSigner interface {
	Public() crypto.ValidatorPubKey
	Sign(crypto.Hash32) ([]byte, error)
}

type checkpointWire struct {
	Version            uint32                           `json:"version"`
	ActivationHeight   uint64                           `json:"activation_height"`
	Height             uint64                           `json:"height"`
	GenesisHash        string                           `json:"genesis_hash"`
	BlockHash          string                           `json:"block_hash"`
	StateCommitment    string                           `json:"state_commitment"`
	WriteSetCommitment string                           `json:"write_set_commitment"`
	Entries            []CheckpointEntry                `json:"entries"`
	WitnessPublicKey   string                           `json:"witness_public_key"`
	WitnessSignature   string                           `json:"witness_signature"`
	WitnessSignatures  []checkpointWitnessSignatureWire `json:"witness_signatures,omitempty"`
}

type checkpointWitnessSignatureWire struct {
	WitnessPublicKey string `json:"witness_public_key"`
	Signature        string `json:"signature"`
}

// ParseCheckpointAuthorities parses a non-empty set of explicitly pinned
// Ed25519 public keys. Duplicate and zero keys are rejected.
func ParseCheckpointAuthorities(configured []string) ([]crypto.ValidatorPubKey, error) {
	if len(configured) == 0 {
		return nil, fmt.Errorf("avm checkpoint: explicit trusted witness key is required")
	}
	authorities := make([]crypto.ValidatorPubKey, 0, len(configured))
	seen := make(map[string]struct{}, len(configured))
	for i, encoded := range configured {
		raw, err := hex.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: malformed trusted witness key at index %d", i)
		}
		pub, err := crypto.ValidatorPubKeyFromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: trusted witness key at index %d must be exactly 32 bytes", i)
		}
		allZero := true
		for _, b := range pub {
			if b != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			return nil, fmt.Errorf("avm checkpoint: zero trusted witness key at index %d", i)
		}
		if _, duplicate := seen[string(pub)]; duplicate {
			return nil, fmt.Errorf("avm checkpoint: duplicate trusted witness key at index %d", i)
		}
		seen[string(pub)] = struct{}{}
		authorities = append(authorities, pub)
	}
	return authorities, nil
}

// StateSnapshotCommitment hashes a deterministic, strictly key-ordered
// snapshot. It rejects duplicate or empty keys rather than normalizing them.
func StateSnapshotCommitment(entries []CheckpointEntry) (crypto.Hash32, error) {
	if len(entries) > maxAVMCheckpointEntries {
		return crypto.Hash32{}, fmt.Errorf("avm checkpoint: too many state entries")
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(avmStateSnapshotDomain))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(len(entries)))
	_, _ = hasher.Write(number[:])
	for i, entry := range entries {
		if len(entry.Key) == 0 {
			return crypto.Hash32{}, fmt.Errorf("avm checkpoint: empty state key at entry %d", i)
		}
		if i > 0 && bytes.Compare(entries[i-1].Key, entry.Key) >= 0 {
			return crypto.Hash32{}, fmt.Errorf("avm checkpoint: state entries are not strictly key ordered")
		}
		binary.BigEndian.PutUint64(number[:], uint64(len(entry.Key)))
		_, _ = hasher.Write(number[:])
		_, _ = hasher.Write(entry.Key)
		binary.BigEndian.PutUint64(number[:], uint64(len(entry.Value)))
		_, _ = hasher.Write(number[:])
		_, _ = hasher.Write(entry.Value)
	}
	var commitment crypto.Hash32
	copy(commitment[:], hasher.Sum(nil))
	return commitment, nil
}

func checkpointMessage(checkpoint *AVMCheckpoint) crypto.Hash32 {
	var version [4]byte
	var heights [16]byte
	binary.BigEndian.PutUint32(version[:], checkpoint.Version)
	binary.BigEndian.PutUint64(heights[:8], checkpoint.ActivationHeight)
	binary.BigEndian.PutUint64(heights[8:], checkpoint.Height)
	return crypto.HashBytes(
		[]byte(avmCheckpointDomain),
		version[:],
		heights[:],
		checkpoint.GenesisHash[:],
		checkpoint.BlockHash[:],
		checkpoint.StateCommitment[:],
		checkpoint.WriteSetCommitment[:],
		checkpoint.WitnessPublicKey,
	)
}

func checkpointQuorumMessage(checkpoint *AVMCheckpoint) crypto.Hash32 {
	var version [4]byte
	var heights [16]byte
	binary.BigEndian.PutUint32(version[:], checkpoint.Version)
	binary.BigEndian.PutUint64(heights[:8], checkpoint.ActivationHeight)
	binary.BigEndian.PutUint64(heights[8:], checkpoint.Height)
	return crypto.HashBytes(
		[]byte(avmQuorumCheckpointDomain),
		version[:],
		heights[:],
		checkpoint.GenesisHash[:],
		checkpoint.BlockHash[:],
		checkpoint.StateCommitment[:],
		checkpoint.WriteSetCommitment[:],
	)
}

func checkpointAuthorityAllowed(authorities []crypto.ValidatorPubKey, key crypto.ValidatorPubKey) bool {
	for _, authority := range authorities {
		if authority.Equals(key) {
			return true
		}
	}
	return false
}

func validateCheckpointAuthorities(authorities []crypto.ValidatorPubKey, threshold int) error {
	if threshold < 2 {
		return fmt.Errorf("avm checkpoint: quorum threshold must be at least 2")
	}
	if threshold > len(authorities) {
		return fmt.Errorf("avm checkpoint: quorum threshold exceeds configured authority count")
	}
	seen := make(map[string]struct{}, len(authorities))
	for i, authority := range authorities {
		if len(authority) != 32 {
			return fmt.Errorf("avm checkpoint: trusted witness key at index %d must be 32 bytes", i)
		}
		allZero := true
		for _, b := range authority {
			if b != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			return fmt.Errorf("avm checkpoint: zero trusted witness key at index %d", i)
		}
		if _, duplicate := seen[string(authority)]; duplicate {
			return fmt.Errorf("avm checkpoint: duplicate trusted witness key at index %d", i)
		}
		seen[string(authority)] = struct{}{}
	}
	return nil
}

func cloneCheckpointEntries(entries []CheckpointEntry) []CheckpointEntry {
	cloned := make([]CheckpointEntry, len(entries))
	for i, entry := range entries {
		cloned[i] = CheckpointEntry{
			Key:   bytes.Clone(entry.Key),
			Value: bytes.Clone(entry.Value),
		}
	}
	return cloned
}

func canonicalCheckpointEntries(entries []CheckpointEntry) ([]CheckpointEntry, error) {
	canonical := cloneCheckpointEntries(entries)
	sort.Slice(canonical, func(i, j int) bool {
		return bytes.Compare(canonical[i].Key, canonical[j].Key) < 0
	})
	if _, err := StateSnapshotCommitment(canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}

// NewQuorumCheckpointDraft creates a v2 checkpoint payload before any witness
// signs it. All witness signatures cover the same deterministic metadata and
// state root; the signature set itself is not part of the signed message.
func NewQuorumCheckpointDraft(
	activationHeight uint64,
	height uint64,
	genesisHash crypto.Hash32,
	blockHash crypto.Hash32,
	writeSetCommitment crypto.Hash32,
	entries []CheckpointEntry,
) (*AVMCheckpoint, error) {
	if activationHeight == 0 || height < activationHeight {
		return nil, fmt.Errorf("avm checkpoint: invalid activation or checkpoint height")
	}
	if genesisHash.Zero() || blockHash.Zero() {
		return nil, fmt.Errorf("avm checkpoint: zero genesis or checkpoint block hash")
	}
	canonicalEntries, err := canonicalCheckpointEntries(entries)
	if err != nil {
		return nil, err
	}
	stateCommitment, err := StateSnapshotCommitment(canonicalEntries)
	if err != nil {
		return nil, err
	}
	return &AVMCheckpoint{
		Version:            AVMCheckpointQuorumVersion,
		ActivationHeight:   activationHeight,
		Height:             height,
		GenesisHash:        genesisHash,
		BlockHash:          blockHash,
		StateCommitment:    stateCommitment,
		WriteSetCommitment: writeSetCommitment,
		Entries:            canonicalEntries,
	}, nil
}

// ValidateQuorumCheckpointDraft checks the immutable payload before a witness
// signs it. It rejects already signed or legacy v1 checkpoints.
func ValidateQuorumCheckpointDraft(checkpoint *AVMCheckpoint, activationHeight uint64) error {
	if checkpoint == nil {
		return fmt.Errorf("avm checkpoint: nil checkpoint draft")
	}
	if checkpoint.Version != AVMCheckpointQuorumVersion {
		return fmt.Errorf("avm checkpoint: expected quorum version %d, got %d", AVMCheckpointQuorumVersion, checkpoint.Version)
	}
	if checkpoint.ActivationHeight != activationHeight || activationHeight == 0 {
		return fmt.Errorf("avm checkpoint: activation height mismatch")
	}
	if checkpoint.Height < activationHeight || checkpoint.GenesisHash.Zero() || checkpoint.BlockHash.Zero() {
		return fmt.Errorf("avm checkpoint: invalid height or zero chain hash")
	}
	if len(checkpoint.WitnessPublicKey) != 0 || len(checkpoint.WitnessSignature) != 0 ||
		len(checkpoint.WitnessSignatures) != 0 {
		return fmt.Errorf("avm checkpoint: expected unsigned quorum draft")
	}
	stateCommitment, err := StateSnapshotCommitment(checkpoint.Entries)
	if err != nil {
		return err
	}
	if stateCommitment != checkpoint.StateCommitment {
		return fmt.Errorf("avm checkpoint: state commitment mismatch")
	}
	return nil
}

// CheckpointPayloadEqual compares the signed fields and full state of two
// checkpoints while ignoring their signature sets.
func CheckpointPayloadEqual(left, right *AVMCheckpoint) bool {
	if left == nil || right == nil ||
		left.Version != right.Version ||
		left.ActivationHeight != right.ActivationHeight ||
		left.Height != right.Height ||
		left.GenesisHash != right.GenesisHash ||
		left.BlockHash != right.BlockHash ||
		left.StateCommitment != right.StateCommitment ||
		left.WriteSetCommitment != right.WriteSetCommitment ||
		len(left.Entries) != len(right.Entries) {
		return false
	}
	for i := range left.Entries {
		if !bytes.Equal(left.Entries[i].Key, right.Entries[i].Key) ||
			!bytes.Equal(left.Entries[i].Value, right.Entries[i].Value) {
			return false
		}
	}
	return true
}

// SignCheckpointDraft creates one detached signature. The signer retains
// custody of its key; the returned public signature can be assembled with
// signatures collected from other witnesses.
func SignCheckpointDraft(
	checkpoint *AVMCheckpoint,
	activationHeight uint64,
	signer CheckpointSigner,
) (CheckpointWitnessSignature, error) {
	if err := ValidateQuorumCheckpointDraft(checkpoint, activationHeight); err != nil {
		return CheckpointWitnessSignature{}, err
	}
	if signer == nil {
		return CheckpointWitnessSignature{}, fmt.Errorf("avm checkpoint: signer is required")
	}
	public := signer.Public()
	if len(public) != 32 {
		return CheckpointWitnessSignature{}, fmt.Errorf("avm checkpoint: signer public key must be 32 bytes")
	}
	publicKey := append(crypto.ValidatorPubKey(nil), public...)
	message := checkpointQuorumMessage(checkpoint)
	signature, err := signer.Sign(message)
	if err != nil {
		return CheckpointWitnessSignature{}, fmt.Errorf("avm checkpoint: sign quorum draft: %w", err)
	}
	if !publicKey.Verify(message, signature) {
		return CheckpointWitnessSignature{}, fmt.Errorf("avm checkpoint: signer returned an invalid signature")
	}
	return CheckpointWitnessSignature{
		WitnessPublicKey: publicKey,
		Signature:        bytes.Clone(signature),
	}, nil
}

// AttachCheckpointSignatures returns a signed v2 checkpoint after verifying
// the complete collected signature set against the configured m-of-n policy.
func AttachCheckpointSignatures(
	draft *AVMCheckpoint,
	signatures []CheckpointWitnessSignature,
	activationHeight uint64,
	authorities []crypto.ValidatorPubKey,
	threshold int,
) (*AVMCheckpoint, error) {
	if err := ValidateQuorumCheckpointDraft(draft, activationHeight); err != nil {
		return nil, err
	}
	if err := validateCheckpointAuthorities(authorities, threshold); err != nil {
		return nil, err
	}
	signed := *draft
	signed.Entries = cloneCheckpointEntries(draft.Entries)
	signed.WitnessSignatures = make([]CheckpointWitnessSignature, len(signatures))
	for i, signature := range signatures {
		signed.WitnessSignatures[i] = CheckpointWitnessSignature{
			WitnessPublicKey: append(crypto.ValidatorPubKey(nil), signature.WitnessPublicKey...),
			Signature:        bytes.Clone(signature.Signature),
		}
	}
	sort.Slice(signed.WitnessSignatures, func(i, j int) bool {
		return bytes.Compare(signed.WitnessSignatures[i].WitnessPublicKey, signed.WitnessSignatures[j].WitnessPublicKey) < 0
	})
	if err := VerifyCheckpointQuorum(&signed, activationHeight, authorities, threshold); err != nil {
		return nil, err
	}
	return &signed, nil
}

// VerifyCheckpointQuorum verifies the full v2 payload and every supplied
// signature. Invalid, duplicate, unauthorized, or unsorted signatures fail
// the checkpoint even if enough other signatures would meet the threshold.
func VerifyCheckpointQuorum(
	checkpoint *AVMCheckpoint,
	activationHeight uint64,
	authorities []crypto.ValidatorPubKey,
	threshold int,
) error {
	if checkpoint == nil {
		return fmt.Errorf("avm checkpoint: nil checkpoint")
	}
	if err := validateCheckpointAuthorities(authorities, threshold); err != nil {
		return err
	}
	if checkpoint.Version != AVMCheckpointQuorumVersion {
		return fmt.Errorf("avm checkpoint: quorum mode requires version %d, got %d", AVMCheckpointQuorumVersion, checkpoint.Version)
	}
	if checkpoint.ActivationHeight != activationHeight || activationHeight == 0 {
		return fmt.Errorf("avm checkpoint: activation height mismatch")
	}
	if checkpoint.Height < activationHeight || checkpoint.GenesisHash.Zero() || checkpoint.BlockHash.Zero() {
		return fmt.Errorf("avm checkpoint: invalid height or zero chain hash")
	}
	if len(checkpoint.WitnessPublicKey) != 0 || len(checkpoint.WitnessSignature) != 0 {
		return fmt.Errorf("avm checkpoint: quorum checkpoint contains legacy v1 signature fields")
	}
	if len(checkpoint.WitnessSignatures) < threshold {
		return fmt.Errorf("avm checkpoint: insufficient witness signatures: got %d, require %d", len(checkpoint.WitnessSignatures), threshold)
	}
	if len(checkpoint.WitnessSignatures) > len(authorities) {
		return fmt.Errorf("avm checkpoint: signature count exceeds configured authority count")
	}
	stateCommitment, err := StateSnapshotCommitment(checkpoint.Entries)
	if err != nil {
		return err
	}
	if stateCommitment != checkpoint.StateCommitment {
		return fmt.Errorf("avm checkpoint: state commitment mismatch")
	}
	message := checkpointQuorumMessage(checkpoint)
	var previous crypto.ValidatorPubKey
	for i, signature := range checkpoint.WitnessSignatures {
		if i > 0 && bytes.Compare(previous, signature.WitnessPublicKey) >= 0 {
			return fmt.Errorf("avm checkpoint: witness signatures are not uniquely key ordered")
		}
		if !checkpointAuthorityAllowed(authorities, signature.WitnessPublicKey) {
			return fmt.Errorf("avm checkpoint: unauthorized witness signature at index %d", i)
		}
		if !signature.WitnessPublicKey.Verify(message, signature.Signature) {
			return fmt.Errorf("avm checkpoint: invalid witness signature at index %d", i)
		}
		previous = signature.WitnessPublicKey
	}
	return nil
}

// SignCheckpoint creates a legacy v1 operator-attested checkpoint.
//
// Deprecated: quorum-configured nodes must use NewQuorumCheckpointDraft,
// SignCheckpointDraft, and AttachCheckpointSignatures.
func SignCheckpoint(
	activationHeight uint64,
	height uint64,
	genesisHash crypto.Hash32,
	blockHash crypto.Hash32,
	writeSetCommitment crypto.Hash32,
	entries []CheckpointEntry,
	signer CheckpointSigner,
	authorities []crypto.ValidatorPubKey,
) (*AVMCheckpoint, error) {
	if signer == nil {
		return nil, fmt.Errorf("avm checkpoint: signer is required")
	}
	pub := signer.Public()
	if len(pub) != 32 {
		return nil, fmt.Errorf("avm checkpoint: signer public key must be 32 bytes")
	}
	if !checkpointAuthorityAllowed(authorities, pub) {
		return nil, fmt.Errorf("avm checkpoint: local signer is not an explicitly trusted witness")
	}
	if activationHeight == 0 || height < activationHeight {
		return nil, fmt.Errorf("avm checkpoint: invalid activation or checkpoint height")
	}
	if genesisHash.Zero() || blockHash.Zero() {
		return nil, fmt.Errorf("avm checkpoint: zero genesis or checkpoint block hash")
	}
	canonicalEntries, err := canonicalCheckpointEntries(entries)
	if err != nil {
		return nil, err
	}
	stateCommitment, err := StateSnapshotCommitment(canonicalEntries)
	if err != nil {
		return nil, err
	}
	checkpoint := &AVMCheckpoint{
		Version:            AVMCheckpointVersion,
		ActivationHeight:   activationHeight,
		Height:             height,
		GenesisHash:        genesisHash,
		BlockHash:          blockHash,
		StateCommitment:    stateCommitment,
		WriteSetCommitment: writeSetCommitment,
		Entries:            canonicalEntries,
		WitnessPublicKey:   append(crypto.ValidatorPubKey(nil), pub...),
	}
	signature, err := signer.Sign(checkpointMessage(checkpoint))
	if err != nil {
		return nil, fmt.Errorf("avm checkpoint: sign: %w", err)
	}
	checkpoint.WitnessSignature = bytes.Clone(signature)
	return checkpoint, nil
}

// VerifyCheckpoint validates the format, snapshot digest, witness pin, and
// Ed25519 signature. The caller must separately verify the canonical block
// hash and bounded replay range against its local chain.
func VerifyCheckpoint(
	checkpoint *AVMCheckpoint,
	activationHeight uint64,
	authorities []crypto.ValidatorPubKey,
) error {
	if checkpoint == nil {
		return fmt.Errorf("avm checkpoint: nil checkpoint")
	}
	if checkpoint.Version != AVMCheckpointVersion {
		return fmt.Errorf("avm checkpoint: unsupported version %d", checkpoint.Version)
	}
	if len(checkpoint.WitnessSignatures) != 0 {
		return fmt.Errorf("avm checkpoint: legacy v1 checkpoint contains v2 witness signatures")
	}
	if checkpoint.ActivationHeight != activationHeight || activationHeight == 0 {
		return fmt.Errorf("avm checkpoint: activation height mismatch")
	}
	if checkpoint.Height < activationHeight || checkpoint.GenesisHash.Zero() || checkpoint.BlockHash.Zero() {
		return fmt.Errorf("avm checkpoint: invalid height or zero chain hash")
	}
	if !checkpointAuthorityAllowed(authorities, checkpoint.WitnessPublicKey) {
		return fmt.Errorf("avm checkpoint: witness is not configured as trusted")
	}
	if len(checkpoint.WitnessPublicKey) != 32 {
		return fmt.Errorf("avm checkpoint: witness public key must be 32 bytes")
	}
	stateCommitment, err := StateSnapshotCommitment(checkpoint.Entries)
	if err != nil {
		return err
	}
	if stateCommitment != checkpoint.StateCommitment {
		return fmt.Errorf("avm checkpoint: state commitment mismatch")
	}
	if !checkpoint.WitnessPublicKey.Verify(checkpointMessage(checkpoint), checkpoint.WitnessSignature) {
		return fmt.Errorf("avm checkpoint: invalid trusted witness signature")
	}
	return nil
}

// ReadCheckpointFile reads one bounded JSON checkpoint. A missing file is
// reported separately so callers can choose a safe full replay for short
// histories or require an offline-seeded checkpoint for long histories.
func ReadCheckpointFile(path string) (*AVMCheckpoint, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("avm checkpoint: open: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("avm checkpoint: stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("avm checkpoint: not a regular file")
	}
	if info.Size() <= 0 || info.Size() > maxAVMCheckpointFileBytes {
		return nil, false, fmt.Errorf("avm checkpoint: file size %d outside supported bounds", info.Size())
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxAVMCheckpointFileBytes+1))
	decoder.DisallowUnknownFields()
	var wire checkpointWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, false, fmt.Errorf("avm checkpoint: decode: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, false, fmt.Errorf("avm checkpoint: trailing JSON value")
		}
		return nil, false, fmt.Errorf("avm checkpoint: trailing data: %w", err)
	}
	checkpoint, err := checkpointFromWire(wire)
	if err != nil {
		return nil, false, err
	}
	return checkpoint, true, nil
}

// WriteCheckpointFile replaces a checkpoint atomically. The file and its
// temporary predecessor are mode 0600; a torn write never replaces the last
// valid checkpoint.
func WriteCheckpointFile(path string, checkpoint *AVMCheckpoint) error {
	return writeCheckpointFileWithLimit(path, checkpoint, maxAVMCheckpointFileBytes)
}

func writeCheckpointFileWithLimit(path string, checkpoint *AVMCheckpoint, maxBytes int64) error {
	if checkpoint == nil {
		return fmt.Errorf("avm checkpoint: nil checkpoint")
	}
	if maxBytes <= 0 {
		return fmt.Errorf("avm checkpoint: file size limit must be positive")
	}
	if len(checkpoint.Entries) > maxAVMCheckpointEntries {
		return fmt.Errorf("avm checkpoint: too many state entries")
	}
	wire := checkpointToWire(checkpoint)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("avm checkpoint: create directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".avm-checkpoint-*.tmp")
	if err != nil {
		return fmt.Errorf("avm checkpoint: create temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("avm checkpoint: chmod temporary file: %w", err)
	}
	encoder := json.NewEncoder(&checkpointLimitWriter{
		writer: temp, remaining: maxBytes, limit: maxBytes,
	})
	if err := encoder.Encode(wire); err != nil {
		_ = temp.Close()
		return fmt.Errorf("avm checkpoint: encode: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("avm checkpoint: sync temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("avm checkpoint: close temporary file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("avm checkpoint: atomically replace: %w", err)
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("avm checkpoint: open parent directory: %w", err)
	}
	defer dirFile.Close()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("avm checkpoint: sync parent directory: %w", err)
	}
	return nil
}

type checkpointLimitWriter struct {
	writer    io.Writer
	remaining int64
	limit     int64
}

func (w *checkpointLimitWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, fmt.Errorf("avm checkpoint: encoded file exceeds %d bytes", w.limit)
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return n, err
}

func checkpointToWire(checkpoint *AVMCheckpoint) checkpointWire {
	witnessSignatures := make([]checkpointWitnessSignatureWire, len(checkpoint.WitnessSignatures))
	for i, signature := range checkpoint.WitnessSignatures {
		witnessSignatures[i] = checkpointWitnessSignatureWire{
			WitnessPublicKey: hex.EncodeToString(signature.WitnessPublicKey),
			Signature:        hex.EncodeToString(signature.Signature),
		}
	}
	return checkpointWire{
		Version:            checkpoint.Version,
		ActivationHeight:   checkpoint.ActivationHeight,
		Height:             checkpoint.Height,
		GenesisHash:        hex.EncodeToString(checkpoint.GenesisHash[:]),
		BlockHash:          hex.EncodeToString(checkpoint.BlockHash[:]),
		StateCommitment:    hex.EncodeToString(checkpoint.StateCommitment[:]),
		WriteSetCommitment: hex.EncodeToString(checkpoint.WriteSetCommitment[:]),
		Entries:            checkpoint.Entries,
		WitnessPublicKey:   hex.EncodeToString(checkpoint.WitnessPublicKey),
		WitnessSignature:   hex.EncodeToString(checkpoint.WitnessSignature),
		WitnessSignatures:  witnessSignatures,
	}
}

func checkpointFromWire(wire checkpointWire) (*AVMCheckpoint, error) {
	decodeHash := func(name, encoded string) (crypto.Hash32, error) {
		var hash crypto.Hash32
		raw, err := hex.DecodeString(encoded)
		if err != nil || len(raw) != len(hash) {
			return hash, fmt.Errorf("avm checkpoint: malformed %s", name)
		}
		copy(hash[:], raw)
		return hash, nil
	}
	genesisHash, err := decodeHash("genesis hash", wire.GenesisHash)
	if err != nil {
		return nil, err
	}
	blockHash, err := decodeHash("block hash", wire.BlockHash)
	if err != nil {
		return nil, err
	}
	stateCommitment, err := decodeHash("state commitment", wire.StateCommitment)
	if err != nil {
		return nil, err
	}
	writeSetCommitment, err := decodeHash("write-set commitment", wire.WriteSetCommitment)
	if err != nil {
		return nil, err
	}
	var publicKey crypto.ValidatorPubKey
	var signature []byte
	if wire.WitnessPublicKey != "" || wire.WitnessSignature != "" {
		publicBytes, err := hex.DecodeString(wire.WitnessPublicKey)
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: malformed witness public key")
		}
		publicKey, err = crypto.ValidatorPubKeyFromBytes(publicBytes)
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: malformed witness public key")
		}
		signature, err = hex.DecodeString(wire.WitnessSignature)
		if err != nil || len(signature) != 64 {
			return nil, fmt.Errorf("avm checkpoint: malformed witness signature")
		}
	}
	if len(wire.Entries) > maxAVMCheckpointEntries {
		return nil, fmt.Errorf("avm checkpoint: too many state entries")
	}
	witnessSignatures := make([]CheckpointWitnessSignature, len(wire.WitnessSignatures))
	for i, encoded := range wire.WitnessSignatures {
		publicBytes, err := hex.DecodeString(encoded.WitnessPublicKey)
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: malformed witness public key at signature %d", i)
		}
		witnessPublicKey, err := crypto.ValidatorPubKeyFromBytes(publicBytes)
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: malformed witness public key at signature %d", i)
		}
		witnessSignature, err := hex.DecodeString(encoded.Signature)
		if err != nil || len(witnessSignature) != 64 {
			return nil, fmt.Errorf("avm checkpoint: malformed witness signature at index %d", i)
		}
		witnessSignatures[i] = CheckpointWitnessSignature{
			WitnessPublicKey: witnessPublicKey,
			Signature:        witnessSignature,
		}
	}
	return &AVMCheckpoint{
		Version:            wire.Version,
		ActivationHeight:   wire.ActivationHeight,
		Height:             wire.Height,
		GenesisHash:        genesisHash,
		BlockHash:          blockHash,
		StateCommitment:    stateCommitment,
		WriteSetCommitment: writeSetCommitment,
		Entries:            wire.Entries,
		WitnessPublicKey:   publicKey,
		WitnessSignature:   signature,
		WitnessSignatures:  witnessSignatures,
	}, nil
}
