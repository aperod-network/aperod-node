package consensus

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestRingCTActivationDoesNotDisableValidatorRewards(t *testing.T) {
	const (
		activation = uint64(100)
		reward     = uint64(10_000_000)
	)

	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	rewardAddress := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	engine := NewEngine(Config{
		RewardAddress:            string(rewardAddress),
		BlockRewardNAPR:          reward,
		RingCTV4ActivationHeight: activation,
	}, core.NewChain(), core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	expected, err := core.BuildMintTx(rewardAddress, reward, activation)
	if err != nil {
		t.Fatal(err)
	}
	valid := &core.Block{
		Header: core.BlockHeader{
			Height:  activation,
			BaseFee: core.InitialBaseFeePerByte,
		},
	}
	rewardBlock := &core.Block{
		Header: valid.Header,
		Txs:    []core.Transaction{*expected},
	}
	if err := engine.validateCoinbasePolicy(rewardBlock); err != nil {
		t.Fatalf("configured validator reward rejected at RingCT activation: %v", err)
	}

	extraMint, err := core.BuildMintTx(rewardAddress, reward+1, activation)
	if err != nil {
		t.Fatal(err)
	}
	extra := &core.Block{
		Header: valid.Header,
		Txs:    []core.Transaction{*expected, *extraMint},
	}
	if err := engine.validateCoinbasePolicy(extra); err != nil {
		t.Fatalf("structurally valid pre-authorization coinbase prefix rejected: %v", err)
	}

	// Historical replay below the activation height retains the previous
	// structural policy.
	historical := &core.Block{
		Header: core.BlockHeader{Height: activation - 1},
		Txs:    []core.Transaction{*expected, *extraMint},
	}
	if err := engine.validateCoinbasePolicy(historical); err != nil {
		t.Fatalf("pre-activation historical block was rejected: %v", err)
	}
}

func TestAuthorizedValidatorRewardActivationAndConsensusAmount(t *testing.T) {
	const activation = uint64(100)
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	rewardAddress := crypto.AddressFromKeys(crypto.MainnetByte, recipient)
	parentHash := crypto.HashBytes([]byte("reward-activation-parent"))

	newEngine := func() *Engine {
		return NewEngine(Config{
			Validators:                          []crypto.ValidatorPubKey{validatorPub},
			BlockRewardNAPR:                     DefaultPoolBlockRewardNAPR,
			StakingPoolNAPR:                     2 * DefaultPoolBlockRewardNAPR,
			TailRewardNAPR:                      defaultTailRewardNAPR,
			RingCTV4ActivationHeight:            50,
			RewardAuthorizationActivationHeight: activation,
		}, core.NewChain(), core.NewMempool(core.DefaultMempoolConfig()),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	engine := newEngine()

	authorized, err := core.BuildAuthorizedRewardTx(
		rewardAddress,
		AuthorizedBlockRewardNAPR,
		activation,
		parentHash,
		validatorPriv,
	)
	if err != nil {
		t.Fatal(err)
	}
	valid := &core.Block{
		Header: core.BlockHeader{
			Height:       activation,
			PrevHash:     parentHash,
			ValidatorPub: validatorPub,
			BaseFee:      core.InitialBaseFeePerByte,
		},
		Txs: []core.Transaction{*authorized},
	}
	if err := engine.validateCoinbasePolicy(valid); err != nil {
		t.Fatalf("valid authorized reward rejected: %v", err)
	}

	// A restarted validator reconstructs the complete authorization decision
	// from the block itself; no replay map or local reward settings are needed.
	restarted := newEngine()
	if err := restarted.validateCoinbasePolicy(valid); err != nil {
		t.Fatalf("valid authorized reward rejected after restart: %v", err)
	}

	wrongAmount, err := core.BuildAuthorizedRewardTx(
		rewardAddress,
		AuthorizedBlockRewardNAPR+1,
		activation,
		parentHash,
		validatorPriv,
	)
	if err != nil {
		t.Fatal(err)
	}
	wrongAmountBlock := *valid
	wrongAmountBlock.Txs = []core.Transaction{*wrongAmount}
	if err := engine.validateCoinbasePolicy(&wrongAmountBlock); err == nil {
		t.Fatal("proposer-signed reward above the consensus amount was accepted")
	}

	missing := *valid
	missing.Txs = nil
	if err := engine.validateCoinbasePolicy(&missing); err == nil {
		t.Fatal("activated block without its authorized reward was accepted")
	}

	duplicate := *valid
	duplicate.Txs = []core.Transaction{*authorized, *authorized}
	if err := engine.validateCoinbasePolicy(&duplicate); err == nil {
		t.Fatal("duplicate authorized rewards were accepted")
	}

	// Before authorization activation, the existing structural coinbase policy
	// remains active even though RingCT v4 has already activated.
	preAuth := &core.Block{
		Header: core.BlockHeader{
			Height:       activation - 1,
			PrevHash:     parentHash,
			ValidatorPub: validatorPub,
			BaseFee:      core.InitialBaseFeePerByte,
		},
	}
	legacyReward, err := core.BuildMintTx(
		rewardAddress,
		DefaultPoolBlockRewardNAPR,
		activation-1,
	)
	if err != nil {
		t.Fatal(err)
	}
	preAuth.Txs = []core.Transaction{*legacyReward}
	if err := engine.validateCoinbasePolicy(preAuth); err != nil {
		t.Fatalf("pre-authorization pool reward rejected after RingCT activation: %v", err)
	}
}

func TestHandleIncomingBlockRejectsUnauthorizedExtraCoinbase(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	rewardAddress := crypto.AddressFromKeys(crypto.MainnetByte, recipient)

	genesisHeader := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().Add(-time.Second).UnixNano(),
		ValidatorPub: validatorPub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := genesisHeader.Sign(validatorPriv); err != nil {
		t.Fatal(err)
	}
	genesis := &core.Block{Header: genesisHeader}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	utxos := core.NewUTXOSet()
	engine := NewEngine(Config{
		Validators:                          []crypto.ValidatorPubKey{validatorPub},
		RingCTV4ActivationHeight:            1,
		RewardAuthorizationActivationHeight: 1,
	}, chain, core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	authorizedReward, err := core.BuildAuthorizedRewardTx(
		rewardAddress,
		AuthorizedBlockRewardNAPR,
		1,
		genesis.Hash(),
		validatorPriv,
	)
	if err != nil {
		t.Fatal(err)
	}
	unauthorizedMint, err := core.BuildMintTx(rewardAddress, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	block := &core.Block{
		Header: core.BlockHeader{
			Height:       1,
			PrevHash:     genesis.Hash(),
			Timestamp:    time.Now().UnixNano(),
			Round:        1,
			ValidatorPub: validatorPub,
			BaseFee:      core.InitialBaseFeePerByte,
		},
		Txs: []core.Transaction{*authorizedReward, *unauthorizedMint},
	}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	if err := block.Header.Sign(validatorPriv); err != nil {
		t.Fatal(err)
	}

	if err := engine.handleIncomingBlock(block); err == nil {
		t.Fatal("P2P ingress accepted a block with an unauthorized extra coinbase")
	}
	if tip := chain.Tip(); tip == nil || tip.Header.Height != 0 {
		t.Fatalf("rejected block changed canonical tip: %+v", tip)
	}
	if got := len(utxos.All()); got != 0 {
		t.Fatalf("rejected block changed UTXO state: got %d entries", got)
	}
}

func TestLocalProductionBuildsAuthorizedValidatorReward(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Destroy()
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	rewardAddress := crypto.AddressFromKeys(crypto.MainnetByte, recipient)

	genesisHeader := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := genesisHeader.Sign(priv); err != nil {
		t.Fatal(err)
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(&core.Block{Header: genesisHeader}); err != nil {
		t.Fatal(err)
	}
	utxos := core.NewUTXOSet()
	engine := NewEngine(Config{
		Validators:                          []crypto.ValidatorPubKey{pub},
		MyKey:                               locked,
		RewardAddress:                       string(rewardAddress),
		BlockRewardNAPR:                     DefaultPoolBlockRewardNAPR,
		StakingPoolNAPR:                     2 * DefaultPoolBlockRewardNAPR,
		TailRewardNAPR:                      defaultTailRewardNAPR,
		RingCTV4ActivationHeight:            1,
		RewardAuthorizationActivationHeight: 1,
	}, chain, core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	if err := engine.tick(); err != nil {
		t.Fatalf("produce authorized reward block: %v", err)
	}
	block := chain.GetByHeight(1)
	if block == nil {
		t.Fatal("produced block missing")
	}
	if len(block.Txs) != 1 {
		t.Fatalf("produced block tx count = %d, want exactly one authorized reward", len(block.Txs))
	}
	auth, err := core.ValidateAuthorizedRewardTx(
		&block.Txs[0],
		block.Header.Height,
		block.Header.PrevHash,
		block.Header.ValidatorPub,
	)
	if err != nil {
		t.Fatalf("produced reward is not consensus-valid: %v", err)
	}
	if auth.Amount != DefaultPoolBlockRewardNAPR {
		t.Fatalf("authorized reward = %d, want pool reward %d", auth.Amount, DefaultPoolBlockRewardNAPR)
	}
	engine.DecrementPool(block.Header.Height)
	if got, want := engine.StakingPoolRemaining(), DefaultPoolBlockRewardNAPR; got != want {
		t.Fatalf("pool remaining after authorized reward = %d, want %d", got, want)
	}
}

func TestStrictFeePolicyUsesConsensusRate(t *testing.T) {
	const activation = uint64(100)
	engine := NewEngine(Config{
		RingCTV4ActivationHeight: activation,
	}, core.NewChain(), core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	tx := core.Transaction{
		Version: core.TxVersionCommitmentBinding,
		Inputs:  []core.RingInput{{}},
		Outputs: []core.Output{{}},
	}
	tx.Fee = tx.MinFeeAt(core.InitialBaseFeePerByte)
	valid := &core.Block{
		Header: core.BlockHeader{
			Height:  activation,
			BaseFee: core.InitialBaseFeePerByte,
		},
		Txs: []core.Transaction{tx},
	}
	if err := engine.validateBlockEconomics(valid); err != nil {
		t.Fatalf("minimum-fee transaction rejected: %v", err)
	}

	underpaid := *valid
	underpaid.Txs = append([]core.Transaction(nil), valid.Txs...)
	underpaid.Txs[0].Fee--
	if err := engine.validateBlockEconomics(&underpaid); err == nil {
		t.Fatal("transaction below the consensus fee floor was accepted")
	}

	wrongRate := *valid
	wrongRate.Header.BaseFee++
	if err := engine.validateBlockEconomics(&wrongRate); err == nil {
		t.Fatal("block with a non-consensus base fee was accepted")
	}

	// Zero remains valid only below the activation boundary.  Accepting it in
	// new blocks would make restart reconstruction depend on local memory.
	legacyEncoding := *valid
	legacyEncoding.Header.BaseFee = 0
	if err := engine.validateBlockEconomics(&legacyEncoding); err == nil {
		t.Fatal("zero base-fee encoding was accepted after activation")
	}

	preActivation := underpaid
	preActivation.Header.Height = activation - 1
	preActivation.Header.BaseFee = 0
	if err := engine.validateBlockEconomics(&preActivation); err != nil {
		t.Fatalf("pre-activation historical transaction rejected: %v", err)
	}
}

func TestActivationFeeAnchorSurvivesRestart(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	genesisHeader := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := genesisHeader.Sign(priv); err != nil {
		t.Fatal(err)
	}
	genesis := &core.Block{Header: genesisHeader}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}

	legacyHeader := core.BlockHeader{
		Height:       1,
		PrevHash:     genesis.Hash(),
		Timestamp:    genesisHeader.Timestamp + 1,
		Round:        1,
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
		BaseFee:      0,
	}
	if err := legacyHeader.Sign(priv); err != nil {
		t.Fatal(err)
	}
	if err := chain.AddBlock(&core.Block{Header: legacyHeader}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(Config{
		RingCTV4ActivationHeight: 2,
	}, chain, core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := engine.expectedBaseFeeAt(2); got != core.InitialBaseFeePerByte {
		t.Fatalf("activation fee after restart = %d, want %d", got, core.InitialBaseFeePerByte)
	}
	if err := engine.validateBlockEconomics(&core.Block{
		Header: core.BlockHeader{Height: 2, BaseFee: core.InitialBaseFeePerByte},
	}); err != nil {
		t.Fatalf("first strict block rejected after restart: %v", err)
	}
}

func TestHaltedEngineRejectsFurtherConsensusWork(t *testing.T) {
	engine := NewEngine(Config{}, core.NewChain(),
		core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.halted.Store(true)
	if err := engine.tick(); err == nil {
		t.Fatal("halted engine attempted local production")
	}
	if err := engine.handleIncomingBlock(&core.Block{}); err == nil {
		t.Fatal("halted engine attempted incoming block processing")
	}
}

func TestConsecutiveLocalBlocksAdvanceBaseFee(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Destroy()

	genesisHeader := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := genesisHeader.Sign(priv); err != nil {
		t.Fatal(err)
	}
	genesis := &core.Block{Header: genesisHeader}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	utxos := core.NewUTXOSet()
	engine := NewEngine(Config{
		Validators:               []crypto.ValidatorPubKey{pub},
		MyKey:                    locked,
		RingCTV4ActivationHeight: 1,
	}, chain, core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	if err := engine.tick(); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	first := chain.GetByHeight(1)
	if first == nil {
		t.Fatal("first locally produced block missing")
	}
	wantSecondFee := nextBaseFee(first.Header.BaseFee, first.Size())

	if err := engine.tick(); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	second := chain.GetByHeight(2)
	if second == nil {
		t.Fatal("second locally produced block missing")
	}
	if second.Header.BaseFee != wantSecondFee {
		t.Fatalf("second block base fee = %d, want %d", second.Header.BaseFee, wantSecondFee)
	}
}
