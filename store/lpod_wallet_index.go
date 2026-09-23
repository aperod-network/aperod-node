// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package store

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

func lpodWalletPrefix(address crypto.Address) []byte {
	h := crypto.HashBytes([]byte(address))
	return append([]byte("lpod/wallet/v1/"), h[:]...)
}
func (d *DB) IsUTXOSpentChecked(hash crypto.Hash32, index uint32) (bool, error) {
	raw, err := d.get(spentUTXOKey(hash, index))
	return raw != nil, err
}
func lpodWalletBlockKey(hash crypto.Hash32) []byte {
	return append([]byte("lpod/wallet-block/v1/"), hash[:]...)
}

var lpodWalletReadyKey = []byte("lpod/wallet-ready/v1")

func (d *DB) LPoDWalletIndexReady(funding crypto.Hash32) (bool, error) {
	raw, err := d.get(lpodWalletReadyKey)
	return bytes.Equal(raw, funding[:]), err
}

// This is a discovery index only. Monetary transitions and payout validation
// remain authoritative and unchanged. Keys are committed alongside the block,
// settlement and UTXOs, never manufactured from wallet SQL metadata.
func (d *DB) appendLPoDWalletIndex(batch *leveldb.Batch, b *core.Block, r *LPoDSettlement) error {
	after, _, err := d.PreviewLPoD(b.Header.Height, r)
	if err != nil {
		return err
	}
	if after == nil || after.Allocation == nil {
		return fmt.Errorf("lpod: funded checkpoint required for wallet discovery")
	}
	candidates := map[crypto.Address]bool{r.Leader: true}
	for _, p := range after.Positions {
		candidates[p.Deposit.Beneficiary] = true
	}
	owners := map[crypto.Point32]crypto.Address{}
	for a := range candidates {
		// The public deterministic ephemeral/one-time key is independent of amount.
		out, err := core.BuildLPoDPayoutOutput(a, 1, b.Header.Height, r.Parent, after.Digest())
		if err != nil {
			return err
		}
		owners[out.OneTimePub] = a
	}
	keys := [][]byte{}
	if b.Header.Height == after.Allocation.FundingHeight {
		funding := b.Hash()
		batch.Put(lpodWalletReadyKey, funding[:])
		keys = append(keys, lpodWalletReadyKey)
	}
	for _, tx := range b.Txs {
		if !tx.IsLPoDPayout() {
			continue
		}
		th := tx.Hash()
		for i, out := range tx.Outputs {
			address, ok := owners[out.OneTimePub]
			if !ok {
				return fmt.Errorf("lpod: payout beneficiary index mismatch")
			}
			key := lpodWalletPrefix(address)
			var suffix [12]byte
			binary.BigEndian.PutUint64(suffix[:8], b.Header.Height)
			binary.BigEndian.PutUint32(suffix[8:], uint32(i))
			key = append(key, suffix[:]...)
			bh := b.Hash()
			key = append(key, bh[:]...)
			key = append(key, th[:]...)
			u := StoredUTXO{TxHash: th, OutputIndex: uint32(i), OneTimePub: out.OneTimePub, TxPubKey: out.TxPubKey, AmountCommit: out.AmountCommit, EncAmount: out.EncAmount, BlockHeight: b.Header.Height}
			raw, err := json.Marshal(u)
			if err != nil {
				return err
			}
			batch.Put(key, raw)
			keys = append(keys, key)
		}
	}
	raw, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	batch.Put(lpodWalletBlockKey(b.Hash()), raw)
	return nil
}
func (d *DB) rollbackLPoDWalletIndex(batch *leveldb.Batch, hash crypto.Hash32) error {
	raw, err := d.get(lpodWalletBlockKey(hash))
	if err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	var keys [][]byte
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	for _, k := range keys {
		batch.Delete(k)
	}
	batch.Delete(lpodWalletBlockKey(hash))
	return nil
}

// Bounded address-prefix lookup; no block-history scan. Cursor is tied to this
// address prefix; callers additionally bind all pages to one finalized tip.
func (d *DB) LPoDWalletOutputs(address crypto.Address, cursor string, limit int) ([]StoredUTXO, string, error) {
	if limit < 1 || limit > 128 {
		return nil, "", fmt.Errorf("invalid page limit")
	}
	prefix := lpodWalletPrefix(address)
	iter := d.db.NewIterator(util.BytesPrefix(prefix), nil)
	defer iter.Release()
	ok := iter.First()
	if cursor != "" {
		key, err := hex.DecodeString(cursor)
		if err != nil || !bytes.HasPrefix(key, prefix) || len(key) != len(prefix)+76 {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		ok = iter.Seek(key)
		if ok && bytes.Equal(iter.Key(), key) {
			ok = iter.Next()
		}
	}
	rows := []StoredUTXO{}
	next := ""
	examined := 0
	for ok && examined < limit {
		examined++
		var u StoredUTXO
		if err := json.Unmarshal(iter.Value(), &u); err != nil {
			return nil, "", err
		}
		spent, err := d.get(spentUTXOKey(u.TxHash, u.OutputIndex))
		if err != nil {
			return nil, "", err
		}
		if spent == nil {
			rows = append(rows, u)
		}
		next = hex.EncodeToString(iter.Key())
		ok = iter.Next()
	}
	if !ok {
		next = ""
	}
	return rows, next, iter.Error()
}
