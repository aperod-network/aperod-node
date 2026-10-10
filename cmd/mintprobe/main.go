// Command mintprobe determines the true (amount, height) parameters of an
// on-chain mint transaction by brute-forcing candidate combinations through
// core.BuildMintTx and comparing the resulting deterministic tx hash against
// target hashes observed on chain.
//
// A mint tx built by BuildMintTx has no timestamp or nonce, so its hash is a
// pure function of (spendPub, amount, height).  Matching the hash therefore
// proves both the amount AND the derivation height (which determines the
// one-time pub and key image) without trusting any node-side UTXO store.
//
// Usage:
//
//	go run ./cmd/mintprobe <address> <txhash:blockheight> [...more]
//
// For each target the tool tries amounts passed via -amounts (comma-separated
// APRO values) at heights {0} ∪ [blockheight-10, blockheight+10].
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func main() {
	amountsFlag := flag.String("amounts", "78060,148858320,148858315", "comma-separated candidate amounts in APRO")
	window := flag.Uint64("window", 10, "height search window around each block height")
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mintprobe [-amounts a,b,c] <address> <txhash:blockheight> [...]")
		os.Exit(1)
	}

	addr := crypto.Address(args[0])
	if _, _, _, err := crypto.DecodeAddress(addr); err != nil {
		fmt.Fprintf(os.Stderr, "decode address: %v\n", err)
		os.Exit(1)
	}

	var amounts []uint64
	for _, a := range strings.Split(*amountsFlag, ",") {
		apro, err := strconv.ParseFloat(strings.TrimSpace(a), 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad amount %q: %v\n", a, err)
			os.Exit(1)
		}
		amounts = append(amounts, uint64(apro*1e8+0.5))
	}

	for _, target := range args[1:] {
		parts := strings.SplitN(target, ":", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "bad target %q: want txhash:blockheight\n", target)
			os.Exit(1)
		}
		wantHex := strings.ToLower(parts[0])
		blockH, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad height in %q: %v\n", target, err)
			os.Exit(1)
		}

		fmt.Printf("=== target %s (block %d) ===\n", wantHex[:16], blockH)

		heights := []uint64{0}
		lo := uint64(0)
		if blockH > *window {
			lo = blockH - *window
		}
		for h := lo; h <= blockH+*window; h++ {
			heights = append(heights, h)
		}

		matched := false
		for _, amt := range amounts {
			for _, h := range heights {
				tx, err := core.BuildMintTx(addr, amt, h)
				if err != nil {
					continue
				}
				hash := tx.Hash()
				if fmt.Sprintf("%x", hash[:]) == wantHex {
					fmt.Printf("  MATCH: amount_napr=%d (%.8f APRO) height=%d one_time_pub=%x commit=%x\n",
						amt, float64(amt)/1e8, h, tx.Outputs[0].OneTimePub[:], tx.Outputs[0].AmountCommit[:])
					matched = true
				}
			}
		}
		if !matched {
			fmt.Println("  NO MATCH — try more amounts (-amounts) or a wider window (-window)")
		}
		fmt.Println()
	}
}
