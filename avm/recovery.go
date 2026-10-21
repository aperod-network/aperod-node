package avm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// EnsureCanonicalState verifies the canonical tip AVM marker and persisted AVM
// state. Missing marker/state is rebuilt deterministically from canonical
// blocks. Any conflicting marker or state is treated as corruption and fails
// closed. An activation height of zero keeps the historical "AVM disabled"
// semantics and performs no work.
//
// This compatibility entry point performs the historical full replay. Node
// startup should use EnsureCanonicalStateWithQuorumCheckpoint when trusted
// witness keys and a quorum checkpoint are configured.
func EnsureCanonicalState(
	ctx context.Context,
	db *store.DB,
	activationHeight uint64,
	tipHeight uint64,
	tipHash crypto.Hash32,
) (bool, error) {
	if activationHeight > 0 && tipHeight >= activationHeight {
		replayCount := tipHeight - activationHeight + 1
		if replayCount > MaxAVMCheckpointReplay {
			return false, fmt.Errorf(
				"avm startup: %d-block full replay exceeds limit %d; an authenticated checkpoint is required",
				replayCount, MaxAVMCheckpointReplay,
			)
		}
	}
	return ensureCanonicalState(ctx, db, activationHeight, tipHeight, tipHash, nil)
}

// EnsureCanonicalStateWithCheckpoint resumes from a legacy v1
// operator-attested checkpoint.
//
// Deprecated: quorum-configured production nodes should use
// EnsureCanonicalStateWithQuorumCheckpoint.
func EnsureCanonicalStateWithCheckpoint(
	ctx context.Context,
	db *store.DB,
	activationHeight uint64,
	tipHeight uint64,
	tipHash crypto.Hash32,
	checkpointPath string,
	authorities []crypto.ValidatorPubKey,
	signer CheckpointSigner,
	maxReplayBlocks uint64,
) (bool, error) {
	if activationHeight == 0 || tipHeight < activationHeight {
		return false, nil
	}
	if checkpointPath == "" {
		return false, fmt.Errorf("avm startup: checkpoint path is required")
	}
	if len(authorities) == 0 {
		return false, fmt.Errorf("avm startup: trusted checkpoint witness is required")
	}
	if signer == nil || !checkpointAuthorityAllowed(authorities, signer.Public()) {
		return false, fmt.Errorf("avm startup: local checkpoint signer must be a pinned witness")
	}
	if maxReplayBlocks == 0 {
		maxReplayBlocks = MaxAVMCheckpointReplay
	}
	return ensureCanonicalState(
		ctx, db, activationHeight, tipHeight, tipHash,
		&checkpointRecovery{
			path:            checkpointPath,
			authorities:     authorities,
			signer:          signer,
			maxReplayBlocks: maxReplayBlocks,
		},
	)
}

// EnsureCanonicalStateWithQuorumCheckpoint is the node startup path for
// checkpoint version 2. The checkpoint must carry a valid m-of-n signature
// set; this node does not need private keys from other witnesses. Successful
// replay never downgrades or re-signs a quorum checkpoint.
func EnsureCanonicalStateWithQuorumCheckpoint(
	ctx context.Context,
	db *store.DB,
	activationHeight uint64,
	tipHeight uint64,
	tipHash crypto.Hash32,
	checkpointPath string,
	authorities []crypto.ValidatorPubKey,
	threshold int,
	maxReplayBlocks uint64,
) (bool, error) {
	if activationHeight == 0 || tipHeight < activationHeight {
		return false, nil
	}
	if checkpointPath == "" {
		return false, fmt.Errorf("avm startup: checkpoint path is required")
	}
	if err := validateCheckpointAuthorities(authorities, threshold); err != nil {
		return false, err
	}
	if maxReplayBlocks == 0 {
		maxReplayBlocks = MaxAVMCheckpointReplay
	}
	return ensureCanonicalState(
		ctx, db, activationHeight, tipHeight, tipHash,
		&checkpointRecovery{
			path:             checkpointPath,
			authorities:      authorities,
			witnessThreshold: threshold,
			maxReplayBlocks:  maxReplayBlocks,
		},
	)
}

type checkpointRecovery struct {
	path             string
	authorities      []crypto.ValidatorPubKey
	signer           CheckpointSigner
	witnessThreshold int
	maxReplayBlocks  uint64
}

