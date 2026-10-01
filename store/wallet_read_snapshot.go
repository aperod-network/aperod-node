// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// WalletReadSnapshot pins the wallet-facing namespaces to one LevelDB view.
// It owns the underlying LevelDB snapshot; callers must call Release when done.
type WalletReadSnapshot struct {
	snapshot *leveldb.Snapshot
	release  sync.Once
}

// NewWalletReadSnapshot captures a point-in-time LevelDB view without decoding
// data. Callers can do expensive reads after the snapshot has been captured.
func (d *DB) NewWalletReadSnapshot() (*WalletReadSnapshot, error) {
	snapshot, err := d.db.GetSnapshot()
	if err != nil {
		return nil, err
	}
	return &WalletReadSnapshot{snapshot: snapshot}, nil
}

// Release releases the pinned LevelDB view. It is safe to call more than once.
func (s *WalletReadSnapshot) Release() {
	if s == nil || s.snapshot == nil {
		return
	}
	s.release.Do(s.snapshot.Release)
}

func (s *WalletReadSnapshot) get(key []byte) ([]byte, error) {
	value, err := s.snapshot.Get(key, nil)
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	return value, err
}

// GetTip returns the tip metadata captured by this snapshot.
func (s *WalletReadSnapshot) GetTip() (hash crypto.Hash32, height uint64, err error) {
	hashBytes, err := s.get(append(append([]byte{}, prefixMeta...), []byte("tip/hash")...))
	if err != nil || hashBytes == nil {
		return
	}
	copy(hash[:], hashBytes)
	heightBytes, err := s.get(append(append([]byte{}, prefixMeta...), []byte("tip/height")...))
	if err != nil || len(heightBytes) < 8 {
		return
	}
	height = binary.LittleEndian.Uint64(heightBytes)
	return
}

// GetCanonicalHash returns the exact canonical height-index hash captured by
// this snapshot, reporting malformed entries rather than silently truncating.
func (s *WalletReadSnapshot) GetCanonicalHash(height uint64) (crypto.Hash32, bool, error) {
	value, err := s.get(heightKey(height))
	if err != nil || value == nil {
		return crypto.Hash32{}, false, err
	}
	if len(value) != len(crypto.Hash32{}) {
		return crypto.Hash32{}, false, fmt.Errorf(
			"store: malformed canonical hash at height %d: got %d bytes", height, len(value))
	}
	var hash crypto.Hash32
	copy(hash[:], value)
	return hash, true, nil
}

// GetHashByHeight returns the canonical hash at height, or the zero hash when
// the height is not indexed.
func (s *WalletReadSnapshot) GetHashByHeight(height uint64) (crypto.Hash32, error) {
	hash, _, err := s.GetCanonicalHash(height)
	return hash, err
}

func (s *WalletReadSnapshot) rawBlock(hash crypto.Hash32) ([]byte, error) {
	return s.get(append(append([]byte{}, prefixBlock...), hash[:]...))
}

func (s *WalletReadSnapshot) readCanonicalBlock(height uint64) (*core.Block, error) {
	hash, found, err := s.GetCanonicalHash(height)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("missing canonical block %d", height)
	}
	raw, err := s.rawBlock(hash)
	if err != nil || raw == nil {
		return nil, fmt.Errorf("missing canonical block %d: %v", height, err)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, err
	}
	if block.Hash() != hash || block.Header.Height != height {
		return nil, fmt.Errorf("store: noncanonical block at height %d", height)
	}
	return &block, nil
}

// LoadLPoDCheckpointAt loads and validates the checkpoint at a specific hash
// using only data from this snapshot.
func (s *WalletReadSnapshot) LoadLPoDCheckpointAt(hash crypto.Hash32) (*LPoDCheckpoint, error) {
	return s.LoadLPoDCheckpointAtBounded(hash, 0)
}

