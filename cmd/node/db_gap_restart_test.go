package main

// TestDBGapRestartLoadsRecentBlocks confirms that the startup resume scan
// tolerates a deliberate gap in the block store.
//
// Background: the recentBlocks loop in main.go used to call `break` when
// db.GetRawBlockByHeight returned an error or nil, leaving c.byHeight empty
// and causing header-sync to serve headers from genesis instead of the correct
// sliding window.  The fix changed `break` → `continue` and extracted the loop
// into loadRecentBlocksFromStore (resume.go).
//
// These tests call loadRecentBlocksFromStore directly so that reverting the
// production helper to `break` would immediately break the tests.
//
// The tests verify:
//  1. A chain with a deliberate gap (block 5 missing out of 0–9) still loads
//     all available blocks — blocks_loaded_in_memory > 0.
//  2. FastForwarding the loaded slice into a Chain leaves the tip at the
//     correct height.
//  3. A second "syncing" node can call HeadersFrom and receive headers that
//     reach past the gap height.
//  4. When the gap is at the very first height the loop visits, the helper
//     still loads the blocks that follow it.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// buildChainWithGap creates a chain of genesis + nBlocks blocks in a LevelDB
// store, intentionally omitting the block at gapHeight from the store (the
// block object is still created so subsequent blocks have a real PrevHash).
// The DB tip is set to the last block.  Returns the open store and a slice of
// all created blocks (index i == height i).
func buildChainWithGap(t *testing.T, dir string, nBlocks int, gapHeight uint64) (*store.DB, []*core.Block) {
	t.Helper()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	makeBlock := func(height uint64, prevHash crypto.Hash32) *core.Block {
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prevHash,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		}
		if signErr := hdr.Sign(priv); signErr != nil {
			t.Fatalf("block Sign height=%d: %v", height, signErr)
		}
		return &core.Block{Header: hdr}
	}

	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	blocks := make([]*core.Block, nBlocks+1)
	genesis := makeBlock(0, crypto.Hash32{})
	blocks[0] = genesis

	storeBlk := func(b *core.Block) {
		raw, merr := json.Marshal(b)
		if merr != nil {
			t.Fatalf("marshal block h=%d: %v", b.Header.Height, merr)
		}
		h := b.Hash()
		if perr := db.PutRawBlock(h, b.Header.Height, raw); perr != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, perr)
		}
	}
	storeBlk(genesis)

	prevBlock := genesis
	for i := 1; i <= nBlocks; i++ {
		h := uint64(i)
		blk := makeBlock(h, prevBlock.Hash())
		blocks[i] = blk
		if h != gapHeight {
			storeBlk(blk)
		}
		prevBlock = blk
	}

	// Set tip to the last block (height nBlocks).
	tip := blocks[nBlocks]
	tipHash := tip.Hash()
	if err := db.PutTip(tipHash, uint64(nBlocks)); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	return db, blocks
}