func ensureCanonicalState(
	ctx context.Context,
	db *store.DB,
	activationHeight uint64,
	tipHeight uint64,
	tipHash crypto.Hash32,
	checkpoint *checkpointRecovery,
) (rebuilt bool, err error) {
	if db == nil {
		return false, fmt.Errorf("avm startup: nil database")
	}
	if activationHeight == 0 || tipHeight < activationHeight {
		return false, nil
	}

	replay := NewMemoryStore()
	var (
		startHeight       = activationHeight
		previousHash      crypto.Hash32
		havePreviousHash  bool
		tipCommitment     crypto.Hash32
		haveTipCommitment bool
		strictHistory     bool
		genesisHash       crypto.Hash32
		skipReplay        bool
	)

	if checkpoint != nil {
		strictHistory = true
		var found bool
		genesisHash, found, err = db.GetCanonicalHash(0)
		if err != nil || !found || genesisHash.Zero() {
			if err != nil {
				return false, fmt.Errorf("avm startup: canonical genesis hash: %w", err)
			}
			return false, fmt.Errorf("avm startup: canonical genesis hash is missing")
		}

		snapshot, exists, readErr := ReadCheckpointFile(checkpoint.path)
		if readErr != nil {
			return false, readErr
		}
		if exists {
			var verifyErr error
			if checkpoint.witnessThreshold > 0 {
				verifyErr = VerifyCheckpointQuorum(
					snapshot,
					activationHeight,
					checkpoint.authorities,
					checkpoint.witnessThreshold,
				)
			} else {
				verifyErr = VerifyCheckpoint(snapshot, activationHeight, checkpoint.authorities)
			}
			if verifyErr != nil {
				return false, verifyErr
			}
			if snapshot.Height > tipHeight {
				return false, fmt.Errorf("avm startup: checkpoint is ahead of the canonical tip")
			}
			if snapshot.GenesisHash != genesisHash {
				return false, fmt.Errorf("avm startup: checkpoint belongs to a different genesis")
			}
			indexedHash, found, indexErr := db.GetCanonicalHash(snapshot.Height)
			if indexErr != nil {
				return false, fmt.Errorf("avm startup: checkpoint canonical index: %w", indexErr)
			}
			if !found || indexedHash != snapshot.BlockHash {
				return false, fmt.Errorf("avm startup: checkpoint block is not canonical at height %d", snapshot.Height)
			}
			markerHash, markerCommitment, markerFound, markerErr := db.GetAVMCommitment(snapshot.Height)
			if markerErr != nil {
				return false, fmt.Errorf("avm startup: checkpoint commitment marker: %w", markerErr)
			}
			if !markerFound || markerHash != snapshot.BlockHash ||
				markerCommitment != snapshot.WriteSetCommitment {
				return false, fmt.Errorf("avm startup: checkpoint write-set marker mismatch at height %d", snapshot.Height)
			}
			replayWrites := make([]Write, len(snapshot.Entries))
			for i, entry := range snapshot.Entries {
				replayWrites[i] = Write(entry)
			}
			if err := replay.Apply(replayWrites); err != nil {
				return false, fmt.Errorf("avm startup: load checkpoint state: %w", err)
			}
			previousHash = snapshot.BlockHash
			havePreviousHash = true
			tipCommitment = snapshot.WriteSetCommitment
			haveTipCommitment = snapshot.Height == tipHeight
			if snapshot.Height == tipHeight {
				skipReplay = true
				startHeight = tipHeight
			} else {
				startHeight = snapshot.Height + 1
			}
		} else {
			replayCount := tipHeight - activationHeight + 1
			if replayCount > checkpoint.maxReplayBlocks {
				return false, fmt.Errorf(
					"avm startup: trusted checkpoint is missing; replay of %d AVM blocks exceeds limit %d",
					replayCount, checkpoint.maxReplayBlocks,
				)
			}
			previousHash, found, err = db.GetCanonicalHash(activationHeight - 1)
			if err != nil || !found {
				if err != nil {
					return false, fmt.Errorf("avm startup: canonical parent index at height %d: %w", activationHeight-1, err)
				}
				return false, fmt.Errorf("avm startup: canonical parent missing at height %d", activationHeight-1)
			}
			havePreviousHash = true
		}
		if !skipReplay && tipHeight >= startHeight {
			replayCount := tipHeight - startHeight + 1
			if replayCount > checkpoint.maxReplayBlocks {
				return false, fmt.Errorf(
					"avm startup: checkpoint is too old; replay of %d AVM blocks exceeds limit %d",
					replayCount, checkpoint.maxReplayBlocks,
				)
			}
		}
	} else {
		// Preserve the existing full-history path for callers that have not
		// configured an explicit checkpoint trust root.
		if activationHeight > 0 {
			if parentHash, found, parentErr := db.GetCanonicalHash(activationHeight - 1); parentErr == nil && found {
				previousHash = parentHash
				havePreviousHash = true
			}
		}
	}

	if !skipReplay && tipHeight >= startHeight {
		tipCommitment, err = replayCanonicalRange(
			ctx,
			db,
			replay,
			startHeight,
			tipHeight,
			tipHash,
			previousHash,
			havePreviousHash,
			strictHistory,
		)
		if err != nil {
			return false, err
		}
		haveTipCommitment = true
	}
	if !haveTipCommitment {
		return false, fmt.Errorf("avm startup: checkpoint does not cover the canonical tip")
	}

	markerHash, markerCommitment, markerFound, err := db.GetAVMCommitment(tipHeight)
	if err != nil {
		return false, fmt.Errorf("avm startup: corrupt tip commitment marker: %w", err)
	}
	if markerFound && (markerHash != tipHash || markerCommitment != tipCommitment) {
		return false, fmt.Errorf("avm startup: canonical tip commitment marker mismatch at height %d; preserve the database and restore a verified canonical backup; do not delete the marker to bypass verification", tipHeight)
	}

	expectedWrites := replay.Writes()
	replace := func() error {
		storeWrites := make([]store.AVMWrite, len(expectedWrites))
		for i, write := range expectedWrites {
			storeWrites[i] = store.AVMWrite{Key: write.Key, Value: write.Value}
		}
		return db.ReplaceAVMStateAndCommitment(
			storeWrites, tipHeight, tipHash, tipCommitment,
		)
	}

	if !markerFound {
		if err := replace(); err != nil {
			return false, fmt.Errorf("avm startup: persist rebuilt state: %w", err)
		}
		rebuilt = true
	} else {
		expected := make(map[string][]byte, len(expectedWrites))
		for _, write := range expectedWrites {
			expected[string(write.Key)] = write.Value
		}
		actualCount := 0
		if err := db.IterAVMState(func(key, value []byte) error {
			actualCount++
			expectedValue, ok := expected[string(key)]
			if !ok {
				return fmt.Errorf("unexpected AVM state key %x", key)
			}
			if !bytes.Equal(value, expectedValue) {
				return fmt.Errorf("AVM state value mismatch for key %x", key)
			}
			return nil
		}); err != nil {
			return false, fmt.Errorf("avm startup: persisted state corruption: %w", err)
		}

		stateMissing := actualCount != len(expected)
		if stateMissing {
			if err := replace(); err != nil {
				return false, fmt.Errorf("avm startup: persist rebuilt state: %w", err)
			}
			rebuilt = true
		}
	}

	if checkpoint != nil && checkpoint.witnessThreshold == 0 {
		entries := make([]CheckpointEntry, len(expectedWrites))
		for i, write := range expectedWrites {
			entries[i] = CheckpointEntry(write)
		}
		fresh, err := SignCheckpoint(
			activationHeight,
			tipHeight,
			genesisHash,
			tipHash,
			tipCommitment,
			entries,
			checkpoint.signer,
			checkpoint.authorities,
		)
		if err != nil {
			return rebuilt, fmt.Errorf("avm startup: sign verified checkpoint: %w", err)
		}
		if err := WriteCheckpointFile(checkpoint.path, fresh); err != nil {
			return rebuilt, fmt.Errorf("avm startup: persist verified checkpoint: %w", err)
		}
	}
	return rebuilt, nil
}

