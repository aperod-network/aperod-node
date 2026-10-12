package sale

import (
	"bytes"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"testing"
)

func TestCanonicalSaleBindsOutputAuthorityBudgetAndReplayIdentifiers(t *testing.T) {
	f := setup(t)
	ledger, err := Begin(f.c, f.c.Genesis, f.c.Parent, f.c.Height, f.trusted)
	if err != nil {
		t.Fatal(err)
	}
	a := core.SaleAuthorization{Genesis: f.c.Genesis, Height: f.c.Height, Parent: f.c.Parent,
		Order: crypto.HashBytes([]byte("opaque-order")), Payment: crypto.HashBytes([]byte("opaque-payment")), Amount: 10000000000}
	tx, err := core.BuildSaleTransaction(a, f.address, f.c.Network, f.merchant)
	if err != nil {
		t.Fatal(err)
	}
	if tx.IsCoinbase() {
		t.Fatal("sale must not inherit the legacy mint bypass")
	}
	if err = tx.Validate(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(tx.Extra, []byte(f.address)) {
		t.Fatal("recipient address leaked in public authorization")
	}
	blockHash := crypto.HashBytes([]byte("sale-block"))
	next, err := ledger.ApplyCanonicalTransactions(f.c.Height, f.c.Parent, blockHash, []*core.Transaction{tx})
	if err != nil {
		t.Fatal(err)
	}
	if next.Remaining() != f.c.SaleRemaining-a.Amount || next.Sold() != a.Amount ||
		ledger.Remaining() != f.c.SaleRemaining {
		t.Fatal("non-atomic or wrong debit transition")
	}
	if _, err = ledger.ApplyCanonicalTransactions(f.c.Height, f.c.Parent, blockHash, []*core.Transaction{tx, tx}); err == nil {
		t.Fatal("duplicate sale accepted in the same block")
	}
	a.Height++
	a.Parent = blockHash
	replay, err := core.BuildSaleTransaction(a, f.address, f.c.Network, f.merchant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = next.ApplyCanonicalTransactions(a.Height, a.Parent, crypto.HashBytes([]byte("next")), []*core.Transaction{replay}); err == nil {
		t.Fatal("old order/payment re-signed for another block was accepted")
	}
	// Mutate the actual recipient output, not only an unsigned input field.
	mutated := *tx
	mutated.Outputs = append([]core.Output(nil), tx.Outputs...)
	otherWallet, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	other, err := core.BuildSaleTransaction(a, crypto.AddressFromKeys(f.c.Network, otherWallet), f.c.Network, f.merchant)
	if err != nil {
		t.Fatal(err)
	}
	mutated.Outputs[0].OneTimePub = other.Outputs[0].OneTimePub
	if _, err = ledger.ApplyCanonicalTransactions(f.c.Height, f.c.Parent, blockHash, []*core.Transaction{&mutated}); err == nil {
		t.Fatal("recipient substitution accepted")
	}
	wrongKey, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	a.Height = f.c.Height
	a.Parent = f.c.Parent
	forged, err := core.BuildSaleTransaction(a, f.address, f.c.Network, wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.ApplyCanonicalTransactions(f.c.Height, f.c.Parent, blockHash, []*core.Transaction{forged}); err == nil {
		t.Fatal("unauthorized issuer accepted")
	}
}
