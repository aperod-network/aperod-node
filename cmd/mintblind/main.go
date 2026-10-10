// Command mintblind computes the deterministic mint blind and Pedersen
// commitment for a given address and one or more candidate amounts (in APRO).
//
// Admin mints use a deterministic blind derived from spendPub + amount
// (crypto.DeterministicMintBlind), so given an address and an amount the
// on-chain commitment is fully reproducible.  This tool lets an operator
// determine the TRUE amount of an on-chain mint output by comparing the
// computed commitment against the amount_commit_hex reported by
// GET /api/v1/utxo/{txhash}/{idx}, and simultaneously recovers the blind_hex
// needed to make the UTXO spendable again if the wallet DB lost it.
//
// Usage:
//
//	go run ./cmd/mintblind <address> <amount_apro> [<amount_apro> ...]
//
// Output (one line per amount):
//
//	amount_apro=78060 amount_napr=7806000000000 blind_hex=<64 hex> commit_hex=<64 hex>
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/aperod/aperod/crypto"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: mintblind <address> <amount_apro> [<amount_apro> ...]")
		os.Exit(1)
	}

	addr := crypto.Address(os.Args[1])
	_, spendPub, _, err := crypto.DecodeAddress(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode address: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("address=%s\nspend_pub=%x\n\n", addr, spendPub[:])

	for _, arg := range os.Args[2:] {
		apro, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad amount %q: %v\n", arg, err)
			os.Exit(1)
		}
		napr := uint64(apro*1e8 + 0.5)

		blind, err := crypto.DeterministicMintBlind(spendPub, napr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mint blind for %s: %v\n", arg, err)
			os.Exit(1)
		}
		commit, err := crypto.Commit(napr, blind)
		if err != nil {
			fmt.Fprintf(os.Stderr, "commit for %s: %v\n", arg, err)
			os.Exit(1)
		}
		fmt.Printf("amount_apro=%s amount_napr=%d\n  blind_hex=%x\n  commit_hex=%x\n\n", arg, napr, blind[:], commit[:])
	}
}