func replayCanonicalRange(
	ctx context.Context,
	db *store.DB,
	replay *MemoryStore,
	startHeight uint64,
	tipHeight uint64,
	tipHash crypto.Hash32,
	previousHash crypto.Hash32,
	havePreviousHash bool,
	verifySignatures bool,
) (crypto.Hash32, error) {
	executor := NewBlockExecutor(replay)
	var tipCommitment crypto.Hash32
	for height := startHeight; ; height++ {
		if err := ctx.Err(); err != nil {
			return crypto.Hash32{}, fmt.Errorf("avm startup replay: %w", err)
		}
		indexedHash, found, err := db.GetCanonicalHash(height)
		if err != nil {
			return crypto.Hash32{}, fmt.Errorf("avm startup: canonical index at height %d: %w", height, err)
		}
		if !found {
			return crypto.Hash32{}, fmt.Errorf("avm startup: canonical block missing at height %d", height)
		}
		raw, err := db.GetRawBlock(indexedHash)
		if err != nil {
			return crypto.Hash32{}, fmt.Errorf("avm startup: read block %d: %w", height, err)
		}
		if raw == nil {
			return crypto.Hash32{}, fmt.Errorf("avm startup: canonical block body missing at height %d", height)
		}
		var block core.Block
		if err := json.Unmarshal(raw, &block); err != nil {
			return crypto.Hash32{}, fmt.Errorf("avm startup: corrupt block %d: %w", height, err)
		}
		if block.Header.Height != height || block.Hash() != indexedHash {
			return crypto.Hash32{}, fmt.Errorf("avm startup: canonical block/index mismatch at height %d", height)
		}
		if core.MerkleRoot(block.Txs) != block.Header.MerkleRoot {
			return crypto.Hash32{}, fmt.Errorf("avm startup: corrupt transaction merkle root at height %d", height)
		}
		if havePreviousHash && block.Header.PrevHash != previousHash {
			return crypto.Hash32{}, fmt.Errorf("avm startup: broken canonical linkage at height %d", height)
		}
		if verifySignatures && !block.Header.VerifySignature() {
			return crypto.Hash32{}, fmt.Errorf("avm startup: invalid validator signature at height %d", height)
		}
		if height == tipHeight && indexedHash != tipHash {
			return crypto.Hash32{}, fmt.Errorf("avm startup: supplied tip does not match canonical block at height %d", height)
		}
		prepared, err := executor.PrepareBlock(ctx, &block)
		if err != nil {
			return crypto.Hash32{}, fmt.Errorf("avm startup: replay block %d: %w", height, err)
		}
		markerHash, markerCommitment, markerFound, markerErr := db.GetAVMCommitment(height)
		if markerErr != nil {
			return crypto.Hash32{}, fmt.Errorf("avm startup: read commitment marker at height %d: %w", height, markerErr)
		}
		if markerFound && (markerHash != indexedHash || markerCommitment != prepared.WriteSetCommitment) {
			return crypto.Hash32{}, fmt.Errorf("avm startup: commitment marker mismatch at height %d; preserve the database and restore a verified canonical backup; do not delete the marker to bypass verification", height)
		}
		if err := replay.Apply(prepared.Writes); err != nil {
			return crypto.Hash32{}, fmt.Errorf("avm startup: apply replay block %d: %w", height, err)
		}
		tipCommitment = prepared.WriteSetCommitment
		previousHash = indexedHash
		havePreviousHash = true
		if height == tipHeight {
			break
		}
		if height == math.MaxUint64 {
			return crypto.Hash32{}, fmt.Errorf("avm startup: replay height overflow")
		}
	}
	return tipCommitment, nil
}

