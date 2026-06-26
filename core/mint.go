package core

import (
	"fmt"

	"github.com/aperod/aperod/crypto"
)

// BuildMintTx creates a transparent coinbase-style transaction that mints amount to addr.
//
// Unlike regular RingCT transfers, mint outputs use the spend public key directly as
// OneTimePub (same as CoinbaseTx).  This "transparent" output is visible to the
// block explorer via the simple out.OneTimePub == spendPub scan without needing
// the recipient's view key.  Admin mints are already non-private by nature
// (the operator knows who receives what), so privacy-via-stealth adds no value here.
func BuildMintTx(addr crypto.Address, amount uint64) (*Transaction, error) {
	if amount == 0 {
		return nil, fmt.Errorf("mint amount must be > 0")
	}

	_, spendPub, _, err := crypto.DecodeAddress(addr)
	if err != nil {
		return nil, fmt.Errorf("decode address: %w", err)
	}

	blind, err := crypto.NewBlindFactor()
	if err != nil {
		return nil, fmt.Errorf("blind factor: %w", err)
	}
	commit, err := crypto.Commit(amount, blind)
	if err != nil {
		return nil, fmt.Errorf("pedersen commit: %w", err)
	}

	tx := &Transaction{
		Version: TxVersionBase,
		Inputs:  nil, // coinbase — no ring inputs
		Outputs: []Output{{
			OneTimePub:   spendPub, // transparent: spend pub used directly (like CoinbaseTx)
			AmountCommit: commit,
		}},
		Fee: 0, // mints are fee-exempt (coinbase exemption in mempool)
	}
	return tx, nil
}
