package core

import (
	"fmt"
	"sync"

	"github.com/aperod/aperod/crypto"
)

// Chain tracks the canonical chain state: the tip block and the full index of
// known blocks. Reorganizations are handled via Reorg().
type Chain struct {
	mu     sync.RWMutex
	blocks map[crypto.Hash32]*Block // all known blocks by hash
	byHeight map[uint64]*Block      // canonical chain: height → block
	tip    *Block
	genesis *Block
}

// NewChain creates an empty chain.
func NewChain() *Chain {
	return &Chain{
		blocks:   make(map[crypto.Hash32]*Block),
		byHeight: make(map[uint64]*Block),
	}
}

// SetGenesis installs the genesis block. Must be called once before any other method.
func (c *Chain) SetGenesis(b *Block) error {
	if !b.IsGenesis() {
		return fmt.Errorf("block at height %d is not a genesis block", b.Header.Height)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := b.Hash()
	c.blocks[h] = b
	c.byHeight[0] = b
	c.tip = b
	c.genesis = b
	return nil
}

// Tip returns the current canonical chain tip (highest finalized block).
func (c *Chain) Tip() *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tip
}

// Height returns the current chain height.
func (c *Chain) Height() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tip == nil {
		return 0
	}
	return c.tip.Header.Height
}

// Genesis returns the genesis block.
func (c *Chain) Genesis() *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.genesis
}

// GetByHash retrieves a block by its hash. Returns nil if unknown.
func (c *Chain) GetByHash(h crypto.Hash32) *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blocks[h]
}

// GetByHeight retrieves the canonical block at height h. Returns nil if not yet known.
func (c *Chain) GetByHeight(h uint64) *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byHeight[h]
}

// HasBlock returns true if the block with this hash is already known.
func (c *Chain) HasBlock(h crypto.Hash32) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.blocks[h]
	return ok
}

// AddBlock appends a validated block to the canonical chain.
// The block must extend the current tip.
func (c *Chain) AddBlock(b *Block) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.tip == nil {
		return fmt.Errorf("chain not initialized: call SetGenesis first")
	}
	if b.Header.Height != c.tip.Header.Height+1 {
		return fmt.Errorf("block height %d does not extend tip at %d",
			b.Header.Height, c.tip.Header.Height)
	}
	if b.Header.PrevHash != c.tip.Hash() {
		return fmt.Errorf("block %d: prev hash mismatch", b.Header.Height)
	}

	h := b.Hash()
	c.blocks[h] = b
	c.byHeight[b.Header.Height] = b
	c.tip = b
	return nil
}

// Reorg replaces the canonical chain from forkPoint+1 onwards with newBlocks.
// newBlocks must be contiguous, validated, and have higher cumulative weight.
func (c *Chain) Reorg(forkPoint uint64, newBlocks []*Block) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(newBlocks) == 0 {
		return fmt.Errorf("reorg: no new blocks")
	}
	if newBlocks[0].Header.Height != forkPoint+1 {
		return fmt.Errorf("reorg: new chain starts at %d, expected %d",
			newBlocks[0].Header.Height, forkPoint+1)
	}

	// Verify contiguity
	for i := 1; i < len(newBlocks); i++ {
		if newBlocks[i].Header.Height != newBlocks[i-1].Header.Height+1 {
			return fmt.Errorf("reorg: gap between blocks %d and %d",
				newBlocks[i-1].Header.Height, newBlocks[i].Header.Height)
		}
		if newBlocks[i].Header.PrevHash != newBlocks[i-1].Hash() {
			return fmt.Errorf("reorg: prev hash mismatch at height %d",
				newBlocks[i].Header.Height)
		}
	}

	// Remove old canonical blocks above fork point
	oldTip := c.tip
	for h := oldTip.Header.Height; h > forkPoint; h-- {
		delete(c.byHeight, h)
	}

	// Install new blocks
	for _, b := range newBlocks {
		h := b.Hash()
		c.blocks[h] = b
		c.byHeight[b.Header.Height] = b
	}
	c.tip = newBlocks[len(newBlocks)-1]
	return nil
}

// TailHashes returns the hashes of the last n canonical blocks (for sync requests).
func (c *Chain) TailHashes(n int) []crypto.Hash32 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	tip := c.tip
	if tip == nil {
		return nil
	}

	start := int(tip.Header.Height) - n + 1
	if start < 0 {
		start = 0
	}
	out := make([]crypto.Hash32, 0, n)
	for h := uint64(start); h <= tip.Header.Height; h++ {
		if b, ok := c.byHeight[h]; ok {
			out = append(out, b.Hash())
		}
	}
	return out
}
