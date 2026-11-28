// lpod-audit checks structural presence and linkage only; it does not authenticate
// signatures, proofs, or full-body integrity.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/aperod/aperod/store"
)

func main() {
	copiedDB := flag.String("copied-db", "", "path to a separate copied LevelDB database (required)")
	through := flag.String("through-height", "", "inclusive height to scan from genesis (required)")
	flag.Usage = func() {
		if _, err := fmt.Fprintln(flag.CommandLine.Output(), "lpod-audit performs offline read-only structural body coverage checks.\nIt does not authenticate signatures, proofs, or full-body integrity.\nThose require an independently attested LPoDBodyRootStep plus exact issuance openings."); err != nil {
			slog.Error("write audit usage failed", "err", err)
		}
		flag.PrintDefaults()
	}
	flag.Parse()

	if *copiedDB == "" || *through == "" {
		fmt.Fprintln(os.Stderr, "usage: lpod-audit --copied-db PATH --through-height HEIGHT")
		fmt.Fprintln(os.Stderr, "The path must name a separate copied database, never the live node database.")
		os.Exit(2)
	}
	throughHeight, err := strconv.ParseUint(*through, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --through-height %q: %v\n", *through, err)
		os.Exit(2)
	}

	report, auditErr := store.AuditLPoDBodyCoverageReadOnly(*copiedDB, throughHeight)
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "encode audit report: %v\n", err)
		os.Exit(1)
	}
	if auditErr != nil {
		fmt.Fprintln(os.Stderr, auditErr)
		os.Exit(1)
	}
}
