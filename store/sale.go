package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/syndtr/goleveldb/leveldb"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
)

// SaleMigration is operator configuration, never accepted from a payment API.
// Activation is independently gated by this signed, reconciled witness.
type SaleMigration struct {
	Checkpoint     sale.Checkpoint
	Reconciliation sale.Reconciliation
	Trusted        []crypto.ValidatorPubKey
}
type SaleSettlement struct {
	Migration *SaleMigration
	Before    *sale.State
	After     sale.State
}
type saleJournal struct {
	Before       *sale.State     `json:"before"`
	QueuedOrders []crypto.Hash32 `json:"queued_orders,omitempty"`
}

var saleStateKey = []byte("sale/state/v1")

func saleFence(kind byte, id crypto.Hash32) []byte {
	return append([]byte{'s', 'a', 'l', 'e', '/', kind, '/'}, id[:]...)
}
func saleJournalKey(hash crypto.Hash32) []byte { return append([]byte("sale/journal/v1/"), hash[:]...) }

func (d *DB) LoadSaleState() (*sale.State, error) {
	raw, err := d.get(saleStateKey)
	if err != nil || raw == nil {
		return nil, err
	}
	var s sale.State
	if err = json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("sale: corrupt durable state: %w", err)
	}
	return &s, nil
}
func (d *DB) SaleConsumed(kind byte, id crypto.Hash32) (bool, error) {
	if kind != 'o' && kind != 'p' {
		return false, fmt.Errorf("sale: invalid fence namespace")
	}
	return d.db.Has(saleFence(kind, id), nil)
}

func (d *DB) PrepareSaleBlock(b *core.Block, m *SaleMigration) (*SaleSettlement, error) {
	if b == nil {
		return nil, fmt.Errorf("sale: nil block")
	}
	before, err := d.LoadSaleState()
	if err != nil {
		return nil, err
	}
	txs := []*core.Transaction{}
	for i := range b.Txs {
		tx := &b.Txs[i]
		if tx.IsSale() {
			txs = append(txs, tx)
		}
	}
	if before == nil && len(txs) == 0 && (m == nil || b.Header.Height < m.Checkpoint.Height) {
		return nil, nil
	}
	if m == nil || len(txs) > 128 {
		return nil, fmt.Errorf("sale: approved migration missing or block limit exceeded")
	}
	if b.Header.MerkleRoot != core.MerkleRoot(b.Txs) {
		return nil, fmt.Errorf("sale: transaction body does not match header")
	}
	seenOutputs := map[crypto.Point32]bool{}
	for _, tx := range txs {
		for _, out := range tx.Outputs {
			if seenOutputs[out.OneTimePub] {
				return nil, fmt.Errorf("sale: duplicate recipient output key")
			}
			exists, err := d.db.Has(ringMemberKey(out.OneTimePub), nil)
			if err != nil {
				return nil, err
			}
			if exists {
				return nil, fmt.Errorf("sale: recipient output key already exists")
			}
			seenOutputs[out.OneTimePub] = true
		}
	}
	c := m.Checkpoint
	// Recheck the externally configured authority set, including after restart.
	if err = c.Verify(c.Genesis, c.Parent, c.Height, m.Trusted); err != nil {
		return nil, err
	}
	tip, height, err := d.GetTip()
	if err != nil {
		return nil, err
	}
	if tip != b.Header.PrevHash || height+1 != b.Header.Height {
		return nil, fmt.Errorf("sale: block is not the durable tip successor")
	}
	start := sale.InitialState(c)
	if before == nil {
		if b.Header.Height != c.Height || tip != c.Parent {
			return nil, fmt.Errorf("sale: missed activation parent")
		}
		raw, err := d.GetRawBlockByHeight(0)
		if err != nil {
			return nil, err
		}
		var genesis core.Block
		if json.Unmarshal(raw, &genesis) != nil || genesis.Hash() != c.Genesis {
			return nil, fmt.Errorf("sale: canonical genesis mismatch")
		}
		l, err := d.LoadLPoDCheckpoint()
		if err != nil {
			return nil, err
		}
		if err = VerifySaleReserveFloors(l, m.Reconciliation); err != nil {
			return nil, err
		}
		knownIssued, err := SaleKnownIssuedFloor(l)
		if err != nil {
			return nil, err
		}
		if err = sale.VerifyReconciledCheckpoint(c, m.Reconciliation, c.Genesis, tip, l.Digest(), c.Height,
			knownIssued, m.Trusted); err != nil {
			return nil, err
		}
	} else {
		start = *before
	}
	after, err := start.Advance(c, b.Header.Height, b.Header.PrevHash, b.Hash(), txs, d.SaleConsumed)
	if err != nil {
		return nil, err
	}
	return &SaleSettlement{Migration: m, Before: before, After: after}, nil
}

