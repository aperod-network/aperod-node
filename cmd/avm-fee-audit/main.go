// Command avm-fee-audit inspects a copied Aperod chain database without
// modifying it. It deliberately accepts database paths only under /tmp.
package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type auditReport struct {
	TipHeight                     uint64             `json:"canonical_tip_height"`
	TipHash                       string             `json:"canonical_tip_hash"`
	ExpectedTipHeight             uint64             `json:"expected_tip_height"`
	ExpectedTipHash               string             `json:"expected_tip_hash"`
	StartHeight                   uint64             `json:"start_height"`
	EndHeight                     uint64             `json:"end_height"`
	AVMActivationHeight           uint64             `json:"avm_activation_height"`
	RewardActivationHeight        uint64             `json:"reward_authorization_activation_height"`
	AVMTransactions               uint64             `json:"avm_transactions"`
	RequiredGasNAPRO              uint64             `json:"required_gas_napro"`
	ActivatedRewardsWithAVM       uint64             `json:"activated_authorized_rewards_with_avm"`
	LegacyGasAsTipRewards         uint64             `json:"legacy_gas_as_tip_rewards"`
	GasExcludedFromTipsRewards    uint64             `json:"gas_excluded_from_tips_rewards"`
	UnclassifiedAuthorizedRewards uint64             `json:"unclassified_authorized_rewards"`
	InspectedRewards              []rewardInspection `json:"reward_inspection,omitempty"`
}

type rewardInspection struct {
	Height                      uint64  `json:"height"`
	AuthorizedRewardAmountNAPRO *uint64 `json:"authorized_reward_amount_napro"`
}

const maxRewardEraFeeExamples = 8
const maxRewardEraAVMWireExamples = 8

type rewardEraAuditReport struct {
	TipHeight                    uint64                `json:"canonical_tip_height"`
	TipHash                      string                `json:"canonical_tip_hash"`
	ExpectedTipHeight            uint64                `json:"expected_tip_height"`
	ExpectedTipHash              string                `json:"expected_tip_hash"`
	StartHeight                  uint64                `json:"start_height"`
	EndHeight                    uint64                `json:"end_height"`
	AVMActivationHeight          uint64                `json:"avm_activation_height"`
	RewardActivationHeight       uint64                `json:"reward_authorization_activation_height"`
	RewardCutoverHeight          uint64                `json:"reward_cutover_height"`
	PreCutoverBaseNAPRO          uint64                `json:"pre_cutover_base_napro"`
	PostCutoverBaseNAPRO         uint64                `json:"post_cutover_base_napro"`
	PreCutoverBlocks             uint64                `json:"pre_cutover_blocks"`
	PostCutoverBlocks            uint64                `json:"post_cutover_blocks"`
	FeeBearingBlocks             uint64                `json:"fee_bearing_blocks"`
	AVMTransactions              uint64                `json:"avm_transactions"`
	PreCutoverFeeBearingBlocks   uint64                `json:"pre_cutover_fee_bearing_blocks"`
	PostCutoverFeeBearingBlocks  uint64                `json:"post_cutover_fee_bearing_blocks"`
	PreCutoverAVMTransactions    uint64                `json:"pre_cutover_avm_transactions"`
	PostCutoverAVMTransactions   uint64                `json:"post_cutover_avm_transactions"`
	PreCutoverPriorityTipsNAPRO  uint64                `json:"pre_cutover_priority_tips_napro"`
	PostCutoverPriorityTipsNAPRO uint64                `json:"post_cutover_priority_tips_napro"`
	TotalTransactionFeesNAPRO    uint64                `json:"total_transaction_fees_napro"`
	TotalLegacyFeeFloorNAPRO     uint64                `json:"total_legacy_fee_floor_napro"`
	TotalPriorityTipsNAPRO       uint64                `json:"total_priority_tips_napro"`
	TotalRequiredAVMGasNAPRO     uint64                `json:"total_required_avm_gas_napro"`
	FeeBearingExamples           []rewardEraFeeExample `json:"fee_bearing_examples,omitempty"`
	AVMWireExamples              []avmWireExample      `json:"avm_wire_examples,omitempty"`
}

type rewardEraFeeExample struct {
	Height                      uint64 `json:"height"`
	PriorityTipsNAPRO           uint64 `json:"priority_tips_napro"`
	RequiredAVMGasNAPRO         uint64 `json:"required_avm_gas_napro"`
	AuthorizedRewardAmountNAPRO uint64 `json:"authorized_reward_amount_napro"`
	ExpectedRewardAmountNAPRO   uint64 `json:"expected_reward_amount_napro"`
}

