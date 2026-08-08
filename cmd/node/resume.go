package main

// resume.go — helpers for the node startup block-loading pass.

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/aperod/aperod/core"
)

// blockByHeightGetter is the subset of *store.DB used by the resume pass.
// Defined as an interface so the helper is independently testable.
type blockByHeightGetter interface {
	GetRawBlockByHeight(height uint64) ([]byte, error)
}

// tipAnchorable is the subset of *core.Chain used by anchorTipIfNeeded.
// Defined as an interface so the helper is independently testable.
type tipAnchorable interface {
	Height() uint64
	FastForward(blocks []*core.Block)
}

// loadRecentBlocksFromStore is the production implementation of the startup
// "recentBlocks" loop.  It reads at most core.MaxInMemoryBlocks blocks ending
// at tipHeight, skipping (via continue) any heights that are absent or corrupt
// in the store rather than aborting early.
//
// The continue-on-gap behaviour is the fix for the break regression: with
// break, a single missing block would leave byHeight empty and cause
// header-sync to serve blocks from genesis instead of the correct window.
// anchorTipIfNeeded ensures the in-memory chain tip is at tipHeight.
//
// After loadRecentBlocksFromStore + FastForward the chain tip equals the last
// block that was successfully loaded.  If the window had a gap at tipHeight
// (the tip block itself was absent or corrupt), or if the entire window was
// empty, chain.Height() will be below tipHeight.  Without a correction the
// consensus engine would produce a block at height chain.Height()+1, silently
// branching away from the canonical DB tip.
//
// Fix: fetch the tip block directly from the store and fast-forward it so that
// chain.Height() == tipHeight before the engine starts.
//
// anchorTipIfNeeded is extracted from the inline guard in main.go so it can be
// unit-tested independently.  Reverting the guard to a no-op would immediately
// fail TestTipAnchorGuard*.
func anchorTipIfNeeded(chain tipAnchorable, db blockByHeightGetter, tipHeight uint64, log *slog.Logger) error {
        if chain.Height() >= tipHeight {
                return nil
        }
        tipRaw, tipRawErr := db.GetRawBlockByHeight(tipHeight)
        if tipRawErr != nil || tipRaw == nil {
                return fmt.Errorf("tip block missing from store at height %d — database may be corrupt", tipHeight)
        }
        var tipBlock core.Block
        if jsonErr := json.Unmarshal(tipRaw, &tipBlock); jsonErr != nil {
                return fmt.Errorf("unmarshal tip block at %d: %w", tipHeight, jsonErr)
        }
        chain.FastForward([]*core.Block{&tipBlock})
        log.Info("tip block loaded as in-memory anchor", "height", tipHeight)
        return nil
}

// countMissingBlocksInWindow scans the same height window as
// loadRecentBlocksFromStore (startLoad..tipHeight inclusive) and returns the
// number of heights for which GetRawBlockByHeight returned nil or an error,
// together with the first and last missing heights.  When no blocks are missing
// all three return values are 0.
//
// maxBlocks controls the window size; pass 0 to use core.MaxInMemoryBlocks.
func countMissingBlocksInWindow(db blockByHeightGetter, tipHeight uint64, maxBlocks uint64) (missingCount, firstMissing, lastMissing uint64) {
        maxLoad := maxBlocks
        if maxLoad == 0 {
                maxLoad = uint64(core.MaxInMemoryBlocks)
        }
        startLoad := uint64(1)
        if tipHeight >= maxLoad {
                startLoad = tipHeight - maxLoad + 1
        }
        for h := startLoad; h <= tipHeight; h++ {
                raw, err := db.GetRawBlockByHeight(h)
                if err != nil || raw == nil {
                        missingCount++
                        if firstMissing == 0 {
                                firstMissing = h
                        }
                        lastMissing = h
                }
        }
        return
}

// loadRecentBlocksFromStore reads at most maxBlocks blocks ending at tipHeight,
// skipping (via continue) any heights that are absent or corrupt in the store.
// Pass maxBlocks=0 to use core.MaxInMemoryBlocks (1 000).
func loadRecentBlocksFromStore(db blockByHeightGetter, tipHeight uint64, log *slog.Logger, maxBlocks ...uint64) []*core.Block {
        maxLoad := uint64(core.MaxInMemoryBlocks)
        if len(maxBlocks) > 0 && maxBlocks[0] > 0 {
                maxLoad = maxBlocks[0]
        }
        startLoad := uint64(1)
        if tipHeight >= maxLoad {
                startLoad = tipHeight - maxLoad + 1
        }

        recentBlocks := make([]*core.Block, 0, maxLoad)
        for h := startLoad; h <= tipHeight; h++ {
                raw, err := db.GetRawBlockByHeight(h)
                if err != nil || raw == nil {
                        log.Warn("block missing in store during resume", "height", h)
                        continue // skip sparse gaps; don't abort the whole window
                }
                var b core.Block
                if err := json.Unmarshal(raw, &b); err != nil {
                        log.Warn("block unmarshal failed", "height", h, "err", err)
                        continue
                }
                recentBlocks = append(recentBlocks, &b)
        }
        return recentBlocks
}
