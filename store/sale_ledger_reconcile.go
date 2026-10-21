package store

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// SaleLedgerReconcileReport proves a bounded, hash-bound, sale-free interval.
// It is not a new checkpoint, issuance, or proof of buyer credit.
type SaleLedgerReconcileReport struct {
	Status     string `json:"status"`
	FromHeight uint64 `json:"from_height"`
	TipHeight  uint64 `json:"tip_height"`
	Blocks     uint64 `json:"blocks"`
	Remaining  string `json:"remaining_napro"`
	Sold       string `json:"sold_napro"`
}

// ReconcileSaleLedger only catches up a previously activated ledger across
// canonical bodies containing NO sales. Any sale requires a different full
// reconciliation, not ignoring monetary evidence. Caller must use an exclusive
// stopped database or an immutable copy; expected tip binds operator consent.
// All journals and the final ledger pointer are committed in one synchronous
// batch. No UTXOs, orders, fences, canonical tips or allocation amounts change.
func (d *DB) ReconcileSaleLedger(path string, expectedHash crypto.Hash32, expectedHeight uint64, apply bool) (*SaleLedgerReconcileReport, error) {
	d.canonicalMu.Lock()
	defer d.canonicalMu.Unlock()
	m, err := readSignedSaleMigrationFile(path)
	if err != nil {
		return nil, err
	}
	c := m.Checkpoint
	raw, err := d.GetRawBlockByHeight(0)
	var genesis core.Block
	if err != nil || len(raw) == 0 || json.Unmarshal(raw, &genesis) != nil || genesis.Hash() != c.Genesis {
		return nil, fmt.Errorf("sale: canonical genesis binding unavailable")
	}
	before, err := d.LoadSaleState()
	if err != nil || before == nil {
		return nil, fmt.Errorf("sale: existing activated ledger required")
	}
	tip, height, err := d.GetTip()
	if err != nil || tip != expectedHash || height != expectedHeight || height == ^uint64(0) || tip == (crypto.Hash32{}) {
		return nil, fmt.Errorf("sale: expected canonical tip mismatch")
	}
	if before.Checkpoint != c.Message() || before.Height < c.Height || before.Height > height ||
		before.Sold > c.SaleRemaining || before.Remaining != c.SaleRemaining-before.Sold {
		return nil, fmt.Errorf("sale: signed baseline or budget mismatch")
	}
	if height-before.Height > 100000 {
		return nil, fmt.Errorf("sale: reconciliation interval exceeds bounded limit")
	}
	anchor, found, err := d.GetCanonicalHash(before.Height)
	if err != nil || !found || anchor != before.Hash {
		return nil, fmt.Errorf("sale: ledger is not a canonical anchor")
	}
	current := *before
	batch := new(leveldb.Batch)
	for h := before.Height + 1; h <= height; h++ {
		raw, err := d.GetRawBlockByHeight(h)
		if err != nil || len(raw) == 0 {
			return nil, fmt.Errorf("sale: canonical body unavailable at %d", h)
		}
		var b core.Block
		if json.Unmarshal(raw, &b) != nil || b.Header.Height != h || b.Header.PrevHash != current.Hash ||
			b.Header.MerkleRoot != core.MerkleRoot(b.Txs) {
			return nil, fmt.Errorf("sale: canonical body binding invalid at %d", h)
		}
		hash := b.Hash()
		canonical, found, err := d.GetCanonicalHash(h)
		if err != nil || !found || canonical != hash {
			return nil, fmt.Errorf("sale: canonical hash mismatch at %d", h)
		}
		for _, tx := range b.Txs {
			if tx.IsSale() {
				return nil, fmt.Errorf("sale: interval contains a sale at %d; monetary replay required", h)
			}
		}
		next, err := current.Advance(c, h, current.Hash, hash, nil, d.SaleConsumed)
		if err != nil {
			return nil, err
		}
		if next.Remaining != before.Remaining || next.Sold != before.Sold {
			return nil, fmt.Errorf("sale: budget changed")
		}
		journal, err := json.Marshal(saleJournal{Before: &current})
		if err != nil {
			return nil, err
		}
		previous, err := d.get(saleJournalKey(hash))
		if err != nil {
			return nil, err
		}
		if previous != nil && !bytes.Equal(previous, journal) {
			return nil, fmt.Errorf("sale: conflicting rollback journal at %d", h)
		}
		batch.Put(saleJournalKey(hash), journal)
		current = next
	}
	if current.Hash != tip || current.Height != height {
		return nil, fmt.Errorf("sale: interval does not terminate at canonical tip")
	}
	status := "verified_no_sales"
	if apply && height > before.Height {
		state, err := json.Marshal(current)
		if err != nil {
			return nil, err
		}
		batch.Put(saleStateKey, state)
		if err = d.db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
			return nil, err
		}
		status = "reconciled"
	}
	return &SaleLedgerReconcileReport{Status: status, FromHeight: before.Height, TipHeight: height,
		Blocks: height - before.Height, Remaining: fmt.Sprint(before.Remaining), Sold: fmt.Sprint(before.Sold)}, nil
}
