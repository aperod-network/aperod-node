package consensus

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestStrictCoinbasePolicyDisablesLocalStateDependentMinting(t *testing.T) {
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
		RewardAddress:              string(rewardAddress),
		BlockRewardNAPR:            reward,
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
	if err := engine.validateCoinbasePolicy(valid); err != nil {
		t.Fatalf("mint-free post-activation block rejected: %v", err)
	}

	unauthorized, err := core.BuildMintTx(rewardAddress, reward+1, activation)
	if err != nil {
		t.Fatal(err)
	}
	wrong := &core.Block{
		Header: valid.Header,
		Txs:    []core.Transaction{*unauthorized},
	}
	if err := engine.validateCoinbasePolicy(wrong); err == nil {
		t.Fatal("zero-input mint was accepted after activation")
	}

	extra := &core.Block{
		Header: valid.Header,
		Txs:    []core.Transaction{*expected, *unauthorized},
	}
	if err := engine.validateCoinbasePolicy(extra); err == nil {
		t.Fatal("additional zero-input mint was accepted after activation")
	}

	// Historical replay below the activation height retains the previous
	// structural policy.
	historical := &core.Block{
		Header: core.BlockHeader{Height: activation - 1},
		Txs:    []core.Transaction{*expected, *unauthorized},
	}
	if err := engine.validateCoinbasePolicy(historical); err != nil {
		t.Fatalf("pre-activation historical block was rejected: %v", err)
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