type avmWireExample struct {
	Height     uint64         `json:"height"`
	Version    core.TxVersion `json:"version"`
	Action     core.AVMAction `json:"action"`
	GasLimit   uint64         `json:"gas_limit"`
	CodeLength uint64         `json:"code_length"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "avm-fee-audit: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("avm-fee-audit", flag.ContinueOnError)
	flags.SetOutput(output)
	dbPath := flags.String("db", "", "copied LevelDB directory under /tmp (required)")
	expectedTipHeight := flags.Uint64("expected-tip-height", 0, "tip height independently observed from the live API (required)")
	expectedTipHash := flags.String("expected-tip-hash", "", "64-character tip hash independently observed from the live API (required)")
	start := flags.Uint64("start", 0, "first height to scan, inclusive (default: selected audit's activation height)")
	end := flags.Uint64("end", 0, "last height to scan, inclusive (default: canonical tip)")
	avmActivation := flags.Uint64("avm-activation-height", 0, "AVM activation height from the effective node config (required)")
	rewardActivation := flags.Uint64("reward-activation-height", 0, "authorized-reward activation height from the effective node config (required)")
	inspectRewards := flags.Bool("inspect-rewards", false, "inspect verified public reward amounts over an explicit range of at most 16 heights")
	auditRewardEras := flags.Bool("audit-reward-eras", false, "stream and verify historical authorized-reward base amounts and fee accounting")
	rewardCutover := flags.Uint64("reward-cutover-height", 0, "observed historical reward base cutover height (required with --audit-reward-eras)")
	preCutoverBase := flags.Uint64("pre-cutover-base-napro", 0, "expected authorized reward base before cutover, in nAPRO (required with --audit-reward-eras)")
	postCutoverBase := flags.Uint64("post-cutover-base-napro", 0, "expected authorized reward base from cutover, in nAPRO (required with --audit-reward-eras)")
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: avm-fee-audit --db /tmp/copied/chain.db --expected-tip-height HEIGHT --expected-tip-hash HASH --avm-activation-height HEIGHT --reward-activation-height HEIGHT [--start HEIGHT] [--end HEIGHT] [--inspect-rewards | --audit-reward-eras --reward-cutover-height HEIGHT --pre-cutover-base-napro AMOUNT --post-cutover-base-napro AMOUNT]")
		fmt.Fprintln(output, "Supply tip values independently observed from the live API and activation heights from the effective node config.")
		fmt.Fprintln(output, "--inspect-rewards requires explicit --start and --end spanning at most 16 heights inclusive.")
		fmt.Fprintln(output, "--audit-reward-eras defaults to the reward authorization activation height through tip; explicit ranges are also accepted.")
		fmt.Fprintln(output, "AVM wire examples identify payload fields only; they do not establish AVM execution or user activity.")
		fmt.Fprintln(output, "This audit does not prove API source trust, block-header signatures, state transitions, or AVM execution/state.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.Usage()
			return nil
		}
		return fmt.Errorf("parse arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *dbPath == "" {
		return fmt.Errorf("--db must explicitly name a copied LevelDB directory under /tmp")
	}
	visited := make(map[string]bool, 5)
	flags.Visit(func(f *flag.Flag) {
		visited[f.Name] = true
	})
	for _, name := range []string{
		"expected-tip-height",
		"expected-tip-hash",
		"avm-activation-height",
		"reward-activation-height",
	} {
		if !visited[name] {
			return fmt.Errorf("--%s is required; supply the value independently read from the live API or effective node config", name)
		}
	}
	if *inspectRewards && *auditRewardEras {
		return fmt.Errorf("--inspect-rewards and --audit-reward-eras are mutually exclusive")
	}
	if *auditRewardEras {
		for _, name := range []string{"reward-cutover-height", "pre-cutover-base-napro", "post-cutover-base-napro"} {
			if !visited[name] {
				return fmt.Errorf("--%s is required with --audit-reward-eras", name)
			}
		}
	} else if visited["reward-cutover-height"] || visited["pre-cutover-base-napro"] || visited["post-cutover-base-napro"] {
		return fmt.Errorf("--reward-cutover-height and cutover base flags require --audit-reward-eras")
	}
	if *inspectRewards {
		if !visited["start"] || !visited["end"] {
			return fmt.Errorf("--inspect-rewards requires explicit --start and --end heights")
		}
		if *end < *start {
			return fmt.Errorf("--inspect-rewards requires --end greater than or equal to --start")
		}
		if *end-*start >= 16 {
			return fmt.Errorf("--inspect-rewards range is limited to 16 heights inclusive")
		}
	}
	observedTipHash, err := parseTipHash(*expectedTipHash)
	if err != nil {
		return err
	}
	if !visited["start"] {
		if *auditRewardEras {
			*start = *rewardActivation
		} else {
			*start = *avmActivation
		}
	}
	resolvedPath, err := validateCopyPath(*dbPath)
	if err != nil {
		return err
	}

	db, err := store.OpenReadOnly(resolvedPath)
	if err != nil {
		return fmt.Errorf("open copied LevelDB read-only: %w", err)
	}
	defer db.Close()

	var report any
	if *auditRewardEras {
		rewardReport, err := auditRewardEraDatabase(
			db,
			*start,
			*end,
			visited["end"],
			*expectedTipHeight,
			observedTipHash,
			*avmActivation,
			*rewardActivation,
			*rewardCutover,
			*preCutoverBase,
			*postCutoverBase,
		)
		if err != nil {
			return err
		}
		report = rewardReport
	} else {
		regularReport, err := auditDatabase(db, *start, *end, visited["end"], *expectedTipHeight, observedTipHash, *avmActivation, *rewardActivation, *inspectRewards)
		if err != nil {
			return err
		}
		report = regularReport
	}
	encoder := json.NewEncoder(output)
	if !*auditRewardEras {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write audit report: %w", err)
	}
	return nil
}

func parseTipHash(value string) (crypto.Hash32, error) {
	var hash crypto.Hash32
	if len(value) != hex.EncodedLen(len(hash)) {
		return hash, fmt.Errorf("--expected-tip-hash must contain exactly 64 hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return hash, fmt.Errorf("--expected-tip-hash is not valid hexadecimal")
	}
	copy(hash[:], decoded)
	return hash, nil
}

func validateCopyPath(path string) (string, error) {
	tmpRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return "", fmt.Errorf("resolve /tmp: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve copied database path: %w", err)
	}
	relative, err := filepath.Rel(tmpRoot, resolved)
	if err != nil || relative == "." || relative == ".." ||
		filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("database path must resolve to a directory below /tmp")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect copied database directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("copied database path must be a directory")
	}
	return resolved, nil
}

func auditDatabase(
	db *store.DB,
	start, requestedEnd uint64,
	endWasSet bool,
	expectedTipHeight uint64,
	expectedTipHash crypto.Hash32,
	avmActivation, rewardActivation uint64,
	inspectRewards bool,
) (auditReport, error) {
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		return auditReport{}, fmt.Errorf("read canonical tip: %w", err)
	}
	if tipHash == (crypto.Hash32{}) {
		return auditReport{}, fmt.Errorf("canonical tip hash is missing or zero")
	}
	if tipHeight != expectedTipHeight {
		return auditReport{}, fmt.Errorf("copied database tip height %d does not match independently observed API tip height %d", tipHeight, expectedTipHeight)
	}
	if tipHash != expectedTipHash {
		return auditReport{}, fmt.Errorf("copied database tip hash does not match independently observed API tip hash")
	}
	indexedTip, found, err := db.GetCanonicalHash(tipHeight)
	if err != nil {
		return auditReport{}, fmt.Errorf("read canonical tip index at height %d: %w", tipHeight, err)
	}
	if !found || indexedTip != tipHash {
		return auditReport{}, fmt.Errorf("canonical tip metadata does not match the height index at %d", tipHeight)
	}
	tipBody, err := db.GetRawBlock(tipHash)
	if err != nil {
		return auditReport{}, fmt.Errorf("read canonical tip block body: %w", err)
	}
	if len(tipBody) == 0 {
		return auditReport{}, fmt.Errorf("canonical tip block body is missing at height %d", tipHeight)
	}
	var tipBlock core.Block
	if err := json.Unmarshal(tipBody, &tipBlock); err != nil {
		return auditReport{}, fmt.Errorf("decode canonical tip block at height %d: %w", tipHeight, err)
	}
	if tipBlock.Header.Height != tipHeight || tipBlock.Hash() != tipHash {
		return auditReport{}, fmt.Errorf("canonical tip body hash/height mismatch at height %d", tipHeight)
	}
	if core.MerkleRoot(tipBlock.Txs) != tipBlock.Header.MerkleRoot {
		return auditReport{}, fmt.Errorf("canonical tip transaction merkle root mismatch at height %d", tipHeight)
	}

	end := requestedEnd
	if !endWasSet {
		end = tipHeight
	}
	if end > tipHeight {
		return auditReport{}, fmt.Errorf("end height %d exceeds canonical tip %d", end, tipHeight)
	}
	if start > end {
		return auditReport{}, fmt.Errorf("start height %d exceeds end height %d", start, end)
	}

	report := auditReport{
		TipHeight:              tipHeight,
		TipHash:                fmt.Sprintf("%x", tipHash[:]),
		ExpectedTipHeight:      expectedTipHeight,
		ExpectedTipHash:        fmt.Sprintf("%x", expectedTipHash[:]),
		StartHeight:            start,
		EndHeight:              end,
		AVMActivationHeight:    avmActivation,
		RewardActivationHeight: rewardActivation,
	}
	var previousHash crypto.Hash32
	if start > 0 {
		var ok bool
		previousHash, ok, err = db.GetCanonicalHash(start - 1)
		if err != nil {
			return auditReport{}, fmt.Errorf("read canonical parent index at height %d: %w", start-1, err)
		}
		if !ok {
			return auditReport{}, fmt.Errorf("canonical parent index is missing at height %d", start-1)
		}
	}

	for height := start; ; height++ {
		indexedHash, ok, err := db.GetCanonicalHash(height)
		if err != nil {
			return auditReport{}, fmt.Errorf("read canonical index at height %d: %w", height, err)
		}
		if !ok {
			return auditReport{}, fmt.Errorf("canonical height index is missing at height %d", height)
		}
		raw, err := db.GetRawBlock(indexedHash)
		if err != nil {
			return auditReport{}, fmt.Errorf("read canonical block body at height %d: %w", height, err)
		}
		if len(raw) == 0 {
			return auditReport{}, fmt.Errorf("canonical block body is missing at height %d", height)
		}
		var block core.Block
		if err := json.Unmarshal(raw, &block); err != nil {
			return auditReport{}, fmt.Errorf("decode canonical block at height %d: %w", height, err)
		}
		if block.Header.Height != height || block.Hash() != indexedHash {
			return auditReport{}, fmt.Errorf("canonical block hash/height mismatch at height %d", height)
		}
		if core.MerkleRoot(block.Txs) != block.Header.MerkleRoot {
			return auditReport{}, fmt.Errorf("canonical transaction merkle root mismatch at height %d", height)
		}
		if block.Header.PrevHash != previousHash {
			return auditReport{}, fmt.Errorf("canonical parent hash mismatch at height %d", height)
		}
		if err := auditBlock(&report, &block, rewardActivation, inspectRewards); err != nil {
			return auditReport{}, fmt.Errorf("audit block at height %d: %w", height, err)
		}
		previousHash = indexedHash
		if height == end {
			break
		}
	}
	return report, nil
}

func auditRewardEraDatabase(
	db *store.DB,
	start, requestedEnd uint64,
	endWasSet bool,
	expectedTipHeight uint64,
	expectedTipHash crypto.Hash32,
	avmActivation, rewardActivation uint64,
	cutoverHeight, preCutoverBase, postCutoverBase uint64,
) (rewardEraAuditReport, error) {
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		return rewardEraAuditReport{}, fmt.Errorf("read canonical tip: %w", err)
	}
	if tipHash == (crypto.Hash32{}) {
		return rewardEraAuditReport{}, fmt.Errorf("canonical tip hash is missing or zero")
	}
	if tipHeight != expectedTipHeight {
		return rewardEraAuditReport{}, fmt.Errorf("copied database tip height %d does not match independently observed API tip height %d", tipHeight, expectedTipHeight)
	}
	if tipHash != expectedTipHash {
		return rewardEraAuditReport{}, fmt.Errorf("copied database tip hash does not match independently observed API tip hash")
	}
	indexedTip, found, err := db.GetCanonicalHash(tipHeight)
	if err != nil {
		return rewardEraAuditReport{}, fmt.Errorf("read canonical tip index at height %d: %w", tipHeight, err)
	}
	if !found || indexedTip != tipHash {
		return rewardEraAuditReport{}, fmt.Errorf("canonical tip metadata does not match the height index at %d", tipHeight)
	}
	tipBody, err := db.GetRawBlock(tipHash)
	if err != nil {
		return rewardEraAuditReport{}, fmt.Errorf("read canonical tip block body: %w", err)
	}
	if len(tipBody) == 0 {
		return rewardEraAuditReport{}, fmt.Errorf("canonical tip block body is missing at height %d", tipHeight)
	}
	var tipBlock core.Block
	if err := json.Unmarshal(tipBody, &tipBlock); err != nil {
		return rewardEraAuditReport{}, fmt.Errorf("decode canonical tip block at height %d: %w", tipHeight, err)
	}
	if tipBlock.Header.Height != tipHeight || tipBlock.Hash() != tipHash {
		return rewardEraAuditReport{}, fmt.Errorf("canonical tip body hash/height mismatch at height %d", tipHeight)
	}
	if core.MerkleRoot(tipBlock.Txs) != tipBlock.Header.MerkleRoot {
		return rewardEraAuditReport{}, fmt.Errorf("canonical tip transaction merkle root mismatch at height %d", tipHeight)
	}

	end := requestedEnd
	if !endWasSet {
		end = tipHeight
	}
	if end > tipHeight {
		return rewardEraAuditReport{}, fmt.Errorf("end height %d exceeds canonical tip %d", end, tipHeight)
	}
	if start > end {
		return rewardEraAuditReport{}, fmt.Errorf("start height %d exceeds end height %d", start, end)
	}

	report := rewardEraAuditReport{
		TipHeight:              tipHeight,
		TipHash:                fmt.Sprintf("%x", tipHash[:]),
		ExpectedTipHeight:      expectedTipHeight,
		ExpectedTipHash:        fmt.Sprintf("%x", expectedTipHash[:]),
		StartHeight:            start,
		EndHeight:              end,
		AVMActivationHeight:    avmActivation,
		RewardActivationHeight: rewardActivation,
		RewardCutoverHeight:    cutoverHeight,
		PreCutoverBaseNAPRO:    preCutoverBase,
		PostCutoverBaseNAPRO:   postCutoverBase,
	}
	var previousHash crypto.Hash32
	if start > 0 {
		var ok bool
		previousHash, ok, err = db.GetCanonicalHash(start - 1)
		if err != nil {
			return rewardEraAuditReport{}, fmt.Errorf("read canonical parent index at height %d: %w", start-1, err)
		}
		if !ok {
			return rewardEraAuditReport{}, fmt.Errorf("canonical parent index is missing at height %d", start-1)
		}
	}

	for height := start; ; height++ {
		indexedHash, ok, err := db.GetCanonicalHash(height)
		if err != nil {
			return rewardEraAuditReport{}, fmt.Errorf("read canonical index at height %d: %w", height, err)
		}
		if !ok {
			return rewardEraAuditReport{}, fmt.Errorf("canonical height index is missing at height %d", height)
		}
		raw, err := db.GetRawBlock(indexedHash)
		if err != nil {
			return rewardEraAuditReport{}, fmt.Errorf("read canonical block body at height %d: %w", height, err)
		}
		if len(raw) == 0 {
			return rewardEraAuditReport{}, fmt.Errorf("canonical block body is missing at height %d", height)
		}
		var block core.Block
		if err := json.Unmarshal(raw, &block); err != nil {
			return rewardEraAuditReport{}, fmt.Errorf("decode canonical block at height %d: %w", height, err)
		}
		if block.Header.Height != height || block.Hash() != indexedHash {
			return rewardEraAuditReport{}, fmt.Errorf("canonical block hash/height mismatch at height %d", height)
		}
		if core.MerkleRoot(block.Txs) != block.Header.MerkleRoot {
			return rewardEraAuditReport{}, fmt.Errorf("canonical transaction merkle root mismatch at height %d", height)
		}
		if block.Header.PrevHash != previousHash {
			return rewardEraAuditReport{}, fmt.Errorf("canonical parent hash mismatch at height %d", height)
		}
		if err := auditRewardEraBlock(&report, &block, cutoverHeight, preCutoverBase, postCutoverBase); err != nil {
			return rewardEraAuditReport{}, fmt.Errorf("audit block at height %d: %w", height, err)
		}
		previousHash = indexedHash
		if height == end {
			break
		}
	}
	return report, nil
}

func auditRewardEraBlock(
	report *rewardEraAuditReport,
	block *core.Block,
	cutoverHeight, preCutoverBase, postCutoverBase uint64,
) error {
	for i := range block.Txs {
		if block.Txs[i].IsLPoDPayout() {
			return fmt.Errorf("unsupported LPoD payout block; reward-era audit is pre-LPoD only")
		}
	}

	var blockPriorityTips uint64
	var blockRequiredGas uint64
	var authorizedAmount uint64
	var avmTransactions uint64
	var coinbaseRewards uint64
	var verifiedRewards uint64
	for i := range block.Txs {
		tx := &block.Txs[i]
		if tx.IsAVM() {
			if tx.AVM == nil {
				return fmt.Errorf("AVM transaction %d has no AVM payload", i)
			}
			gasFee, err := core.AVMGasFee(tx.AVM.GasLimit)
			if err != nil {
				return fmt.Errorf("AVM transaction %d gas fee: %w", i, err)
			}
			if err := add(&avmTransactions, 1); err != nil {
				return fmt.Errorf("block AVM transaction count overflows uint64")
			}
			if err := add(&blockRequiredGas, gasFee); err != nil {
				return fmt.Errorf("block required AVM gas overflows uint64")
			}
			if len(report.AVMWireExamples) < maxRewardEraAVMWireExamples {
				report.AVMWireExamples = append(report.AVMWireExamples, avmWireExample{
					Height:     block.Header.Height,
					Version:    tx.Version,
					Action:     tx.AVM.Action,
					GasLimit:   tx.AVM.GasLimit,
					CodeLength: uint64(len(tx.AVM.Code)),
				})
			}
		}
		if tx.IsCoinbase() && !tx.IsStake() {
			coinbaseRewards++
			auth, err := core.ValidateAuthorizedRewardTx(tx, block.Header.Height, block.Header.PrevHash, block.Header.ValidatorPub)
			if err == nil {
				verifiedRewards++
				authorizedAmount = auth.Amount
			}
			continue
		}
		if tx.IsStake() {
			continue
		}

		if err := add(&report.TotalTransactionFeesNAPRO, tx.Fee); err != nil {
			return fmt.Errorf("total transaction fees overflow uint64")
		}
		requiredBeforeGas, err := rewardEraMinimumFee(tx, block.Header.BaseFee)
		if err != nil {
			return fmt.Errorf("transaction %d minimum fee: %w", i, err)
		}
		if intentionalBurn, isBurn := tx.BurnAmount(); isBurn {
			requiredBeforeGas, err = checkedSum(requiredBeforeGas, intentionalBurn)
			if err != nil {
				return fmt.Errorf("transaction %d legacy fee floor overflows uint64", i)
			}
		}
		if err := add(&report.TotalLegacyFeeFloorNAPRO, requiredBeforeGas); err != nil {
			return fmt.Errorf("total legacy fee floor overflows uint64")
		}
		if tx.Fee > requiredBeforeGas {
			tips := tx.Fee - requiredBeforeGas
			if err := add(&blockPriorityTips, tips); err != nil {
				return fmt.Errorf("block priority tips overflow uint64")
			}
			if err := add(&report.TotalPriorityTipsNAPRO, tips); err != nil {
				return fmt.Errorf("total priority tips overflow uint64")
			}
		}
	}

	if err := add(&report.AVMTransactions, avmTransactions); err != nil {
		return fmt.Errorf("total AVM transaction count overflows uint64")
	}
	if err := add(&report.TotalRequiredAVMGasNAPRO, blockRequiredGas); err != nil {
		return fmt.Errorf("total required AVM gas overflows uint64")
	}
	if coinbaseRewards != 1 || verifiedRewards != 1 {
		return fmt.Errorf("block must have exactly one signature-verified authorized coinbase reward; found %d coinbase and %d valid authorization(s)", coinbaseRewards, verifiedRewards)
	}

	expectedAmount := preCutoverBase
	if block.Header.Height >= cutoverHeight {
		var err error
		expectedAmount, err = checkedSum(postCutoverBase, blockPriorityTips)
		if err != nil {
			return fmt.Errorf("expected post-cutover reward overflows uint64")
		}
	}
	if authorizedAmount != expectedAmount {
		return fmt.Errorf("authorized reward mismatch at height %d: got %d nAPRO; expected %d nAPRO", block.Header.Height, authorizedAmount, expectedAmount)
	}

	if block.Header.Height < cutoverHeight {
		if err := add(&report.PreCutoverBlocks, 1); err != nil {
			return err
		}
		if err := add(&report.PreCutoverAVMTransactions, avmTransactions); err != nil {
			return fmt.Errorf("pre-cutover AVM transaction count overflows uint64")
		}
		if err := add(&report.PreCutoverPriorityTipsNAPRO, blockPriorityTips); err != nil {
			return fmt.Errorf("pre-cutover priority tips overflow uint64")
		}
		if blockPriorityTips > 0 {
			if err := add(&report.PreCutoverFeeBearingBlocks, 1); err != nil {
				return fmt.Errorf("pre-cutover fee-bearing block count overflows uint64")
			}
		}
	} else {
		if err := add(&report.PostCutoverBlocks, 1); err != nil {
			return err
		}
		if err := add(&report.PostCutoverAVMTransactions, avmTransactions); err != nil {
			return fmt.Errorf("post-cutover AVM transaction count overflows uint64")
		}
		if err := add(&report.PostCutoverPriorityTipsNAPRO, blockPriorityTips); err != nil {
			return fmt.Errorf("post-cutover priority tips overflow uint64")
		}
		if blockPriorityTips > 0 {
			if err := add(&report.PostCutoverFeeBearingBlocks, 1); err != nil {
				return fmt.Errorf("post-cutover fee-bearing block count overflows uint64")
			}
		}
	}
	if blockPriorityTips > 0 {
		if err := add(&report.FeeBearingBlocks, 1); err != nil {
			return err
		}
		if len(report.FeeBearingExamples) < maxRewardEraFeeExamples {
			report.FeeBearingExamples = append(report.FeeBearingExamples, rewardEraFeeExample{
				Height:                      block.Header.Height,
				PriorityTipsNAPRO:           blockPriorityTips,
				RequiredAVMGasNAPRO:         blockRequiredGas,
				AuthorizedRewardAmountNAPRO: authorizedAmount,
				ExpectedRewardAmountNAPRO:   expectedAmount,
			})
		}
	}
	return nil
}

func rewardEraMinimumFee(tx *core.Transaction, baseFee uint64) (uint64, error) {
	rate := baseFee
	if rate == 0 {
		rate = core.InitialBaseFeePerByte
	}
	size := uint64(tx.Size())
	if size != 0 && rate > ^uint64(0)/size {
		return 0, fmt.Errorf("fee floor overflows uint64")
	}
	return tx.MinFeeAt(baseFee), nil
}

func auditBlock(report *auditReport, block *core.Block, rewardActivation uint64, inspectRewards bool) error {
	for i := range block.Txs {
		if block.Txs[i].IsLPoDPayout() {
			return fmt.Errorf("unsupported LPoD payout block; this audit only classifies pre-LPoD rewards")
		}
	}

	var blockGasNAPRO uint64
	var blockAVMTransactions uint64
	var legacyTips uint64
	for i := range block.Txs {
		tx := &block.Txs[i]
		if tx.IsAVM() {
			if tx.AVM == nil {
				return fmt.Errorf("AVM transaction %d has no AVM payload", i)
			}
			gasFee, err := core.AVMGasFee(tx.AVM.GasLimit)
			if err != nil {
				return fmt.Errorf("AVM transaction %d gas fee: %w", i, err)
			}
			if err := add(&report.AVMTransactions, 1); err != nil {
				return err
			}
			if err := add(&blockAVMTransactions, 1); err != nil {
				return fmt.Errorf("block AVM transaction count overflows uint64")
			}
			if err := add(&report.RequiredGasNAPRO, gasFee); err != nil {
				return fmt.Errorf("required gas total overflows uint64")
			}
			if err := add(&blockGasNAPRO, gasFee); err != nil {
				return fmt.Errorf("block required gas total overflows uint64")
			}
		}
		if tx.IsCoinbase() || tx.IsStake() {
			continue
		}
		minimumFee := tx.MinFeeAt(block.Header.BaseFee)
		requiredBeforeGas := minimumFee
		if intentionalBurn, isBurn := tx.BurnAmount(); isBurn {
			if requiredBeforeGas > ^uint64(0)-intentionalBurn {
				return fmt.Errorf("transaction %d required fee overflows uint64", i)
			}
			requiredBeforeGas += intentionalBurn
		}
		if tx.Fee > requiredBeforeGas {
			if err := add(&legacyTips, tx.Fee-requiredBeforeGas); err != nil {
				return fmt.Errorf("legacy tip total overflows uint64")
			}
		}
	}

	if inspectRewards {
		amount, err := inspectAuthorizedReward(block)
		if err != nil {
			return err
		}
		report.InspectedRewards = append(report.InspectedRewards, rewardInspection{
			Height:                      block.Header.Height,
			AuthorizedRewardAmountNAPRO: amount,
		})
		return nil
	}

	if block.Header.Height < rewardActivation || blockAVMTransactions == 0 {
		return nil
	}
	if legacyTips < blockGasNAPRO {
		return fmt.Errorf("required AVM gas exceeds fee surplus; block economics need review")
	}

	legacyBase := authorizedBaseReward(block.Header.Height)
	legacyAmount, err := checkedSum(legacyBase, legacyTips)
	if err != nil {
		return fmt.Errorf("legacy authorized reward overflows uint64")
	}
	gasBurnAmount, err := checkedSum(legacyBase, legacyTips-blockGasNAPRO)
	if err != nil {
		return fmt.Errorf("gas-excluded authorized reward overflows uint64")
	}

	coinbaseRewards := 0
	verifiedRewards := 0
	classifiedRewards := 0
	var unclassifiedAmount uint64
	for i := range block.Txs {
		tx := &block.Txs[i]
		if !tx.IsCoinbase() || tx.IsStake() {
			continue
		}
		coinbaseRewards++
		auth, err := core.ValidateAuthorizedRewardTx(tx, block.Header.Height, block.Header.PrevHash, block.Header.ValidatorPub)
		if err != nil {
			continue
		}
		verifiedRewards++
		if err := add(&report.ActivatedRewardsWithAVM, 1); err != nil {
			return err
		}
		switch auth.Amount {
		case legacyAmount:
			classifiedRewards++
			if err := add(&report.LegacyGasAsTipRewards, 1); err != nil {
				return err
			}
		case gasBurnAmount:
			classifiedRewards++
			if err := add(&report.GasExcludedFromTipsRewards, 1); err != nil {
				return err
			}
		default:
			unclassifiedAmount = auth.Amount
			if err := add(&report.UnclassifiedAuthorizedRewards, 1); err != nil {
				return err
			}
		}
	}
	if coinbaseRewards != 1 || verifiedRewards != 1 {
		return fmt.Errorf("AVM-bearing reward-era block must have exactly one valid authorized coinbase reward; found %d coinbase and %d valid authorization(s)", coinbaseRewards, verifiedRewards)
	}
	if classifiedRewards != 1 {
		return fmt.Errorf(
			"AVM-bearing reward-era block has an unclassified authorized reward at height %d: got %d nAPRO; expected legacy gas-as-tip %d nAPRO or gas-excluded %d nAPRO; required AVM gas %d nAPRO",
			block.Header.Height,
			unclassifiedAmount,
			legacyAmount,
			gasBurnAmount,
			blockGasNAPRO,
		)
	}
	return nil
}

func inspectAuthorizedReward(block *core.Block) (*uint64, error) {
	var amount *uint64
	found := 0
	for i := range block.Txs {
		tx := &block.Txs[i]
		if !tx.IsCoinbase() || tx.IsStake() {
			continue
		}
		auth, err := core.ValidateAuthorizedRewardTx(tx, block.Header.Height, block.Header.PrevHash, block.Header.ValidatorPub)
		if err != nil {
			if len(tx.Extra) == core.RewardAuthorizationSize {
				return nil, fmt.Errorf("invalid authorized reward at height %d (coinbase index %d)", block.Header.Height, i)
			}
			continue
		}
		found++
		if found > 1 {
			return nil, fmt.Errorf("multiple authorized rewards at height %d", block.Header.Height)
		}
		publicAmount := auth.Amount
		amount = &publicAmount
	}
	return amount, nil
}

func authorizedBaseReward(height uint64) uint64 {
	reward := consensus.AuthorizedBlockRewardNAPR
	halvings := height / consensus.HalvingIntervalBlocks
	for i := uint64(0); i < halvings && reward > 0; i++ {
		reward /= 2
	}
	return reward
}

func checkedSum(a, b uint64) (uint64, error) {
	if a > ^uint64(0)-b {
		return 0, fmt.Errorf("sum overflow")
	}
	return a + b, nil
}

func add(dst *uint64, amount uint64) error {
	if *dst > ^uint64(0)-amount {
		return fmt.Errorf("audit counter overflow")
	}
	*dst += amount
	return nil
}
