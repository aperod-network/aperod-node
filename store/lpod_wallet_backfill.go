// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

const lpodWalletBackfillBatchHeights = 64

var lpodWalletBackfillReadyKey = []byte("lpod/wallet-ready/v2")
var lpodWalletBackfillProgressKey = []byte("lpod/wallet-progress/v2")

type lpodWalletBackfillProgress struct {
	FundingHash  crypto.Hash32 `json:"funding_hash"`
	TargetHash   crypto.Hash32 `json:"target_hash"`
	TargetHeight uint64        `json:"target_height"`
	NextHeight   uint64        `json:"next_height"`
}

// BackfillLPoDWalletIndex rebuilds the native wallet discovery index from
// canonical v3 payout blocks. The readiness marker is only written after the
// complete funding-to-tip range has been replayed and rechecked.
func (d *DB) BackfillLPoDWalletIndex(m *LPoDMigration) error {
	if m == nil || m.Version != 3 {
		return fmt.Errorf("lpod: v3 migration required for wallet index backfill")
	}
	if err := d.CheckLPoDConfig(m); err != nil {
		return err
	}
	if err := d.verifyLPoDV2Genesis(m.Genesis); err != nil {
		return fmt.Errorf("lpod: migration genesis verification failed: %w", err)
	}

	targetHash, targetHeight, err := d.GetTip()
	if err != nil {
		return err
	}
	indexedTip, found, err := d.GetCanonicalHash(targetHeight)
	if err != nil || !found || indexedTip != targetHash {
		return fmt.Errorf("lpod: durable tip does not match canonical height index")
	}
	target, err := d.LoadLPoDCheckpointAt(targetHash)
	if err != nil || target == nil || target.Allocation == nil ||
		target.Allocation.Version != 3 || target.State.LastHeight != targetHeight ||
		target.Allocation.Genesis != m.Genesis ||
		target.Allocation.ReconciliationRoot != m.ReconciliationRoot ||
		target.Allocation.FundingHeight != m.Height || targetHeight < m.Height {
		return fmt.Errorf("lpod: canonical funded v3 checkpoint required for wallet index backfill")
	}
	funding := target.Allocation.FundingBlock
	fundingBlock, err := d.canonicalLPoDBlock(m.Height)
	if err != nil || fundingBlock.Hash() != funding {
		return fmt.Errorf("lpod: canonical v3 funding block mismatch")
	}
	if ready, err := d.LPoDWalletIndexReady(funding); err != nil {
		return err
	} else if ready {
		return nil
	}

	progressRaw, err := d.get(lpodWalletBackfillProgressKey)
	if err != nil {
		return err
	}
	var progress lpodWalletBackfillProgress
	resume := progressRaw != nil && json.Unmarshal(progressRaw, &progress) == nil &&
		progress.FundingHash == funding && progress.TargetHash == targetHash &&
		progress.TargetHeight == targetHeight && progress.NextHeight >= m.Height &&
		progress.NextHeight <= targetHeight+1
	if !resume {
		progress = lpodWalletBackfillProgress{
			FundingHash: funding, TargetHash: targetHash,
			TargetHeight: targetHeight, NextHeight: m.Height,
		}
	}
	progressRaw, err = json.Marshal(progress)
	if err != nil {
		return err
	}
	batch := new(leveldb.Batch)
	batch.Delete(lpodWalletReadyKey)
	batch.Delete(lpodWalletBackfillReadyKey)
	batch.Put(lpodWalletBackfillProgressKey, progressRaw)
	if err := d.db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
		return err
	}

	for progress.NextHeight <= targetHeight {
		end := progress.NextHeight + lpodWalletBackfillBatchHeights - 1
		if end < progress.NextHeight || end > targetHeight {
			end = targetHeight
		}
		batch = new(leveldb.Batch)
		for height := progress.NextHeight; height <= end; height++ {
			block, err := d.canonicalLPoDBlock(height)
			if err != nil {
				return err
			}
			if height == m.Height {
				if block.Hash() != funding {
					return fmt.Errorf("lpod: funding height is not on current canonical branch")
				}
			} else {
				parent, err := d.canonicalLPoDBlock(height - 1)
				if err != nil || block.Header.PrevHash != parent.Hash() {
					return fmt.Errorf("lpod: noncanonical or incomplete ancestry during wallet index replay")
				}
			}
			if err := d.indexHistoricalLPoDWalletPayout(batch, block, m, funding); err != nil {
				return fmt.Errorf("lpod: wallet index replay failed at height %d: %w", height, err)
			}
		}
		progress.NextHeight = end + 1
		progressRaw, err = json.Marshal(progress)
		if err != nil {
			return err
		}
		batch.Put(lpodWalletBackfillProgressKey, progressRaw)
		if err := d.db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
			return err
		}
	}

	currentHash, currentHeight, err := d.GetTip()
	if err != nil || currentHash != targetHash || currentHeight != targetHeight {
		return fmt.Errorf("lpod: durable tip changed during wallet index replay")
	}
	indexedTip, found, err = d.GetCanonicalHash(targetHeight)
	if err != nil || !found || indexedTip != targetHash {
		return fmt.Errorf("lpod: durable tip no longer matches canonical height index")
	}
	if _, err := d.canonicalLPoDBlock(targetHeight); err != nil {
		return err
	}

	batch = new(leveldb.Batch)
	if err := d.mergeLPoDWalletBlockKeys(batch, funding, [][]byte{
		lpodWalletReadyKey, lpodWalletBackfillReadyKey,
	}); err != nil {
		return err
	}
	batch.Put(lpodWalletReadyKey, funding[:])
	batch.Put(lpodWalletBackfillReadyKey, funding[:])
	batch.Delete(lpodWalletBackfillProgressKey)
	return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}

