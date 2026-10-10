package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

const (
	testAVMActivationHeight    = uint64(1_850_000)
	testRewardActivationHeight = uint64(1_750_000)
)

func TestRunCountsAVMGasAndLegacyGasAsTipReward(t *testing.T) {
	tests := []struct {
		name                  string
		includeGasInReward    bool
		wantLegacyRewardCount uint64
		wantGasBurnCount      uint64
	}{
		{name: "legacy reward includes mandatory gas", includeGasInReward: true, wantLegacyRewardCount: 1},
		{name: "reward excludes mandatory gas", wantGasBurnCount: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			databasePath, address := createAVMFixture(t, test.includeGasInReward)
			var output strings.Builder
			args := auditArgs(t, databasePath, 1, 1, 1, 1, true)
			err := run(args, &output)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), string(address)) {
				t.Fatal("audit output disclosed a reward address")
			}
			var report auditReport
			if err := json.Unmarshal([]byte(output.String()), &report); err != nil {
				t.Fatalf("decode JSON report: %v", err)
			}
			if report.AVMTransactions != 1 {
				t.Fatalf("AVM transactions = %d, want 1", report.AVMTransactions)
			}
			if report.RequiredGasNAPRO != 1_000 {
				t.Fatalf("required gas = %d nAPRO, want 1000", report.RequiredGasNAPRO)
			}
			if report.ActivatedRewardsWithAVM != 1 {
				t.Fatalf("verified rewards with AVM = %d, want 1", report.ActivatedRewardsWithAVM)
			}
			if report.LegacyGasAsTipRewards != test.wantLegacyRewardCount {
				t.Fatalf("legacy gas-as-tip rewards = %d, want %d",
					report.LegacyGasAsTipRewards, test.wantLegacyRewardCount)
			}
			if report.GasExcludedFromTipsRewards != test.wantGasBurnCount {
				t.Fatalf("gas-excluded rewards = %d, want %d",
					report.GasExcludedFromTipsRewards, test.wantGasBurnCount)
			}
			if report.UnclassifiedAuthorizedRewards != 0 {
				t.Fatalf("unclassified rewards = %d, want 0", report.UnclassifiedAuthorizedRewards)
			}
		})
	}
}

func TestRunFailsClosedOnMissingInvalidOrUnclassifiedAVMReward(t *testing.T) {
	tests := []struct {
		name               string
		includeReward      bool
		tamperSignature    bool
		rewardAmountOffset uint64
		wantError          string
		wantDetails        []string
	}{
		{name: "missing reward", wantError: "exactly one valid authorized coinbase reward"},
		{name: "invalid authorization", includeReward: true, tamperSignature: true, wantError: "exactly one valid authorized coinbase reward"},
		{
			name:               "unclassified amount",
			includeReward:      true,
			rewardAmountOffset: 1,
			wantError:          "unclassified authorized reward",
			wantDetails: []string{
				"height 1",
				"got 10001008 nAPRO",
				"expected legacy gas-as-tip 10001007 nAPRO",
				"gas-excluded 10000007 nAPRO",
				"required AVM gas 1000 nAPRO",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			databasePath, address := createAVMFixtureWithOptions(t, true, test.includeReward, test.tamperSignature, test.rewardAmountOffset)
			err := runAuditWithActivations(t, databasePath, 1, 1, 1, 1)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("AVM reward audit error = %v, want substring %q", err, test.wantError)
			}
			if strings.Contains(err.Error(), string(address)) {
				t.Fatal("AVM reward diagnostic disclosed a reward address")
			}
			for _, detail := range test.wantDetails {
				if !strings.Contains(err.Error(), detail) {
					t.Fatalf("AVM reward diagnostic %q does not contain %q", err, detail)
				}
			}
		})
	}
}

