package consensus

import (
	"encoding/json"
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
	if err := restarted.requireLPoDParentFinalized(1, block.Hash()); err != nil {
		t.Fatalf("genuine restored quorum certificate did not authorize parent finality: %v", err)
	}
	if err := restarted.requireLPoDParentFinalized(1, crypto.HashBytes([]byte("same height, different hash"))); err == nil {
		t.Fatal("finality was inferred from parent height without its certified hash")
	}
	if err := restarted.requireLPoDParentFinalized(2, block.Hash()); err == nil {
		t.Fatal("finality was inferred from parent hash under a different height")
	}

	otherChain, otherBlock := finalityTestChain(t, "other-fork")
	wrongFork := NewEngine(cfg, otherChain, core.NewMempool(core.DefaultMempoolConfig()), log)
	if wrongFork.IsFinalizedHash(1, otherBlock.Hash()) || wrongFork.IsFinalized(1) {
		t.Fatal("certificate from another fork was accepted")
	}
	if err := wrongFork.requireLPoDParentFinalized(1, otherBlock.Hash()); err == nil {
		t.Fatal("unfinalized alternate-fork parent satisfied activation gate")
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
	if err := engine.requireLPoDParentFinalized(1, block.Hash()); err == nil {
		t.Fatal("forged certificate satisfied activation-parent gate")
	}
	if !engine.halted.Load() {
		t.Fatal("node did not halt on forged durable finality evidence")
	}
}

func TestFinalityCertificateForCanonicalAncestorDoesNotFinalizeTip(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain, ancestor := finalityTestChain(t, "ancestor")
	tip := &core.Block{Header: core.BlockHeader{
		Height: 2, PrevHash: ancestor.Hash(), Timestamp: 3, Round: 2,
	}}
	if err := chain.AddBlock(tip); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err := json.Marshal(ancestor)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(ancestor.Hash(), 1, raw); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(tip.Hash(), tip.Header.Height); err != nil {
		t.Fatal(err)
	}
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), ancestor.Hash().Bytes())
	signature, err := priv.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	certificate := store.FinalityCertificate{
		Version: 1, Height: 1, BlockHash: ancestor.Hash(),
		Votes: []store.FinalityVote{{Validator: pub, Signature: signature}},
	}
	if err := db.SaveFinalityCertificate(certificate); err != nil {
		t.Fatal(err)
	}
	registry := core.NewValidatorRegistry()
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)
	cfg := Config{Validators: []crypto.ValidatorPubKey{pub}, Registry: registry, Store: db, BFTThreshold: 0.667}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	restart := func() *Engine {
		return NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	}

	recovered := restart()
	if recovered.halted.Load() || !recovered.IsFinalizedHash(1, ancestor.Hash()) {
		t.Fatal("authentic canonical ancestor certificate did not restore")
	}
	if recovered.IsFinalized(2) || recovered.IsFinalizedHash(2, tip.Hash()) {
		t.Fatal("ancestor certificate incorrectly finalized the current tip")
	}
	if err := recovered.requireLPoDParentFinalized(2, tip.Hash()); err == nil {
		t.Fatal("ancestor certificate authorized an uncertified LPoD parent")
	}

	// The non-validator's genesis fallback may be a placeholder that was never
	// active in the restored registry. It must not be added after the snapshot.
	_, placeholderPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	nonValidatorCfg := cfg
	nonValidatorCfg.Validators = []crypto.ValidatorPubKey{placeholderPub}
	nonValidatorRestart := NewEngine(nonValidatorCfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	if nonValidatorRestart.halted.Load() || !nonValidatorRestart.IsFinalizedHash(1, ancestor.Hash()) {
		t.Fatal("static fallback changed the restored historical committee")
	}
	if got := len(registry.GetActiveValidators()); got != 1 {
		t.Fatalf("static fallback created a phantom active validator: got %d", got)
	}

	exitedRegistry := core.NewValidatorRegistry()
	exitedRegistry.RestoreFromSnapshot(core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{
		pub.Hex(): {PubKey: pub, Status: core.ValidatorExited},
	}})
	exitedCfg := cfg
	exitedCfg.Registry = exitedRegistry
	if !NewEngine(exitedCfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log).halted.Load() {
		t.Fatal("static fallback re-authorized a validator that exited in the restored registry")
	}

	// A signature that was sufficient for one validator is not a quorum
	// certificate once the active committee contains two validators.
	_, secondPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	twoValidatorRegistry := core.NewValidatorRegistry()
	twoValidatorRegistry.InitFromGenesis(
		[]crypto.ValidatorPubKey{pub, secondPub}, core.MinStakeNAPR*10)
	twoValidatorConfig := Config{
		Validators:   []crypto.ValidatorPubKey{pub, secondPub},
		Registry:     twoValidatorRegistry,
		Store:        db,
		BFTThreshold: 0.667,
	}
	if !NewEngine(twoValidatorConfig, chain, core.NewMempool(core.DefaultMempoolConfig()), log).halted.Load() {
		t.Fatal("insufficient historical votes did not halt consensus")
	}

	if err := db.RepairHeightIndex(1, crypto.HashBytes([]byte("different fork"))); err != nil {
		t.Fatal(err)
	}
	if !restart().halted.Load() {
		t.Fatal("mismatched ancestor height index did not halt consensus")
	}
	if err := db.RepairHeightIndex(1, ancestor.Hash()); err != nil {
		t.Fatal(err)
	}
	badSignature, err := priv.Sign(crypto.HashBytes([]byte("wrong finality message")))
	if err != nil {
		t.Fatal(err)
	}
	certificate.Votes[0].Signature = badSignature
	if err := db.SaveFinalityCertificate(certificate); err != nil {
		t.Fatal(err)
	}
	if !restart().halted.Load() {
		t.Fatal("forged ancestor certificate did not halt consensus")
	}
	certificate.Height = 3
	if err := db.SaveFinalityCertificate(certificate); err != nil {
		t.Fatal(err)
	}
	if !restart().halted.Load() {
		t.Fatal("certificate above the tip did not halt consensus")
	}
}
