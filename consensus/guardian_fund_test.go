package consensus

import (
	"io"
	"log/slog"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func guardianConsensusFixture(t *testing.T, active uint64) (*Engine, crypto.Hash32) {
	t.Helper()
	chain := core.NewChain()
	genesis := &core.Block{Header: core.BlockHeader{Height: 0}}
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	anchor := genesis.Hash()
	engine := NewEngine(Config{
		GuardianFundActivationHeight: active,
		GuardianFundChainAnchor:      anchor,
	}, chain, core.NewMempool(core.DefaultMempoolConfig()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return engine, anchor
}

func TestGuardianFundConsensusPlacementAndHeight(t *testing.T) {
	engine, anchor := guardianConsensusFixture(t, 2)
	guardian, err := core.BuildGuardianFundTx(anchor, 2)
	if err != nil {
		t.Fatal(err)
	}
	coinbase := core.CoinbaseTx(crypto.Point32{}, 1)
	tests := []struct {
		name   string
		height uint64
		txs    []core.Transaction
	}{
		{"early", 1, []core.Transaction{guardian}},
		{"late", 3, []core.Transaction{guardian}},
		{"missing", 2, nil},
		{"duplicate", 2, []core.Transaction{guardian, guardian}},
		{"wrong_position", 2, []core.Transaction{coinbase, core.Transaction{Version: core.TxVersionBase}, guardian}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := engine.validateGuardianFund(&core.Block{
				Header: core.BlockHeader{Height: tc.height},
				Txs:    tc.txs,
			}); err == nil {
				t.Fatal("invalid Guardian block accepted")
			}
		})
	}
	if err := engine.validateGuardianFund(&core.Block{
		Header: core.BlockHeader{Height: 2},
		Txs:    []core.Transaction{coinbase, guardian},
	}); err != nil {
		t.Fatalf("canonical Guardian block rejected: %v", err)
	}
}

func TestGuardianFundConsensusRejectsWrongGenesis(t *testing.T) {
	engine, _ := guardianConsensusFixture(t, 2)
	engine.cfg.GuardianFundChainAnchor = crypto.HashBytes([]byte("wrong-genesis"))
	if err := engine.validateGuardianFund(&core.Block{Header: core.BlockHeader{Height: 2}}); err == nil {
		t.Fatal("wrong genesis anchor accepted")
	}
}

func TestGuardianFundProducerAtHeightGreaterThanOne(t *testing.T) {
	engine, anchor := guardianConsensusFixture(t, 3)
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	engine.cfg.MyKey = locked
	engine.cfg.Validators = []crypto.ValidatorPubKey{pub}
	parent := &core.Block{Header: core.BlockHeader{Height: 2}}
	block, err := engine.produceBlock(3, 3, parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(block.Txs) != 1 || !core.GuardianFundTxAt(&block.Txs[0], anchor, 3) {
		t.Fatalf("producer omitted canonical Guardian transaction: %#v", block.Txs)
	}
}