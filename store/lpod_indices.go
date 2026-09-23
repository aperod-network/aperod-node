// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
)

func appendLPoDIndices(batch *leveldb.Batch, b *core.Block, rollback bool) error {
	for _, tx := range b.Txs {
		hash := tx.Hash()
		for _, in := range tx.Inputs {
			ki, err := crypto.CanonicalKeyImage(in.KeyImage)
			if err != nil {
				return err
			}
			key := append(append([]byte{}, prefixKeyImage...), ki[:]...)
			if rollback {
				batch.Delete(key)
			} else {
				batch.Put(key, []byte{1})
			}
		}
		if tx.IsLPoDPosition() {
			var a core.LPoDPositionAction
			if err := json.Unmarshal(tx.Extra, &a); err != nil {
				return err
			}
			if a.Action == core.LPoDDeposit {
				key := spentUTXOKey(a.SourceTx, a.SourceIndex)
				if rollback {
					batch.Delete(key)
				} else {
					batch.Put(key, []byte{1})
				}
			}
		}
		for i, out := range tx.Outputs {
			index := uint32(i)
			if rollback {
				batch.Delete(utxoKey(hash, index))
				batch.Delete(ringMemberKey(out.OneTimePub))
				batch.Delete(spentUTXOKey(hash, index))
			} else {
				u := StoredUTXO{TxHash: hash, OutputIndex: index, OneTimePub: out.OneTimePub, TxPubKey: out.TxPubKey,
					AmountCommit: out.AmountCommit, EncAmount: out.EncAmount, BlockHeight: b.Header.Height}
				raw, err := json.Marshal(u)
				if err != nil {
					return err
				}
				batch.Put(utxoKey(hash, index), raw)
				batch.Put(ringMemberKey(out.OneTimePub), raw)
			}
		}
	}
	return nil
}

// Ancestor selection restores the native position indices in the same batch as
// reserve/position selection. Alternate branches must first rewind to the common
// parent and then use ordinary validated canonical commits, never jump a tip.
func (d *DB) rollbackLPoDIndices(batch *leveldb.Batch, target crypto.Hash32, height uint64) error {
	bound, err := d.get([]byte("lpod/activation/v1"))
	if err != nil || bound == nil {
		return err
	}
	cursor, current, err := d.GetTip()
	if err != nil {
		return err
	}
	if height > current {
		return fmt.Errorf("lpod: advance branches using validated canonical commits")
	}
	for current > height {
		raw, err := d.GetRawBlock(cursor)
		if err != nil {
			return err
		}
		var block core.Block
		if json.Unmarshal(raw, &block) != nil || block.Hash() != cursor || block.Header.Height != current {
			return fmt.Errorf("lpod: full canonical body required to rewind principal indices")
		}
		if err := appendLPoDIndices(batch, &block, true); err != nil {
			return err
		}
if err := d.rollbackLPoDWalletIndex(batch, block.Hash()); err != nil { return err }
		batch.Delete(heightKey(current))
		cursor = block.Header.PrevHash
		current--
	}
	if cursor != target {
		return fmt.Errorf("lpod: selected rollback tip is not an ancestor")
	}
	return nil
}
