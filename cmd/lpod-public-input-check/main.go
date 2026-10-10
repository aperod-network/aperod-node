// lpod-public-input-check validates already-public activation inputs against an
// offline recovered DB. It never signs, starts a node, or opens a writable DB.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func main() {
	dbPath := flag.String("copied-db", "", "offline recovered database only")
	witness := flag.String("witness", "", "existing signed public witness")
	config := flag.String("public-config", "", "allowlisted public config JSON")
	flag.Parse()
	if *dbPath == "" || *witness == "" || *config == "" {
		fail("all three paths are required")
	}
	var c struct {
		Consensus struct {
			Authorities []string `json:"lpod_v3_trust_authorities"`
		} `json:"consensus"`
	}
	readJSON(*config, &c)
	var m store.LPoDMigration
	readJSON(*witness, &m)
	if m.Version != 3 {
		fail("expected version 3 witness")
	}
	for _, text := range c.Consensus.Authorities {
		pub, err := hex.DecodeString(text)
		if err != nil || len(pub) != 32 {
			fail("invalid configured public authority")
		}
		m.TrustedValidators = append(m.TrustedValidators, crypto.ValidatorPubKey(pub))
	}
	if err := m.VerifyAuthorization(); err != nil {
		fail("authorization: %v", err)
	}
	db, err := store.OpenLPoDAuditReadOnly(*dbPath)
	if err != nil {
		fail("open copied DB: %v", err)
	}
	defer db.Close()
	if err := db.CheckLPoDConfig(nil); err == nil {
		fail("archive does not require an existing activation binding")
	}
	if err := db.CheckLPoDConfig(&m); err != nil {
		fail("bound activation: %v", err)
	}
	parent, found, err := db.GetCanonicalHash(m.Height - 1)
	if err != nil || !found || parent != m.ParentHash {
		fail("witness parent does not match archive index: %v", err)
	}
	root := m.Root()
	if root != m.ReconciliationRoot {
		fail("witness reconciliation root mismatch")
	}
	fmt.Printf("authorization_valid=true archive_bound_config_matches=true parent_matches=true height=%d root=%x genesis=%x parent=%x historical_committee_authenticated=false\n",
		m.Height, root, m.Genesis, parent)
}

func readJSON(path string, target any) {
	raw, err := os.ReadFile(path)
	if err != nil {
		fail("read public input: %v", err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		fail("decode public input: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}