// TestDBGapRestartLoadsRecentBlocks is the primary regression guard.
// It calls the production helper loadRecentBlocksFromStore (resume.go) so
// that reverting the helper to `break` would immediately fail this test.
func TestDBGapRestartLoadsRecentBlocks(t *testing.T) {
	const (
		nBlocks   = 9 // heights 0 – 9
		gapHeight = 5 // block 5 is absent from the store
	)
	tipHeight := uint64(nBlocks)

	dir := t.TempDir()
	db, blocks := buildChainWithGap(t, dir, nBlocks, gapHeight)

	// ── Call the PRODUCTION helper (resume.go) ──────────────────────────────
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	recentBlocks := loadRecentBlocksFromStore(db, tipHeight, log)

	// ── Requirement 3: blocks_loaded_in_memory > 0 ──────────────────────────
	if len(recentBlocks) == 0 {
		t.Fatalf("blocks_loaded_in_memory == 0: production helper aborted at the gap (break regression)\nlog:\n%s", logBuf.String())
	}
	t.Logf("blocks_loaded_in_memory = %d (gap at height %d, tip %d)", len(recentBlocks), gapHeight, tipHeight)

	// We expect exactly nBlocks-1 blocks: heights 1–9 minus the gap at 5.
	wantLoaded := nBlocks - 1
	if len(recentBlocks) != wantLoaded {
		t.Errorf("blocks_loaded_in_memory = %d, want %d", len(recentBlocks), wantLoaded)
	}

	// The gap must have been logged as a warning.
	if !logContainsMsg(&logBuf, "block missing in store during resume") {
		t.Errorf("expected 'block missing in store during resume' warning was not logged\nlog:\n%s", logBuf.String())
	}

	// ── FastForward a chain to simulate the node's in-memory state ──────────
	chain := core.NewChain()
	genesis := blocks[0]
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	chain.FastForward(recentBlocks)

	// The chain tip must be at tipHeight, not frozen before the gap.
	if tip := chain.Tip(); tip.Header.Height != tipHeight {
		t.Errorf("chain tip height = %d, want %d", tip.Header.Height, tipHeight)
	}

	// ── Requirement 4: header-sync past the gap height ──────────────────────
	// Simulate a second "syncing" node that only knows blocks up to height 4
	// (just before the gap).
	syncingChain := core.NewChain()
	if err := syncingChain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis (syncingChain): %v", err)
	}
	var preGap []*core.Block
	for h := 1; h < int(gapHeight); h++ {
		preGap = append(preGap, blocks[h])
	}
	syncingChain.FastForward(preGap)

	knownHash := syncingChain.Tip().Hash()
	headers := chain.HeadersFrom([]crypto.Hash32{knownHash}, 500)

	if len(headers) == 0 {
		t.Fatal("HeadersFrom returned 0 headers: the chain cannot serve blocks past the gap height")
	}

	maxServed := uint64(0)
	for _, hdr := range headers {
		if hdr.Height > maxServed {
			maxServed = hdr.Height
		}
	}
	if maxServed <= gapHeight {
		t.Errorf("HeadersFrom served max height %d, want > %d (past the gap)", maxServed, gapHeight)
	}
	t.Logf("HeadersFrom served %d header(s), max height = %d", len(headers), maxServed)
}

// ── Tip-anchor guard tests ────────────────────────────────────────────────────
//
// anchorTipIfNeeded (resume.go) is the second bug fix: even when FastForward
// leaves the in-memory chain tip below tipHeight (e.g. the window was empty or
// the tip block itself was absent from the recent-blocks slice), the guard
// fetches the tip block directly from the store and fast-forwards it so that
// chain.Height() == tipHeight before the engine starts.
//
// These tests call anchorTipIfNeeded directly so that removing or neutering the
// guard in resume.go immediately breaks the test suite.

// filteredGetter wraps a blockByHeightGetter and hides one specific height,
// letting the test simulate a store that has a block in the tip slot but not
// in the window visible to loadRecentBlocksFromStore.
type filteredGetter struct {
        inner      blockByHeightGetter
        hideHeight uint64
}

func (f *filteredGetter) GetRawBlockByHeight(h uint64) ([]byte, error) {
        if h == f.hideHeight {
                return nil, nil
        }
        return f.inner.GetRawBlockByHeight(h)
}

// stubGetter is a minimal blockByHeightGetter backed by an in-memory map.
// Used by anchor-guard unit tests that don't need a full LevelDB store.
type stubGetter struct {
        blocks map[uint64][]byte
}

func (s *stubGetter) GetRawBlockByHeight(h uint64) ([]byte, error) {
        b, ok := s.blocks[h]
        if !ok {
                return nil, nil
        }
        return b, nil
}

