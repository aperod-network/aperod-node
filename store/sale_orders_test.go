package store

import (
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestSaleReservationPaymentAndRollbackObligation(t *testing.T) {
	g, m, key, address, records := saleStoreFixture(t)
	path := t.TempDir()
	d := seedSaleParent(t, path, records)
	defer func() { d.Close() }()
	initial, r := saleTestBlock(t, d, m, key, address, m.Checkpoint.Height, g.Hash(), "activation")
	commitSaleFixture(t, d, initial, m, r)
	order := SaleOrder{Order: crypto.HashBytes([]byte("reserved-order")), Checkpoint: m.Checkpoint.Message(), Recipient: address, Amount: 500 * 100000000, QuoteUSD: "0.08"}
	if err := d.ReserveSaleOrder(order, m.Checkpoint.Network); err != nil {
		t.Fatal(err)
	}
	if err := d.ReserveSaleOrder(order, m.Checkpoint.Network); err != nil {
		t.Fatal("exact retry rejected", err)
	}
	changed := order
	changed.Amount++
	if d.ReserveSaleOrder(changed, m.Checkpoint.Network) == nil {
		t.Fatal("conflicting retry accepted")
	}
	tooMuch := order
	tooMuch.Order = crypto.HashBytes([]byte("oversubscribed"))
	tooMuch.Amount = m.Checkpoint.SaleRemaining
	if d.ReserveSaleOrder(tooMuch, m.Checkpoint.Network) == nil {
		t.Fatal("already reserved allocation sold twice")
	}
	if txs, err := d.PendingPaidSales(m.Checkpoint.Genesis, initial.Header.Height+1, initial.Hash(), m.Checkpoint.Network, key); err != nil || len(txs) != 0 {
		t.Fatal("unpaid reservation became spendable", err)
	}
	invoice := SaleInvoice{ProviderID: "payment-123", Currency: "btc", Address: "bc1-test", Amount: "0.001000000000000001", PriceUSD: "0.08"}
	if err := d.BindSaleInvoice(order.Order, invoice); err != nil {
		t.Fatal(err)
	}
	if err := d.BindSaleInvoice(order.Order, invoice); err != nil {
		t.Fatal("invoice retry rejected", err)
	}
	wrongInvoice := invoice
	wrongInvoice.Amount = "0.002"
	if d.BindSaleInvoice(order.Order, wrongInvoice) == nil {
		t.Fatal("quote mutated")
	}
	payment := SaleProviderPayment(invoice.ProviderID)
	if err := d.MarkSaleOrderPaid(order.Order, payment); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkSaleOrderPaid(order.Order, payment); err != nil {
		t.Fatal("paid retry rejected", err)
	}
	second := order
	second.Order = crypto.HashBytes([]byte("second-order"))
	if err := d.ReserveSaleOrder(second, m.Checkpoint.Network); err != nil {
		t.Fatal(err)
	}
	if d.BindSaleInvoice(second.Order, invoice) == nil {
		t.Fatal("same invoice assigned twice")
	}
	if d.MarkSaleOrderPaid(second.Order, payment) == nil {
		t.Fatal("payment assigned twice")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := d.SaleOrder(order.Order)
	if err != nil || restored == nil || restored.Invoice == nil || *restored.Invoice != invoice || restored.QuoteUSD != "0.08" {
		t.Fatal("immutable exact invoice or frozen price lost on reopening", err)
	}
	txs, err := d.PendingPaidSales(m.Checkpoint.Genesis, initial.Header.Height+1, initial.Hash(), m.Checkpoint.Network, key)
	if err != nil || len(txs) != 1 {
		t.Fatal("paid obligation lost at restart", err)
	}
	block, settlement := saleTestBlock(t, d, m, key, address, initial.Header.Height+1, initial.Hash(), "replace")
	block.Txs[len(block.Txs)-1] = txs[0]
	settlement.Transactions = append([]core.Transaction(nil), block.Txs[2:]...)
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	block.Header.Sign(key)
	commitSaleFixture(t, d, block, m, settlement)
	delivery, err := d.SaleOrderDelivery(order.Order)
	if err != nil || delivery == nil || delivery.Block != block.Hash() || delivery.Transaction != txs[0].Hash() {
		t.Fatal("exact canonical payment receipt missing", err)
	}
	if pending, err := d.PendingPaidSales(m.Checkpoint.Genesis, block.Header.Height+1, block.Hash(), m.Checkpoint.Network, key); err != nil || len(pending) != 0 {
		t.Fatal("canonical payment remained pending", err)
	}
	reserved, err := d.ReservedSaleOrders()
	if err != nil || reserved != second.Amount {
		t.Fatal("wrong remaining reservations", err)
	}
	if err = d.PutTip(initial.Hash(), initial.Header.Height); err != nil {
		t.Fatal(err)
	}
	if delivery, err = d.SaleOrderDelivery(order.Order); err != nil || delivery != nil {
		t.Fatal("rolled back delivery reported as included", err)
	}
	retry, err := d.PendingPaidSales(m.Checkpoint.Genesis, initial.Header.Height+1, initial.Hash(), m.Checkpoint.Network, key)
	if err != nil || len(retry) != 1 {
		t.Fatal("rollback discarded paid obligation", err)
	}
	auth, _ := retry[0].SaleAuthorization()
	if auth.Payment != payment || auth.Order != order.Order || auth.Amount != order.Amount {
		t.Fatal("retry changed paid intent")
	}
	// A private order is persisted as exact integer JSON, not a float.
	raw, err := d.get(saleOrderKey(order.Order))
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	json.Unmarshal(raw, &persisted)
	if _, ok := persisted["amount_napro"].(string); !ok {
		t.Fatal("inexact persisted amount")
	}
}
