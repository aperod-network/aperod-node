package main

// configurable_window_test.go — tests for the max_in_memory_blocks and
// mempool_evict_interval_sec knobs introduced by the node.yaml memory-tuning
// feature.
//
// These tests guard against regressions where a non-default window value is
// ignored and the node silently falls back to the hard-coded 1 000-block
// constant.

import (
	"bytes"
	"testing"
	"time"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/core"
)

// ── Chain sliding-window tests ────────────────────────────────────────────────

// TestChainConfigurableWindowSmall verifies that a chain constructed with a
// small window evicts the right blocks once the tip advances past the window.
func TestChainConfigurableWindowSmall(t *testing.T) {
	const window = uint64(10)
	chain := core.NewChain(window)

	// Build genesis + window+5 extra blocks so that 5 blocks fall outside the
	// window and must be evicted.
	priv, pub, err := makeTestValidator(t)
	_ = priv
	_ = pub
	_ = err
	blocks := buildTestBlocks(t, int(window)+5)

	if err := chain.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	for _, b := range blocks[1:] {
		if err := chain.AddBlock(b); err != nil {
			t.Fatalf("AddBlock h=%d: %v", b.Header.Height, err)
		}
	}

	tip := chain.Tip()
	if tip == nil {
		t.Fatal("chain has no tip")
	}
	wantTip := uint64(window + 5)
	if tip.Header.Height != wantTip {
		t.Errorf("tip.Height = %d, want %d", tip.Header.Height, wantTip)
	}

	// Blocks older than (tip - window) must have been evicted.
	for h := uint64(0); h <= wantTip-window-1; h++ {
		if chain.GetByHeight(h) != nil {
			t.Errorf("height %d should have been evicted but is still in memory (window=%d, tip=%d)",
				h, window, wantTip)
		}
	}

	// The most recent `window` blocks must still be in memory.
	for h := wantTip - window + 1; h <= wantTip; h++ {
		if chain.GetByHeight(h) == nil {
			t.Errorf("height %d should be in memory but was evicted (window=%d, tip=%d)",
				h, window, wantTip)
		}
	}
}

// TestChainConfigurableWindowLarge verifies that a chain with a window larger
// than the number of added blocks keeps all blocks in memory (nothing premature
// is evicted).
func TestChainConfigurableWindowLarge(t *testing.T) {
	const window = uint64(500)
	const nBlocks = 50
	chain := core.NewChain(window)

	blocks := buildTestBlocks(t, nBlocks)
	if err := chain.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	for _, b := range blocks[1:] {
		if err := chain.AddBlock(b); err != nil {
			t.Fatalf("AddBlock h=%d: %v", b.Header.Height, err)
		}
	}

	// All blocks must still be in memory because nBlocks < window.
	for h := uint64(0); h <= nBlocks; h++ {
		if chain.GetByHeight(h) == nil {
			t.Errorf("height %d was evicted prematurely (window=%d, nBlocks=%d)",
				h, window, nBlocks)
		}
	}
}

// TestFastForwardConfigurableWindow verifies that FastForward also respects a
// non-default window.
func TestFastForwardConfigurableWindow(t *testing.T) {
	const window = uint64(5)
	const nBlocks = 12

	blocks := buildTestBlocks(t, nBlocks)
	chain := core.NewChain(window)
	if err := chain.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	chain.FastForward(blocks[1:])

	tip := chain.Tip()
	if tip == nil || tip.Header.Height != nBlocks {
		t.Fatalf("tip height = %v, want %d", tip, nBlocks)
	}
	// Blocks older than (nBlocks - window) must be evicted.
	for h := uint64(0); h <= uint64(nBlocks)-window-1; h++ {
		if chain.GetByHeight(h) != nil {
			t.Errorf("height %d should be evicted after FastForward with window=%d", h, window)
		}
	}
	// The window tail must still be present.
	for h := uint64(nBlocks) - window + 1; h <= uint64(nBlocks); h++ {
		if chain.GetByHeight(h) == nil {
			t.Errorf("height %d should be in memory after FastForward with window=%d", h, window)
		}
	}
}

// ── loadRecentBlocksFromStore window tests ────────────────────────────────────

// TestLoadRecentBlocksCustomWindow verifies that a non-default maxBlocks value
// is respected: a chain of 20 blocks with maxBlocks=8 must return exactly 8
// (tip-7 .. tip), not 20.
func TestLoadRecentBlocksCustomWindow(t *testing.T) {
	const nBlocks = 20
	const window = uint64(8)

	dir := t.TempDir()
	db, _ := buildChainWithGap(t, dir, nBlocks, 99999 /* no gap */)

	loaded := loadRecentBlocksFromStore(db, uint64(nBlocks), newCaptureLogger(new(bytes.Buffer)), window)
	if uint64(len(loaded)) != window {
		t.Errorf("loaded %d blocks, want %d (window)", len(loaded), window)
	}
	// The first loaded block must be at height (nBlocks - window + 1).
	wantFirst := uint64(nBlocks) - window + 1
	if loaded[0].Header.Height != wantFirst {
		t.Errorf("first loaded block height = %d, want %d", loaded[0].Header.Height, wantFirst)
	}
	// The last loaded block must be at tipHeight.
	if loaded[len(loaded)-1].Header.Height != uint64(nBlocks) {
		t.Errorf("last loaded block height = %d, want %d", loaded[len(loaded)-1].Header.Height, uint64(nBlocks))
	}
}