// LoadLPoDCheckpointAtBounded rejects oversized serialized checkpoints before
// JSON decoding. All related block and funding-checkpoint reads use this same
// snapshot as well.
func (s *WalletReadSnapshot) LoadLPoDCheckpointAtBounded(hash crypto.Hash32, maxBytes int) (*LPoDCheckpoint, error) {
	budget := &lpodCheckpointReadBudget{maxBytes: maxBytes}
	return s.loadLPoDCheckpointAtWithBudget(hash, budget)
}

func (s *WalletReadSnapshot) loadLPoDCheckpointAtWithBudget(
	hash crypto.Hash32,
	budget *lpodCheckpointReadBudget,
) (*LPoDCheckpoint, error) {
	return loadLPoDCheckpointWithReadBudget(
		hash, 0, budget, s.get, s.rawBlock, s.readCanonicalBlock,
		func(checkpointHash crypto.Hash32) (*LPoDCheckpoint, error) {
			return s.loadLPoDCheckpointAtWithBudget(checkpointHash, budget)
		},
	)
}

// LPoDWalletIndexReady reports whether the captured wallet index was committed
// for the requested funding block.
func (s *WalletReadSnapshot) LPoDWalletIndexReady(funding crypto.Hash32) (bool, error) {
	value, err := s.get(lpodWalletReadyKey)
	return string(value) == string(funding[:]), err
}

// LPoDWalletOutputs returns one bounded page from the captured wallet index,
// filtering spend markers from the same LevelDB snapshot.
func (s *WalletReadSnapshot) LPoDWalletOutputs(
	address crypto.Address,
	cursor string,
	limit int,
) ([]StoredUTXO, string, error) {
	if limit < 1 || limit > 128 {
		return nil, "", fmt.Errorf("invalid page limit")
	}
	prefix := lpodWalletPrefix(address)
	iter := s.snapshot.NewIterator(util.BytesPrefix(prefix), nil)
	defer iter.Release()
	ok := iter.First()
	if cursor != "" {
		key, err := hex.DecodeString(cursor)
		if err != nil || !bytes.HasPrefix(key, prefix) || len(key) != len(prefix)+76 {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		ok = iter.Seek(key)
		if ok && string(iter.Key()) == string(key) {
			ok = iter.Next()
		}
	}
	rows := []StoredUTXO{}
	next := ""
	examined := 0
	for ok && examined < limit {
		examined++
		var output StoredUTXO
		if err := json.Unmarshal(iter.Value(), &output); err != nil {
			return nil, "", err
		}
		spent, err := s.get(spentUTXOKey(output.TxHash, output.OutputIndex))
		if err != nil {
			return nil, "", err
		}
		if spent == nil {
			rows = append(rows, output)
		}
		next = hex.EncodeToString(iter.Key())
		ok = iter.Next()
	}
	if !ok {
		next = ""
	}
	return rows, next, iter.Error()
}

// GetUTXO retrieves an output from the captured active-output namespace.
func (s *WalletReadSnapshot) GetUTXO(txHash crypto.Hash32, outIdx uint32) (*StoredUTXO, error) {
	data, err := s.get(utxoKey(txHash, outIdx))
	if err != nil || data == nil {
		return nil, err
	}
	var output StoredUTXO
	if err := json.Unmarshal(data, &output); err != nil {
		return nil, err
	}
	return &output, nil
}

// IsUTXOSpentChecked checks the captured spent-UTXO index.
func (s *WalletReadSnapshot) IsUTXOSpentChecked(hash crypto.Hash32, index uint32) (bool, error) {
	value, err := s.get(spentUTXOKey(hash, index))
	return value != nil, err
}

// IsKeyImageSpent checks the captured key-image index, matching DB
// canonicalization and raw-key fallback behavior.
func (s *WalletReadSnapshot) IsKeyImageSpent(ki crypto.KeyImage) (bool, error) {
	if canonical, err := crypto.CanonicalKeyImage(ki); err == nil {
		ki = canonical
	}
	key := append(append([]byte{}, prefixKeyImage...), ki[:]...)
	value, err := s.get(key)
	return value != nil, err
}
