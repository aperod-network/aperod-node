// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

const lpodEarningsBackfillBatchHeights = 64
const lpodEarningsBackfillDeleteBatch = 512

type lpodEarningsProgress struct {
	FundingHash  crypto.Hash32 `json:"funding_hash"`
	TargetHash   crypto.Hash32 `json:"target_hash"`
	TargetHeight uint64        `json:"target_height"`
	NextHeight   uint64        `json:"next_height"`
}

// BackfillLPoDEarningsIndex rebuilds the versioned derived index without
// changing canonical chain state. It requires the durable tip's complete
// canonical ancestry and checkpoint bodies; an interrupted replay never has a
// ready marker, and a restart resumes only the identical target.
func (d *DB) BackfillLPoDEarningsIndex(m *LPoDMigration) error {
	if m == nil || m.Version != 3 {
		return fmt.Errorf("lpod: v3 migration required for earnings index backfill")
	}
	targetHash, targetHeight, err := d.GetTip()
	if err != nil {
		return err
	}
	indexedTip, found, err := d.GetCanonicalHash(targetHeight)
	if err != nil || !found || indexedTip != targetHash {
		if invalidateErr := d.invalidateLPoDEarningsReadiness(); invalidateErr != nil {
			return invalidateErr
		}
		return fmt.Errorf("lpod: durable tip does not match canonical height index")
	}
	target, err := d.LoadLPoDCheckpointAt(targetHash)
	if err != nil || target == nil || target.Allocation == nil ||
		target.Allocation.Version != 3 || target.State.LastHeight != targetHeight ||
		target.Allocation.Genesis != m.Genesis ||
		target.Allocation.ReconciliationRoot != m.ReconciliationRoot ||
		target.Allocation.FundingHeight != m.Height {
		return fmt.Errorf("lpod: canonical funded v3 checkpoint required for earnings backfill")
	}
	if err := d.CheckLPoDConfig(m); err != nil {
		return err
	}
	if err := d.verifyLPoDV2Genesis(m.Genesis); err != nil {
		return fmt.Errorf("lpod: migration genesis verification failed: %w", err)
	}
	funding := target.Allocation.FundingBlock
	if targetHeight < m.Height {
		return fmt.Errorf("lpod: durable tip precedes v3 funding")
	}
	fundingBlock, err := d.canonicalLPoDBlock(m.Height)
	if err != nil || fundingBlock.Hash() != funding {
		return fmt.Errorf("lpod: canonical v3 funding block mismatch")
	}
	if ready, err := d.LPoDEarningsIndexReady(funding, targetHash, targetHeight); err != nil {
		return err
	} else if ready {
		return nil
	}

	progressRaw, err := d.get(lpodEarningsProgressKey)
	if err != nil {
		return err
	}
	var progress lpodEarningsProgress
	resume := false
	if progressRaw != nil && json.Unmarshal(progressRaw, &progress) == nil &&
		progress.FundingHash == funding && progress.TargetHash == targetHash &&
		progress.TargetHeight == targetHeight && progress.NextHeight >= m.Height &&
		progress.NextHeight <= targetHeight+1 {
		resume = true
	}
	if !resume {
		// Reuse a completed canonical prefix when extending readiness to a
		// newer durable tip. Otherwise discard all old-fork and partial rows.
		start := m.Height
		rawReady, err := d.get(lpodEarningsReadyKey)
		if err != nil {
			return err
		}
		if len(rawReady) == 3+32+32+8 && bytes.Equal(rawReady[:3], []byte("v2/")) &&
			bytes.Equal(rawReady[3:35], funding[:]) {
			var oldTip crypto.Hash32
			copy(oldTip[:], rawReady[35:67])
			oldHeight := binary.BigEndian.Uint64(rawReady[67:75])
			if oldHeight >= m.Height && oldHeight < targetHeight {
				block, blockErr := d.canonicalLPoDBlock(oldHeight)
				if blockErr == nil && block.Hash() == oldTip {
					start = oldHeight + 1
					resume = true
				}
			}
		}
		if !resume {
			batch := new(leveldb.Batch)
			batch.Delete(lpodEarningsReadyKey)
			batch.Delete(lpodEarningsProgressKey)
			if err := d.db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
				return err
			}
			if err := d.clearLPoDEarningsRows(); err != nil {
				return err
			}
		}
		progress = lpodEarningsProgress{FundingHash: funding, TargetHash: targetHash, TargetHeight: targetHeight, NextHeight: start}
		progressRaw, err = json.Marshal(progress)
		if err != nil {
			return err
		}
		batch := new(leveldb.Batch)
		batch.Delete(lpodEarningsReadyKey)
		batch.Put(lpodEarningsProgressKey, progressRaw)
		if err := d.db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
			return err
		}
	}

	for progress.NextHeight <= targetHeight {
		end := progress.NextHeight + lpodEarningsBackfillBatchHeights - 1
		if end > targetHeight {
			end = targetHeight
		}
		batch := new(leveldb.Batch)
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
					return fmt.Errorf("lpod: noncanonical or incomplete ancestry during earnings replay")
				}
			}
			refs, err := d.classifyHistoricalLPoDBlock(block, m, funding)
			if err != nil {
				return fmt.Errorf("lpod: earnings replay failed at height %d: %w", height, err)
			}
			for _, ref := range refs {
				key := lpodEarningsPrefix(ref.address)
				var suffix [12]byte
				binary.BigEndian.PutUint64(suffix[:8], block.Header.Height)
				binary.BigEndian.PutUint32(suffix[8:], ref.outputIndex)
				key = append(key, suffix[:]...)
				blockHash := block.Hash()
				key = append(key, blockHash[:]...)
				key = append(key, ref.txHash[:]...)
				raw, err := json.Marshal(ref.StoredUTXO)
				if err != nil {
					return err
				}
				batch.Put(key, raw)
			}
		}
		progress.NextHeight = end + 1
		progressRaw, err = json.Marshal(progress)
		if err != nil {
			return err
		}
		batch.Put(lpodEarningsProgressKey, progressRaw)
		if err := d.db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
			return err
		}
	}

	currentHash, currentHeight, err := d.GetTip()
	if err != nil || currentHash != targetHash || currentHeight != targetHeight {
		return fmt.Errorf("lpod: durable tip changed during earnings replay")
	}
	indexedTip, found, err = d.GetCanonicalHash(targetHeight)
	if err != nil || !found || indexedTip != targetHash {
		return fmt.Errorf("lpod: durable tip no longer matches canonical height index")
	}
	if _, err := d.canonicalLPoDBlock(targetHeight); err != nil {
		return err
	}
	batch := new(leveldb.Batch)
	batch.Put(lpodEarningsReadyKey, earningsReadyValue(funding, targetHash, targetHeight))
	batch.Delete(lpodEarningsProgressKey)
	return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}