// BuildCheckpointDraftFromCanonical performs a full, read-only AVM replay and
// returns an unsigned v2 draft. Each independent witness should run the same
// full replay against its own canonical database before signing the draft.
func BuildCheckpointDraftFromCanonical(
	ctx context.Context,
	db *store.DB,
	activationHeight uint64,
	tipHeight uint64,
	tipHash crypto.Hash32,
) (*AVMCheckpoint, error) {
	if db == nil {
		return nil, fmt.Errorf("avm checkpoint: nil database")
	}
	if activationHeight == 0 || tipHeight < activationHeight {
		return nil, fmt.Errorf("avm checkpoint: invalid activation or tip height")
	}
	genesisHash, found, err := db.GetCanonicalHash(0)
	if err != nil || !found || genesisHash.Zero() {
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: canonical genesis hash: %w", err)
		}
		return nil, fmt.Errorf("avm checkpoint: canonical genesis hash missing")
	}
	_, storedTipHeight, err := db.GetTip()
	if err != nil {
		return nil, fmt.Errorf("avm checkpoint: read stored tip: %w", err)
	}
	if tipHeight > storedTipHeight {
		return nil, fmt.Errorf("avm checkpoint: supplied tip is above the stored canonical tip")
	}
	indexedTipHash, found, err := db.GetCanonicalHash(tipHeight)
	if err != nil || !found || indexedTipHash != tipHash {
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: read canonical target tip: %w", err)
		}
		return nil, fmt.Errorf("avm checkpoint: supplied tip is not canonical")
	}
	parentHash, found, err := db.GetCanonicalHash(activationHeight - 1)
	if err != nil || !found {
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: canonical activation parent: %w", err)
		}
		return nil, fmt.Errorf("avm checkpoint: canonical activation parent missing")
	}
	replay := NewMemoryStore()
	writeSetCommitment, err := replayCanonicalRange(
		ctx, db, replay, activationHeight, tipHeight, tipHash, parentHash, true, true,
	)
	if err != nil {
		return nil, fmt.Errorf("avm checkpoint: verify canonical history: %w", err)
	}
	writes := replay.Writes()
	entries := make([]CheckpointEntry, len(writes))
	for i, write := range writes {
		entries[i] = CheckpointEntry(write)
	}
	return NewQuorumCheckpointDraft(
		activationHeight, tipHeight, genesisHash, tipHash, writeSetCommitment, entries,
	)
}