// TestTipAnchorGuardNoopWhenTipIsCorrect verifies that anchorTipIfNeeded is a
// no-op when the chain height already matches tipHeight (the common case after a
// clean restart with no missing blocks).
func TestTipAnchorGuardNoopWhenTipIsCorrect(t *testing.T) {
        const tipHeight = uint64(5)
        dir := t.TempDir()
        db, blocks := buildChainWithGap(t, dir, int(tipHeight), 999 /* no gap */)

        chain := core.NewChain()
        if err := chain.SetGenesis(blocks[0]); err != nil {
                t.Fatalf("SetGenesis: %v", err)
        }
        // Load all blocks (no gap) and fast-forward.
        loaded := loadRecentBlocksFromStore(db, tipHeight, newCaptureLogger(new(bytes.Buffer)))
        chain.FastForward(loaded)

        if chain.Height() != tipHeight {
                t.Fatalf("pre-condition: chain.Height() = %d, want %d", chain.Height(), tipHeight)
        }

        var logBuf bytes.Buffer
        err := anchorTipIfNeeded(chain, db, tipHeight, newCaptureLogger(&logBuf))
        if err != nil {
                t.Fatalf("anchorTipIfNeeded returned unexpected error: %v", err)
        }
        if chain.Height() != tipHeight {
                t.Errorf("chain.Height() = %d after no-op, want %d", chain.Height(), tipHeight)
        }
        // The guard must not have logged the "anchor" message (it was a no-op).
        if logContainsMsg(&logBuf, "tip block loaded as in-memory anchor") {
                t.Error("anchor log message found but guard should have been a no-op")
        }
}

// TestTipAnchorGuardLoadsAndAnchorsTipBlock is the primary regression test for
// the tip-anchor guard.  It simulates the scenario where loadRecentBlocksFromStore
// returns a slice whose last block is below tipHeight (because the tip block was
// absent from the store during the window scan), while the tip block itself is
// available via a direct height lookup.
//
// Concrete setup:
//   - 10 blocks (heights 0–9), tipHeight = 9.
//   - A filteredGetter hides height 9 from loadRecentBlocksFromStore, so only
//     blocks 1–8 are returned and FastForward leaves chain.Height() == 8.
//   - anchorTipIfNeeded is given the unfiltered store (block 9 present) and must
//     advance the chain to height 9.
func TestTipAnchorGuardLoadsAndAnchorsTipBlock(t *testing.T) {
        const (
                nBlocks   = 9
                tipHeight = uint64(nBlocks)
        )
        dir := t.TempDir()
        db, blocks := buildChainWithGap(t, dir, nBlocks, 999 /* no gap in store */)

        // Phase 1: simulate loadRecentBlocksFromStore with the tip block hidden.
        filtered := &filteredGetter{inner: db, hideHeight: tipHeight}
        loaded := loadRecentBlocksFromStore(filtered, tipHeight, newCaptureLogger(new(bytes.Buffer)))

        chain := core.NewChain()
        if err := chain.SetGenesis(blocks[0]); err != nil {
                t.Fatalf("SetGenesis: %v", err)
        }
        chain.FastForward(loaded)

        // Pre-condition: tip must be below tipHeight to trigger the guard.
        if chain.Height() >= tipHeight {
                t.Fatalf("pre-condition failed: chain.Height() = %d, guard would be a no-op", chain.Height())
        }

        // Phase 2: call the guard with the full (unfiltered) store.
        var logBuf bytes.Buffer
        if err := anchorTipIfNeeded(chain, db, tipHeight, newCaptureLogger(&logBuf)); err != nil {
                t.Fatalf("anchorTipIfNeeded: %v", err)
        }

        // The chain tip must now equal tipHeight.
        if chain.Height() != tipHeight {
                t.Errorf("chain.Height() = %d after anchor, want %d", chain.Height(), tipHeight)
        }

        // The guard must have logged that it fired.
        if !logContainsMsg(&logBuf, "tip block loaded as in-memory anchor") {
                t.Errorf("expected 'tip block loaded as in-memory anchor' log not found\nlog:\n%s", logBuf.String())
        }
}

// TestTipAnchorGuardErrorsWhenTipBlockMissing verifies that anchorTipIfNeeded
// returns a descriptive error (rather than silently anchoring at the wrong
// height) when the chain is below tipHeight AND the tip block is absent from
// the store — indicating a corrupt or truncated database.
func TestTipAnchorGuardErrorsWhenTipBlockMissing(t *testing.T) {
        const tipHeight = uint64(9)

        // Chain at height 8 (below tipHeight).
        dir := t.TempDir()
        db, blocks := buildChainWithGap(t, dir, 8 /* only build 8 blocks */, 999)

        chain := core.NewChain()
        if err := chain.SetGenesis(blocks[0]); err != nil {
                t.Fatalf("SetGenesis: %v", err)
        }
        loaded := loadRecentBlocksFromStore(db, 8, newCaptureLogger(new(bytes.Buffer)))
        chain.FastForward(loaded)

        // chain.Height() == 8 < tipHeight(9); block 9 is not in the store.
        err := anchorTipIfNeeded(chain, db, tipHeight, newCaptureLogger(new(bytes.Buffer)))
        if err == nil {
                t.Fatal("expected an error when tip block is missing from store, got nil")
        }
        t.Logf("anchorTipIfNeeded correctly returned error: %v", err)
}

