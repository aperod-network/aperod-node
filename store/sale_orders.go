package store

import (
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// SaleOrder is private operator storage, never public chain metadata.
// Reserve before creating an external invoice. Mark paid only after verifying
// the provider's payment identity, currency and exact received amount.
type SaleOrder struct {
	Order      crypto.Hash32  `json:"order"`
	Checkpoint crypto.Hash32  `json:"checkpoint"`
	Recipient  crypto.Address `json:"recipient"`
	Amount     uint64         `json:"amount_napro,string"`
	Payment    crypto.Hash32  `json:"payment"`
	Invoice    *SaleInvoice   `json:"invoice,omitempty"`
	QuoteUSD   string         `json:"quote_usd,omitempty"`
}

// SaleDelivery is canonical inclusion, not a buyer recovery acknowledgement.
type SaleDelivery struct {
	Block       crypto.Hash32
	Transaction crypto.Hash32
	Height      uint64
}

func (d *DB) SaleOrderDelivery(id crypto.Hash32) (*SaleDelivery, error) {
	d.canonicalMu.Lock()
	defer d.canonicalMu.Unlock()
	return d.saleOrderDeliveryLocked(id)
}

func (d *DB) saleOrderDeliveryLocked(id crypto.Hash32) (*SaleDelivery, error) {
	raw, err := d.get(saleFence('o', id))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("sale: corrupt delivery fence")
	}
	var hash crypto.Hash32
	copy(hash[:], raw)
	body, err := d.GetRawBlock(hash)
	if err != nil {
		return nil, err
	}
	var block core.Block
	if body == nil || json.Unmarshal(body, &block) != nil || block.Hash() != hash {
		return nil, fmt.Errorf("sale: delivery body unavailable")
	}
	canonical, err := d.get(heightKey(block.Header.Height))
	if err != nil {
		return nil, err
	}
	if len(canonical) != 32 || string(canonical) != string(hash[:]) {
		return nil, fmt.Errorf("sale: delivery is no longer canonical")
	}
	for _, tx := range block.Txs {
		if !tx.IsSale() {
			continue
		}
		auth, err := tx.SaleAuthorization()
		if err != nil {
			return nil, err
		}
		if auth.Order == id {
			return &SaleDelivery{Block: hash, Transaction: tx.Hash(), Height: block.Header.Height}, nil
		}
	}
	return nil, fmt.Errorf("sale: delivery fence has no canonical transaction")
}

func saleOrderKey(id crypto.Hash32) []byte   { return append([]byte("sale/order/"), id[:]...) }
func salePendingKey(id crypto.Hash32) []byte { return append([]byte("sale/pending/"), id[:]...) }
func saleOrderPaymentKey(id crypto.Hash32) []byte {
	return append([]byte("sale/order-payment/"), id[:]...)
}

func (d *DB) SaleOrder(id crypto.Hash32) (*SaleOrder, error) {
	raw, err := d.get(saleOrderKey(id))
	if err != nil || raw == nil {
		return nil, err
	}
	var order SaleOrder
	if err := json.Unmarshal(raw, &order); err != nil {
		return nil, fmt.Errorf("sale: corrupt order: %w", err)
	}
	if order.Order != id || order.Amount == 0 {
		return nil, fmt.Errorf("sale: inconsistent order")
	}
	return &order, nil
}

func (d *DB) pendingSaleOrders() ([]SaleOrder, error) {
	it := d.db.NewIterator(util.BytesPrefix([]byte("sale/pending/")), nil)
	defer it.Release()
	var orders []SaleOrder
	for it.Next() {
		if len(orders) >= 4096 || len(it.Value()) != 32 {
			return nil, fmt.Errorf("sale: pending order bound or identity invalid")
		}
		var id crypto.Hash32
		copy(id[:], it.Value())
		order, err := d.SaleOrder(id)
		if err != nil {
			return nil, err
		}
		if order == nil {
			return nil, fmt.Errorf("sale: orphan pending reservation")
		}
		orders = append(orders, *order)
	}
	return orders, it.Error()
}

// ReservedSaleOrders is an issuer-local obligation, not extra issuance.
func (d *DB) ReservedSaleOrders() (uint64, error) {
	d.canonicalMu.Lock()
	defer d.canonicalMu.Unlock()
	orders, err := d.pendingSaleOrders()
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, order := range orders {
		if order.Amount > ^uint64(0)-total {
			return 0, fmt.Errorf("sale: reservation overflow")
		}
		total += order.Amount
	}
	return total, nil
}

