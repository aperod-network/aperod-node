package consensus

import (
	"io"
	"log/slog"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func finalityTestChain(t *testing.T, marker string) (*core.Chain, *core.Block) {
	t.Helper()
	chain := core.NewChain()
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, Timestamp: 1}}
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	block := &core.Block{Header: core.BlockHeader{Height: 1, PrevHash: genesis.Hash(), Timestamp: 2,
		Round: uint32(len(marker))}}
	if err := chain.AddBlock(block); err != nil {
		t.Fatal(err)
	}
	return chain, block
}

func TestFinalityCertificateSurvivesRestartAndRejectsWrongFork(t *testing.T) {
	var privs []crypto.ValidatorPrivKey
	var pubs []crypto.ValidatorPubKey
	for range 3 {
		priv, pub, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
		privs, pubs = append(privs, priv), append(pubs, pub)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	chain, block := finalityTestChain(t, "canonical")
	registry := core.NewValidatorRegistry()
	registry.InitFromGenesis(pubs, core.MinStakeNAPR*10)
	cfg := Config{Validators: pubs, Registry: registry, Store: db, BFTThreshold: 0.667}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), block.Hash().Bytes())
	for i := range privs {
		signature, err := privs[i].Sign(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.HandleVote(FinalizeMsg{BlockHash: block.Hash(), Height: 1,
			ValidatorPub: pubs[i], Signature: signature}); err != nil {
			t.Fatal(err)
		}
	}
	if !engine.IsFinalizedHash(1, block.Hash()) {
		t.Fatal("live quorum did not finalize block")
	}
	restarted := NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	if !restarted.IsFinalizedHash(1, block.Hash()) || !restarted.IsFinalized(1) {
		t.Fatal("authentic tip certificate was not restored")
	}

	otherChain, otherBlock := finalityTestChain(t, "other-fork")
	wrongFork := NewEngine(cfg, otherChain, core.NewMempool(core.DefaultMempoolConfig()), log)
	if wrongFork.IsFinalizedHash(1, otherBlock.Hash()) || wrongFork.IsFinalized(1) {
		t.Fatal("certificate from another fork was accepted")
	}
	if !wrongFork.halted.Load() {
		t.Fatal("node did not halt on a durable certificate bound to another fork")
	}
}

func TestForgedFinalityCertificateFailsClosed(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	_, attacker, _ := crypto.GenerateValidatorKey()
	chain, block := finalityTestChain(t, "forged")
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	badSignature, _ := priv.Sign(crypto.HashBytes([]byte("not-finality")))
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{Version: 1, Height: 1, BlockHash: block.Hash(),
		Votes: []store.FinalityVote{{Validator: attacker, Signature: badSignature}}}); err != nil {
		t.Fatal(err)
	}
	registry := core.NewValidatorRegistry()
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)
	engine := NewEngine(Config{Validators: []crypto.ValidatorPubKey{pub}, Registry: registry, Store: db, BFTThreshold: 0.667},
		chain, core.NewMempool(core.DefaultMempoolConfig()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if engine.IsFinalized(1) || engine.IsFinalizedHash(1, block.Hash()) {
		t.Fatal("forged certificate restored finality")
	}
	if !engine.halted.Load() {
		t.Fatal("node did not halt on forged durable finality evidence")
	}
}