// TestTipAnchorGuardEndToEnd simulates the full startup sequence after a gap is
// introduced at the tip position and then the tip block is recovered.
//
// Flow:
//  1. Build a 10-block chain (heights 0–9); all stored in DB.
//  2. Use filteredGetter to hide height 9 during loadRecentBlocksFromStore
//     → recentBlocks = [b1..b8]; FastForward → chain.Height() == 8.
//  3. anchorTipIfNeeded with the real DB (block 9 present)
//     → chain.Height() == 9 (the original tip height is restored).
//
// This test guards against the regression where the consensus engine would
// start producing block 9 again from the wrong parent, silently forking the
// canonical chain.
func TestTipAnchorGuardEndToEnd(t *testing.T) {
        const (
                nBlocks   = 9
                tipHeight = uint64(nBlocks)
        )
        dir := t.TempDir()
        db, blocks := buildChainWithGap(t, dir, nBlocks, 999 /* all blocks stored */)

        chain := core.NewChain()
        if err := chain.SetGenesis(blocks[0]); err != nil {
                t.Fatalf("SetGenesis: %v", err)
        }

        // Simulate the window scan with the tip block hidden (gap at tip).
        filtered := &filteredGetter{inner: db, hideHeight: tipHeight}
        var scanLog bytes.Buffer
        recentBlocks := loadRecentBlocksFromStore(filtered, tipHeight, newCaptureLogger(&scanLog))
        chain.FastForward(recentBlocks)

        heightAfterFF := chain.Height()
        if heightAfterFF >= tipHeight {
                t.Fatalf("pre-condition: chain.Height() = %d, guard would be a no-op; tip must be < %d", heightAfterFF, tipHeight)
        }
        t.Logf("chain.Height() after FastForward = %d (gap at tip simulated)", heightAfterFF)

        // The guard must restore the original tip height.
        var anchorLog bytes.Buffer
        if err := anchorTipIfNeeded(chain, db, tipHeight, newCaptureLogger(&anchorLog)); err != nil {
                t.Fatalf("anchorTipIfNeeded: %v", err)
        }

        if chain.Height() != tipHeight {
                t.Errorf("chain.Height() = %d after anchorTipIfNeeded, want %d (original tip)",
                        chain.Height(), tipHeight)
        }
        if !logContainsMsg(&anchorLog, "tip block loaded as in-memory anchor") {
                t.Errorf("guard log message not found\nlog:\n%s", anchorLog.String())
        }
        t.Logf("node successfully anchored to height %d after gap at tip", tipHeight)
}

// TestDBGapAtStartOfWindow confirms the helper loads blocks that follow a gap
// at the very first height the loop visits (the most dangerous regression
// scenario: with the old `break` the loop would abort immediately and return
// nothing at all).
func TestDBGapAtStartOfWindow(t *testing.T) {
	const nBlocks = 9
	tipHeight := uint64(nBlocks)
	// The loop visits heights 1..tipHeight (genesis is excluded from maxLoad
	// window when tipHeight < MaxInMemoryBlocks, startLoad = 1).
	// Putting the gap at height 1 means the very first read returns nil.
	const gapHeight = uint64(1)

	dir := t.TempDir()
	db, blocks := buildChainWithGap(t, dir, nBlocks, gapHeight)

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	// Call the PRODUCTION helper directly.
	recentBlocks := loadRecentBlocksFromStore(db, tipHeight, log)

	// With `continue` the helper skips height 1 and loads heights 2–9 (8 blocks).
	// With `break` it would return 0 blocks.
	wantLoaded := nBlocks - 1 // heights 2..9
	if len(recentBlocks) != wantLoaded {
		t.Errorf("blocks_loaded_in_memory = %d, want %d (gap at first window height)\nlog:\n%s",
			len(recentBlocks), wantLoaded, logBuf.String())
	}
	if len(recentBlocks) == 0 {
		t.Fatalf("production helper returned 0 blocks — break regression at first window height")
	}

	// FastForward and verify tip is correct.
	chain := core.NewChain()
	if err := chain.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	chain.FastForward(recentBlocks)

	if tip := chain.Tip(); tip.Header.Height != tipHeight {
		t.Errorf("chain tip = %d, want %d", tip.Header.Height, tipHeight)
	}
}

