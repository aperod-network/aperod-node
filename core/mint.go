package core

import (
	"fmt"

	"github.com/aperod/aperod/crypto"
)

// BuildMintTx creates a coinbase-style transaction that mints amount to addr.
// Used for admin-initiated mints; bypasses ring signatures (coinbase exemption).
// The transaction has no inputs (IsCoinbase() == true), so the mempool and
// block validator skip ring sig and range proof requirements.
func BuildMintTx(addr crypto.Address, amount uint64) (*Transaction, error) {
	if amount == 0 {
		return nil, fmt.Errorf("mint amount must be > 0")
	}
	out, _, err := txBuildOutput(addr, amount)
	if err != nil {
		return nil, fmt.Errorf("build mint output: %w", err)
	}
	tx := &Transaction{
		Version: TxVersionBase,
		Inputs:  nil, // coinbase — no ring inputs
		Outputs: []Output{out},
		Fee:     0, // mints have no fee (coinbase exemption)
	}
	return tx, nil
}
