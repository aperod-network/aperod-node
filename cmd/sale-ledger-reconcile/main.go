// Explicit offline maintenance. Never runs a node or loads a signing key.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func main() {
	cfgPath := flag.String("config", "", "explicit node configuration")
	copyPath := flag.String("copy", "", "completed copy chain.db; empty means stopped configured database")
	tipHex := flag.String("expected-tip", "", "exact canonical tip hash")
	height := flag.Uint64("expected-height", 0, "exact canonical tip height")
	apply := flag.Bool("apply", false, "atomically apply validated no-sale catch-up")
	flag.Parse()
	if *cfgPath == "" {
		fail("explicit configuration required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fail("configuration unavailable")
	}
	if cfg.DataDir == "" || cfg.Consensus.SaleMigrationFile == "" {
		fail("signed sale configuration required")
	}
	raw, err := hex.DecodeString(*tipHex)
	if err != nil || len(raw) != 32 || *height == 0 {
		fail("explicit nonzero expected tip required")
	}
	var hash crypto.Hash32
	copy(hash[:], raw)
	target := *copyPath
	if target == "" {
		target = filepath.Join(cfg.DataDir, "chain.db")
	}
	var d *store.DB
	if *apply {
		d, err = store.Open(target)
	} else {
		d, err = store.OpenReadOnly(target)
	}
	if err != nil {
		fail("exclusive database unavailable; stop the producer before apply")
	}
	report, err := d.ReconcileSaleLedger(cfg.Consensus.SaleMigrationFile, hash, *height, *apply)
	closeErr := d.Close()
	if err != nil {
		fail(err.Error())
	}
	if closeErr != nil {
		fail("database close failed")
	}
	if json.NewEncoder(os.Stdout).Encode(report) != nil {
		fail("receipt write failed")
	}
}
func fail(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }
