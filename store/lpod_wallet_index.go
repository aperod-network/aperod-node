// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
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
var lpodEarningsReadyKey = []byte("lpod/earnings-ready/v2")
var lpodEarningsProgressKey = []byte("lpod/earnings-progress/v2")

func (d *DB) LPoDWalletIndexReady(funding crypto.Hash32) (bool, error) {
	raw, err := d.get(lpodWalletReadyKey)
	return bytes.Equal(raw, funding[:]), err
}

func (d *DB) LPoDEarningsIndexReady(funding, tip crypto.Hash32, height uint64) (bool, error) {
	raw, err := d.get(lpodEarningsReadyKey)
	expected := earningsReadyValue(funding, tip, height)
	return bytes.Equal(raw, expected), err
}

func earningsReadyValue(funding, tip crypto.Hash32, height uint64) []byte {
	value := make([]byte, 0, 3+32+32+8)
	value = append(value, []byte("v2/")...)
	value = append(value, funding[:]...)
	value = append(value, tip[:]...)
	var heightBytes [8]byte
	binary.BigEndian.PutUint64(heightBytes[:], height)
	return append(value, heightBytes[:]...)
}

func lpodEarningsPrefix(address crypto.Address) []byte {
	h := crypto.HashBytes([]byte(address))
	return append([]byte("lpod/earnings-output/v2/"), h[:]...)
}

func isLPoDPureReward(reward, returnedPrincipal uint64) bool {
	return reward > 0 && returnedPrincipal == 0
}