func (d *DB) indexHistoricalLPoDWalletPayout(
	batch *leveldb.Batch,
	block *core.Block,
	m *LPoDMigration,
	funding crypto.Hash32,
) error {
	if len(block.Txs) < 2 || !block.Txs[0].IsLPoDPayout() {
		return fmt.Errorf("missing canonical v10 payout")
	}
	after, err := d.LoadLPoDCheckpointAt(block.Hash())
	if err != nil || after == nil || after.Allocation == nil ||
		after.Allocation.Version != 3 || after.Allocation.FundingBlock != funding ||
		after.Allocation.Genesis != m.Genesis ||
		after.Allocation.ReconciliationRoot != m.ReconciliationRoot ||
		after.Allocation.FundingHeight != m.Height ||
		after.State.LastHeight != block.Header.Height {
		return fmt.Errorf("canonical v3 transition checkpoint mismatch")
	}
	if err := after.State.Validate(); err != nil {
		return fmt.Errorf("invalid canonical v3 checkpoint state: %w", err)
	}
	checkpointDigest, err := block.Txs[1].LPoDCheckpointDigest()
	if err != nil || checkpointDigest != after.Digest() {
		return fmt.Errorf("block checkpoint digest mismatch")
	}
	auth, err := block.Txs[0].LPoDPayoutAuthorization()
	if err != nil || auth.Digest != after.Digest() {
		return fmt.Errorf("invalid payout authorization")
	}
	owners, err := lpodWalletOwners(
		auth.Leader, after, block.Header.Height, block.Header.PrevHash, after.Digest(),
	)
	if err != nil {
		return err
	}

	tx := &block.Txs[0]
	txHash := tx.Hash()
	blockHash := block.Hash()
	keys := make([][]byte, 0, len(tx.Outputs))
	for outputIndex, output := range tx.Outputs {
		address, ok := owners[output.OneTimePub]
		if !ok {
			return fmt.Errorf("payout beneficiary index mismatch")
		}
		key := lpodWalletPrefix(address)
		var suffix [12]byte
		binary.BigEndian.PutUint64(suffix[:8], block.Header.Height)
		binary.BigEndian.PutUint32(suffix[8:], uint32(outputIndex))
		key = append(key, suffix[:]...)
		key = append(key, blockHash[:]...)
		key = append(key, txHash[:]...)
		stored := StoredUTXO{
			TxHash: txHash, OutputIndex: uint32(outputIndex),
			OneTimePub: output.OneTimePub, TxPubKey: output.TxPubKey,
			AmountCommit: output.AmountCommit, EncAmount: output.EncAmount,
			BlockHeight: block.Header.Height,
		}
		raw, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		batch.Put(key, raw)
		keys = append(keys, key)
	}
	return d.mergeLPoDWalletBlockKeys(batch, blockHash, keys)
}

func (d *DB) mergeLPoDWalletBlockKeys(
	batch *leveldb.Batch,
	blockHash crypto.Hash32,
	keys [][]byte,
) error {
	blockKey := lpodWalletBlockKey(blockHash)
	raw, err := d.get(blockKey)
	if err != nil {
		return err
	}
	var existing [][]byte
	if raw != nil {
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("invalid wallet block-index record: %w", err)
		}
	}
	seen := make(map[string]struct{}, len(existing)+len(keys))
	for _, key := range existing {
		seen[string(key)] = struct{}{}
	}
	for _, key := range keys {
		if _, ok := seen[string(key)]; ok {
			continue
		}
		seen[string(key)] = struct{}{}
		existing = append(existing, append([]byte(nil), key...))
	}
	encoded, err := json.Marshal(existing)
	if err != nil {
		return err
	}
	batch.Put(blockKey, encoded)
	return nil
}