func TestInspectRewardsReportsVerifiedPublicAmountsByHeight(t *testing.T) {
	databasePath, address := createAVMFixtureWithOptions(t, true, true, false, 1)
	args := auditArgs(t, databasePath, 0, 1, 1, 1, true)
	args = append(args, "--inspect-rewards")
	var output strings.Builder
	if err := run(args, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), string(address)) || strings.Contains(strings.ToLower(output.String()), "signature") {
		t.Fatal("inspection output disclosed reward address or signing data")
	}
	var report auditReport
	if err := json.Unmarshal([]byte(output.String()), &report); err != nil {
		t.Fatalf("decode inspection report: %v", err)
	}
	if len(report.InspectedRewards) != 2 {
		t.Fatalf("inspected reward records = %d, want one for each of two heights", len(report.InspectedRewards))
	}
	if report.InspectedRewards[0].Height != 0 || report.InspectedRewards[0].AuthorizedRewardAmountNAPRO != nil {
		t.Fatalf("legacy height inspection = %+v, want height 0 and no public authorization amount", report.InspectedRewards[0])
	}
	if report.InspectedRewards[1].Height != 1 || report.InspectedRewards[1].AuthorizedRewardAmountNAPRO == nil {
		t.Fatalf("authorized reward inspection = %+v, want public amount at height 1", report.InspectedRewards[1])
	}
	if got, want := *report.InspectedRewards[1].AuthorizedRewardAmountNAPRO, consensus.AuthorizedBlockRewardNAPR+1_008; got != want {
		t.Fatalf("public authorized reward amount = %d nAPRO, want %d", got, want)
	}
}

func TestInspectRewardsFailsOnInvalidAuthorizedReward(t *testing.T) {
	databasePath, address := createAVMFixtureWithOptions(t, true, true, true, 0)
	args := auditArgs(t, databasePath, 1, 1, 1, 1, true)
	args = append(args, "--inspect-rewards")
	err := run(args, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "invalid authorized reward at height 1") {
		t.Fatalf("invalid authorized reward error = %v", err)
	}
	if strings.Contains(err.Error(), string(address)) {
		t.Fatal("invalid reward diagnostic disclosed a reward address")
	}
}