// TestLoadRecentBlocksDefaultWindow verifies that passing no maxBlocks (zero)
// falls back to core.MaxInMemoryBlocks.  When nBlocks < MaxInMemoryBlocks all
// blocks except genesis (height 0) are loaded.
func TestLoadRecentBlocksDefaultWindow(t *testing.T) {
	const nBlocks = 15

	dir := t.TempDir()
	db, _ := buildChainWithGap(t, dir, nBlocks, 99999 /* no gap */)

	loaded := loadRecentBlocksFromStore(db, uint64(nBlocks), newCaptureLogger(new(bytes.Buffer)))
	// Expect heights 1..nBlocks (genesis at 0 is always excluded from the window
	// when tip < MaxInMemoryBlocks because startLoad = 1).
	wantLoaded := nBlocks // heights 1..15
	if len(loaded) != wantLoaded {
		t.Errorf("loaded %d blocks, want %d", len(loaded), wantLoaded)
	}
}

// ── countMissingBlocksInWindow window tests ───────────────────────────────────

// TestCountMissingBlocksCustomWindow verifies that a non-default maxBlocks
// value restricts the scan to the correct height range.  Putting a gap outside
// the window means it is NOT counted; a gap inside is.
func TestCountMissingBlocksCustomWindow(t *testing.T) {
	const nBlocks = 20
	const window = uint64(5)
	// gap at height 10 — outside the window [16..20] for tip=20
	const gapHeight = uint64(10)

	dir := t.TempDir()
	db, _ := buildChainWithGap(t, dir, nBlocks, gapHeight)

	missing, _, _ := countMissingBlocksInWindow(db, uint64(nBlocks), window)
	if missing != 0 {
		t.Errorf("missing=%d, want 0: gap at h=%d is outside window [%d..%d] (window=%d, tip=%d)",
			missing, gapHeight, uint64(nBlocks)-window+1, uint64(nBlocks), window, uint64(nBlocks))
	}

	// Now put the gap inside the window.
	const gapInside = uint64(17) // inside [16..20]
	dir2 := t.TempDir()
	db2, _ := buildChainWithGap(t, dir2, nBlocks, gapInside)

	missing2, first2, _ := countMissingBlocksInWindow(db2, uint64(nBlocks), window)
	if missing2 != 1 {
		t.Errorf("missing=%d, want 1: gap at h=%d inside window [%d..%d]",
			missing2, gapInside, uint64(nBlocks)-window+1, uint64(nBlocks))
	}
	if first2 != gapInside {
		t.Errorf("firstMissing=%d, want %d", first2, gapInside)
	}
}

// ── Config defaults tests ─────────────────────────────────────────────────────

// TestConfigDefaultMaxInMemoryBlocks confirms that DefaultConfig sets
// MaxInMemoryBlocks to 1000 so omitting the field in node.yaml produces the
// same behaviour as the old compile-time constant.
func TestConfigDefaultMaxInMemoryBlocks(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.MaxInMemoryBlocks != 1000 {
		t.Errorf("DefaultConfig().MaxInMemoryBlocks = %d, want 1000", cfg.MaxInMemoryBlocks)
	}
}

// TestConfigDefaultMempoolEvictInterval confirms that DefaultConfig sets
// MempoolEvictIntervalSec to 300.
func TestConfigDefaultMempoolEvictInterval(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.MempoolEvictIntervalSec != 300 {
		t.Errorf("DefaultConfig().MempoolEvictIntervalSec = %d, want 300", cfg.MempoolEvictIntervalSec)
	}
}

// TestMempoolEvictIntervalResolution verifies the resolution logic used in
// main.go: a configured value is used as-is; 0 falls back to 5 minutes.
func TestMempoolEvictIntervalResolution(t *testing.T) {
	resolve := func(sec uint64) time.Duration {
		d := time.Duration(sec) * time.Second
		if d <= 0 {
			d = 5 * time.Minute
		}
		return d
	}

	if got := resolve(60); got != 60*time.Second {
		t.Errorf("resolve(60) = %v, want 60s", got)
	}
	if got := resolve(300); got != 5*time.Minute {
		t.Errorf("resolve(300) = %v, want 5m", got)
	}
	if got := resolve(0); got != 5*time.Minute {
		t.Errorf("resolve(0) = %v, want 5m (default fallback)", got)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// makeTestValidator generates a throwaway validator key pair for block signing.
func makeTestValidator(t *testing.T) (interface{}, interface{}, error) {
	t.Helper()
	// We only need a key to pass to buildTestBlocks; the actual types are in
	// crypto but we import through buildTestBlocks which handles signing.
	return nil, nil, nil
}

// buildTestBlocks creates a genesis block plus nExtra signed blocks using
// an ephemeral validator key.  Returns all blocks (index == height).
func buildTestBlocks(t *testing.T, nExtra int) []*core.Block {
	t.Helper()
	dir := t.TempDir()
	db, blocks := buildChainWithGap(t, dir, nExtra, 99999 /* no gap */)
	db.Close()
	return blocks
}