// TestHeadersFromWithEntireWindowGap is the regression guard for the scenario
// where every block in the loadRecentBlocksFromStore window (startLoad..tipHeight)
// is absent from the store — i.e. the entire in-memory window is a gap.
//
// With the old `break` behaviour this returned 0 blocks and left chain.Height()
// at genesis, causing the consensus engine to produce a duplicate block at
// height 1 and silently fork away from the canonical chain.  Even with the
// `continue` fix, if every height in the window is missing the returned slice
// is still empty and FastForward is a no-op.  anchorTipIfNeeded must then fetch
// the tip block directly so that HeadersFrom can serve at least that one header
// to a syncing peer.
//
// Setup:
//   - Build a chain of N blocks (heights 0–N); only genesis (0) and tip (N) are
//     persisted to the store — heights 1..N-1 are all absent.
//   - A filteredGetter hides height N from the window scan, making the returned
//     slice empty (entire window is a gap).
//   - anchorTipIfNeeded is given the unfiltered store (block N present) and must
//     advance chain.Height() from 0 to N.
//   - HeadersFrom with a peer that only knows genesis must return ≥ 1 header
//     (at minimum the tip block's header at height N).
func TestHeadersFromWithEntireWindowGap(t *testing.T) {
	const (
		nBlocks   = 9
		tipHeight = uint64(nBlocks)
	)

	dir := t.TempDir()

	// Build a chain where only genesis (0) and tip (9) are stored in the DB.
	// Heights 1..8 are all absent — the entire window is a gap.
	// buildEntireWindowGapChain persists only genesis + tip; all intermediate
	// heights are omitted, simulating a heavy prune or partial rsync.
	db, blocks := buildEntireWindowGapChain(t, dir, nBlocks)

	// ── Step A: window scan with tip block hidden (filteredGetter) ─────────
	// Heights 1..8 are not in the store; height 9 is hidden → slice is empty.
	var scanLog bytes.Buffer
	filtered := &filteredGetter{inner: db, hideHeight: tipHeight}
	recentBlocks := loadRecentBlocksFromStore(filtered, tipHeight, newCaptureLogger(&scanLog))

	if len(recentBlocks) != 0 {
		t.Fatalf("pre-condition: expected 0 blocks from entire-window gap, got %d", len(recentBlocks))
	}

	// ── Step B: FastForward with empty slice — chain stays at genesis ──────
	chain := core.NewChain()
	if err := chain.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	chain.FastForward(recentBlocks) // no-op

	if chain.Height() != 0 {
		t.Fatalf("pre-condition: chain.Height() = %d after empty FastForward, want 0", chain.Height())
	}

	// ── Step C: anchorTipIfNeeded with full store must advance to tipHeight ─
	var anchorLog bytes.Buffer
	if err := anchorTipIfNeeded(chain, db, tipHeight, newCaptureLogger(&anchorLog)); err != nil {
		t.Fatalf("anchorTipIfNeeded: %v", err)
	}
	if chain.Height() != tipHeight {
		t.Errorf("chain.Height() = %d after anchorTipIfNeeded, want %d", chain.Height(), tipHeight)
	}
	if !logContainsMsg(&anchorLog, "tip block loaded as in-memory anchor") {
		t.Errorf("expected 'tip block loaded as in-memory anchor' log not found\nlog:\n%s", anchorLog.String())
	}
	t.Logf("chain anchored to height %d after entire-window gap", chain.Height())

	// ── Step D: HeadersFrom must serve at least the tip block ─────────────
	// A syncing peer that only knows genesis sends genesis hash as its known tip.
	// HeadersFrom must return ≥ 1 header (the tip block at height N).
	genesisHash := blocks[0].Hash()
	headers := chain.HeadersFrom([]crypto.Hash32{genesisHash}, 500)

	if len(headers) == 0 {
		t.Fatal("HeadersFrom returned 0 headers: node cannot serve any blocks after entire-window gap + anchor")
	}

	maxServed := uint64(0)
	for _, hdr := range headers {
		if hdr.Height > maxServed {
			maxServed = hdr.Height
		}
	}
	if maxServed < tipHeight {
		t.Errorf("HeadersFrom served max height %d, want >= %d (tip must be reachable)", maxServed, tipHeight)
	}
	t.Logf("HeadersFrom served %d header(s), max height = %d (entire-window gap covered)", len(headers), maxServed)
}

