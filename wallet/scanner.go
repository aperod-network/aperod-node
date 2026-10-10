// Package wallet — high-level wallet scanner combining DerivedKeys + core.WalletScanner.
// Wraps core.WalletScanner so callers only need a DerivedKeys or raw view/spend keys.
package wallet

import (
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// Scanner is a view-only wallet scanner tied to a single derived key set.
// It wraps core.WalletScanner and decodes owned UTXOs from a chain or block.
type Scanner struct {
	inner *core.WalletScanner
	addr  crypto.Address
}

// NewScannerFromDerived creates a Scanner from a DerivedKeys set.
// viewPriv is available via dk.Keys.View.Private.
func NewScannerFromDerived(dk *DerivedKeys) *Scanner {
	s := core.NewWalletScanner(
		dk.Keys.View.Private,
		dk.Keys.Spend.Public,
		dk.Keys.View.Public,
		crypto.TestnetByte,
	)
	return &Scanner{inner: s, addr: dk.Address}
}

// NewScannerFromKeys creates a Scanner from raw key material.
func NewScannerFromKeys(viewPriv crypto.Scalar32, spendPub, viewPub crypto.Point32, net crypto.NetworkByte) *Scanner {
	return &Scanner{
		inner: core.NewWalletScanner(viewPriv, spendPub, viewPub, net),
		addr:  crypto.EncodeAddress(net, spendPub, viewPub),
	}
}

// Address returns the wallet address this scanner is associated with.
func (s *Scanner) Address() crypto.Address { return s.addr }

// ScanBlock scans a single block and returns owned UTXOs.
func (s *Scanner) ScanBlock(block *core.Block) []core.OwnedUTXO {
	return s.inner.ScanBlock(block)
}

// ScanChain scans blocks [startHeight, endHeight] (inclusive) and returns all
// owned UTXOs found. Stops early if chain.GetByHeight returns nil.
func (s *Scanner) ScanChain(chain *core.Chain, startHeight, endHeight uint64) []core.OwnedUTXO {
	return s.inner.ScanChain(chain, startHeight, endHeight)
}

// Balance scans the entire chain up to its tip height and returns the total
// confirmed balance (in base units) along with the owned UTXO slice.
func (s *Scanner) Balance(chain *core.Chain) (balance uint64, owned []core.OwnedUTXO) {
	tip := chain.Height()
	owned = s.ScanChain(chain, 0, tip)
	balance = core.Balance(owned)
	return
}

// SpendableUTXOs returns owned UTXOs with amount > 0, suitable as TxBuilder inputs.
func SpendableUTXOs(owned []core.OwnedUTXO) []core.OwnedUTXO {
	out := make([]core.OwnedUTXO, 0, len(owned))
	for _, u := range owned {
		if u.Amount > 0 {
			out = append(out, u)
		}
	}
	return out
}