// This is a discovery index only. Monetary transitions and payout validation
// remain authoritative and unchanged. Keys are committed alongside the block,
// settlement and UTXOs, never manufactured from wallet SQL metadata.
func (d *DB) appendLPoDWalletIndex(batch *leveldb.Batch, b *core.Block, r *LPoDSettlement) error {
	after, payments, err := d.PreviewLPoD(b.Header.Height, r)
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
		if after.Allocation.Version == 3 {
			ready := earningsReadyValue(funding, funding, b.Header.Height)
			batch.Put(lpodEarningsReadyKey, ready)
			keys = append(keys, lpodEarningsReadyKey)
		}
	}
	earnings, err := d.classifyLPoDEarnings(b, r, after, payments)
	if err != nil {
		return err
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
		if b.Header.Height > after.Allocation.FundingHeight {
			rawReady, err := d.get(lpodEarningsReadyKey)
			if err != nil {
				return err
			}
			expectedParent := earningsReadyValue(after.Allocation.FundingBlock, b.Header.PrevHash, b.Header.Height-1)
			if bytes.Equal(rawReady, expectedParent) {
				batch.Put(lpodEarningsReadyKey, earningsReadyValue(after.Allocation.FundingBlock, b.Hash(), b.Header.Height))
				keys = append(keys, lpodEarningsReadyKey)
			}
		}
	}
	if after.Allocation.Version == 3 {
		for _, u := range earnings {
			key := lpodEarningsPrefix(u.address)
			var suffix [12]byte
			binary.BigEndian.PutUint64(suffix[:8], b.Header.Height)
			binary.BigEndian.PutUint32(suffix[8:], u.outputIndex)
			key = append(key, suffix[:]...)
			bh := b.Hash()
			key = append(key, bh[:]...)
			key = append(key, u.txHash[:]...)
			raw, err := json.Marshal(u.StoredUTXO)
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

type lpodEarningsRef struct {
	StoredUTXO
	address     crypto.Address
	txHash      crypto.Hash32
	outputIndex uint32
}

// classifyLPoDEarnings only indexes outputs whose exact v10 payout can be
// reconstructed from the canonical transition. If an output combines any
// returned principal with rewards, the entire output is intentionally omitted.
func (d *DB) classifyLPoDEarnings(b *core.Block, r *LPoDSettlement, after *LPoDCheckpoint, payments []lpod.Payment) ([]lpodEarningsRef, error) {
	if after.Allocation == nil || after.Allocation.Version != 3 {
		return nil, nil
	}
	before, err := d.LoadLPoDCheckpoint()
	if err != nil {
		return nil, err
	}
	if before == nil {
		before, err = d.appendLPoDMigration(new(leveldb.Batch), crypto.Hash32{}, b.Header.Height, r.Migration)
		if err != nil {
			return nil, err
		}
	}
	accrued, _, _, err := d.positionTransition(before, b.Header.Height, r)
	if err != nil {
		return nil, err
	}
	expected, err := d.PayoutLPoD(b.Header.Height, r, after, payments)
	if err != nil {
		return nil, err
	}
	var actual *core.Transaction
	for i := range b.Txs {
		if !b.Txs[i].IsLPoDPayout() {
			continue
		}
		if actual != nil {
			return nil, fmt.Errorf("lpod: ambiguous payout transaction for earnings index")
		}
		actual = &b.Txs[i]
	}
	if actual == nil || !reflect.DeepEqual(*actual, expected) {
		return nil, fmt.Errorf("lpod: canonical payout does not match v10 earnings reconstruction")
	}

	rewards := map[crypto.Address]uint64{}
	principal := map[crypto.Address]uint64{}
	for _, p := range payments {
		if p.Leader > 0 {
			if p.VaultID != r.Proposer {
				return nil, fmt.Errorf("lpod: non-proposer leader reward")
			}
			rewards[r.Leader], err = lpodAdd(rewards[r.Leader], p.Leader)
			if err != nil {
				return nil, err
			}
		}
	}
	for id, p := range after.Positions {
		old := accrued.Positions[id]
		if p.Due > old.Due {
			return nil, fmt.Errorf("lpod: invalid payout entitlement")
		}
		rewards[p.Deposit.Beneficiary], err = lpodAdd(rewards[p.Deposit.Beneficiary], old.Due-p.Due)
		if err != nil {
			return nil, err
		}
		prior := before.Positions[id].Withdrawn
		if p.Withdrawn < prior {
			return nil, fmt.Errorf("lpod: principal return counter decreased")
		}
		principal[p.Deposit.Beneficiary], err = lpodAdd(principal[p.Deposit.Beneficiary], p.Withdrawn-prior)
		if err != nil {
			return nil, err
		}
	}

	addresses := make([]string, 0, len(rewards)+len(principal))
	seen := map[crypto.Address]bool{}
	for a := range rewards {
		seen[a] = true
	}
	for a := range principal {
		seen[a] = true
	}
	for a := range seen {
		if rewards[a] > 0 || principal[a] > 0 {
			addresses = append(addresses, string(a))
		}
	}
	sort.Strings(addresses)
	if len(addresses) != len(expected.Outputs) {
		return nil, fmt.Errorf("lpod: payout output classification is ambiguous")
	}
	out := make([]lpodEarningsRef, 0, len(expected.Outputs))
	for i, text := range addresses {
		address := crypto.Address(text)
		total, err := lpodAdd(rewards[address], principal[address])
		if err != nil {
			return nil, err
		}
		if total == 0 {
			return nil, fmt.Errorf("lpod: zero canonical payout classification")
		}
		if !isLPoDPureReward(rewards[address], principal[address]) {
			// Mixed outputs must never be presented as earnings-only, and
			// principal-only outputs are not earnings.
			continue
		}
		u := StoredUTXO{TxHash: actual.Hash(), OutputIndex: uint32(i), OneTimePub: actual.Outputs[i].OneTimePub,
			TxPubKey: actual.Outputs[i].TxPubKey, AmountCommit: actual.Outputs[i].AmountCommit,
			EncAmount: actual.Outputs[i].EncAmount, BlockHeight: b.Header.Height, AmountNAPRO: rewards[address]}
		out = append(out, lpodEarningsRef{StoredUTXO: u, address: address, txHash: u.TxHash, outputIndex: uint32(i)})
	}
	return out, nil
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

func (d *DB) LPoDEarningsOutputs(address crypto.Address, cursor string, limit int) ([]StoredUTXO, string, error) {
	if limit < 1 || limit > 128 {
		return nil, "", fmt.Errorf("invalid page limit")
	}
	prefix := lpodEarningsPrefix(address)
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
		next = hex.EncodeToString(iter.Key())
		rows = append(rows, u)
		ok = iter.Next()
	}
	if !ok {
		next = ""
	}
	return rows, next, iter.Error()
}
