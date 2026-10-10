package avm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// EnsureCanonicalState verifies the canonical tip AVM marker and persisted AVM
// state. Missing marker/state is rebuilt deterministically from canonical
// blocks. Any conflicting marker or state is treated as corruption and fails
// closed. An activation height of zero keeps the historical "AVM disabled"
// semantics and performs no work.
func EnsureCanonicalState(
	ctx context.Context,
	db *store.DB,
	activationHeight uint64,
	tipHeight uint64,
	tipHash crypto.Hash32,
) (rebuilt bool, err error) {
	if db == nil {
		return false, fmt.Errorf("avm startup: nil database")
	}
	if activationHeight == 0 || tipHeight < activationHeight {
		return false, nil
	}

	replay := NewMemoryStore()
	executor := NewBlockExecutor(replay)
	var tipCommitment crypto.Hash32
	var previousHash crypto.Hash32
	for height := activationHeight; height <= tipHeight; height++ {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("avm startup replay: %w", err)
		}
		indexedHash, found, err := db.GetCanonicalHash(height)
		if err != nil {
			return false, fmt.Errorf("avm startup: canonical index at height %d: %w", height, err)
		}
		if !found {
			return false, fmt.Errorf("avm startup: canonical block missing at height %d", height)
		}
		raw, err := db.GetRawBlock(indexedHash)
		if err != nil {
			return false, fmt.Errorf("avm startup: read block %d: %w", height, err)
		}
		if raw == nil {
			return false, fmt.Errorf("avm startup: canonical block body missing at height %d", height)
		}
		var block core.Block
		if err := json.Unmarshal(raw, &block); err != nil {
			return false, fmt.Errorf("avm startup: corrupt block %d: %w", height, err)
		}
		if block.Header.Height != height || block.Hash() != indexedHash {
			return false, fmt.Errorf("avm startup: canonical block/index mismatch at height %d", height)
		}
		if core.MerkleRoot(block.Txs) != block.Header.MerkleRoot {
			return false, fmt.Errorf("avm startup: corrupt transaction merkle root at height %d", height)
		}
		if height > activationHeight && block.Header.PrevHash != previousHash {
			return false, fmt.Errorf("avm startup: broken canonical linkage at height %d", height)
		}
		if height == tipHeight && indexedHash != tipHash {
			return false, fmt.Errorf("avm startup: supplied tip does not match canonical block at height %d", height)
		}
		prepared, err := executor.PrepareBlock(ctx, &block)
		if err != nil {
			return false, fmt.Errorf("avm startup: replay block %d: %w", height, err)
		}
		if err := replay.Apply(prepared.Writes); err != nil {
			return false, fmt.Errorf("avm startup: apply replay block %d: %w", height, err)
		}
		if height == tipHeight {
			tipCommitment = prepared.WriteSetCommitment
		}
		previousHash = indexedHash
	}

	markerHash, markerCommitment, markerFound, err := db.GetAVMCommitment(tipHeight)
	if err != nil {
		return false, fmt.Errorf("avm startup: corrupt tip commitment marker: %w", err)
	}
	if markerFound && (markerHash != tipHash || markerCommitment != tipCommitment) {
		return false, fmt.Errorf("avm startup: canonical tip commitment marker mismatch at height %d", tipHeight)
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
	// Without the atomic marker there is no durable evidence that any existing
	// state belongs to this tip. Replace it wholesale; this also handles an old
	// value for a key overwritten by the interrupted block.
	if !markerFound {
		if err := replace(); err != nil {
			return false, fmt.Errorf("avm startup: persist rebuilt state: %w", err)
		}
		return true, nil
	}

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
	if markerFound && !stateMissing {
		return false, nil
	}
	if err := replace(); err != nil {
		return false, fmt.Errorf("avm startup: persist rebuilt state: %w", err)
	}
	return true, nil
}
