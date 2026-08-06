package core_test

// Tests for the sliding-window memory-eviction fix in Chain (Task #1290).
//
// Verifies that FastForward, FastForwardWithIndex, and Reorg all respect the
// MaxInMemoryBlocks limit so that large catch-up batches (P2P sync of 10 000+
// blocks) do not inflate the in-memory block maps unboundedly.

import (
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// makeTestBlock builds a minimal signed block at the given height.
func makeChainTestBlock(t *testing.T, height uint64, prev crypto.Hash32) (*core.Block, crypto.ValidatorPrivKey, crypto.ValidatorPubKey) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	hdr := core.BlockHeader{
		Height:       height,
		PrevHash:     prev,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatalf("Sign h=%d: %v", height, err)
	}
	return &core.Block{Header: hdr}, priv, pub
}

// makeChainBlocks builds nBlocks signed blocks starting at height 1
// (genesis at height 0 is prepended internally).
func makeTestChainBlocks(t *testing.T, n int) []*core.Block {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	sign := func(hdr *core.BlockHeader) {
		if err := hdr.Sign(priv); err != nil {
			t.Fatalf("Sign: %v", err)
		}
	}

	genesis := &core.Block{Header: core.BlockHeader{
		Height: 0, ValidatorPub: pub, MerkleRoot: core.MerkleRoot(nil), Timestamp: 1,
	}}
	sign(&genesis.Header)

	blocks := []*core.Block{genesis}
	for i := 1; i <= n; i++ {
		prev := blocks[len(blocks)-1]
		hdr := core.BlockHeader{
			Height:       uint64(i),
			PrevHash:     prev.Hash(),
			Timestamp:    int64(i) + 1,
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		}
		sign(&hdr)
		blocks = append(blocks, &core.Block{Header: hdr})
	}
	return blocks
}

// ─── FastForward eviction ────────────────────────────────────────────────────

// TestFastForward_RespectsMaxInMemoryBlocks confirms that loading more than
// MaxInMemoryBlocks blocks via FastForward evicts the oldest ones.
func TestFastForward_RespectsMaxInMemoryBlocks(t *testing.T) {
	const over = 500 // blocks above the window
	total := core.MaxInMemoryBlocks + over

	blocks := makeTestChainBlocks(t, total)

	c := core.NewChain()
	if err := c.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	// FastForward the full chain (genesis already set, so pass height 1+).
	c.FastForward(blocks[1:])

	// Tip should be the last block.
	if c.Tip().Header.Height != uint64(total) {
		t.Errorf("tip height: want %d, got %d", total, c.Tip().Header.Height)
	}

	// Blocks within the window must be present.
	windowStart := uint64(total) - core.MaxInMemoryBlocks + 1
	for h := windowStart; h <= uint64(total); h++ {
		if c.GetByHeight(h) == nil {
			t.Errorf("block at height %d should be in window but is missing", h)
		}
	}

	// Blocks outside the window must have been evicted.
	for h := uint64(1); h < windowStart; h++ {
		if c.GetByHeight(h) != nil {
			t.Errorf("block at height %d should be evicted but is still present", h)
			break // report once
		}
	}
}

// TestFastForward_SmallBatch confirms no eviction when the batch fits the window.
func TestFastForward_SmallBatch(t *testing.T) {
	const n = 100 // well within MaxInMemoryBlocks
	blocks := makeTestChainBlocks(t, n)

	c := core.NewChain()
	if err := c.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	c.FastForward(blocks[1:])

	for h := uint64(1); h <= uint64(n); h++ {
		if c.GetByHeight(h) == nil {
			t.Errorf("block at height %d unexpectedly evicted in small batch", h)
		}
	}
}

// ─── FastForwardWithIndex eviction ───────────────────────────────────────────