func TestInspectRewardsRequiresExplicitRangeOfAtMost16Heights(t *testing.T) {
	baseArgs := []string{
		"--db", "/tmp/not-opened",
		"--expected-tip-height", "1",
		"--expected-tip-hash", strings.Repeat("a", 64),
		"--avm-activation-height", "1",
		"--reward-activation-height", "1",
		"--inspect-rewards",
	}
	for _, test := range []struct {
		name      string
		rangeArgs []string
		wantError string
	}{
		{name: "missing start", rangeArgs: []string{"--end", "1"}, wantError: "requires explicit --start and --end"},
		{name: "missing end", rangeArgs: []string{"--start", "0"}, wantError: "requires explicit --start and --end"},
		{name: "more than sixteen", rangeArgs: []string{"--start", "0", "--end", "16"}, wantError: "limited to 16 heights inclusive"},
		{name: "sixteen is allowed", rangeArgs: []string{"--start", "0", "--end", "15"}, wantError: "resolve copied database path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append(append([]string{}, baseArgs...), test.rangeArgs...)
			err := run(args, &strings.Builder{})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("inspection range error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestAuditRewardErasChecksCutoverAndLegacyGasAsTips(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	recipientKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	recipient := crypto.AddressFromKeys(crypto.MainnetByte, recipientKeys)
	genesis := makeBlock(0, crypto.Hash32{}, nil)
	preCutoverFeeTx := core.Transaction{
		Version: core.TxVersionBase,
		Inputs:  []core.RingInput{{}},
	}
	preCutoverFeeFloor := preCutoverFeeTx.MinFeeAt(core.InitialBaseFeePerByte)
	const preCutoverTip = uint64(5)
	preCutoverFeeTx.Fee = preCutoverFeeFloor + preCutoverTip
	beforeCutover := makeAuthorizedRewardBlock(t, 1, genesis.Hash(), validatorPriv, validatorPub, recipient, 300_000_000, []core.Transaction{preCutoverFeeTx})

	avmTx := core.Transaction{
		Version: core.TxVersionAVM,
		Inputs:  []core.RingInput{{}},
		AVM:     &core.AVMPayload{Action: core.AVMDeployContract, Code: []byte{0x00, 0x01, 0xff}, GasLimit: 100},
	}
	requiredFee := avmTx.MinFeeAt(core.InitialBaseFeePerByte)
	gasFee, err := core.AVMGasFee(avmTx.AVM.GasLimit)
	if err != nil {
		t.Fatal(err)
	}
	const priorityTip = uint64(7)
	avmTx.Fee = requiredFee + gasFee + priorityTip
	tips := gasFee + priorityTip
	const postCutoverBase = uint64(10_000_000)
	atCutover := makeAuthorizedRewardBlock(t, 2, beforeCutover.Hash(), validatorPriv, validatorPub, recipient, postCutoverBase+tips, []core.Transaction{avmTx})

	databasePath := createFixture(t, func(db *store.DB) error {
		for _, block := range []*core.Block{genesis, beforeCutover, atCutover} {
			if err := putBlock(db, block); err != nil {
				return err
			}
		}
		return nil
	})
	args := auditArgs(t, databasePath, 0, 0, 1, 1, false)
	args = append(args,
		"--audit-reward-eras",
		"--reward-cutover-height", "2",
		"--pre-cutover-base-napro", "300000000",
		"--post-cutover-base-napro", "10000000",
	)
	var output strings.Builder
	if err := run(args, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), string(recipient)) || strings.Contains(strings.ToLower(output.String()), "signature") {
		t.Fatal("reward-era report disclosed reward address or signing data")
	}
	if strings.Contains(output.String(), "AAH/") || strings.Contains(output.String(), `"contract_id"`) ||
		strings.Contains(output.String(), `"calldata"`) || strings.Contains(output.String(), `"access_list"`) {
		t.Fatal("reward-era report disclosed AVM contract data")
	}
	var report rewardEraAuditReport
	if err := json.Unmarshal([]byte(output.String()), &report); err != nil {
		t.Fatalf("decode reward-era report: %v", err)
	}
	if report.StartHeight != 1 || report.EndHeight != 2 {
		t.Fatalf("default reward-era range = %d..%d, want reward activation through tip (1..2)", report.StartHeight, report.EndHeight)
	}
	if report.PreCutoverBlocks != 1 || report.PostCutoverBlocks != 1 {
		t.Fatalf("cutover block counts = %d pre, %d post; want 1 each", report.PreCutoverBlocks, report.PostCutoverBlocks)
	}
	if report.FeeBearingBlocks != 2 || report.AVMTransactions != 1 {
		t.Fatalf("fee/AVM block counts = %d/%d, want 2/1", report.FeeBearingBlocks, report.AVMTransactions)
	}
	if report.PreCutoverFeeBearingBlocks != 1 || report.PostCutoverFeeBearingBlocks != 1 ||
		report.PreCutoverAVMTransactions != 0 || report.PostCutoverAVMTransactions != 1 ||
		report.PreCutoverPriorityTipsNAPRO != preCutoverTip ||
		report.PostCutoverPriorityTipsNAPRO != tips {
		t.Fatalf("per-era fee aggregates = pre(fee blocks %d, AVM %d, tips %d), post(fee blocks %d, AVM %d, tips %d)",
			report.PreCutoverFeeBearingBlocks, report.PreCutoverAVMTransactions, report.PreCutoverPriorityTipsNAPRO,
			report.PostCutoverFeeBearingBlocks, report.PostCutoverAVMTransactions, report.PostCutoverPriorityTipsNAPRO,
		)
	}
	if report.TotalTransactionFeesNAPRO != preCutoverFeeTx.Fee+avmTx.Fee ||
		report.TotalLegacyFeeFloorNAPRO != preCutoverFeeFloor+requiredFee ||
		report.TotalPriorityTipsNAPRO != preCutoverTip+tips ||
		report.TotalRequiredAVMGasNAPRO != gasFee {
		t.Fatalf("fee/gas aggregates = fees %d, floor %d, tips %d, gas %d",
			report.TotalTransactionFeesNAPRO,
			report.TotalLegacyFeeFloorNAPRO,
			report.TotalPriorityTipsNAPRO,
			report.TotalRequiredAVMGasNAPRO,
		)
	}
	if len(report.FeeBearingExamples) != 2 {
		t.Fatalf("fee-bearing examples = %d, want 2", len(report.FeeBearingExamples))
	}
	if len(report.AVMWireExamples) != 1 {
		t.Fatalf("AVM wire examples = %d, want 1", len(report.AVMWireExamples))
	}
	avmExample := report.AVMWireExamples[0]
	if avmExample.Height != 2 || avmExample.Version != core.TxVersionAVM ||
		avmExample.Action != core.AVMDeployContract || avmExample.GasLimit != 100 ||
		avmExample.CodeLength != 3 {
		t.Fatalf("AVM wire example = %+v", avmExample)
	}
	preExample := report.FeeBearingExamples[0]
	if preExample.Height != 1 || preExample.PriorityTipsNAPRO != preCutoverTip ||
		preExample.AuthorizedRewardAmountNAPRO != 300_000_000 ||
		preExample.ExpectedRewardAmountNAPRO != 300_000_000 {
		t.Fatalf("pre-cutover fee-bearing example = %+v", preExample)
	}
	example := report.FeeBearingExamples[1]
	if example.Height != 2 || example.PriorityTipsNAPRO != tips ||
		example.RequiredAVMGasNAPRO != gasFee ||
		example.AuthorizedRewardAmountNAPRO != postCutoverBase+tips ||
		example.ExpectedRewardAmountNAPRO != postCutoverBase+tips {
		t.Fatalf("fee-bearing example = %+v", example)
	}
	if strings.Contains(output.String(), "\n  \"") {
		t.Fatal("reward-era aggregate JSON should be compact")
	}
}

func TestAuditRewardErasOmitsAVMWireExamplesWhenNone(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	recipientKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	recipient := crypto.AddressFromKeys(crypto.MainnetByte, recipientKeys)
	genesis := makeBlock(0, crypto.Hash32{}, nil)
	block := makeAuthorizedRewardBlock(t, 1, genesis.Hash(), validatorPriv, validatorPub, recipient, 300_000_000, nil)
	databasePath := createFixture(t, func(db *store.DB) error {
		if err := putBlock(db, genesis); err != nil {
			return err
		}
		return putBlock(db, block)
	})
	args := auditArgs(t, databasePath, 1, 1, 1, 1, true)
	args = append(args,
		"--audit-reward-eras",
		"--reward-cutover-height", "2",
		"--pre-cutover-base-napro", "300000000",
		"--post-cutover-base-napro", "10000000",
	)
	var output strings.Builder
	if err := run(args, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `"avm_wire_examples"`) {
		t.Fatal("report included AVM wire examples when no AVM transaction existed")
	}
	var report rewardEraAuditReport
	if err := json.Unmarshal([]byte(output.String()), &report); err != nil {
		t.Fatalf("decode reward-era report: %v", err)
	}
	if len(report.AVMWireExamples) != 0 {
		t.Fatalf("AVM wire examples = %d, want none", len(report.AVMWireExamples))
	}
}

func TestAuditRewardErasBoundsAVMWireExamples(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	recipientKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	recipient := crypto.AddressFromKeys(crypto.MainnetByte, recipientKeys)
	previous := crypto.HashBytes([]byte("bounded AVM examples parent"))
	txs := make([]core.Transaction, maxRewardEraAVMWireExamples+2)
	for i := range txs {
		txs[i] = core.Transaction{
			Version: core.TxVersionAVM,
			Inputs:  []core.RingInput{{}},
			AVM:     &core.AVMPayload{Action: core.AVMExecuteContract, GasLimit: 100},
		}
	}
	block := makeAuthorizedRewardBlock(t, 2, previous, validatorPriv, validatorPub, recipient, 10_000_000, txs)
	var report rewardEraAuditReport
	if err := auditRewardEraBlock(&report, block, 2, 300_000_000, 10_000_000); err != nil {
		t.Fatal(err)
	}
	if len(report.AVMWireExamples) != maxRewardEraAVMWireExamples {
		t.Fatalf("AVM wire examples = %d, want bounded at %d", len(report.AVMWireExamples), maxRewardEraAVMWireExamples)
	}
}

func TestAuditRewardErasFailsClosedOnFirstRewardMismatch(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	recipientKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	recipient := crypto.AddressFromKeys(crypto.MainnetByte, recipientKeys)
	genesis := makeBlock(0, crypto.Hash32{}, nil)
	block := makeAuthorizedRewardBlock(t, 1, genesis.Hash(), validatorPriv, validatorPub, recipient, 300_000_001, nil)
	databasePath := createFixture(t, func(db *store.DB) error {
		if err := putBlock(db, genesis); err != nil {
			return err
		}
		return putBlock(db, block)
	})
	args := auditArgs(t, databasePath, 1, 1, 1, 1, true)
	args = append(args,
		"--audit-reward-eras",
		"--reward-cutover-height", "2",
		"--pre-cutover-base-napro", "300000000",
		"--post-cutover-base-napro", "10000000",
	)
	err = run(args, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "authorized reward mismatch at height 1") ||
		!strings.Contains(err.Error(), "got 300000001 nAPRO; expected 300000000 nAPRO") {
		t.Fatalf("reward mismatch error = %v", err)
	}
	if strings.Contains(err.Error(), string(recipient)) {
		t.Fatal("reward mismatch diagnostic disclosed a reward address")
	}
}

func TestAuditRewardErasRequiresCutoverInputs(t *testing.T) {
	required := []string{
		"--db", "/tmp/not-opened",
		"--expected-tip-height", "1",
		"--expected-tip-hash", strings.Repeat("a", 64),
		"--avm-activation-height", "1",
		"--reward-activation-height", "1",
		"--audit-reward-eras",
		"--reward-cutover-height", "2",
		"--pre-cutover-base-napro", "300000000",
		"--post-cutover-base-napro", "10000000",
	}
	for _, missing := range []string{
		"--reward-cutover-height",
		"--pre-cutover-base-napro",
		"--post-cutover-base-napro",
	} {
		t.Run(missing, func(t *testing.T) {
			args := omitFlag(append([]string(nil), required...), missing)
			if err := run(args, &strings.Builder{}); err == nil ||
				!strings.Contains(err.Error(), missing+" is required with --audit-reward-eras") {
				t.Fatalf("missing cutover input error = %v", err)
			}
		})
	}
}

func TestAuditRewardErasFailsOnFeeFloorOverflow(t *testing.T) {
	tx := core.Transaction{
		Version: core.TxVersionBase,
		Inputs:  []core.RingInput{{}},
	}
	block := makeBlock(1, crypto.HashBytes([]byte("parent")), []core.Transaction{tx})
	block.Header.BaseFee = ^uint64(0)
	var report rewardEraAuditReport
	err := auditRewardEraBlock(&report, block, 2, 300_000_000, 10_000_000)
	if err == nil || !strings.Contains(err.Error(), "minimum fee: fee floor overflows uint64") {
		t.Fatalf("minimum-fee overflow error = %v", err)
	}
}

func TestRunDefaultsToAVMActivationThroughTip(t *testing.T) {
	previous := crypto.HashBytes([]byte("activation parent"))
	block := makeBlock(testAVMActivationHeight, previous, nil)
	databasePath := createFixture(t, func(db *store.DB) error {
		if err := db.RepairHeightIndex(testAVMActivationHeight-1, previous); err != nil {
			return err
		}
		return putBlock(db, block)
	})

	var output strings.Builder
	if err := run(auditArgs(t, databasePath, 0, 0, testAVMActivationHeight, testRewardActivationHeight, false), &output); err != nil {
		t.Fatal(err)
	}
	var report auditReport
	if err := json.Unmarshal([]byte(output.String()), &report); err != nil {
		t.Fatalf("decode JSON report: %v", err)
	}
	if report.StartHeight != testAVMActivationHeight ||
		report.EndHeight != testAVMActivationHeight {
		t.Fatalf("default range = %d..%d, want %d..%d",
			report.StartHeight, report.EndHeight,
			testAVMActivationHeight, testAVMActivationHeight)
	}
}

func TestRunRequiresIndependentTipAndEffectiveActivationInputs(t *testing.T) {
	tests := []struct {
		name string
		flag string
	}{
		{name: "tip height", flag: "--expected-tip-height"},
		{name: "tip hash", flag: "--expected-tip-hash"},
		{name: "AVM activation", flag: "--avm-activation-height"},
		{name: "reward activation", flag: "--reward-activation-height"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := []string{
				"--db", "/tmp/not-opened",
				"--expected-tip-height", "1",
				"--expected-tip-hash", strings.Repeat("a", 64),
				"--avm-activation-height", "1850000",
				"--reward-activation-height", "1750000",
			}
			args = omitFlag(args, test.flag)
			if err := run(args, &strings.Builder{}); err == nil ||
				!strings.Contains(err.Error(), test.flag+" is required") {
				t.Fatalf("missing required input error = %v", err)
			}
		})
	}
}

func TestRunRejectsMalformedExpectedTipHash(t *testing.T) {
	args := []string{
		"--db", "/tmp/not-opened",
		"--expected-tip-height", "1",
		"--expected-tip-hash", "abc",
		"--avm-activation-height", "1",
		"--reward-activation-height", "1",
	}
	if err := run(args, &strings.Builder{}); err == nil ||
		!strings.Contains(err.Error(), "exactly 64 hexadecimal characters") {
		t.Fatalf("malformed expected-tip hash error = %v", err)
	}
}

func TestRunRejectsTipDifferentFromIndependentObservationBeforeScanning(t *testing.T) {
	databasePath, _ := createAVMFixture(t, true)
	args := auditArgs(t, databasePath, 0, 1, 1, 1, true)
	args = replaceFlag(args, "--expected-tip-height", "0")
	if err := run(args, &strings.Builder{}); err == nil ||
		!strings.Contains(err.Error(), "does not match independently observed API tip height") {
		t.Fatalf("tip mismatch error = %v", err)
	}

	args = auditArgs(t, databasePath, 0, 1, 1, 1, true)
	args = replaceFlag(args, "--expected-tip-hash", strings.Repeat("0", 64))
	if err := run(args, &strings.Builder{}); err == nil ||
		!strings.Contains(err.Error(), "does not match independently observed API tip hash") {
		t.Fatalf("tip hash mismatch error = %v", err)
	}
}

func TestAuditFailsClosedForMissingCanonicalIndex(t *testing.T) {
	databasePath := createFixture(t, func(db *store.DB) error {
		genesis := makeBlock(0, crypto.Hash32{}, nil)
		if err := putBlock(db, genesis); err != nil {
			return err
		}
		// Deliberately omit height 1. The height-2 block's parent hash is not
		// enough to stand in for a missing canonical height index.
		block2 := makeBlock(2, crypto.HashBytes([]byte("missing height one")), nil)
		if err := putBlock(db, block2); err != nil {
			return err
		}
		return db.PutTip(block2.Hash(), 2)
	})

	err := runAudit(t, databasePath, 1, 2)
	if err == nil || !strings.Contains(err.Error(), "height index is missing at height 1") {
		t.Fatalf("missing canonical index error = %v", err)
	}
}

func TestAuditFailsClosedForMissingCanonicalBody(t *testing.T) {
	missingHash := crypto.HashBytes([]byte("missing canonical body"))
	databasePath := createFixture(t, func(db *store.DB) error {
		genesis := makeBlock(0, crypto.Hash32{}, nil)
		block1 := makeBlock(1, genesis.Hash(), nil)
		block2 := makeBlock(2, block1.Hash(), nil)
		for _, block := range []*core.Block{genesis, block1, block2} {
			if err := putBlock(db, block); err != nil {
				return err
			}
		}
		if err := db.RepairHeightIndex(1, missingHash); err != nil {
			return err
		}
		return db.PutTip(block2.Hash(), 2)
	})

	err := runAudit(t, databasePath, 1, 2)
	if err == nil || !strings.Contains(err.Error(), "block body is missing at height 1") {
		t.Fatalf("missing canonical body error = %v", err)
	}
}

func TestAuditFailsClosedForIndexedHashMismatch(t *testing.T) {
	badIndexHash := crypto.HashBytes([]byte("wrong indexed hash"))
	databasePath := createFixture(t, func(db *store.DB) error {
		genesis := makeBlock(0, crypto.Hash32{}, nil)
		block1 := makeBlock(1, genesis.Hash(), nil)
		block2 := makeBlock(2, block1.Hash(), nil)
		for _, block := range []*core.Block{genesis, block1, block2} {
			if err := putBlock(db, block); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(block1)
		if err != nil {
			return err
		}
		if err := db.PutRawBlock(badIndexHash, 1, raw); err != nil {
			return err
		}
		return db.PutTip(block2.Hash(), 2)
	})

	err := runAudit(t, databasePath, 1, 2)
	if err == nil || !strings.Contains(err.Error(), "block hash/height mismatch at height 1") {
		t.Fatalf("indexed hash mismatch error = %v", err)
	}
}

func TestAuditRejectsTipMetadataMismatch(t *testing.T) {
	genesis := makeBlock(0, crypto.Hash32{}, nil)
	block1 := makeBlock(1, genesis.Hash(), nil)
	databasePath := createFixture(t, func(db *store.DB) error {
		if err := putBlock(db, genesis); err != nil {
			return err
		}
		if err := putBlock(db, block1); err != nil {
			return err
		}
		return db.PutTip(crypto.HashBytes([]byte("wrong tip")), 1)
	})

	args := auditArgsWithExpectedTip(t, databasePath, 0, 1, 1, block1.Hash(), 1, 1, true)
	if err := run(args, &strings.Builder{}); err == nil ||
		!strings.Contains(err.Error(), "does not match independently observed API tip hash") {
		t.Fatalf("copy/API tip mismatch error = %v", err)
	}
}

func TestAuditRejectsBodyOutsideCommittedMerkleRoot(t *testing.T) {
	genesis := makeBlock(0, crypto.Hash32{}, nil)
	block := makeBlock(1, genesis.Hash(), nil)
	databasePath := createFixture(t, func(db *store.DB) error {
		if err := putBlock(db, genesis); err != nil {
			return err
		}
		raw, err := json.Marshal(block)
		if err != nil {
			return err
		}
		var altered core.Block
		if err := json.Unmarshal(raw, &altered); err != nil {
			return err
		}
		altered.Txs = []core.Transaction{{
			Version: core.TxVersionAVM,
			Inputs:  []core.RingInput{{}},
			AVM:     &core.AVMPayload{GasLimit: 100},
		}}
		raw, err = json.Marshal(&altered)
		if err != nil {
			return err
		}
		if err := db.PutRawBlock(block.Hash(), 1, raw); err != nil {
			return err
		}
		return db.PutTip(block.Hash(), 1)
	})

	if err := runAudit(t, databasePath, 0, 1); err == nil ||
		!strings.Contains(err.Error(), "merkle root mismatch") {
		t.Fatalf("tampered body error = %v", err)
	}
}

func TestAuditFailsClosedOnLPoDPayoutBlock(t *testing.T) {
	genesis := makeBlock(0, crypto.Hash32{}, nil)
	payout := core.Transaction{Version: core.TxVersionLPoDPayout}
	block := makeBlock(1, genesis.Hash(), []core.Transaction{payout})
	databasePath := createFixture(t, func(db *store.DB) error {
		if err := putBlock(db, genesis); err != nil {
			return err
		}
		return putBlock(db, block)
	})

	err := runAuditWithActivations(t, databasePath, 1, 1, 1, 1)
	if err == nil || !strings.Contains(err.Error(), "unsupported LPoD payout block") {
		t.Fatalf("LPoD payout audit error = %v", err)
	}
}

func TestValidateCopyPathRejectsOutsideTmpAndSymlinkEscape(t *testing.T) {
	if _, err := validateCopyPath("/etc"); err == nil {
		t.Fatal("accepted a path outside /tmp")
	}
	link := filepath.Join(t.TempDir(), "outside")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	if _, err := validateCopyPath(link); err == nil {
		t.Fatal("accepted a symlink resolving outside /tmp")
	}
}

func TestRunRequiresExplicitDatabasePath(t *testing.T) {
	args := []string{
		"--expected-tip-height", "1",
		"--expected-tip-hash", strings.Repeat("a", 64),
		"--avm-activation-height", "1",
		"--reward-activation-height", "1",
	}
	if err := run(args, &strings.Builder{}); err == nil ||
		!strings.Contains(err.Error(), "--db must explicitly name") {
		t.Fatalf("missing database path error = %v", err)
	}
}

func createAVMFixture(t *testing.T, includeGasInReward bool) (string, crypto.Address) {
	return createAVMFixtureWithOptions(t, includeGasInReward, true, false, 0)
}

func createAVMFixtureWithOptions(
	t *testing.T,
	includeGasInReward bool,
	includeReward bool,
	tamperSignature bool,
	rewardAmountOffset uint64,
) (string, crypto.Address) {
	t.Helper()
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, recipient)
	genesis := makeBlock(0, crypto.Hash32{}, nil)

	avmTx := core.Transaction{
		Version: core.TxVersionAVM,
		Inputs:  []core.RingInput{{}},
		AVM:     &core.AVMPayload{GasLimit: 100},
	}
	gasFee, err := core.AVMGasFee(avmTx.AVM.GasLimit)
	if err != nil {
		t.Fatal(err)
	}
	minimumFee := avmTx.MinFeeAt(core.InitialBaseFeePerByte)
	const priorityTip = uint64(7)
	avmTx.Fee = minimumFee + gasFee + priorityTip
	legacyTips := gasFee + priorityTip
	rewardTips := legacyTips
	if !includeGasInReward {
		rewardTips -= gasFee
	}
	txs := []core.Transaction{avmTx}
	if includeReward {
		rewardAmount := consensus.AuthorizedBlockRewardNAPR + rewardTips + rewardAmountOffset
		rewardTx, err := core.BuildAuthorizedRewardTx(
			address,
			rewardAmount,
			1,
			genesis.Hash(),
			validatorPriv,
		)
		if err != nil {
			t.Fatal(err)
		}
		if tamperSignature {
			rewardTx.Extra[len(rewardTx.Extra)-1] ^= 1
		}
		txs = append([]core.Transaction{*rewardTx}, txs...)
	}
	block := &core.Block{
		Header: core.BlockHeader{
			Height:       1,
			Timestamp:    2,
			PrevHash:     genesis.Hash(),
			ValidatorPub: validatorPub,
			BaseFee:      core.InitialBaseFeePerByte,
		},
		Txs: txs,
	}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	return createFixture(t, func(db *store.DB) error {
		if err := putBlock(db, genesis); err != nil {
			return err
		}
		if err := putBlock(db, block); err != nil {
			return err
		}
		return db.PutTip(block.Hash(), 1)
	}), address
}

func createFixture(t *testing.T, populate func(*store.DB) error) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "avm-fee-audit-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	databasePath := filepath.Join(root, "chain.db")
	db, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := populate(db); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return databasePath
}

