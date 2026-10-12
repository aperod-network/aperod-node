// sale-preflight only reads a stopped-node copy. It does not sign, reserve,
// initialize, repair or activate an allocation ledger.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

func run() error {
	copied := flag.String("copied-db", "", "separate, quiesced LevelDB copy")
	claimsPath := flag.String("claims", "", "optional operator declaration; produces an unsigned snapshot candidate only")
	burnReview := flag.String("recorded-burn-napro", "", "optional recorded counter for unsigned post-burn classification review, not historical proof")
	walletNetReview := flag.String("outside-wallet-net-napro", "", "optional declared net issued holdings outside LPoD balance/principal, including validator self-stake")
	netMode := flag.Bool("net-allocation-candidate", false, "unsigned V2 candidate; requires explicit wallet accounting and operator claims")
	walletRoot := flag.String("wallet-accounting-root", "", "exact lowercase hash of the outside-wallet accounting declaration")
	flag.Parse()
	if *netMode && (*burnReview == "" || *walletNetReview == "" || *walletRoot == "" || *claimsPath == "") {
		return fmt.Errorf("V2 candidate requires recorded burn, outside wallet net, wallet root and claims")
	}
	if !*netMode && *walletRoot != "" {
		return fmt.Errorf("wallet accounting root requires V2 candidate mode")
	}
	if *walletNetReview != "" && *burnReview == "" {
		return fmt.Errorf("-outside-wallet-net-napro requires the same -recorded-burn-napro baseline")
	}
	if *claimsPath != "" && !*netMode {
		return fmt.Errorf("claims require explicit net-allocation candidate mode; legacy sale budgets are not supported")
	}
	if *copied == "" {
		return fmt.Errorf("-copied-db is required; never target the running database")
	}
	target, err := filepath.EvalSymlinks(*copied)
	if err != nil {
		return err
	}
	// Production's live path is explicitly excluded even though the storage
	// wrapper itself is read-only and refuses to create a missing database.
	if target == "/opt/aperod/data/testnet/chain.db" {
		return fmt.Errorf("refusing the live production database")
	}
	db, err := store.OpenReadOnly(target)
	if err != nil {
		return err
	}
	defer db.Close()
	tip, height, err := db.GetTip()
	if err != nil {
		return err
	}
	checkpoint, err := db.LoadLPoDCheckpoint()
	if err != nil {
		return err
	}
	if checkpoint == nil || checkpoint.Allocation == nil || checkpoint.Allocation.NominalSnapshot == nil {
		return fmt.Errorf("canonical nominal funding declaration unavailable")
	}
	allocation := checkpoint.Allocation
	issued, err := store.SaleKnownIssuedFloor(checkpoint)
	if err != nil {
		return err
	}
	sales, err := db.LoadSaleState()
	if err != nil {
		return err
	}
	coverage, err := reviewUTXOMaterial(db.IterUTXOs)
	if err != nil {
		return err
	}
	digest := checkpoint.Digest()
	genesisRaw, genesisErr := db.GetRawBlockByHeight(0)
	var genesis core.Block
	genesisDecoded := genesisErr == nil && len(genesisRaw) != 0 && json.Unmarshal(genesisRaw, &genesis) == nil
	genesisMatches := genesisDecoded && genesis.Hash() == allocation.Genesis
	report := map[string]any{
		"scope":  "read-only operator snapshot; not historical issuance or finality proof",
		"height": height, "tip_hash": hex.EncodeToString(tip[:]),
		"lpod_commitment":                          hex.EncodeToString(digest[:]),
		"lpod_height":                              checkpoint.State.LastHeight,
		"declared_issuance_floor_napro":            strconv.FormatUint(issued, 10),
		"nominal_declaration_before_rewards_napro": strconv.FormatUint(allocation.NominalSnapshot.NominalCirculatingBefore, 10),
		"post_declaration_reward_inflow_napro":     strconv.FormatUint(checkpoint.State.RewardInflow, 10),
		"nominal_eligible_napro":                   strconv.FormatUint(allocation.NominalEligible, 10),
		"lpod_unassigned_napro":                    strconv.FormatUint(allocation.Remaining, 10),
		"initial_validator_remaining_napro":        strconv.FormatUint(allocation.InitialValidatorRemaining, 10),
		"preexisting_guardian_reservation_napro":   strconv.FormatUint(allocation.NominalSnapshot.PreexistingGuardianReservation, 10),
		"validator_remaining_napro":                strconv.FormatUint(allocation.ValidatorRemaining, 10),
		"sale_ledger_present":                      sales != nil,
		"utxo_material_coverage":                   coverage,
		"sale_activation_authorized":               false,
		"remaining_claims_reconciled":              false,
		"genesis_body_available":                   genesisDecoded,
		"genesis_body_matches_declaration":         genesisMatches,
	}
	epoch, err := epochReview(checkpoint)
	if err != nil {
		return err
	}
	report["lpod_allocation_epoch_review"] = epoch
	if *burnReview != "" {
		burned, err := exactAmount(*burnReview)
		if err != nil {
			return err
		}
		var wallets *uint64
		if *walletNetReview != "" {
			v, err := exactAmount(*walletNetReview)
			if err != nil {
				return err
			}
			wallets = &v
		}
		review, err := store.ReviewPostBurnSaleSource(checkpoint, burned, wallets)
		if err != nil {
			return err
		}
		report["post_burn_source_review"] = review
		report["post_burn_source_review_scope"] = "unsigned classification; outside-wallet net and burn baseline require authority attestation"
		if *netMode {
			if sales != nil || checkpoint.State.LastHeight != height || !genesisMatches {
				return fmt.Errorf("net candidate requires exact unactivated canonical snapshot")
			}
			claims, err := loadClaims(*claimsPath)
			if err != nil {
				return err
			}
			r, budget, err := netCandidate(checkpoint, tip, *claims, burned, *wallets, *walletRoot)
			if err != nil {
				return err
			}
			report["net_candidate_reconciliation"] = r
			report["net_candidate_sale_budget_napro"] = strconv.FormatUint(budget, 10)
			report["checkpoint_signing_authorized"] = false
			report["net_candidate_scope"] = "unsigned forward V2 declaration; wallet accounting and burn baseline are operator attestations, not historical proof"
		}
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sale preflight:", err)
		os.Exit(1)
	}
}