func (d *DB) appendSaleSettlement(batch *leveldb.Batch, data []byte, hash crypto.Hash32, height uint64, p *SaleSettlement) error {
	var b core.Block
	if json.Unmarshal(data, &b) != nil {
		s, err := d.LoadSaleState()
		if err != nil {
			return err
		}
		if s != nil || p != nil {
			return fmt.Errorf("sale: canonical body required")
		}
		return nil
	}
	hasSale := false
	for _, tx := range b.Txs {
		hasSale = hasSale || tx.IsSale()
	}
	s, err := d.LoadSaleState()
	if err != nil {
		return err
	}
	if p == nil {
		if s != nil || hasSale {
			return fmt.Errorf("sale: missing prepared canonical settlement")
		}
		return nil
	}
	if b.Hash() != hash || b.Header.Height != height {
		return fmt.Errorf("sale: body context mismatch")
	}
	actual, err := d.PrepareSaleBlock(&b, p.Migration)
	if err != nil {
		return err
	}
	if actual == nil || actual.After != p.After {
		return fmt.Errorf("sale: stale or forged settlement")
	}
	raw, err := json.Marshal(actual.After)
	if err != nil {
		return err
	}
	batch.Put(saleStateKey, raw)
	journal := saleJournal{Before: actual.Before}
	for txIndex, tx := range b.Txs {
		if !tx.IsSale() {
			continue
		}
		a, err := tx.SaleAuthorization()
		if err != nil {
			return err
		}
		order, err := d.SaleOrder(a.Order)
		if err != nil {
			return err
		}
		if order != nil {
			if order.Checkpoint != actual.After.Checkpoint || order.Amount != a.Amount || order.Payment != a.Payment {
				return fmt.Errorf("sale: canonical transaction conflicts with reserved payment")
			}
			journal.QueuedOrders = append(journal.QueuedOrders, a.Order)
			batch.Delete(salePendingKey(a.Order))
		}
		batch.Put(saleFence('o', a.Order), hash[:])
		batch.Put(saleFence('p', a.Payment), hash[:])
		th := tx.Hash()
		var location [12]byte
		binary.BigEndian.PutUint64(location[:8], height)
		binary.BigEndian.PutUint32(location[8:], uint32(txIndex))
		batch.Put(append(append([]byte{}, prefixTxIdx...), th[:]...), location[:])
		for i, out := range tx.Outputs {
			u := StoredUTXO{TxHash: th, OutputIndex: uint32(i), OneTimePub: out.OneTimePub, TxPubKey: out.TxPubKey,
				AmountCommit: out.AmountCommit, EncAmount: out.EncAmount, BlockHeight: height}
			raw, err := json.Marshal(u)
			if err != nil {
				return err
			}
			batch.Put(utxoKey(th, uint32(i)), raw)
			batch.Put(ringMemberKey(out.OneTimePub), raw)
		}
	}
	raw, err = json.Marshal(journal)
	if err != nil {
		return err
	}
	batch.Put(saleJournalKey(hash), raw)
	return nil
}

func (d *DB) restoreSale(batch *leveldb.Batch, target crypto.Hash32, height uint64) error {
	s, err := d.LoadSaleState()
	if err != nil || s == nil {
		return err
	}
	cursor, current, err := d.GetTip()
	if err != nil {
		return err
	}
	if s.Hash != cursor || s.Height != current || height > current {
		return fmt.Errorf("sale: inconsistent rollback clock")
	}
	for current > height {
		raw, err := d.get(saleJournalKey(cursor))
		if err != nil {
			return err
		}
		var journal saleJournal
		if raw == nil || json.Unmarshal(raw, &journal) != nil {
			return fmt.Errorf("sale: rollback witness missing")
		}
		body, err := d.GetRawBlock(cursor)
		if err != nil {
			return err
		}
		var b core.Block
		if json.Unmarshal(body, &b) != nil || b.Hash() != cursor || b.Header.Height != current {
			return fmt.Errorf("sale: rollback body missing")
		}
		for _, id := range journal.QueuedOrders {
			order, err := d.SaleOrder(id)
			if err != nil {
				return err
			}
			if order == nil || order.Payment == (crypto.Hash32{}) {
				return fmt.Errorf("sale: rollback payment obligation missing")
			}
			batch.Put(salePendingKey(id), id[:])
		}
		for _, tx := range b.Txs {
			if !tx.IsSale() {
				continue
			}
			a, err := tx.SaleAuthorization()
			if err != nil {
				return err
			}
			batch.Delete(saleFence('o', a.Order))
			batch.Delete(saleFence('p', a.Payment))
			th := tx.Hash()
			batch.Delete(append(append([]byte{}, prefixTxIdx...), th[:]...))
			for i, out := range tx.Outputs {
				batch.Delete(utxoKey(th, uint32(i)))
				batch.Delete(ringMemberKey(out.OneTimePub))
				batch.Delete(spentUTXOKey(th, uint32(i)))
			}
		}
		batch.Delete(heightKey(current))
		batch.Delete(saleJournalKey(cursor))
		cursor = b.Header.PrevHash
		current--
		s = journal.Before
		if s == nil && current > height {
			return fmt.Errorf("sale: rollback crosses activation without witness")
		}
		if s != nil && (s.Hash != cursor || s.Height != current) {
			return fmt.Errorf("sale: corrupt rollback witness")
		}
	}
	if cursor != target {
		return fmt.Errorf("sale: rollback target is not an ancestor")
	}
	if s == nil {
		batch.Delete(saleStateKey)
	} else {
		raw, err := json.Marshal(s)
		if err != nil {
			return err
		}
		batch.Put(saleStateKey, raw)
	}
	return nil
}
