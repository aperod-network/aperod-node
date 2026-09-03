package main

import (
	"fmt"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/wallet"
)

// deriveMainnetAddress is deliberately kept independent of syscall/js so the
// wallet derivation used by the browser has a small, directly testable boundary.
// wallet.DeriveFromMnemonic currently constructs a testnet Address in hd.go;
// re-encoding its public keys here is therefore required for the wallet UI.
func deriveMainnetAddress(mnemonic string, account, index uint32) (string, error) {
	derived, err := wallet.DeriveFromMnemonic(mnemonic, "", account, index)
	if err != nil {
		return "", fmt.Errorf("derive wallet: %w", err)
	}
	return crypto.AddressFromKeys(crypto.MainnetByte, derived.Keys).String(), nil
}
