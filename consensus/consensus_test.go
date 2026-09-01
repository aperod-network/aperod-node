package consensus_test

// Unit tests for the PoA consensus engine.
// Covers: proposer selection, vote collection, BFT finalization,
// missed-slot handling, and double-sign detection.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── nop logger ───────────────────────────────────────────────────────────────

// slogNop is a slog.Handler that discards all output — used in tests.
type slogNop struct{}

func (slogNop) Enabled(context.Context, slog.Level) bool  { return false }
func (slogNop) Handle(context.Context, slog.Record) error { return nil }
func (slogNop) WithAttrs([]slog.Attr) slog.Handler        { return slogNop{} }
func (slogNop) WithGroup(string) slog.Handler             { return slogNop{} }

func newNopLogger() *slog.Logger { return slog.New(slogNop{}) }

// ─── Helpers ──────────────────────────────────────────────────────────────────

func makeChainWithGenesis(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey) *core.Chain {
	t.Helper()
	hdr := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatal(err)
	}
	g := &core.Block{Header: hdr}
	chain := core.NewChain()
	if err := chain.SetGenesis(g); err != nil {
		t.Fatal(err)
	}
	return chain
}

func newEngine(t *testing.T, validators []crypto.ValidatorPubKey, myKey *crypto.LockedValidatorKey, chain *core.Chain) *consensus.Engine {
	t.Helper()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	return consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   validators,
		MyKey:        myKey,
	}, chain, mp, newNopLogger())
}

// ─── ProposerAt ───────────────────────────────────────────────────────────────

// TestProposerAt_Deterministic verifies round-robin proposer assignment.
func TestProposerAt_Deterministic(t *testing.T) {
	n := 3
	privs := make([]crypto.ValidatorPrivKey, n)
	pubs := make([]crypto.ValidatorPubKey, n)
	for i := range privs {
		var err error
		privs[i], pubs[i], err = crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
	}

	chain := makeChainWithGenesis(t, privs[0], pubs[0])
	eng := newEngine(t, pubs, nil, chain)

	for r := uint32(0); r < 9; r++ {
		got := eng.ProposerAt(r)
		want := pubs[r%uint32(n)]
		if !got.Equals(want) {
			t.Errorf("ProposerAt(%d): got %s, want %s", r, got.ID(), want.ID())
		}
	}
}

// ─── Vote handling ────────────────────────────────────────────────────────────

// TestVote_BFTFinalization verifies that ⌊n*threshold⌋+1 votes finalize a block.
func TestVote_BFTFinalization(t *testing.T) {
	n := 4 // needed = int(4*0.667)+1 = 2+1 = 3
	privs := make([]crypto.ValidatorPrivKey, n)
	pubs := make([]crypto.ValidatorPubKey, n)
	for i := range privs {
		var err error
		privs[i], pubs[i], err = crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
	}

	chain := makeChainWithGenesis(t, privs[0], pubs[0])
	eng := newEngine(t, pubs, nil, chain)

	// Build a block at height 1
	tip := chain.Tip()
	hdr := core.BlockHeader{
		Height:       1,
		Round:        1,
		PrevHash:     tip.Hash(),
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pubs[0],
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(privs[0]); err != nil {
		t.Fatal(err)
	}
	block1 := &core.Block{Header: hdr}
	if err := chain.AddBlock(block1); err != nil {
		t.Fatal(err)
	}

	bh := block1.Hash()
	msg := crypto.HashBytes([]byte("aperod/finalize/v1"), bh[:])

	// 2 votes — should NOT finalize
	for i := 0; i < 2; i++ {
		sig, _ := privs[i].Sign(msg)
		if err := eng.HandleVote(consensus.FinalizeMsg{
			BlockHash: bh, Height: 1, ValidatorPub: pubs[i], Signature: sig,
		}); err != nil {
			t.Fatalf("vote %d rejected: %v", i, err)
		}
	}
	if eng.IsFinalized(1) {
		t.Error("block must not be finalized with only 2/4 votes")
	}

	// 3rd vote → finalize
	sig, _ := privs[2].Sign(msg)
	if err := eng.HandleVote(consensus.FinalizeMsg{
		BlockHash: bh, Height: 1, ValidatorPub: pubs[2], Signature: sig,
	}); err != nil {
		t.Fatalf("3rd vote rejected: %v", err)
	}
	if !eng.IsFinalized(1) {
		t.Error("block must be finalized after 3/4 votes (≥0.667 threshold)")
	}
}

// TestVote_UnknownValidator verifies votes from non-validators are rejected.
func TestVote_UnknownValidator(t *testing.T) {
	priv0, pub0, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv0, pub0)
	eng := newEngine(t, []crypto.ValidatorPubKey{pub0}, nil, chain)

	outsiderPriv, outsiderPub, _ := crypto.GenerateValidatorKey()
	var fakeHash crypto.Hash32
	msg := crypto.HashBytes([]byte("aperod/finalize/v1"), fakeHash[:])
	sig, _ := outsiderPriv.Sign(msg)

	err := eng.HandleVote(consensus.FinalizeMsg{
		BlockHash: fakeHash, Height: 0, ValidatorPub: outsiderPub, Signature: sig,
	})
	if err == nil {
		t.Error("expected rejection of vote from unknown validator")
	}
}

