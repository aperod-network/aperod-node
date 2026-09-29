package consensus_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestFutureVoteReplaysOnlyAfterCanonicalBlockAcceptance(t *testing.T) {
	priv, pub, genesis, block := buildValidatorGenesisAndBlock(t)
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	utxos := core.NewUTXOSet()
	engine := consensus.NewEngine(consensus.Config{
		OnCanonicalBlock: noopCanonicalPersistence,
		BlockTime:        20 * time.Millisecond, BFTThreshold: 0.667,
		Validators:               []crypto.ValidatorPubKey{pub},
		RingCTV4ActivationHeight: ^uint64(0),
	}, chain, core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)
	msg := crypto.HashBytes([]byte("aperod/finalize/v1"), block.Hash().Bytes())
	sig, err := priv.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	vote := consensus.FinalizeMsg{BlockHash: block.Hash(), Height: 1, ValidatorPub: pub, Signature: sig}
	if err := engine.HandleVote(vote); err != nil {
		t.Fatalf("valid vote arriving before its block should be staged: %v", err)
	}
	if engine.IsFinalized(1) {
		t.Fatal("staged vote finalized a block before canonical acceptance")
	}

	stop := make(chan struct{})
	defer close(stop)
	go engine.Run(stop)
	engine.NewBlockCh() <- block
	deadline := time.After(2 * time.Second)
	for !engine.IsFinalized(1) {
		select {
		case <-deadline:
			t.Fatalf("early vote was not replayed after block acceptance (height=%d)", chain.Height())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if chain.Tip().Hash() != block.Hash() {
		t.Fatal("accepted tip does not match staged vote")
	}
}

func TestLocalVoteBroadcastCallbackAndDurableRestore(t *testing.T) {
	priv, pub, genesis, block := buildValidatorGenesisAndBlock(t)
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	locked, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Destroy()
	utxos := core.NewUTXOSet()
	voteC := make(chan consensus.FinalizeMsg, 1)
	cfg := consensus.Config{
		OnCanonicalBlock: func(b *core.Block, _ *avm.PreparedBlock) error {
			raw, err := json.Marshal(b)
			if err != nil {
				return err
			}
			if err := db.PutRawBlock(b.Hash(), b.Header.Height, raw); err != nil {
				return err
			}
			return db.PutTip(b.Hash(), b.Header.Height)
		},
		OnVoteCast: func(vote consensus.FinalizeMsg) { voteC <- vote },
		BlockTime:  time.Hour, BFTThreshold: 0.667,
		Validators: []crypto.ValidatorPubKey{pub}, MyKey: locked, Store: db,
		RingCTV4ActivationHeight: ^uint64(0),
	}
	engine := consensus.NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)
	stop := make(chan struct{})
	go engine.Run(stop)
	engine.NewBlockCh() <- block
	select {
	case vote := <-voteC:
		if vote.Height != 1 || vote.BlockHash != block.Hash() || !vote.ValidatorPub.Equals(pub) {
			t.Fatalf("unexpected broadcast vote: %+v", vote)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local vote callback did not deliver after successful handleVote")
	}
	close(stop)

	vote, ok := engine.LatestLocalVote()
	if !ok || vote.Height != 1 || vote.BlockHash != block.Hash() {
		t.Fatal("latest durable local vote unavailable before restart")
	}
	restored := consensus.NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	retried, ok := restored.LatestLocalVote()
	if !ok || retried.Height != vote.Height || retried.BlockHash != vote.BlockHash ||
		!retried.ValidatorPub.Equals(vote.ValidatorPub) || string(retried.Signature) != string(vote.Signature) {
		t.Fatal("exact durable vote was not restored for retry")
	}
	wrongChain := core.NewChain()
	if err := wrongChain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	alternateHeader := block.Header
	alternateHeader.Timestamp++
	if err := alternateHeader.Sign(priv); err != nil {
		t.Fatal(err)
	}
	alternateBlock := &core.Block{Header: alternateHeader, Txs: block.Txs}
	if err := wrongChain.AddBlock(alternateBlock); err != nil {
		t.Fatal(err)
	}
	wrongFork := consensus.NewEngine(cfg, wrongChain, core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	if _, ok := wrongFork.LatestLocalVote(); ok {
		t.Fatal("durable local vote was restored for a different canonical fork")
	}

	// A higher durable tip does not make an old height-index entry proof of
	// ancestry. Construct a durable alternate h=1 -> h=2 branch while leaving
	// the height-1 index stale at the original vote hash.
	alternateChain := core.NewChain()
	if err := alternateChain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	alternateBranchHeader := block.Header
	alternateBranchHeader.Timestamp++
	if err := alternateBranchHeader.Sign(priv); err != nil {
		t.Fatal(err)
	}
	alternateBranchBlock := &core.Block{Header: alternateBranchHeader, Txs: block.Txs}
	if err := alternateChain.AddBlock(alternateBranchBlock); err != nil {
		t.Fatal(err)
	}
	rawAlternate, err := json.Marshal(alternateBranchBlock)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(alternateBranchBlock.Hash(), 1, rawAlternate); err != nil {
		t.Fatal(err)
	}
	if err := db.RepairHeightIndex(1, block.Hash()); err != nil {
		t.Fatal(err)
	}
	alternateTipHeader := core.BlockHeader{
		Height: 2, Round: 2, PrevHash: alternateBranchBlock.Hash(),
		Timestamp: alternateBranchHeader.Timestamp + 1, ValidatorPub: pub,
		MerkleRoot: core.MerkleRoot(nil),
	}
	if err := alternateTipHeader.Sign(priv); err != nil {
		t.Fatal(err)
	}
	alternateTip := &core.Block{Header: alternateTipHeader}
	if err := alternateChain.AddBlock(alternateTip); err != nil {
		t.Fatal(err)
	}
	rawAlternateTip, err := json.Marshal(alternateTip)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(alternateTip.Hash(), 2, rawAlternateTip); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(alternateTip.Hash(), 2); err != nil {
		t.Fatal(err)
	}
	alternateRestart := consensus.NewEngine(cfg, alternateChain,
		core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	if _, ok := alternateRestart.LatestLocalVote(); ok {
		t.Fatal("stale higher height index authorized a vote absent from durable tip ancestry")
	}

	// PutTip rollback deliberately leaves the higher height index untouched.
	// That stale entry must not authorize replay of the old vote.
	if err := db.RepairHeightIndex(1, block.Hash()); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(block.Hash(), block.Header.Height); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(genesis.Hash(), 0); err != nil {
		t.Fatal(err)
	}
	rolledBackChain := core.NewChain()
	if err := rolledBackChain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	recoveryCfg := cfg
	recoveryCfg.BlockTime = 25 * time.Millisecond
	rolledBack := consensus.NewEngine(recoveryCfg, rolledBackChain,
		core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	if _, ok := rolledBack.LatestLocalVote(); ok {
		t.Fatal("vote above rolled-back durable tip was retried from stale height index")
	}
	rollbackStop := make(chan struct{})
	go rolledBack.Run(rollbackStop)
	select {
	case produced := <-rolledBack.ProducedCh():
		close(rollbackStop)
		t.Fatalf("producer continued on rolled-back chain at height %d", produced.Header.Height)
	case <-time.After(125 * time.Millisecond):
	}
	close(rollbackStop)

	// The producer may resume only after durable tip metadata and ancestry
	// once again prove the exact previously signed block canonical.
	if err := db.PutTip(block.Hash(), block.Header.Height); err != nil {
		t.Fatal(err)
	}
	safeRestart := consensus.NewEngine(recoveryCfg, chain,
		core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	if vote, ok := safeRestart.LatestLocalVote(); !ok || vote.BlockHash != block.Hash() {
		t.Fatal("canonical durable local vote was not restored after safe tip recovery")
	}
	safeUTXOs := core.NewUTXOSet()
	safeRestart.SetTxVerifier(core.NewTxVerifier(safeUTXOs), safeUTXOs)
	safeStop := make(chan struct{})
	go safeRestart.Run(safeStop)
	defer close(safeStop)
	select {
	case produced := <-safeRestart.ProducedCh():
		if produced.Header.Height != 2 {
			t.Fatalf("safe producer resumed at height %d, want 2", produced.Header.Height)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer remained halted after durable canonical ancestry was restored")
	}
}

func TestFutureVoteQueueRejectsInvalidDuplicateAndFarFuture(t *testing.T) {
	priv, pub, genesis, _ := buildValidatorGenesisAndBlock(t)
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	engine := consensus.NewEngine(consensus.Config{
		OnCanonicalBlock: noopCanonicalPersistence,
		BlockTime:        20 * time.Millisecond, BFTThreshold: 0.667,
		Validators: []crypto.ValidatorPubKey{pub},
	}, chain, core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	hash := crypto.HashBytes([]byte("future vote target"))
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), hash.Bytes())
	signature, err := priv.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	vote := consensus.FinalizeMsg{Height: 1, BlockHash: hash, ValidatorPub: pub, Signature: signature}
	if err := engine.HandleVote(vote); err != nil {
		t.Fatalf("first authenticated near-tip vote was rejected: %v", err)
	}
	if err := engine.HandleVote(vote); err == nil {
		t.Fatal("duplicate staged vote was accepted")
	}
	invalid := vote
	invalid.Height = 2
	invalid.Signature = []byte("invalid")
	if err := engine.HandleVote(invalid); err == nil {
		t.Fatal("invalid signature was staged")
	}
	far := vote
	far.Height = 4 // farther than the bounded two-height window from genesis
	if err := engine.HandleVote(far); err == nil {
		t.Fatal("far-future vote was staged")
	}
	if engine.IsFinalized(1) || engine.IsFinalized(2) || engine.IsFinalized(4) {
		t.Fatal("unaccepted future vote created phantom finality")
	}
}
