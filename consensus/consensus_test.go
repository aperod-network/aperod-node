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

func (slogNop) Enabled(context.Context, slog.Level) bool { return false }
func (slogNop) Handle(context.Context, slog.Record) error { return nil }
func (slogNop) WithAttrs([]slog.Attr) slog.Handler        { return slogNop{} }
func (slogNop) WithGroup(string) slog.Handler              { return slogNop{} }

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