func putBlock(db *store.DB, block *core.Block) error {
	raw, err := json.Marshal(block)
	if err != nil {
		return err
	}
	if err := db.PutRawBlock(block.Hash(), block.Header.Height, raw); err != nil {
		return err
	}
	return db.PutTip(block.Hash(), block.Header.Height)
}

func makeBlock(height uint64, previous crypto.Hash32, txs []core.Transaction) *core.Block {
	block := &core.Block{
		Header: core.BlockHeader{
			Height:    height,
			Timestamp: int64(height + 1),
			PrevHash:  previous,
			BaseFee:   core.InitialBaseFeePerByte,
		},
		Txs: txs,
	}
	block.Header.MerkleRoot = core.MerkleRoot(txs)
	return block
}

func makeAuthorizedRewardBlock(
	t *testing.T,
	height uint64,
	previous crypto.Hash32,
	validatorPriv crypto.ValidatorPrivKey,
	validatorPub crypto.ValidatorPubKey,
	recipient crypto.Address,
	amount uint64,
	transactions []core.Transaction,
) *core.Block {
	t.Helper()
	rewardTx, err := core.BuildAuthorizedRewardTx(recipient, amount, height, previous, validatorPriv)
	if err != nil {
		t.Fatal(err)
	}
	txs := append([]core.Transaction{*rewardTx}, transactions...)
	block := &core.Block{
		Header: core.BlockHeader{
			Height:       height,
			Timestamp:    int64(height + 1),
			PrevHash:     previous,
			ValidatorPub: validatorPub,
			BaseFee:      core.InitialBaseFeePerByte,
		},
		Txs: txs,
	}
	block.Header.MerkleRoot = core.MerkleRoot(txs)
	return block
}

