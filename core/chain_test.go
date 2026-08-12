package core_test

// chain_test.go — tests for Chain sliding-window memory eviction and reorg
// handling after cache eviction.
//
// Tests:
//  1. After adding MaxInMemoryBlocks+N blocks, the oldest N blocks are evicted.
//  2. The most recent MaxInMemoryBlocks blocks remain accessible via GetByHeight.
//  3. The chain tip is correct after eviction.
//  4. Reorg succeeds when the fork-point block has already been evicted from
//     the in-memory cache (covers the LevelDB fall-through path).

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

	// Verify: the in-memory block count equals EXACTLY the window size — the
	// cap holds, with no hidden accumulation in the blocks map.
	if got := c.InMemoryBlockCount(); got != core.MaxInMemoryBlocks {
		t.Errorf("InMemoryBlockCount = %d, want exactly %d after eviction", got, core.MaxInMemoryBlocks)
	}
}

// TestChain_MemoryCapConfigurableWindow (#462) — a custom (smaller) window
// passed to NewChain is enforced exactly, proving the cap logic follows the
// configured value rather than the compile-time constant.
func TestChain_MemoryCapConfigurableWindow(t *testing.T) {
	const window = 25
	const extra = 7
	const total = window + extra

	c := core.NewChain(window)
	genesis := makeMinimalBlock(0, crypto.Hash32{})
	if err := c.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

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

	if got := c.InMemoryBlockCount(); got != window {
		t.Errorf("InMemoryBlockCount = %d, want exactly %d (configured window)", got, window)
	}
	// Oldest surviving height is total-window+1; everything below is evicted.
	for h := uint64(0); h <= uint64(total-window); h++ {
		if c.GetByHash(hashes[h]) != nil {
			t.Errorf("evicted block at height %d still reachable by hash", h)
		}
	}
}