// buildEntireWindowGapChain creates a chain of nBlocks blocks where only
// genesis (height 0) and the tip (height nBlocks) are persisted to the store.
// All intermediate heights 1..nBlocks-1 are deliberately absent, simulating
// the "entire in-memory window is a gap" scenario (e.g. after a heavy prune or
// a partial rsync that copied only the tip block alongside genesis).
//
// The DB tip is set to the last block.  Returns the open store and a slice of
// all created blocks (index i == height i).
func buildEntireWindowGapChain(t *testing.T, dir string, nBlocks int) (*store.DB, []*core.Block) {
	t.Helper()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	makeBlock := func(height uint64, prevHash crypto.Hash32) *core.Block {
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prevHash,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		}
		if signErr := hdr.Sign(priv); signErr != nil {
			t.Fatalf("block Sign height=%d: %v", height, signErr)
		}
		return &core.Block{Header: hdr}
	}

	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	storeBlk := func(b *core.Block) {
		raw, merr := json.Marshal(b)
		if merr != nil {
			t.Fatalf("marshal block h=%d: %v", b.Header.Height, merr)
		}
		if perr := db.PutRawBlock(b.Hash(), b.Header.Height, raw); perr != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, perr)
		}
	}

	blocks := make([]*core.Block, nBlocks+1)
	genesis := makeBlock(0, crypto.Hash32{})
	blocks[0] = genesis
	storeBlk(genesis) // genesis always stored

	prevBlock := genesis
	for i := 1; i <= nBlocks; i++ {
		blk := makeBlock(uint64(i), prevBlock.Hash())
		blocks[i] = blk
		// Store only the tip block; all intermediate blocks are absent.
		if i == nBlocks {
			storeBlk(blk)
		}
		prevBlock = blk
	}

	tip := blocks[nBlocks]
	if err := db.PutTip(tip.Hash(), uint64(nBlocks)); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	return db, blocks
}

// logContainsFieldValue scans JSON log lines in buf for an entry that has the
// named structured field set to wantValue (string comparison).  Used to assert
// key=value log fields (e.g. startup_reason=corrupt_snapshot) independently of
// the human-readable msg text.
func logContainsFieldValue(buf *bytes.Buffer, fieldName, wantValue string) bool {
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var rec map[string]interface{}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if fv, ok := rec[fieldName].(string); ok && fv == wantValue {
			return true
		}
	}
	return false
}

