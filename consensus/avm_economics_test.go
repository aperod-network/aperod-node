package consensus

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestValidateBlockEconomicsRequiresAVMGasPayment(t *testing.T) {
	engine := NewEngine(
		Config{RingCTV4ActivationHeight: 1},
		core.NewChain(10),
		core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	tx := core.Transaction{
		Version: core.TxVersionAVM,
		AVM:     &core.AVMPayload{GasLimit: 1_000},
		Inputs:  []core.RingInput{{}},
	}
	baseFee := core.InitialBaseFeePerByte
	tx.Fee = tx.MinFeeAt(baseFee)
	block := &core.Block{
		Header: core.BlockHeader{Height: 1, BaseFee: baseFee},
		Txs:    []core.Transaction{tx},
	}
	if err := engine.validateBlockEconomics(block); err == nil || !strings.Contains(err.Error(), "fee below") {
		t.Fatalf("underpaid AVM gas accepted: %v", err)
	}
	gasFee, err := core.AVMGasFee(tx.AVM.GasLimit)
	if err != nil {
		t.Fatalf("AVMGasFee: %v", err)
	}
	block.Txs[0].Fee += gasFee
	if err := engine.validateBlockEconomics(block); err != nil {
		t.Fatalf("fully paid AVM gas rejected: %v", err)
	}
}

func TestValidateBlockEconomicsEnforcesAVMBlockGasLimit(t *testing.T) {
	engine := NewEngine(
		Config{RingCTV4ActivationHeight: 1},
		core.NewChain(10),
		core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	baseFee := core.InitialBaseFeePerByte
	first := core.Transaction{
		Version: core.TxVersionAVM,
		AVM:     &core.AVMPayload{GasLimit: 6_000_000},
		Inputs:  []core.RingInput{{}},
	}
	second := core.Transaction{
		Version: core.TxVersionAVM,
		AVM:     &core.AVMPayload{GasLimit: 6_000_000},
		Inputs:  []core.RingInput{{}},
	}
	for _, tx := range []*core.Transaction{&first, &second} {
		gasFee, err := core.AVMGasFee(tx.AVM.GasLimit)
		if err != nil {
			t.Fatalf("AVMGasFee: %v", err)
		}
		tx.Fee = tx.MinFeeAt(baseFee) + gasFee
	}
	block := &core.Block{
		Header: core.BlockHeader{Height: 1, BaseFee: baseFee},
		Txs:    []core.Transaction{first, second},
	}
	if err := engine.validateBlockEconomics(block); err == nil || !strings.Contains(err.Error(), "block gas limit") {
		t.Fatalf("over-gas block accepted: %v", err)
	}
}

func TestAVMGasBurnAccountingActivatesAtConfiguredHeight(t *testing.T) {
	const (
		activationHeight = uint64(100)
		gasLimit         = uint64(100)
		explicitBurn     = uint64(50)
		tip              = uint64(7)
	)
	baseFee := core.InitialBaseFeePerByte
	tx := core.Transaction{
		Version: core.TxVersionAVM,
		AVM:     &core.AVMPayload{GasLimit: gasLimit},
		Inputs:  []core.RingInput{{}},
		Extra:   core.IntentionalBurnExtra(explicitBurn),
	}
	minimumFee := tx.MinFeeAt(baseFee)
	gasFee, err := core.AVMGasFee(gasLimit)
	if err != nil {
		t.Fatalf("AVMGasFee: %v", err)
	}
	tx.Fee = minimumFee + gasFee + explicitBurn + tip
	engine := NewEngine(
		Config{AVMGasBurnActivationHeight: activationHeight},
		core.NewChain(),
		core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	beforeBurned, beforeTips := engine.blockFeeStatsAtHeight(
		[]core.Transaction{tx}, baseFee, activationHeight-1,
	)
	if want := minimumFee + explicitBurn; beforeBurned != want {
		t.Fatalf("pre-activation burned = %d, want %d", beforeBurned, want)
	}
	if want := gasFee + tip; beforeTips != want {
		t.Fatalf("pre-activation tips = %d, want %d", beforeTips, want)
	}

	atBurned, atTips := engine.blockFeeStatsAtHeight(
		[]core.Transaction{tx}, baseFee, activationHeight,
	)
	if want := minimumFee + explicitBurn + gasFee; atBurned != want {
		t.Fatalf("activation-height burned = %d, want %d", atBurned, want)
	}
	if atTips != tip {
		t.Fatalf("activation-height tips = %d, want %d", atTips, tip)
	}

	tx.Extra = nil
	plainMinimumFee := tx.MinFeeAt(baseFee)
	tx.Fee = plainMinimumFee + gasFee
	exactGasBurned, exactGasTips := engine.blockFeeStatsAtHeight(
		[]core.Transaction{tx}, baseFee, activationHeight,
	)
	if want := plainMinimumFee + gasFee; exactGasBurned != want {
		t.Fatalf("gas-only activation burn = %d, want %d", exactGasBurned, want)
	}
	if exactGasTips != 0 {
		t.Fatalf("gas-only activation tips = %d, want 0", exactGasTips)
	}
}

func TestAuthorizedRewardAVMGasBurnBoundaryAndRestart(t *testing.T) {
	const (
		avmActivation    = uint64(80)
		rewardActivation = uint64(50)
		gasActivation    = uint64(101)
		gasLimit         = uint64(100)
	)
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	rewardAddress := crypto.AddressFromKeys(crypto.MainnetByte, recipient)
	parentHash := crypto.HashBytes([]byte("avm-gas-burn-parent"))
	newEngine := func() *Engine {
		return NewEngine(
			Config{
				Validators:                          []crypto.ValidatorPubKey{validatorPub},
				RewardAddress:                       string(rewardAddress),
				AVMActivationHeight:                 avmActivation,
				AVMGasBurnActivationHeight:          gasActivation,
				RewardAuthorizationActivationHeight: rewardActivation,
			},
			core.NewChain(),
			core.NewMempool(core.DefaultMempoolConfig()),
			slog.New(slog.NewTextHandler(io.Discard, nil)),
		)
	}

	engine := newEngine()
	avmTx := core.Transaction{
		Version: core.TxVersionAVM,
		AVM:     &core.AVMPayload{GasLimit: gasLimit},
		Inputs:  []core.RingInput{{}},
	}
	baseFee := engine.expectedBaseFeeAt(gasActivation)
	minimumFee := avmTx.MinFeeAt(baseFee)
	gasFee, err := core.AVMGasFee(gasLimit)
	if err != nil {
		t.Fatalf("AVMGasFee: %v", err)
	}
	avmTx.Fee = minimumFee + gasFee

	buildBlock := func(height, rewardAmount uint64) *core.Block {
		t.Helper()
		reward, buildErr := core.BuildAuthorizedRewardTx(
			rewardAddress,
			rewardAmount,
			height,
			parentHash,
			validatorPriv,
		)
		if buildErr != nil {
			t.Fatalf("build authorized reward at %d: %v", height, buildErr)
		}
		return &core.Block{
			Header: core.BlockHeader{
				Height:       height,
				PrevHash:     parentHash,
				ValidatorPub: validatorPub,
				BaseFee:      baseFee,
			},
			Txs: []core.Transaction{*reward, avmTx},
		}
	}

	preActivationBase := blockRewardAtHeight(AuthorizedBlockRewardNAPR, gasActivation-1)
	preActivationReward, err := engine.authorizedRewardAmount(gasActivation-1, []core.Transaction{avmTx})
	if err != nil {
		t.Fatalf("pre-activation reward: %v", err)
	}
	if want := preActivationBase + gasFee; preActivationReward != want {
		t.Fatalf("pre-activation authorized reward = %d, want legacy gas tip total %d",
			preActivationReward, want)
	}
	preActivationBlock := buildBlock(gasActivation-1, preActivationReward)
	if err := engine.validateCoinbasePolicy(preActivationBlock); err != nil {
		t.Fatalf("pre-activation legacy gas-as-tip block rejected: %v", err)
	}

	postActivationBase := blockRewardAtHeight(AuthorizedBlockRewardNAPR, gasActivation)
	postActivationReward, err := engine.authorizedRewardAmount(gasActivation, []core.Transaction{avmTx})
	if err != nil {
		t.Fatalf("post-activation reward: %v", err)
	}
	if postActivationReward != postActivationBase {
		t.Fatalf("post-activation gas-only reward = %d, want base %d",
			postActivationReward, postActivationBase)
	}
	oversizedLegacyBlock := buildBlock(gasActivation, postActivationBase+gasFee)
	if err := engine.validateCoinbasePolicy(oversizedLegacyBlock); err == nil {
		t.Fatal("pre-activation gas-as-tip reward accepted at AVM gas-burn activation")
	}
	postActivationBlock := buildBlock(gasActivation, postActivationReward)
	if err := engine.validateCoinbasePolicy(postActivationBlock); err != nil {
		t.Fatalf("gas-burn activation reward rejected: %v", err)
	}

	restarted := newEngine()
	restartedReward, err := restarted.expectedAuthorizedRewardAmount(postActivationBlock)
	if err != nil {
		t.Fatalf("restarted validator expected reward: %v", err)
	}
	if restartedReward != postActivationReward {
		t.Fatalf("restarted validator reward = %d, want %d", restartedReward, postActivationReward)
	}
	if err := restarted.validateCoinbasePolicy(postActivationBlock); err != nil {
		t.Fatalf("restarted validator rejected activation block: %v", err)
	}
}