func (d *DB) invalidateLPoDEarningsReadiness() error {
	batch := new(leveldb.Batch)
	batch.Delete(lpodEarningsReadyKey)
	batch.Delete(lpodEarningsProgressKey)
	return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}

func (d *DB) clearLPoDEarningsRows() error {
	iter := d.db.NewIterator(util.BytesPrefix([]byte("lpod/earnings-output/v2/")), nil)
	defer iter.Release()
	batch := new(leveldb.Batch)
	count := 0
	for ok := iter.First(); ok; ok = iter.Next() {
		batch.Delete(append([]byte{}, iter.Key()...))
		count++
		if count == lpodEarningsBackfillDeleteBatch {
			if err := d.db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
				return err
			}
			batch.Reset()
			count = 0
		}
	}
	if err := iter.Error(); err != nil {
		return err
	}
	if count > 0 {
		return d.db.Write(batch, &opt.WriteOptions{Sync: true})
	}
	return nil
}

func (d *DB) canonicalLPoDBlock(height uint64) (*core.Block, error) {
	rawHash, err := d.get(heightKey(height))
	if err != nil || len(rawHash) != 32 {
		return nil, fmt.Errorf("lpod: missing canonical block hash at height %d", height)
	}
	var hash crypto.Hash32
	copy(hash[:], rawHash)
	raw, err := d.GetRawBlock(hash)
	if err != nil || raw == nil {
		return nil, fmt.Errorf("lpod: missing canonical block body at height %d", height)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil || block.Hash() != hash ||
		block.Header.Height != height || block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
		return nil, fmt.Errorf("lpod: invalid canonical block body at height %d", height)
	}
	return &block, nil
}