func (d *DB) ReserveSaleOrder(order SaleOrder, network crypto.NetworkByte) error {
	d.canonicalMu.Lock()
	defer d.canonicalMu.Unlock()
	actual, _, _, err := crypto.DecodeAddress(order.Recipient)
	if order.QuoteUSD != "" && !validSalePrice(order.QuoteUSD) {
		return fmt.Errorf("sale: invalid frozen quote")
	}
	if err != nil || actual != network || order.Order == (crypto.Hash32{}) ||
		order.Payment != (crypto.Hash32{}) || order.Invoice != nil || order.Amount == 0 {
		return fmt.Errorf("sale: invalid reservation")
	}
	state, err := d.LoadSaleState()
	if err != nil {
		return err
	}
	if state == nil || state.Checkpoint != order.Checkpoint {
		return fmt.Errorf("sale: activated checkpoint unavailable")
	}
	tip, height, err := d.GetTip()
	if err != nil {
		return err
	}
	if tip != state.Hash || height != state.Height {
		return fmt.Errorf("sale: state does not match canonical tip")
	}
	existing, err := d.SaleOrder(order.Order)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Recipient != order.Recipient || existing.Amount != order.Amount || existing.Checkpoint != order.Checkpoint || existing.QuoteUSD != order.QuoteUSD {
			return fmt.Errorf("sale: conflicting reservation retry")
		}
		return nil
	}
	used, err := d.SaleConsumed('o', order.Order)
	if err != nil {
		return err
	}
	if used {
		return fmt.Errorf("sale: order already consumed")
	}
	pending, err := d.pendingSaleOrders()
	if err != nil {
		return err
	}
	if len(pending) >= 4096 {
		return fmt.Errorf("sale: pending order capacity exceeded")
	}
	available := state.Remaining
	for _, previous := range pending {
		if previous.Checkpoint != state.Checkpoint || previous.Amount > available {
			return fmt.Errorf("sale: inconsistent reserved allocation")
		}
		available -= previous.Amount
	}
	if order.Amount > available {
		return fmt.Errorf("sale: allocation is already reserved")
	}
	raw, err := json.Marshal(order)
	if err != nil {
		return err
	}
	batch := new(leveldb.Batch)
	batch.Put(saleOrderKey(order.Order), raw)
	batch.Put(salePendingKey(order.Order), order.Order[:])
	return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}

func (d *DB) MarkSaleOrderPaid(id, payment crypto.Hash32) error {
	d.canonicalMu.Lock()
	defer d.canonicalMu.Unlock()
	if payment == (crypto.Hash32{}) {
		return fmt.Errorf("sale: empty payment identity")
	}
	order, err := d.SaleOrder(id)
	if err != nil {
		return err
	}
	if order == nil {
		return fmt.Errorf("sale: unreserved paid order")
	}
	if order.Invoice != nil && SaleProviderPayment(order.Invoice.ProviderID) != payment {
		return fmt.Errorf("sale: payment does not match bound invoice")
	}
	if bound, err := d.get(append([]byte("sale/invoice/"), payment[:]...)); err != nil {
		return err
	} else if bound != nil && string(bound) != string(id[:]) {
		return fmt.Errorf("sale: payment invoice belongs to another order")
	}
	if order.Payment != (crypto.Hash32{}) {
		if order.Payment == payment {
			return nil
		}
		return fmt.Errorf("sale: conflicting paid retry")
	}
	if other, err := d.get(saleOrderPaymentKey(payment)); err != nil {
		return err
	} else if other != nil {
		return fmt.Errorf("sale: payment belongs to another order")
	}
	if used, err := d.SaleConsumed('p', payment); err != nil {
		return err
	} else if used {
		return fmt.Errorf("sale: payment already consumed")
	}
	order.Payment = payment
	raw, err := json.Marshal(order)
	if err != nil {
		return err
	}
	batch := new(leveldb.Batch)
	batch.Put(saleOrderKey(id), raw)
	batch.Put(saleOrderPaymentKey(payment), id[:])
	return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}

// PendingPaidSales uses only durable paid orders and the current block context.
// A crash before canonical commit leaves the obligation pending for retry.
func (d *DB) PendingPaidSales(genesis crypto.Hash32, height uint64, parent crypto.Hash32,
	network crypto.NetworkByte, signer interface {
		Sign(crypto.Hash32) ([]byte, error)
	}) ([]core.Transaction, error) {
	d.canonicalMu.Lock()
	defer d.canonicalMu.Unlock()
	orders, err := d.pendingSaleOrders()
	if err != nil {
		return nil, err
	}
	state, err := d.LoadSaleState()
	if err != nil {
		return nil, err
	}
	if state == nil {
		// The activation block establishes the signed budget before invoices
		// may reserve it. Reorged paid obligations wait for reactivation.
		return nil, nil
	}
	if height != state.Height+1 || parent != state.Hash {
		return nil, fmt.Errorf("sale: stale payout context")
	}
	available := state.Remaining
	for _, order := range orders {
		if order.Checkpoint != state.Checkpoint || order.Amount > available {
			return nil, fmt.Errorf("sale: outstanding allocation obligations exceed canonical budget")
		}
		available -= order.Amount
	}
	var txs []core.Transaction
	for _, order := range orders {
		if order.Checkpoint != state.Checkpoint {
			return nil, fmt.Errorf("sale: pending order checkpoint changed")
		}
		if order.Payment == (crypto.Hash32{}) {
			continue
		}
		tx, err := core.BuildSaleTransaction(core.SaleAuthorization{Genesis: genesis, Height: height,
			Parent: parent, Order: order.Order, Payment: order.Payment, Amount: order.Amount}, order.Recipient, network, signer)
		if err != nil {
			return nil, err
		}
		txs = append(txs, *tx)
		if len(txs) == 128 {
			break
		}
	}
	return txs, nil
}
