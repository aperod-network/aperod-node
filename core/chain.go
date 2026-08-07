package core

import (
        "fmt"
        "sync"

        "github.com/aperod/aperod/crypto"
)

// MaxInMemoryBlocks is the sliding-window size of blocks kept in RAM.
// Older blocks are evicted to keep memory usage bounded.
// 1 000 blocks at 3 s/block ≈ 50 minutes of history — enough for reorgs.
const MaxInMemoryBlocks = 1_000

// TxLocation records where a transaction lives in the chain.
type TxLocation struct {
        Block    *Block
        TxIndex  int
}

// Chain tracks the canonical chain state: the tip block and the full index of
// known blocks. Reorganizations are handled via Reorg().
type Chain struct {
        mu       sync.RWMutex
        blocks   map[crypto.Hash32]*Block     // recent blocks by hash
        byHeight map[uint64]*Block            // canonical chain: height → block
        txIndex  map[crypto.Hash32]TxLocation // tx hash → location
        tip      *Block
        genesis  *Block
}

// NewChain creates an empty chain.
func NewChain() *Chain {
        return &Chain{
                blocks:   make(map[crypto.Hash32]*Block),
                byHeight: make(map[uint64]*Block),
                txIndex:  make(map[crypto.Hash32]TxLocation),
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
        c.indexTxs(b)
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
        c.indexTxs(b)

        // Evict the block that has fallen outside the sliding window.
        if b.Header.Height >= MaxInMemoryBlocks {
                evictH := b.Header.Height - MaxInMemoryBlocks
                if old, ok := c.byHeight[evictH]; ok {
                        delete(c.byHeight, evictH)
                        delete(c.blocks, old.Hash())
                        for _, tx := range old.Txs {
                                delete(c.txIndex, tx.Hash())
                        }
                }
        }

        return nil
}

// FastForward installs a slice of pre-validated blocks into the chain without
// checking parent-hash contiguity. Used during startup to restore only the
// recent window of blocks from the store, skipping the full historical replay.
// Blocks must be sorted by ascending height; the last block becomes the tip.
//
// The same MaxInMemoryBlocks sliding-window eviction as AddBlock is applied so
// that large catch-up batches (e.g. P2P sync of 10 000+ blocks) do not
// accumulate unboundedly in RAM.
func (c *Chain) FastForward(blocks []*Block) {
        if len(blocks) == 0 {
                return
        }
        c.mu.Lock()
        defer c.mu.Unlock()
        for _, b := range blocks {
                h := b.Hash()
                c.blocks[h] = b
                c.byHeight[b.Header.Height] = b
                c.tip = b
                c.indexTxs(b)
                // Evict the block that has fallen outside the sliding window,
                // mirroring AddBlock so memory stays bounded during bulk loads.
                if b.Header.Height >= MaxInMemoryBlocks {
                        evictH := b.Header.Height - MaxInMemoryBlocks
                        if old, ok := c.byHeight[evictH]; ok {
                                delete(c.byHeight, evictH)
                                delete(c.blocks, old.Hash())
                                for _, tx := range old.Txs {
                                        delete(c.txIndex, tx.Hash())
                                }
                        }
                }
        }
}

// TxIndexEntry is a pre-built tx location record used by FastForwardWithIndex
// to avoid recomputing tx.Hash() at startup.
type TxIndexEntry struct {
        Height uint64
        TxIdx  int
}

// FastForwardWithIndex is like FastForward but skips the expensive tx.Hash()
// recomputation. It accepts a pre-built index (txHash → TxIndexEntry) loaded
// from the persistent store, resolving each entry to a Block pointer from the
// blocks being loaded. Entries that don't resolve (height outside the window)
// are silently skipped.
//
// As with FastForward, the MaxInMemoryBlocks sliding-window eviction is applied
// during the first pass so memory stays bounded.
func (c *Chain) FastForwardWithIndex(blocks []*Block, txEntries map[crypto.Hash32]TxIndexEntry) {
        if len(blocks) == 0 {
                return
        }
        c.mu.Lock()
        defer c.mu.Unlock()
        // First pass: populate blocks and byHeight maps with sliding-window eviction.
        for _, b := range blocks {
                h := b.Hash()
                c.blocks[h] = b
                c.byHeight[b.Header.Height] = b
                c.tip = b
                if b.Header.Height >= MaxInMemoryBlocks {
                        evictH := b.Header.Height - MaxInMemoryBlocks
                        if old, ok := c.byHeight[evictH]; ok {
                                delete(c.byHeight, evictH)
                                delete(c.blocks, old.Hash())
                                for _, tx := range old.Txs {
                                        delete(c.txIndex, tx.Hash())
                                }
                        }
                }
        }
        // Second pass: populate tx index from pre-built entries.
        // Entries whose block was evicted above are silently skipped.
        for txHash, entry := range txEntries {
                blk := c.byHeight[entry.Height]
                if blk == nil {
                        continue
                }
                if entry.TxIdx < 0 || entry.TxIdx >= len(blk.Txs) {
                        continue
                }
                c.txIndex[txHash] = TxLocation{Block: blk, TxIndex: entry.TxIdx}
        }
}

// indexTxs adds all transactions in b to the tx index. Caller must hold mu.
func (c *Chain) indexTxs(b *Block) {
        for i, tx := range b.Txs {
                h := tx.Hash()
                c.txIndex[h] = TxLocation{Block: b, TxIndex: i}
        }
}

// GetTransaction looks up a transaction by its hash in the canonical chain.
// Returns (tx, location, true) if found, or zero values and false otherwise.
func (c *Chain) GetTransaction(hash crypto.Hash32) (Transaction, TxLocation, bool) {
        c.mu.RLock()
        defer c.mu.RUnlock()
        loc, ok := c.txIndex[hash]
        if !ok {
                return Transaction{}, TxLocation{}, false
        }
        return loc.Block.Txs[loc.TxIndex], loc, true
}

// BlockPruner is the interface used by PruneOldData to strip transaction data
// from blocks older than a given height.  *store.DB satisfies this interface.
type BlockPruner interface {
	PruneBlocksOlderThan(pruneBelow uint64) (int, error)
}

// PruneOldData removes full transaction data (RingCT signatures, Bulletproofs)
// from blocks older than keepBlocks heights relative to the current tip.  The
// block headers and height index remain intact so chain integrity is not
// affected.  UTXO set and key-image records are never touched.
//
// Safe to call from a background goroutine; it reads the tip under its own
// lock and then delegates the disk operation to pruner.
//
// Returns the number of blocks whose transaction data was erased, or 0 if
// pruning is not yet due (tip too low).
func (c *Chain) PruneOldData(pruner BlockPruner, keepBlocks uint64) (int, error) {
	tip := c.Tip()
	if tip == nil || tip.Header.Height <= keepBlocks {
		return 0, nil
	}
	pruneBelow := tip.Header.Height - keepBlocks
	return pruner.PruneBlocksOlderThan(pruneBelow)
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

        // Remove old canonical blocks above fork point from both indexes.
        oldTip := c.tip
        for h := oldTip.Header.Height; h > forkPoint; h-- {
                if old, ok := c.byHeight[h]; ok {
                        delete(c.blocks, old.Hash())
                        delete(c.byHeight, h)
                        // Remove tx index entries for the orphaned block.
                        for _, tx := range old.Txs {
                                delete(c.txIndex, tx.Hash())
                        }
                }
        }

        // Install new blocks and index their transactions so GetTransaction works.
        // Apply the same MaxInMemoryBlocks sliding-window eviction as AddBlock so
        // a reorg that brings in many blocks does not inflate the maps unboundedly.
        for _, b := range newBlocks {
                h := b.Hash()
                c.blocks[h] = b
                c.byHeight[b.Header.Height] = b
                c.indexTxs(b)
                if b.Header.Height >= MaxInMemoryBlocks {
                        evictH := b.Header.Height - MaxInMemoryBlocks
                        if old, ok := c.byHeight[evictH]; ok {
                                delete(c.byHeight, evictH)
                                delete(c.blocks, old.Hash())
                                for _, tx := range old.Txs {
                                        delete(c.txIndex, tx.Hash())
                                }
                        }
                }
        }
        c.tip = newBlocks[len(newBlocks)-1]
        return nil
}

// HeadersFrom returns up to limit block headers that come after the highest
// block in knownHashes that is present in the canonical chain.  This is the
// standard blockchain sync handshake: the remote sends its tail hashes, we
// find the common ancestor and return the headers they are missing.
//
// If none of the knownHashes are found the method returns headers from height 1
// (skipping genesis, which both parties are expected to share).
// Implements the p2p.HeaderProvider interface.
func (c *Chain) HeadersFrom(knownHashes []crypto.Hash32, limit int) []BlockHeader {
        c.mu.RLock()
        defer c.mu.RUnlock()

        if c.tip == nil {
                return nil
        }

        // Find the highest canonical height that matches any knownHash.
        startHeight := uint64(1) // default: send from block 1 (skip genesis)
        for _, h := range knownHashes {
                if b, ok := c.blocks[h]; ok {
                        // Confirm this block is on the canonical chain.
                        if canon, ok2 := c.byHeight[b.Header.Height]; ok2 && canon.Hash() == h {
                                next := b.Header.Height + 1
                                if next > startHeight {
                                        startHeight = next
                                }
                        }
                }
        }

        if startHeight > c.tip.Header.Height {
                return nil // already in sync
        }

        if limit <= 0 || limit > 500 {
                limit = 500
        }

        headers := make([]BlockHeader, 0, limit)
        for h := startHeight; h <= c.tip.Header.Height && len(headers) < limit; h++ {
                if b, ok := c.byHeight[h]; ok {
                        headers = append(headers, b.Header)
                }
        }
        return headers
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