func (d *DB) classifyHistoricalLPoDBlock(block *core.Block, m *LPoDMigration, funding crypto.Hash32) ([]lpodEarningsRef, error) {
	if len(block.Txs) < 2 || !block.Txs[0].IsLPoDPayout() {
		return nil, fmt.Errorf("missing canonical v10 payout")
	}
	after, err := d.LoadLPoDCheckpointAt(block.Hash())
	if err != nil || after == nil || after.Allocation == nil ||
		after.Allocation.Version != 3 || after.Allocation.FundingBlock != funding ||
		after.Allocation.Genesis != m.Genesis ||
		after.Allocation.ReconciliationRoot != m.ReconciliationRoot ||
		after.Allocation.FundingHeight != m.Height || after.State.LastHeight != block.Header.Height {
		return nil, fmt.Errorf("canonical v3 transition checkpoint mismatch")
	}
	digest, err := block.Txs[1].LPoDCheckpointDigest()
	if err != nil || digest != after.Digest() {
		return nil, fmt.Errorf("block checkpoint digest mismatch")
	}
	auth, err := block.Txs[0].LPoDPayoutAuthorization()
	if err != nil || auth.Digest != after.Digest() {
		return nil, fmt.Errorf("invalid payout authorization")
	}

	var before *LPoDCheckpoint
	if block.Header.Height > m.Height {
		before, err = d.LoadLPoDCheckpointAt(block.Header.PrevHash)
		if err != nil || before == nil || before.Allocation == nil ||
			before.Allocation.FundingBlock != funding || before.State.LastHeight+1 != block.Header.Height {
			return nil, fmt.Errorf("missing canonical parent checkpoint")
		}
	}
	rewards := map[crypto.Address]uint64{}
	principal := map[crypto.Address]uint64{}
	var priorAngel, priorLeader, priorReturned uint64
	if before != nil {
		priorAngel, priorLeader, priorReturned = before.State.AngelPaid, before.State.LeaderPaid, before.PrincipalReturned
	}
	if after.State.AngelPaid < priorAngel || after.State.LeaderPaid < priorLeader ||
		after.PrincipalReturned < priorReturned {
		return nil, fmt.Errorf("canonical payout counters decreased")
	}
	angelPaid := after.State.AngelPaid - priorAngel
	leaderPaid := after.State.LeaderPaid - priorLeader
	returned := after.PrincipalReturned - priorReturned
	if leaderPaid > 0 {
		rewards[auth.Leader] = leaderPaid
	}

	var positionPaid, principalReturned uint64
	if before != nil {
		parentBlock, err := d.canonicalLPoDBlock(block.Header.Height - 1)
		if err != nil || parentBlock.Hash() != block.Header.PrevHash ||
			block.Header.Timestamp <= parentBlock.Header.Timestamp {
			return nil, fmt.Errorf("invalid canonical accrual clock")
		}
		elapsed := uint64(block.Header.Timestamp - parentBlock.Header.Timestamp)
		if elapsed > LPoDMaxElapsedNS {
			elapsed = LPoDMaxElapsedNS
		}
		for id, old := range before.Positions {
			next, exists := after.Positions[id]
			if !exists {
				if !old.Returned || old.Due != 0 {
					return nil, fmt.Errorf("position disappeared with unpaid balance")
				}
				continue
			}
			if next.Withdrawn < old.Withdrawn {
				return nil, fmt.Errorf("principal return counter decreased")
			}
			principalDelta := next.Withdrawn - old.Withdrawn
			principalReturned, err = lpodAdd(principalReturned, principalDelta)
			if err != nil {
				return nil, err
			}
			principal[next.Deposit.Beneficiary], err = lpodAdd(principal[next.Deposit.Beneficiary], principalDelta)
			if err != nil {
				return nil, err
			}
			accrued := old.Due
			if old.UnlockHeight == 0 {
				q, err := inferHistoricalLPoDAccrual(old, next, elapsed)
				if err != nil {
					return nil, err
				}
				accrued, err = lpodAdd(accrued, q)
				if err != nil {
					return nil, err
				}
			} else if next.APRCarry != old.APRCarry {
				return nil, fmt.Errorf("returned position carry changed")
			}
			if next.Due > accrued {
				return nil, fmt.Errorf("position liability increased beyond canonical accrual")
			}
			paid := accrued - next.Due
			positionPaid, err = lpodAdd(positionPaid, paid)
			if err != nil {
				return nil, err
			}
			rewards[next.Deposit.Beneficiary], err = lpodAdd(rewards[next.Deposit.Beneficiary], paid)
			if err != nil {
				return nil, err
			}
		}
		for id, next := range after.Positions {
			if _, exists := before.Positions[id]; !exists && (next.Due != 0 || next.APRCarry != 0) {
				return nil, fmt.Errorf("new position has noncanonical retroactive accrual")
			}
			if _, exists := before.Positions[id]; !exists && next.Withdrawn > 0 {
				principalReturned, err = lpodAdd(principalReturned, next.Withdrawn)
				if err != nil {
					return nil, err
				}
				principal[next.Deposit.Beneficiary], err = lpodAdd(principal[next.Deposit.Beneficiary], next.Withdrawn)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	if positionPaid != angelPaid || principalReturned != returned {
		return nil, fmt.Errorf("position transition does not conserve paid rewards and principal")
	}
	if before == nil && (len(after.Positions) != 0 || angelPaid != 0 || returned != 0) {
		return nil, fmt.Errorf("funding transition contains unclassifiable position payouts")
	}

	addressSet := map[crypto.Address]bool{}
	for address := range rewards {
		addressSet[address] = true
	}
	for address := range principal {
		addressSet[address] = true
	}
	addresses := make([]string, 0, len(addressSet))
	for address := range addressSet {
		if rewards[address] > 0 || principal[address] > 0 {
			addresses = append(addresses, string(address))
		}
	}
	sort.Strings(addresses)
	expected := core.Transaction{Version: core.TxVersionLPoDPayout}
	expected.Extra, err = json.Marshal(core.LPoDPayoutAuthorization{Leader: auth.Leader, Digest: after.Digest()})
	if err != nil {
		return nil, err
	}
	for _, text := range addresses {
		address := crypto.Address(text)
		total, err := lpodAdd(rewards[address], principal[address])
		if err != nil {
			return nil, err
		}
		output, err := core.BuildLPoDPayoutOutput(address, total, block.Header.Height, block.Header.PrevHash, after.Digest())
		if err != nil {
			return nil, err
		}
		expected.Outputs = append(expected.Outputs, output)
	}
	if !reflect.DeepEqual(block.Txs[0], expected) {
		return nil, fmt.Errorf("canonical payout does not match reconstructed transition")
	}
	refs := make([]lpodEarningsRef, 0, len(addresses))
	for i, text := range addresses {
		address := crypto.Address(text)
		if !isLPoDPureReward(rewards[address], principal[address]) {
			continue
		}
		output := block.Txs[0].Outputs[i]
		u := StoredUTXO{TxHash: block.Txs[0].Hash(), OutputIndex: uint32(i),
			OneTimePub: output.OneTimePub, TxPubKey: output.TxPubKey,
			AmountCommit: output.AmountCommit, EncAmount: output.EncAmount,
			BlockHeight: block.Header.Height, AmountNAPRO: rewards[address]}
		refs = append(refs, lpodEarningsRef{StoredUTXO: u, address: address,
			txHash: u.TxHash, outputIndex: uint32(i)})
	}
	return refs, nil
}

func inferHistoricalLPoDAccrual(before, after LPoDPosition, elapsed uint64) (uint64, error) {
	if after.APRCarry >= lpodAPRDenominator || before.APRCarry >= lpodAPRDenominator {
		return 0, fmt.Errorf("invalid historical APR carry")
	}
	remaining := before.Deposit.Amount - before.Withdrawn
	values := map[uint64]bool{}
	if after.APRCarry == before.APRCarry {
		values[0] = true // canonical inactive-vault case
	}
	// Enumerate the complete APR schedule used by lpod.TierFor. A carry that
	// admits multiple distinct accrual amounts is deliberately not guessed.
	for _, rate := range []uint64{3, 5, 6, 8, 9, 10} {
		n := new(big.Int).SetUint64(remaining)
		n.Mul(n, new(big.Int).SetUint64(rate))
		n.Mul(n, new(big.Int).SetUint64(elapsed))
		n.Add(n, new(big.Int).SetUint64(before.APRCarry))
		q, rem := new(big.Int), new(big.Int)
		q.QuoRem(n, new(big.Int).SetUint64(lpodAPRDenominator), rem)
		if rem.Uint64() == after.APRCarry {
			if !q.IsUint64() {
				return 0, fmt.Errorf("historical accrual overflow")
			}
			values[q.Uint64()] = true
		}
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("historical APR accrual is ambiguous")
	}
	for q := range values {
		return q, nil
	}
	return 0, fmt.Errorf("historical APR accrual unavailable")
}