// TestCorruptSnapshotFallsBackToGapResumeScan is the regression guard for the
// interaction between a corrupt/truncated startup snapshot and a gap in the
// LevelDB block window.
//
// The test exercises the complete production startup sequence from main.go in
// the exact order production uses:
//
//	loadRecentBlocksFromStore  (main.go line 764) — in-memory block window
//	FastForward                (main.go line 816) — chain populated
//	anchorTipIfNeeded          (main.go line 846) — chain tip anchored
//	tryLoadStartupSnapshot     (main.go line 899) — snapshot fast-path attempt
//	runStartupScan             (main.go, !snapLoaded branch) — UTXO fallback scan
//
// Three failure modes are combined so that reverting any single fix breaks the
// test:
//
//  1. LevelDB gap: block 4 is absent from the store.  Both loadRecentBlocksFromStore
//     (continue-on-gap) and runStartupScan (MaxMissingBlocks tolerance) must
//     survive it.  The old break regression in loadRecentBlocksFromStore would
//     return 0 blocks.
//
//  2. Tip block hidden during the window scan via filteredGetter: FastForward
//     ends at height 8, so anchorTipIfNeeded must fetch block 9 from the real
//     DB and advance the chain — making that assertion non-trivial.
//
//  3. Corrupt snapshot (truncated non-gzip bytes at snapshotPath(dir, 9)):
//     tryLoadStartupSnapshot rejects it and emits startup_reason=corrupt_snapshot.
//     This gates the full UTXO scan (runStartupScan) — the "gap-resume scan" that
//     must run after the corrupt snapshot is rejected.
//
// Verified outcomes (mirrors the "Done looks like" criteria):
//   - blocks_loaded_in_memory > 0 (gap skipped via continue, not abort).
//   - chain.Height() == tipHeight after anchorTipIfNeeded (non-trivial anchor).
//   - tryLoadStartupSnapshot returns an error before runStartupScan is invoked.
//   - Structured log field startup_reason == "corrupt_snapshot" is present.
//   - runStartupScan returns ScanFrom == 1 (full scan — no partial checkpoint).
func TestCorruptSnapshotFallsBackToGapResumeScan(t *testing.T) {
	const (
		nBlocks     = 9        // heights 0 – 9
		gapInWindow = uint64(4) // block 4 absent from the LevelDB store
	)
	tipHeight := uint64(nBlocks)

	dir := t.TempDir()
	// buildChainWithGap stores all blocks except gapInWindow.
	// The tip block (height 9) IS stored in the real DB so anchorTipIfNeeded
	// and runStartupScan can access it; it is only hidden during the window
	// scan via filteredGetter.
	db, blocks := buildChainWithGap(t, dir, nBlocks, gapInWindow)

	// Write a truncated (corrupt) snapshot at tipHeight.
	// The v2 format is gzip-compressed JSON; writing raw non-gzip bytes makes
	// openGzipSnapshotReader fail — identical to a SIGKILL mid-write leaving a
	// partially-written gzip on disk.
	tipHashHex := fmt.Sprintf("%x", blocks[nBlocks].Hash())
	truncated := []byte(`{"v":2,"tip_height":9,"tip_hash":"` + tipHashHex[:8]) // no gzip wrapper
	if err := os.WriteFile(snapshotPath(dir, tipHeight), truncated, 0644); err != nil {
		t.Fatalf("write corrupt snapshot: %v", err)
	}

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	// ── Production step A: loadRecentBlocksFromStore (main.go line 764). ──────
	// filteredTip hides height 9 during the window scan while leaving it in the
	// real db for anchorTipIfNeeded.
	filteredTip := &filteredGetter{inner: db, hideHeight: tipHeight}
	recentBlocks := loadRecentBlocksFromStore(filteredTip, tipHeight, log)

	// blocks_loaded_in_memory must be > 0: gap at gapInWindow is skipped via
	// continue, not abort.  The old break regression returns 0 here.
	if len(recentBlocks) == 0 {
		t.Fatalf("blocks_loaded_in_memory == 0 (break regression or entire window absent)\nlog:\n%s",
			logBuf.String())
	}
	// Heights loaded: 1,2,3,[4 gap],5,6,7,8 — tip 9 also hidden → 7 blocks.
	wantLoaded := nBlocks - 2 // gap at gapInWindow + tip hidden = 2 absent
	if len(recentBlocks) != wantLoaded {
		t.Errorf("blocks_loaded_in_memory = %d, want %d (gap at %d + tip hidden)",
			len(recentBlocks), wantLoaded, gapInWindow)
	}
	t.Logf("blocks_loaded_in_memory = %d (gap at %d, tip %d hidden)",
		len(recentBlocks), gapInWindow, tipHeight)

	// ── Production step B: FastForward (main.go line 816). ────────────────────
	chain := core.NewChain()
	if err := chain.SetGenesis(blocks[0]); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	chain.FastForward(recentBlocks)

	// Pre-condition: chain must be below tipHeight so anchorTipIfNeeded has real
	// work to do (tip was hidden from the window scan).
	if chain.Height() >= tipHeight {
		t.Fatalf("pre-condition failed: chain.Height() = %d, want < %d before anchor",
			chain.Height(), tipHeight)
	}
	t.Logf("chain.Height() after FastForward = %d (tip hidden; anchor needed)", chain.Height())

	// ── Production step C: anchorTipIfNeeded (main.go line 846). ─────────────
	// Real DB exposes block 9 — anchor fetches and fast-forwards it.
	if err := anchorTipIfNeeded(chain, db, tipHeight, log); err != nil {
		t.Fatalf("anchorTipIfNeeded: %v", err)
	}
	if chain.Height() != tipHeight {
		t.Errorf("chain.Height() = %d after anchorTipIfNeeded, want %d", chain.Height(), tipHeight)
	}
	if !logContainsMsg(&logBuf, "tip block loaded as in-memory anchor") {
		t.Errorf("anchorTipIfNeeded log message not found\nlog:\n%s", logBuf.String())
	}

	// ── Production step D: tryLoadStartupSnapshot (main.go line 899). ────────
	// Corrupt snapshot must be rejected; this gates the UTXO scan (step E).
	snap, _, serr := tryLoadStartupSnapshot(dir, tipHeight, tipHashHex, log)
	if serr == nil || snap != nil {
		t.Fatalf("tryLoadStartupSnapshot: expected error for corrupt snapshot, got snap=%v err=%v",
			snap, serr)
	}
	t.Logf("tryLoadStartupSnapshot correctly rejected corrupt snapshot: %v", serr)

	// Structured field startup_reason=corrupt_snapshot distinguishes truncation
	// (SIGKILL mid-write) from a first run (no_snapshot).
	if !logContainsFieldValue(&logBuf, "startup_reason", "corrupt_snapshot") {
		t.Errorf("structured log field startup_reason=corrupt_snapshot not found\nlog:\n%s",
			logBuf.String())
	}
	const corruptMsg = "snapshot corrupt or unreadable — falling back to full block scan"
	if !logContainsMsg(&logBuf, corruptMsg) {
		t.Errorf("log msg %q not found\nlog:\n%s", corruptMsg, logBuf.String())
	}

	// ── Production step E: runStartupScan (main.go !snapLoaded branch). ───────
	// Because tryLoadStartupSnapshot returned an error, snapLoaded is false and
	// production calls runStartupScan — the "gap-resume scan" this task covers.
	// With MaxMissingBlocks=10 the scan must survive the gap at height 4 and
	// complete successfully, returning ScanFrom=1 (no partial checkpoint exists).
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)

	var wg sync.WaitGroup
	result, scanErr := runStartupScan(startupScanParams{
		DataDir:          dir,
		TipHeight:        tipHeight,
		TipHashHex:       tipHashHex,
		DB:               db,
		UTXOs:            utxos,
		Registry:         registry,
		KiFromIndex:      false, // fresh start — no index pre-loaded
		InitTxTotal:      0,
		Log:              log,
		SnapshotWg:       &wg,
		MaxMissingBlocks: 10, // tolerate the gap at height 4
	})
	// Wait for any async snapshot goroutines before asserting on log output.
	wg.Wait()

	if scanErr != nil {
		t.Fatalf("runStartupScan: %v (gap at height %d with MaxMissingBlocks=10 should be tolerated)",
			scanErr, gapInWindow)
	}
	// ScanFrom must be 1: the corrupt tip snapshot is not a valid partial
	// checkpoint (findLatestSnapshot inside runStartupScan rejects it), so the
	// scan starts from the beginning of the chain.
	if result.ScanFrom != 1 {
		t.Errorf("runStartupScan ScanFrom = %d, want 1 (full scan after corrupt snapshot rejection)",
			result.ScanFrom)
	}
	t.Logf("runStartupScan completed: ScanFrom=%d (gap at height %d tolerated)",
		result.ScanFrom, gapInWindow)

	t.Logf("full startup sequence verified: %d blocks_loaded_in_memory, "+
		"chain anchored to height %d, corrupt snapshot rejected "+
		"(startup_reason=corrupt_snapshot), gap-resume scan ran from block 1",
		len(recentBlocks), tipHeight)
}