// BuildVerifiedCheckpointFromCanonical builds a legacy operator-attested v1
// checkpoint. Quorum-configured nodes reject v1 and must use a v2 draft plus
// independently collected signatures.
func BuildVerifiedCheckpointFromCanonical(
	ctx context.Context,
	db *store.DB,
	activationHeight uint64,
	tipHeight uint64,
	tipHash crypto.Hash32,
	signer CheckpointSigner,
	authorities []crypto.ValidatorPubKey,
) (*AVMCheckpoint, error) {
	if db == nil {
		return nil, fmt.Errorf("avm checkpoint: nil database")
	}
	if activationHeight == 0 || tipHeight < activationHeight {
		return nil, fmt.Errorf("avm checkpoint: invalid activation or tip height")
	}
	if signer == nil || !checkpointAuthorityAllowed(authorities, signer.Public()) {
		return nil, fmt.Errorf("avm checkpoint: builder signer must be a pinned witness")
	}
	genesisHash, found, err := db.GetCanonicalHash(0)
	if err != nil || !found || genesisHash.Zero() {
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: canonical genesis hash: %w", err)
		}
		return nil, fmt.Errorf("avm checkpoint: canonical genesis hash missing")
	}
	storedTipHash, storedTipHeight, err := db.GetTip()
	if err != nil {
		return nil, fmt.Errorf("avm checkpoint: read tip: %w", err)
	}
	if storedTipHash != tipHash || storedTipHeight != tipHeight {
		return nil, fmt.Errorf("avm checkpoint: supplied tip differs from stored tip")
	}
	parentHash, found, err := db.GetCanonicalHash(activationHeight - 1)
	if err != nil || !found {
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: canonical activation parent: %w", err)
		}
		return nil, fmt.Errorf("avm checkpoint: canonical activation parent missing")
	}
	replay := NewMemoryStore()
	writeSetCommitment, err := replayCanonicalRange(
		ctx, db, replay, activationHeight, tipHeight, tipHash, parentHash, true, true,
	)
	if err != nil {
		return nil, fmt.Errorf("avm checkpoint: verify canonical history: %w", err)
	}
	writes := replay.Writes()
	entries := make([]CheckpointEntry, len(writes))
	for i, write := range writes {
		entries[i] = CheckpointEntry(write)
	}
	checkpoint, err := SignCheckpoint(
		activationHeight, tipHeight, genesisHash, tipHash, writeSetCommitment,
		entries, signer, authorities,
	)
	if err != nil {
		return nil, err
	}
	return checkpoint, nil
}

