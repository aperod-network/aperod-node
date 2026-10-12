package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// SaleInvoice is private merchant metadata, never consensus/public metadata.
// Decimal strings are immutable; the API display float is not a payment quote.
type SaleInvoice struct {
	ProviderID string `json:"provider_id"`
	Currency   string `json:"currency"`
	Address    string `json:"address"`
	Amount     string `json:"amount"`
	PriceUSD   string `json:"price_usd"`
}

var saleProviderID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var saleCurrency = regexp.MustCompile(`^[a-z0-9]{2,32}$`)
var saleDecimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,29})(\.[0-9]{1,30})?$`)

func (i SaleInvoice) Valid() bool {
	value, ok := new(big.Rat).SetString(i.Amount)
	return saleProviderID.MatchString(i.ProviderID) && saleCurrency.MatchString(i.Currency) &&
		saleDecimal.MatchString(i.Amount) && ok && value.Sign() > 0 && validSalePrice(i.PriceUSD) &&
		len(i.Address) > 0 && len(i.Address) <= 256 &&
		!strings.ContainsAny(i.Address, " \t\r\n\x00")
}

func validSalePrice(price string) bool {
	v, ok := new(big.Rat).SetString(price)
	return saleDecimal.MatchString(price) && ok && v.Sign() > 0
}

func SaleProviderPayment(id string) crypto.Hash32 {
	return crypto.Hash32(sha256.Sum256([]byte("aperod/protocol-sale/payment/v1\x00NOWPayments\x00" + id)))
}

// BindSaleInvoice synchronously fences provider identity across all orders.
// Identical retries survive a lost HTTP response or process restart.
func (d *DB) BindSaleInvoice(id crypto.Hash32, invoice SaleInvoice) error {
	d.canonicalMu.Lock()
	defer d.canonicalMu.Unlock()
	if !invoice.Valid() {
		return fmt.Errorf("sale: invalid exact invoice")
	}
	order, err := d.SaleOrder(id)
	if err != nil {
		return err
	}
	if order == nil {
		return fmt.Errorf("sale: invoice has no reservation")
	}
	if order.QuoteUSD == "" || invoice.PriceUSD != order.QuoteUSD {
		return fmt.Errorf("sale: invoice price does not match frozen quote")
	}
	if order.Invoice != nil {
		if *order.Invoice == invoice {
			return nil
		}
		return fmt.Errorf("sale: immutable invoice changed")
	}
	if order.Payment != (crypto.Hash32{}) {
		return fmt.Errorf("sale: invoice binding after payment refused")
	}
	payment := SaleProviderPayment(invoice.ProviderID)
	key := append([]byte("sale/invoice/"), payment[:]...)
	existing, err := d.get(key)
	if err != nil {
		return err
	}
	if existing != nil {
		return fmt.Errorf("sale: invoice already belongs to another order")
	}
	if used, err := d.SaleConsumed('p', payment); err != nil {
		return err
	} else if used {
		return fmt.Errorf("sale: invoice payment already consumed")
	}
	if existing, err := d.get(saleOrderPaymentKey(payment)); err != nil {
		return err
	} else if existing != nil {
		return fmt.Errorf("sale: payment identity already assigned")
	}
	order.Invoice = &invoice
	raw, err := json.Marshal(order)
	if err != nil {
		return err
	}
	batch := new(leveldb.Batch)
	batch.Put(saleOrderKey(id), raw)
	batch.Put(key, id[:])
	return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}