func runAudit(t *testing.T, databasePath string, start, end uint64) error {
	t.Helper()
	return runAuditWithActivations(t, databasePath, start, end, testAVMActivationHeight, testRewardActivationHeight)
}

func runAuditWithActivations(
	t *testing.T,
	databasePath string,
	start, end, avmActivation, rewardActivation uint64,
) error {
	t.Helper()
	args := auditArgs(t, databasePath, start, end, avmActivation, rewardActivation, true)
	return run(args, &strings.Builder{})
}

func auditArgs(
	t *testing.T,
	databasePath string,
	start, end, avmActivation, rewardActivation uint64,
	includeRange bool,
) []string {
	t.Helper()
	db, err := store.OpenReadOnly(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	tipHash, tipHeight, err := db.GetTip()
	if closeErr := db.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--db", databasePath,
		"--expected-tip-height", stringUint(tipHeight),
		"--expected-tip-hash", hex.EncodeToString(tipHash[:]),
		"--avm-activation-height", stringUint(avmActivation),
		"--reward-activation-height", stringUint(rewardActivation),
	}
	if includeRange {
		args = append(args, "--start", stringUint(start), "--end", stringUint(end))
	}
	return args
}

func auditArgsWithExpectedTip(
	t *testing.T,
	databasePath string,
	start, end, expectedHeight uint64,
	expectedHash crypto.Hash32,
	avmActivation, rewardActivation uint64,
	includeRange bool,
) []string {
	t.Helper()
	args := auditArgs(t, databasePath, start, end, avmActivation, rewardActivation, includeRange)
	args = replaceFlag(args, "--expected-tip-height", stringUint(expectedHeight))
	return replaceFlag(args, "--expected-tip-hash", hex.EncodeToString(expectedHash[:]))
}

func replaceFlag(args []string, name, value string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			args[i+1] = value
			return args
		}
	}
	return append(args, name, value)
}

func omitFlag(args []string, name string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return append(args[:i], args[i+2:]...)
		}
	}
	return args
}

func stringUint(value uint64) string {
	return strconv.FormatUint(value, 10)
}
