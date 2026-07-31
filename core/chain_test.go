package core_test

// chain_test.go — tests for Chain sliding-window memory eviction (#462).
//
// Tests:
//  1. After adding MaxInMemoryBlocks+N blocks, the oldest N blocks are evicted.
//  2. The most recent MaxInMemoryBlocks blocks remain accessible via GetByHeight.
//  3. The chain tip is correct after eviction.

import (
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// makeMinimalBlock creates a lightweight block for chain eviction tests.
// AddBlock only validates height and PrevHash, so no signature is required.
func makeMinimalBlock(height uint64, prevHash crypto.Hash32) *core.Block {
	header := core.BlockHeader{
		Height:    height,
		PrevHash:  prevHash,
		Timestamp: time.Now().UnixNano() + int64(height),
		Round:     uint32(height),
	}
	return &core.Block{Header: header}
}

// TestChain_MemoryCapEviction (#462) — after adding MaxInMemoryBlocks+extra
// blocks, the earliest extra blocks must be evicted and only the sliding
// window of MaxInMemoryBlocks blocks remains in memory.
func TestChain_MemoryCapEviction(t *testing.T) {
	const extra = 5
	total := core.MaxInMemoryBlocks + extra

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	c := core.NewChain()
	genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
	if err := c.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	// Keep track of each block's hash so we can check eviction.
	hashes := make([]crypto.Hash32, total+1)
	hashes[0] = genesis.Hash()

	prev := genesis
	for h := uint64(1); h <= uint64(total); h++ {
		b := makeMinimalBlock(h, prev.Hash())
		if err := c.AddBlock(b); err != nil {
			t.Fatalf("AddBlock(height=%d): %v", h, err)
		}
		hashes[h] = b.Hash()
		prev = b
	}

	// Verify: tip height is correct.
	if c.Height() != uint64(total) {
		t.Errorf("Tip height: got %d, want %d", c.Height(), total)
	}

	// When block at height H is added, the block at H-MaxInMemoryBlocks is evicted.
	// After adding heights 1..total, the evicted heights are 0..(total-MaxInMemoryBlocks).
	evictedUpTo := uint64(total - core.MaxInMemoryBlocks) // inclusive
	for h := uint64(0); h <= evictedUpTo; h++ {
		if c.GetByHeight(h) != nil {
			t.Errorf("height %d should have been evicted from memory, but GetByHeight returned a block", h)
		}
		if c.GetByHash(hashes[h]) != nil {
			t.Errorf("hash of evicted block at height %d should not be in memory", h)
		}
	}

	// Verify: the most recent MaxInMemoryBlocks blocks are still accessible.
	for h := evictedUpTo + 1; h <= uint64(total); h++ {
		if c.GetByHeight(h) == nil {
			t.Errorf("height %d should be in the sliding window, but GetByHeight returned nil", h)
		}
	}
}

// TestChain_MemoryCapTipIntact (#462) — the chain tip remains correct and
// accessible after the eviction window slides.
func TestChain_MemoryCapTipIntact(t *testing.T) {
	const total = core.MaxInMemoryBlocks + 3

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	c := core.NewChain()
	genesis := makeBlock(t, priv, pub, 0, crypto.Hash32{})
	c.SetGenesis(genesis)

	prev := genesis
	for h := uint64(1); h <= uint64(total); h++ {
		b := makeMinimalBlock(h, prev.Hash())
		if err := c.AddBlock(b); err != nil {
			t.Fatalf("AddBlock(height=%d): %v", h, err)
		}
		prev = b
	}

	tip := c.Tip()
	if tip == nil {
		t.Fatal("Tip() returned nil after adding blocks")
	}
	if tip.Header.Height != uint64(total) {
		t.Errorf("Tip height: got %d, want %d", tip.Header.Height, total)
	}
}