// CreateQuorumCheckpointDraftFromCanonicalState snapshots the already
// validated current AVM state and binds it to the current canonical tip. The
// caller must have validated this state through startup replay or consensus.
func CreateQuorumCheckpointDraftFromCanonicalState(
	db *store.DB,
	activationHeight uint64,
	height uint64,
	blockHash crypto.Hash32,
) (*AVMCheckpoint, error) {
	if db == nil {
		return nil, fmt.Errorf("avm checkpoint: nil database")
	}
	if activationHeight == 0 || height < activationHeight {
		return nil, fmt.Errorf("avm checkpoint: invalid activation or checkpoint height")
	}
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		return nil, fmt.Errorf("avm checkpoint: read canonical tip: %w", err)
	}
	if tipHeight != height || tipHash != blockHash {
		return nil, fmt.Errorf("avm checkpoint: requested block is not the current canonical tip")
	}
	indexedHash, found, err := db.GetCanonicalHash(height)
	if err != nil || !found || indexedHash != blockHash {
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: read canonical checkpoint index: %w", err)
		}
		return nil, fmt.Errorf("avm checkpoint: checkpoint block is not canonical")
	}
	genesisHash, found, err := db.GetCanonicalHash(0)
	if err != nil || !found || genesisHash.Zero() {
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: read canonical genesis: %w", err)
		}
		return nil, fmt.Errorf("avm checkpoint: canonical genesis is missing")
	}
	markerHash, writeSetCommitment, markerFound, err := db.GetAVMCommitment(height)
	if err != nil {
		return nil, fmt.Errorf("avm checkpoint: read tip write-set marker: %w", err)
	}
	if !markerFound || markerHash != blockHash {
		return nil, fmt.Errorf("avm checkpoint: canonical tip write-set marker is missing or mismatched")
	}
	entries := make([]CheckpointEntry, 0)
	if err := db.IterAVMState(func(key, value []byte) error {
		entries = append(entries, CheckpointEntry{Key: key, Value: value})
		if len(entries) > maxAVMCheckpointEntries {
			return fmt.Errorf("avm checkpoint: too many state entries")
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("avm checkpoint: read current AVM state: %w", err)
	}
	tipHashAfterSnapshot, tipHeightAfterSnapshot, err := db.GetTip()
	if err != nil {
		return nil, fmt.Errorf("avm checkpoint: recheck canonical tip: %w", err)
	}
	if tipHeightAfterSnapshot != height || tipHashAfterSnapshot != blockHash {
		return nil, fmt.Errorf("avm checkpoint: canonical tip changed while snapshotting state")
	}
	return NewQuorumCheckpointDraft(
		activationHeight, height, genesisHash, blockHash, writeSetCommitment, entries,
	)
}

// CreateCheckpointFromCanonicalState creates a legacy v1 operator-attested
// checkpoint from the already-validated current AVM state.
//
// Deprecated: quorum-configured nodes must use a v2 draft and independent
// witness signatures.
func CreateCheckpointFromCanonicalState(
	db *store.DB,
	activationHeight uint64,
	height uint64,
	blockHash crypto.Hash32,
	signer CheckpointSigner,
	authorities []crypto.ValidatorPubKey,
) (*AVMCheckpoint, error) {
	if db == nil {
		return nil, fmt.Errorf("avm checkpoint: nil database")
	}
	if activationHeight == 0 || height < activationHeight {
		return nil, fmt.Errorf("avm checkpoint: invalid activation or checkpoint height")
	}
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		return nil, fmt.Errorf("avm checkpoint: read canonical tip: %w", err)
	}
	if tipHeight != height || tipHash != blockHash {
		return nil, fmt.Errorf("avm checkpoint: requested block is not the current canonical tip")
	}
	indexedHash, found, err := db.GetCanonicalHash(height)
	if err != nil || !found || indexedHash != blockHash {
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: read canonical checkpoint index: %w", err)
		}
		return nil, fmt.Errorf("avm checkpoint: checkpoint block is not canonical")
	}
	genesisHash, found, err := db.GetCanonicalHash(0)
	if err != nil || !found || genesisHash.Zero() {
		if err != nil {
			return nil, fmt.Errorf("avm checkpoint: read canonical genesis: %w", err)
		}
		return nil, fmt.Errorf("avm checkpoint: canonical genesis is missing")
	}
	markerHash, writeSetCommitment, markerFound, err := db.GetAVMCommitment(height)
	if err != nil {
		return nil, fmt.Errorf("avm checkpoint: read tip write-set marker: %w", err)
	}
	if !markerFound || markerHash != blockHash {
		return nil, fmt.Errorf("avm checkpoint: canonical tip write-set marker is missing or mismatched")
	}
	entries := make([]CheckpointEntry, 0)
	if err := db.IterAVMState(func(key, value []byte) error {
		entries = append(entries, CheckpointEntry{Key: key, Value: value})
		if len(entries) > maxAVMCheckpointEntries {
			return fmt.Errorf("avm checkpoint: too many state entries")
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("avm checkpoint: read current AVM state: %w", err)
	}
	return SignCheckpoint(
		activationHeight, height, genesisHash, blockHash, writeSetCommitment,
		entries, signer, authorities,
	)
}