// TestChain_FastForwardMemoryCap (#462) — bulk-loading more blocks than the
// window via FastForward (the startup / P2P catch-up path) must apply the
// same eviction so memory stays bounded.
func TestChain_FastForwardMemoryCap(t *testing.T) {
	const window = 30
	const total = window + 12

	c := core.NewChain(window)
	genesis := makeMinimalBlock(0, crypto.Hash32{})
	if err := c.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	blocks := make([]*core.Block, 0, total)
	prev := genesis
	for h := uint64(1); h <= uint64(total); h++ {
		b := makeMinimalBlock(h, prev.Hash())
		blocks = append(blocks, b)
		prev = b
	}
	c.FastForward(blocks)

	if c.Height() != uint64(total) {
		t.Errorf("Height after FastForward = %d, want %d", c.Height(), total)
	}
	if got := c.InMemoryBlockCount(); got != window {
		t.Errorf("InMemoryBlockCount after FastForward = %d, want exactly %d", got, window)
	}
	if c.GetByHeight(0) != nil {
		t.Error("genesis should have been evicted by the FastForward sliding window")
	}
	if c.GetByHeight(uint64(total)) == nil {
		t.Error("tip block missing after FastForward")
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

// TestChain_ReorgAfterCacheEviction verifies that Reorg succeeds when the
// fork-point block has already been evicted from the in-memory sliding-window
// cache.  This exercises the code path that would fall back to the on-disk
// store in a full node, ensuring that reducing MaxInMemoryBlocks from 10 000
// to 1 000 does not silently break reorg processing for deep forks.
//
// Setup:
//   - Build a canonical chain of MaxInMemoryBlocks+forkPoint+1 blocks so that
//     the block at forkPoint is guaranteed to have been evicted from c.blocks
//     and c.byHeight before the reorg is attempted.
//   - Fork point is chosen well inside the evicted region (50 blocks below the
//     eviction boundary) to give a clear margin.
//   - Five alternate blocks are built on top of the (evicted) fork-point block.
//
// Assertions:
//  1. Reorg returns nil — the chain accepts the alternate branch.
//  2. Tip advances to the last alternate block.
//  3. Old canonical blocks above the fork point that were in the cache are
//     removed (GetByHeight returns nil for a sample height in that range).
//  4. The alternate blocks are installed and accessible via GetByHeight.
//  5. The fork-point block itself remains unavailable (still evicted).
func TestChain_ReorgAfterCacheEviction(t *testing.T) {
	// forkPoint is the height at which the alternate chain branches off.
	// We pick 50 so there is a comfortable margin inside the evicted region.
	const forkPoint = uint64(50)
	// altLen is the number of blocks in the alternate branch.
	const altLen = 5

	// Build the canonical chain long enough to evict the block at forkPoint.
	// Adding block at height H evicts the block at H-MaxInMemoryBlocks.
	// Block at forkPoint is evicted when we add height = forkPoint+MaxInMemoryBlocks.
	// We add a few extra blocks beyond that to confirm eviction is stable.
	const totalHeight = forkPoint + core.MaxInMemoryBlocks + 10

	// Use a minimal genesis (no validator key needed; SetGenesis only checks
	// height==0 and PrevHash=={}).
	genesis := makeMinimalBlock(0, crypto.Hash32{})

	c := core.NewChain()
	if err := c.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	// Walk the chain, recording the hash of the block at forkPoint so we can
	// build a valid alternate branch from it.
	var forkPointHash crypto.Hash32
	prev := genesis
	for h := uint64(1); h <= totalHeight; h++ {
		b := makeMinimalBlock(h, prev.Hash())
		if err := c.AddBlock(b); err != nil {
			t.Fatalf("AddBlock(height=%d): %v", h, err)
		}
		if h == forkPoint {
			forkPointHash = b.Hash()
		}
		prev = b
	}

	// Confirm the fork-point block has been evicted from the in-memory cache.
	if c.GetByHeight(forkPoint) != nil {
		t.Fatalf("expected block at height %d to be evicted before reorg, but it is still in cache", forkPoint)
	}

	// Build the alternate branch: altLen blocks rooted at forkPoint.
	// Each block's PrevHash connects to its predecessor within the alt branch;
	// the first block's PrevHash connects to the (evicted) canonical block at
	// forkPoint, establishing the correct ancestry even though that block is
	// no longer in RAM.
	altBlocks := make([]*core.Block, altLen)
	altPrev := forkPointHash
	for i := 0; i < altLen; i++ {
		h := forkPoint + uint64(i) + 1
		blk := makeMinimalBlock(h, altPrev)
		altBlocks[i] = blk
		altPrev = blk.Hash()
	}

	// Perform the reorg — must succeed even though the fork-point block is
	// no longer in the in-memory cache.
	if err := c.Reorg(forkPoint, altBlocks); err != nil {
		t.Fatalf("Reorg(forkPoint=%d, altLen=%d): unexpected error: %v", forkPoint, altLen, err)
	}

	// 1. Tip must be the last alternate block.
	wantTipHeight := forkPoint + altLen
	tip := c.Tip()
	if tip == nil {
		t.Fatal("Tip() is nil after reorg")
	}
	if tip.Header.Height != wantTipHeight {
		t.Errorf("Tip height after reorg: got %d, want %d", tip.Header.Height, wantTipHeight)
	}

	// 2. A canonical block from the old branch that was in the cache before the
	//    reorg (e.g. the previous tip) must have been removed.
	if c.GetByHeight(totalHeight) != nil {
		t.Errorf("old tip at height %d should be removed after reorg, but GetByHeight returned a block", totalHeight)
	}

	// 3. The alternate blocks must now be the canonical chain at their heights.
	for i, b := range altBlocks {
		h := forkPoint + uint64(i) + 1
		got := c.GetByHeight(h)
		if got == nil {
			t.Errorf("alt block at height %d not found after reorg", h)
			continue
		}
		if got.Hash() != b.Hash() {
			t.Errorf("height %d: canonical block hash mismatch after reorg", h)
		}
	}

	// 4. The fork-point block itself must still be absent from the in-memory
	//    cache (it was evicted before the reorg and Reorg does not re-insert it).
	if c.GetByHeight(forkPoint) != nil {
		t.Errorf("fork-point block at height %d should remain evicted after reorg", forkPoint)
	}
}