// TestFastForwardWithIndex_RespectsMaxInMemoryBlocks mirrors the FastForward
// test but exercises the pre-built tx-index code path.
func TestFastForwardWithIndex_RespectsMaxInMemoryBlocks(t *testing.T) {
	const over = 300
	total := core.MaxInMemoryBlocks + over

	blocks := makeTestChainBlocks(t, total)

	c := core.NewChain()
	if err := c.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	// Pass an empty tx-index map (blocks have no transactions).
	c.FastForwardWithIndex(blocks[1:], nil)

	windowStart := uint64(total) - core.MaxInMemoryBlocks + 1
	for h := uint64(1); h < windowStart; h++ {
		if c.GetByHeight(h) != nil {
			t.Errorf("FastForwardWithIndex: block at height %d not evicted", h)
			break
		}
	}
	if c.Tip().Header.Height != uint64(total) {
		t.Errorf("tip: want %d, got %d", total, c.Tip().Header.Height)
	}
}

// ─── AddBlock eviction (regression guard) ────────────────────────────────────

// TestAddBlock_RespectsMaxInMemoryBlocks is a regression guard: AddBlock always
// had eviction; this test confirms it still does after the refactor.
func TestAddBlock_RespectsMaxInMemoryBlocks(t *testing.T) {
	const over = 50
	total := core.MaxInMemoryBlocks + over

	blocks := makeTestChainBlocks(t, total)

	c := core.NewChain()
	if err := c.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	for _, b := range blocks[1:] {
		if err := c.AddBlock(b); err != nil {
			t.Fatalf("AddBlock h=%d: %v", b.Header.Height, err)
		}
	}

	windowStart := uint64(total) - core.MaxInMemoryBlocks + 1
	for h := uint64(1); h < windowStart; h++ {
		if c.GetByHeight(h) != nil {
			t.Errorf("AddBlock: block at height %d not evicted", h)
			break
		}
	}
	for h := windowStart; h <= uint64(total); h++ {
		if c.GetByHeight(h) == nil {
			t.Errorf("AddBlock: block at height %d missing from window", h)
		}
	}
}

// ─── Reorg eviction ──────────────────────────────────────────────────────────

// TestReorg_RespectsMaxInMemoryBlocks verifies that a reorg installing many
// new blocks also evicts entries outside the sliding window.
func TestReorg_RespectsMaxInMemoryBlocks(t *testing.T) {
	const base = 50 // establish a short chain first
	blocks := makeTestChainBlocks(t, base)

	c := core.NewChain()
	if err := c.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	for _, b := range blocks[1:] {
		if err := c.AddBlock(b); err != nil {
			t.Fatalf("AddBlock: %v", err)
		}
	}

	// Build a longer fork from height 1 that exceeds MaxInMemoryBlocks.
	const forkLen = core.MaxInMemoryBlocks + 100
	priv, pub, _ := crypto.GenerateValidatorKey()
	sign := func(hdr *core.BlockHeader) {
		if err := hdr.Sign(priv); err != nil {
			t.Fatalf("Sign: %v", err)
		}
	}
	forkBlocks := make([]*core.Block, forkLen)
	prevHash := blocks[0].Hash() // fork from genesis
	for i := 0; i < forkLen; i++ {
		hdr := core.BlockHeader{
			Height:       uint64(i + 1),
			PrevHash:     prevHash,
			Timestamp:    int64(i+1) * 1000,
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		}
		sign(&hdr)
		b := &core.Block{Header: hdr}
		forkBlocks[i] = b
		prevHash = b.Hash()
	}

	if err := c.Reorg(0, forkBlocks); err != nil {
		t.Fatalf("Reorg: %v", err)
	}

	tipH := uint64(forkLen)
	if c.Tip().Header.Height != tipH {
		t.Errorf("tip after reorg: want %d, got %d", tipH, c.Tip().Header.Height)
	}

	windowStart := tipH - core.MaxInMemoryBlocks + 1
	for h := uint64(1); h < windowStart; h++ {
		if c.GetByHeight(h) != nil {
			t.Errorf("Reorg: block at height %d not evicted", h)
			break
		}
	}
}
