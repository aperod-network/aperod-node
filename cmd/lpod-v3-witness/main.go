// lpod-v3-witness prepares a nominal-reclassification witness from explicit
// operator-supplied declarations and signs it only after a fail-closed
// preflight of a caller-confirmed frozen DB copy. It never writes to the DB.
//
// This tool supports only the operator-assumed one-validator case. The
// supplied registry JSON is not authenticated by its canonical height/hash:
// that anchor identifies a block, not registry contents. One valid certificate
// vote does not prove a quorum or the full committee. The caller must explicitly
// acknowledge both limitations and confirm that the DB copy is frozen. Nominal
// circulation and eligibility are trust-attested declarations, not measured
// wallet supply or historical-issuance proofs. This tool does not replace
// consensus startup's committee/certificate restoration or coordinated-upgrade
// checks.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/aperod/aperod/crypto"
)

type options struct {
	dbPath                           string
	outputPath                       string
	keyPath                          string
	registryPath                     string
	height                           uint64
	parentHash                       crypto.Hash32
	genesis                          crypto.Hash32
	nominalCirculatingAPRO           uint64
	eligibleNominalAPRO              uint64
	validatorRemainingNAPRO          uint64
	expectedValidator                crypto.ValidatorPubKey
	registryHeight                   uint64
	registryHash                     crypto.Hash32
	acceptOperatorRegistryAssumption bool
	confirmFrozenCopy                bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lpod-v3-witness:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("lpod-v3-witness", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var dbPath, outputPath, keyPath, registryPath string
	var parentHex, genesisHex, validatorHex, registryHashHex string
	var height, registryHeight uint64
	var nominalCirculatingAPRO, eligibleNominalAPRO, validatorRemainingNAPRO uint64
	var acceptOperatorRegistryAssumption, confirmFrozenCopy bool
	fs.StringVar(&dbPath, "db", "", "offline copied LevelDB directory under /tmp")
	fs.StringVar(&outputPath, "output", "", "new witness JSON path (exclusive create, mode 0600)")
	fs.StringVar(&keyPath, "validator-key", "", "raw 32-byte seed or 64-byte validator key file (mode 0600)")
	fs.StringVar(&registryPath, "validator-set-json", "", "operator-trusted registry JSON under /tmp; block anchor does not authenticate its contents")
	fs.Uint64Var(&height, "activation-height", 0, "exact future activation height")
	fs.StringVar(&parentHex, "expected-parent-hash", "", "exact expected parent hash (64 hex characters)")
	fs.StringVar(&genesisHex, "genesis-hash", "", "expected genesis hash (64 hex characters)")
	fs.Uint64Var(&nominalCirculatingAPRO, "nominal-circulating-before-apro", 0, "enter exactly 6230000000 APRO (approved operator declaration; not measured supply or proof of availability)")
	fs.Uint64Var(&eligibleNominalAPRO, "eligible-nominal-apro", 0, "enter exactly 1000000000 APRO (approved operator declaration; not independent proof of availability)")
	fs.Uint64Var(&validatorRemainingNAPRO, "validator-remaining-napro", 0, "exact remaining validator budget from the copied DB, in nAPRO")
	fs.StringVar(&validatorHex, "expected-validator-pubkey", "", "expected validator public key (64 hex characters)")
	fs.Uint64Var(&registryHeight, "validator-set-height", 0, "canonical height anchoring the supplied registry snapshot")
	fs.StringVar(&registryHashHex, "validator-set-hash", "", "canonical hash anchoring the supplied registry snapshot (does not authenticate registry contents)")
	fs.BoolVar(&acceptOperatorRegistryAssumption, "accept-operator-registry-assumption", false, "REQUIRED: acknowledge registry contents are operator-trusted and one vote does not prove a quorum")
	fs.BoolVar(&confirmFrozenCopy, "confirm-frozen-copy", false, "REQUIRED: confirm --db is an offline frozen copy; /tmp alone does not prove this")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	supplied := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { supplied[f.Name] = true })
	for _, name := range []string{
		"db", "output", "validator-key", "validator-set-json", "activation-height",
		"expected-parent-hash", "genesis-hash", "nominal-circulating-before-apro",
		"eligible-nominal-apro", "validator-remaining-napro", "expected-validator-pubkey",
		"validator-set-height", "validator-set-hash",
	} {
		if !supplied[name] {
			return fmt.Errorf("required flag --%s was not supplied", name)
		}
	}

	var err error
	opts := options{
		dbPath:                           dbPath,
		outputPath:                       outputPath,
		keyPath:                          keyPath,
		registryPath:                     registryPath,
		height:                           height,
		nominalCirculatingAPRO:           nominalCirculatingAPRO,
		eligibleNominalAPRO:              eligibleNominalAPRO,
		validatorRemainingNAPRO:          validatorRemainingNAPRO,
		registryHeight:                   registryHeight,
		acceptOperatorRegistryAssumption: acceptOperatorRegistryAssumption,
		confirmFrozenCopy:                confirmFrozenCopy,
	}
	if opts.parentHash, err = parseHash(parentHex); err != nil {
		return fmt.Errorf("expected parent hash: %w", err)
	}
	if opts.genesis, err = parseHash(genesisHex); err != nil {
		return fmt.Errorf("genesis hash: %w", err)
	}
	if opts.registryHash, err = parseHash(registryHashHex); err != nil {
		return fmt.Errorf("validator-set hash: %w", err)
	}
	rawPub, err := hex.DecodeString(validatorHex)
	if err != nil || len(rawPub) != 32 {
		return fmt.Errorf("expected validator public key must be exactly 32 bytes of hex")
	}
	opts.expectedValidator = crypto.ValidatorPubKey(rawPub)
	return prepareAndSign(opts)
}