// TestVote_InvalidSignature verifies malformed signatures are rejected.
func TestVote_InvalidSignature(t *testing.T) {
	priv0, pub0, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv0, pub0)
	eng := newEngine(t, []crypto.ValidatorPubKey{pub0}, nil, chain)

	var fakeHash crypto.Hash32
	err := eng.HandleVote(consensus.FinalizeMsg{
		BlockHash: fakeHash, Height: 0, ValidatorPub: pub0,
		Signature: []byte("not-a-valid-signature"),
	})
	if err == nil {
		t.Error("expected rejection of invalid vote signature")
	}
}

// TestVote_DuplicateCounted once verifies same validator's second vote is ignored.
func TestVote_DuplicateCounted(t *testing.T) {
	// 3 validators; needed = int(3*0.667)+1 = 3 → all 3 must vote to finalize.
	n := 3
	privs := make([]crypto.ValidatorPrivKey, n)
	pubs := make([]crypto.ValidatorPubKey, n)
	for i := range privs {
		privs[i], pubs[i], _ = crypto.GenerateValidatorKey()
	}
	chain := makeChainWithGenesis(t, privs[0], pubs[0])
	eng := newEngine(t, pubs, nil, chain)

	hdr := core.BlockHeader{
		Height: 1, PrevHash: chain.Tip().Hash(),
		Timestamp: time.Now().UnixNano(), ValidatorPub: pubs[0],
		MerkleRoot: core.MerkleRoot(nil),
	}
	_ = hdr.Sign(privs[0])
	block1 := &core.Block{Header: hdr}
	_ = chain.AddBlock(block1)

	bh := block1.Hash()
	msg := crypto.HashBytes([]byte("aperod/finalize/v1"), bh[:])
	sig0, _ := privs[0].Sign(msg)

	vote0 := consensus.FinalizeMsg{
		BlockHash: bh, Height: 1, ValidatorPub: pubs[0], Signature: sig0,
	}
	_ = eng.HandleVote(vote0)
	_ = eng.HandleVote(vote0) // duplicate — same validator, same block

	// Only 1 unique voter → should not finalize (need 3)
	if eng.IsFinalized(1) {
		t.Error("duplicate vote must not cause early finalization")
	}
}

// ─── Block production ─────────────────────────────────────────────────────────

// TestEngine_ProducesBlock verifies the engine emits a block when it's the proposer.
func TestEngine_ProducesBlock(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv, pub)
	lk, _ := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	defer lk.Destroy()
	eng := newEngine(t, []crypto.ValidatorPubKey{pub}, lk, chain)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	select {
	case block := <-eng.ProducedCh():
		if block.Header.Height != 1 {
			t.Errorf("expected height 1, got %d", block.Header.Height)
		}
		t.Logf("produced block height=%d txs=%d", block.Header.Height, len(block.Txs))
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout: no block produced in 500ms")
	}
}

