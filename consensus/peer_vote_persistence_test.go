package consensus_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestLatestLocalVoteWaitsForPeerCanonicalPersistence(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	genesisHeader := core.BlockHeader{
		Timestamp:    time.Now().UTC().UnixNano(),
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
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	persistBlock := func(block *core.Block) error {
		raw, err := json.Marshal(block)
		if err != nil {
			return err
		}
		if err := db.PutRawBlock(block.Hash(), block.Header.Height, raw); err != nil {
			return err
		}
		return db.PutTip(block.Hash(), block.Header.Height)
	}
	if err := persistBlock(genesis); err != nil {
		t.Fatal(err)
	}
	locked, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Destroy()

	// Produce a real local vote at height 1 and persist it before introducing
	// the peer transition under test.
	localVotes := make(chan consensus.FinalizeMsg, 1)
	baseConfig := consensus.Config{
		OnCanonicalBlock: func(block *core.Block, _ *avm.PreparedBlock) error {
			return persistBlock(block)
		},
		OnVoteCast: func(vote consensus.FinalizeMsg) { localVotes <- vote },
		BlockTime:  10 * time.Millisecond, BFTThreshold: 0.667,
		Validators: []crypto.ValidatorPubKey{pub}, MyKey: locked, Store: db,
		StopProducingAtHeight:    2,
		RingCTV4ActivationHeight: ^uint64(0),
	}
	engine := consensus.NewEngine(baseConfig, chain,
		core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	utxos := core.NewUTXOSet()
	engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)
	localStop := make(chan struct{})
	localDone := make(chan struct{})
	go func() {
		engine.Run(localStop)
		close(localDone)
	}()
	var stopLocalOnce sync.Once
	stopLocal := func() {
		stopLocalOnce.Do(func() { close(localStop) })
		<-localDone
	}
	defer stopLocal()
	select {
	case vote := <-localVotes:
		if vote.Height != 1 {
			t.Fatalf("initial local vote height = %d, want 1", vote.Height)
		}
	case <-time.After(3 * time.Second):
		stopLocal()
		t.Fatal("initial durable local vote was not produced")
	}
	stopLocal()

	initialVote, ok := engine.LatestLocalVote()
	if !ok || initialVote.Height != 1 {
		t.Fatalf("initial durable vote unavailable: (%+v, %t)", initialVote, ok)
	}
	block1 := chain.Tip()
	if block1 == nil || block1.Header.Height != 1 || block1.Hash() != initialVote.BlockHash {
		t.Fatalf("initial local vote does not match canonical block 1: vote=%+v tip=%v",
			initialVote, block1)
	}

	// Have an independent peer produce a valid continuation, rather than
	// synthesizing a block that might fail consensus validation for unrelated
	// reasons.
	peerChain := core.NewChain()
	if err := peerChain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	if err := peerChain.AddBlock(block1); err != nil {
		t.Fatal(err)
	}
	peer := consensus.NewEngine(consensus.Config{
		OnCanonicalBlock:         noopCanonicalPersistence,
		BlockTime:                10 * time.Millisecond,
		BFTThreshold:             0.667,
		Validators:               []crypto.ValidatorPubKey{pub},
		MyKey:                    locked,
		StopProducingAtHeight:    4,
		RingCTV4ActivationHeight: ^uint64(0),
	}, peerChain, core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	peerUTXOs := core.NewUTXOSet()
	peer.SetTxVerifier(core.NewTxVerifier(peerUTXOs), peerUTXOs)
	peerStop := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		peer.Run(peerStop)
		close(peerDone)
	}()
	var stopPeerOnce sync.Once
	stopPeer := func() {
		stopPeerOnce.Do(func() { close(peerStop) })
		<-peerDone
	}
	defer stopPeer()
	var block2, block3 *core.Block
	select {
	case block2 = <-peer.ProducedCh():
	case <-time.After(3 * time.Second):
		stopPeer()
		t.Fatal("peer did not produce a valid height-2 block")
	}
	select {
	case block3 = <-peer.ProducedCh():
	case <-time.After(3 * time.Second):
		stopPeer()
		t.Fatal("peer did not produce a valid height-3 block")
	}
	stopPeer()
	if block2.Header.Height != 2 || block3.Header.Height != 3 ||
		block3.Header.PrevHash != block2.Hash() {
		t.Fatalf("peer produced an unexpected continuation: block2=%v block3=%v", block2, block3)
	}

	persistingPeerBlock := make(chan struct{})
	releasePersistence := make(chan struct{})
	var releaseOnce sync.Once
	releasePeerCommit := func() {
		releaseOnce.Do(func() { close(releasePersistence) })
	}
	persistedHeights := make(chan uint64, 2)
	baseConfig.MyKey = nil // This node observes peer blocks; it does not produce.
	baseConfig.BlockTime = time.Hour
	baseConfig.OnCanonicalBlock = func(block *core.Block, _ *avm.PreparedBlock) error {
		if block.Header.Height == 2 {
			close(persistingPeerBlock)
			<-releasePersistence
		}
		if err := persistBlock(block); err != nil {
			return err
		}
		persistedHeights <- block.Header.Height
		return nil
	}
	engine = consensus.NewEngine(baseConfig, chain,
		core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	utxos = core.NewUTXOSet()
	engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	runDone := make(chan struct{})
	go func() {
		engine.Run(stop)
		close(runDone)
	}()
	defer func() {
		releasePeerCommit()
		select {
		case <-runDone:
		default:
			close(stop)
			<-runDone
		}
	}()
	engine.NewBlockCh() <- block2
	select {
	case <-persistingPeerBlock:
	case <-time.After(3 * time.Second):
		t.Fatal("valid peer block did not reach canonical persistence")
	}

	memoryTip := chain.Tip()
	durableHash, durableHeight, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	if memoryTip == nil || memoryTip.Header.Height != 2 ||
		memoryTip.Hash() != block2.Hash() || durableHeight != 1 ||
		durableHash != block1.Hash() {
		t.Fatalf("test missed the memory/durable boundary: memory=%v durable=(%x,%d)",
			memoryTip, durableHash, durableHeight)
	}

	type voteResult struct {
		vote consensus.FinalizeMsg
		ok   bool
	}
	result := make(chan voteResult, 1)
	callStarted := make(chan struct{})
	go func() {
		close(callStarted)
		vote, ok := engine.LatestLocalVote()
		result <- voteResult{vote: vote, ok: ok}
	}()
	<-callStarted
	select {
	case got := <-result:
		t.Fatalf("LatestLocalVote returned during the memory/durable mismatch: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}

	releasePeerCommit()
	select {
	case got := <-result:
		if !got.ok || got.vote.Height != initialVote.Height ||
			got.vote.BlockHash != initialVote.BlockHash {
			t.Fatalf("LatestLocalVote after peer persistence = (%+v, %t), want initial canonical vote",
				got.vote, got.ok)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("LatestLocalVote did not resume after peer persistence")
	}
	indexedHash, found, err := db.GetCanonicalHash(initialVote.Height)
	if err != nil {
		t.Fatal(err)
	}
	if !found || indexedHash != initialVote.BlockHash {
		t.Fatalf("returned vote is not on the durable canonical chain: found=%t hash=%x",
			found, indexedHash)
	}
	select {
	case height := <-persistedHeights:
		if height != 2 {
			t.Fatalf("first peer persistence completed at height %d, want 2", height)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("peer block 2 did not finish persistence")
	}

	// A subsequent valid peer block also succeeds. If the transient mismatch
	// had falsely halted the engine, handleIncomingBlock would reject it.
	engine.NewBlockCh() <- block3
	select {
	case height := <-persistedHeights:
		if height != 3 {
			t.Fatalf("subsequent peer persistence completed at height %d, want 3", height)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("engine halted after the transient memory/durable mismatch")
	}
}
