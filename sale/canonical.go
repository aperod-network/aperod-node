package sale

import (
	"fmt"
	"math"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ApplyCanonicalTransactions validates actual confidential outputs as well as
// their debits. This remains a pure transition: it does NOT persist a block,
// create a durable UTXO, enable consensus activation or authorize invoices.
func (s *Ledger) ApplyCanonicalTransactions(height uint64, parent, blockHash crypto.Hash32,
	transactions []*core.Transaction) (*Ledger, error) {
	if s == nil || s.seen == nil || s.paid == nil || height == 0 || height != s.height+1 ||
		parent != s.hash || blockHash == zeroHash || blockHash == parent {
		return nil, fmt.Errorf("sale: missing or noncanonical ledger parent")
	}
	next := &Ledger{checkpoint: s.checkpoint, height: height, hash: blockHash,
		remaining: s.remaining, sold: s.sold,
		seen: make(map[crypto.Hash32]struct{}, len(s.seen)+len(transactions)),
		paid: make(map[crypto.Hash32]struct{}, len(s.paid)+len(transactions))}
	for id := range s.seen {
		next.seen[id] = struct{}{}
	}
	for id := range s.paid {
		next.paid[id] = struct{}{}
	}
	for _, tx := range transactions {
		if tx == nil {
			return nil, fmt.Errorf("sale: nil authorized transaction")
		}
		a, err := tx.SaleAuthorization()
		if err != nil {
			return nil, err
		}
		if a.Genesis != s.checkpoint.Genesis || a.Height != height || a.Parent != parent ||
			!s.checkpoint.SaleAuthority.Verify(tx.SaleSigningMessage(), a.Signature[:]) {
			return nil, fmt.Errorf("sale: unauthorized output or activation context")
		}
		if _, found := next.seen[a.Order]; found {
			return nil, fmt.Errorf("sale: purchase order already consumed")
		}
		if _, found := next.paid[a.Payment]; found {
			return nil, fmt.Errorf("sale: payment already consumed")
		}
		if a.Amount > next.remaining || a.Amount > math.MaxUint64-next.sold {
			return nil, fmt.Errorf("sale: insufficient allocation")
		}
		next.seen[a.Order] = struct{}{}
		next.paid[a.Payment] = struct{}{}
		next.remaining -= a.Amount
		next.sold += a.Amount
	}
	return next, nil
}