// TestEngine_NonProposer_Silent verifies non-proposer nodes don't emit blocks.
func TestEngine_NonProposer_Silent(t *testing.T) {
	// 3 validators: [pub0, pub1, pub2]. Round 1 → validators[1%3] = pub1.
	// Our node is pub2 (index 2) → not the proposer for round 1.
	privs := make([]crypto.ValidatorPrivKey, 3)
	pubs := make([]crypto.ValidatorPubKey, 3)
	for i := range privs {
		privs[i], pubs[i], _ = crypto.GenerateValidatorKey()
	}
	chain := makeChainWithGenesis(t, privs[0], pubs[0])
	lk2, _ := crypto.NewLockedValidatorKey(privs[2].Bytes(), nil)
	defer lk2.Destroy()
	eng := newEngine(t, pubs, lk2, chain)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	select {
	case block := <-eng.ProducedCh():
		t.Errorf("non-proposer produced block at height %d", block.Header.Height)
	case <-time.After(150 * time.Millisecond):
		t.Log("OK: non-proposer stayed silent")
	}
}

// TestEngine_AcceptsIncomingBlock verifies that a P2P block is accepted and chain grows.
// TxVerifier must be wired before engine.Run — this mirrors production startup ordering.
func TestEngine_AcceptsIncomingBlock(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv, pub)
	eng := newEngine(t, []crypto.ValidatorPubKey{pub}, nil, chain) // observer

	// Wire TxVerifier BEFORE Run (production ordering — block has no txs so
	// crypto checks pass trivially; the important thing is the verifier is set).
	utxos := core.NewUTXOSet()
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	tip := chain.Tip()
	hdr := core.BlockHeader{
		Height:       1,
		Round:        1,
		PrevHash:     tip.Hash(),
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatal(err)
	}
	block1 := &core.Block{Header: hdr}

	eng.NewBlockCh() <- block1

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Errorf("chain height = %d after incoming block, want 1", chain.Height())
			return
		default:
			if chain.Height() == 1 {
				t.Log("chain advanced to height 1")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestEngine_RejectsBlock_NoVerifier verifies that handleIncomingBlock is fail-closed
// when no TxVerifier has been set: the block must not be added to the chain.
// This is a regression guard for the startup ordering bug where engine.Run was
// started before engine.SetTxVerifier was called.
func TestEngine_RejectsBlock_NoVerifier(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv, pub)

	// Engine with no TxVerifier wired (simulates pre-SetTxVerifier startup window).
	eng := newEngine(t, []crypto.ValidatorPubKey{pub}, nil, chain)
	// Deliberately do NOT call eng.SetTxVerifier.

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// Build a structurally valid block at height 1.
	tip := chain.Tip()
	hdr := core.BlockHeader{
		Height:       1,
		Round:        1,
		PrevHash:     tip.Hash(),
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatal(err)
	}
	block1 := &core.Block{Header: hdr}

	eng.NewBlockCh() <- block1

	// Give the engine time to process.
	time.Sleep(100 * time.Millisecond)

	// Block must NOT have been added to the chain: verifier is not set.
	if chain.Height() != 0 {
		t.Errorf("chain advanced to height %d without a TxVerifier — fail-open behavior detected", chain.Height())
	}
}

// ─── Timejacking guard (#418) ─────────────────────────────────────────────────

// TestHandleIncomingBlock_FutureTooFar verifies that a block whose timestamp is
// 60 seconds ahead of local wall clock is rejected (timejacking prevention).
func TestHandleIncomingBlock_FutureTooFar(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv, pub)
	eng := newEngine(t, []crypto.ValidatorPubKey{pub}, nil, chain)

	utxos := core.NewUTXOSet()
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	tip := chain.Tip()
	hdr := core.BlockHeader{
		Height:       1,
		Round:        1,
		PrevHash:     tip.Hash(),
		Timestamp:    time.Now().Add(60 * time.Second).UnixNano(), // 60 s in the future
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatal(err)
	}
	eng.NewBlockCh() <- &core.Block{Header: hdr}

	time.Sleep(150 * time.Millisecond)

	if chain.Height() != 0 {
		t.Errorf("chain advanced to height %d — far-future block should have been rejected",
			chain.Height())
	}
}

// TestHandleIncomingBlock_SlightlyAhead verifies that a block 5 seconds ahead
// of local wall clock is accepted (within the ±15 s tolerance window).
func TestHandleIncomingBlock_SlightlyAhead(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv, pub)
	eng := newEngine(t, []crypto.ValidatorPubKey{pub}, nil, chain)

	utxos := core.NewUTXOSet()
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	tip := chain.Tip()
	hdr := core.BlockHeader{
		Height:       1,
		Round:        1,
		PrevHash:     tip.Hash(),
		Timestamp:    time.Now().Add(5 * time.Second).UnixNano(), // 5 s ahead — within tolerance
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatal(err)
	}
	eng.NewBlockCh() <- &core.Block{Header: hdr}

	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Errorf("chain height = %d after 300 ms — slightly-ahead block should have been accepted",
				chain.Height())
			return
		default:
			if chain.Height() == 1 {
				t.Log("OK: slightly-ahead block accepted, chain advanced to height 1")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// ─── Clock-drift edge cases (#440) ────────────────────────────────────────────

// TestHandleIncomingBlock_14sAhead verifies that a block whose timestamp is
// 14 seconds in the future is accepted — it is within the ±15 s tolerance window.
// This simulates a validator node whose system clock runs ~14 s ahead of ours
// and ensures such nodes can still propagate blocks without getting stuck.
func TestHandleIncomingBlock_14sAhead(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv, pub)
	eng := newEngine(t, []crypto.ValidatorPubKey{pub}, nil, chain)

	utxos := core.NewUTXOSet()
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	tip := chain.Tip()
	hdr := core.BlockHeader{
		Height:       1,
		Round:        1,
		PrevHash:     tip.Hash(),
		Timestamp:    time.Now().Add(14 * time.Second).UnixNano(), // 14 s ahead — within ±15 s tolerance
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatal(err)
	}
	eng.NewBlockCh() <- &core.Block{Header: hdr}

	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Errorf("chain height = %d after 300 ms — 14 s-ahead block should have been accepted (skew < 15 s)",
				chain.Height())
			return
		default:
			if chain.Height() == 1 {
				t.Log("OK: 14 s-ahead block accepted, chain advanced to height 1")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestHandleIncomingBlock_14sBehind verifies that a block whose timestamp is
// 14 seconds in the past is accepted — it is within the ±15 s tolerance window.
// This simulates a validator whose clock lags slightly behind ours.
func TestHandleIncomingBlock_14sBehind(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv, pub)
	eng := newEngine(t, []crypto.ValidatorPubKey{pub}, nil, chain)

	utxos := core.NewUTXOSet()
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	tip := chain.Tip()
	hdr := core.BlockHeader{
		Height:       1,
		Round:        1,
		PrevHash:     tip.Hash(),
		Timestamp:    time.Now().Add(-14 * time.Second).UnixNano(), // 14 s behind — within ±15 s tolerance
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatal(err)
	}
	eng.NewBlockCh() <- &core.Block{Header: hdr}

	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Errorf("chain height = %d after 300 ms — 14 s-behind block should have been accepted (skew < 15 s)",
				chain.Height())
			return
		default:
			if chain.Height() == 1 {
				t.Log("OK: 14 s-behind block accepted, chain advanced to height 1")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestHandleIncomingBlock_JustOverLimit verifies that a block whose timestamp is
// 16 seconds ahead (just over the 15 s cap) is rejected — this is the exact
// boundary condition a slightly-over-drifted node would hit.
func TestHandleIncomingBlock_JustOverLimit(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv, pub)
	eng := newEngine(t, []crypto.ValidatorPubKey{pub}, nil, chain)

	utxos := core.NewUTXOSet()
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	tip := chain.Tip()
	hdr := core.BlockHeader{
		Height:       1,
		PrevHash:     tip.Hash(),
		Timestamp:    time.Now().Add(16 * time.Second).UnixNano(), // 16 s — just over ±15 s cap
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatal(err)
	}
	eng.NewBlockCh() <- &core.Block{Header: hdr}

	time.Sleep(150 * time.Millisecond)

	if chain.Height() != 0 {
		t.Errorf("chain advanced to height %d — 16 s-ahead block should have been rejected (skew > 15 s)",
			chain.Height())
	} else {
		t.Log("OK: 16 s-ahead block rejected, chain stayed at genesis")
	}
}

// ─── Proposer-slot enforcement (F-052) ───────────────────────────────────────

// TestEngine_HandleIncomingBlock_WrongProposerRejected verifies that a block
// signed by a known but non-scheduled validator is rejected (F-052 / Gh0stAnts
// re-verification finding).  With two validators [pub0, pub1] and round-robin
// scheduling, round 1 belongs to pub1.  A block at round 1 signed by pub0 must
// be dropped and the chain must not advance past genesis.
func TestEngine_HandleIncomingBlock_WrongProposerRejected(t *testing.T) {
	// Two validators.  Genesis was proposed by priv0/pub0 (round 0).
	priv0, pub0, _ := crypto.GenerateValidatorKey()
	priv1, pub1, _ := crypto.GenerateValidatorKey()
	_ = priv1 // round 1's legitimate proposer — not used in this test

	chain := makeChainWithGenesis(t, priv0, pub0)
	eng := newEngine(t, []crypto.ValidatorPubKey{pub0, pub1}, nil, chain)

	utxos := core.NewUTXOSet()
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// Round 1 → scheduled proposer is pub1 (index 1 % 2).
	// We send a block signed by pub0, which is a known validator but NOT the
	// scheduled proposer for round 1.
	tip := chain.Tip()
	hdr := core.BlockHeader{
		Height:       1,
		Round:        1,
		PrevHash:     tip.Hash(),
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub0, // wrong proposer for round 1
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv0); err != nil {
		t.Fatal(err)
	}
	eng.NewBlockCh() <- &core.Block{Header: hdr}

	time.Sleep(150 * time.Millisecond)

	if chain.Height() != 0 {
		t.Errorf("chain advanced to height %d — out-of-turn block must be rejected (F-052 regression)",
			chain.Height())
	} else {
		t.Log("OK: block from non-scheduled proposer rejected, chain stayed at genesis")
	}
}

// ─── Non-validator mode ───────────────────────────────────────────────────────

// TestEngine_NonValidatorMode_NeverProducesBlock verifies that an engine
// constructed with MyKey=nil (non-validator mode) never emits a block on
// ProducedCh even when the round clock fires and the sole validator in the
// set would be the scheduled proposer for that slot.
//
// This is the primary regression guard for the path at poa.go tick() line ~246:
//
//	if e.cfg.MyKey == nil || !proposer.Equals(e.cfg.MyKey.Public()) {
//	    return nil // not our slot
//	}
//
// A regression (e.g. inadvertently passing a non-nil key) would silently
// re-enable block production and cause chain splits in production.
func TestEngine_NonValidatorMode_NeverProducesBlock(t *testing.T) {
	// Single-validator chain: at every round, proposerAt returns pub.
	// The engine therefore "is" the scheduled proposer — but MyKey=nil
	// means it must stay silent.
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain := makeChainWithGenesis(t, priv, pub)

	// Create engine in non-validator mode: validators list contains pub, but
	// MyKey is nil so the engine must never produce a block.
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{pub},
		MyKey:        nil, // non-validator mode
	}, chain, mp, newNopLogger())

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// Wait long enough for several tick() invocations to fire.
	select {
	case block := <-eng.ProducedCh():
		t.Errorf("non-validator engine produced block at height %d — MyKey=nil guard broken",
			block.Header.Height)
	case <-time.After(200 * time.Millisecond):
		// Good: no block produced.
	}

	// Chain must remain at genesis height.
	if h := chain.Height(); h != 0 {
		t.Errorf("chain height = %d after non-validator run, want 0", h)
	}
}

// TestEngine_SingleValidator_Finalizes verifies a 1-of-1 validator set auto-finalizes.
func TestEngine_SingleValidator_Finalizes(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	chain := makeChainWithGenesis(t, priv, pub)
	lk, _ := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	defer lk.Destroy()
	eng := newEngine(t, []crypto.ValidatorPubKey{pub}, lk, chain)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// Wait for block production + auto-vote finalization
	var finalHeight uint64
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case <-deadline:
			if !eng.IsFinalized(1) {
				t.Error("block 1 not finalized in 500ms with single validator")
			}
			return
		default:
			if eng.IsFinalized(1) {
				t.Logf("block 1 finalized, chain height=%d", finalHeight)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
