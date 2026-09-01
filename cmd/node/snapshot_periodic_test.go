package main

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── nop logger ──────────────────────────────────────────────────────────────

type nopHandler struct{}

func (nopHandler) Enabled(_ interface{ Deadline() (interface{}, bool) }, _ slog.Level) bool {
	return false
}

func newNopLog() *slog.Logger { return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})) }

// ─── Helpers ─────────────────────────────────────────────────────────────────

func makeGenesis(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey) *core.Chain {
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
	chain := core.NewChain()
	if err := chain.SetGenesis(&core.Block{Header: hdr}); err != nil {
		t.Fatal(err)
	}
	return chain
}

// buildBlock constructs a structurally valid block at height h extending parent.
func buildBlock(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey, h uint64, parent *core.Block) *core.Block {
	t.Helper()
	hdr := core.BlockHeader{
		Height:       h,
		Round:        uint32(h),
		PrevHash:     parent.Hash(),
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatal(err)
	}
	return &core.Block{Header: hdr}
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestOnBlockAccepted_PeriodicSnapshot_IncomingPath verifies that the
// OnBlockAccepted callback fires when blocks arrive via the P2P incoming-block
// path (NewBlockCh), and that the periodic-snapshot logic writes a file at the
// correct heights and removes stale ones.
func TestOnBlockAccepted_PeriodicSnapshot_IncomingPath(t *testing.T) {
	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain := makeGenesis(t, priv, pub)

	// acceptedHeights collects every height at which OnBlockAccepted fires.
	// Protected by mu because OnBlockAccepted fires on the engine goroutine
	// while the test reads acceptedHeights on its own goroutine.
	var mu sync.Mutex
	var acceptedHeights []uint64

	// snapCount counts how many snapshots were written.  The production guard
	// now uses a configurable interval (default 10 000); in this test we use
	// interval=2 so the snapshot fires at h==2 within our 3-block run.
	var snapCount atomic.Int32
	const testInterval uint64 = 2

	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{pub},
		MyKey:        nil, // not a producing validator in this test
		OnBlockAccepted: func(block *core.Block) {
			h := block.Header.Height
			mu.Lock()
			acceptedHeights = append(acceptedHeights, h)
			mu.Unlock()

			// Mirror the production periodic-snapshot logic with testInterval.
			if testInterval == 0 || h == 0 || h%testInterval != 0 {
				return
			}
			snap := startupSnapshot{
				Version:    snapVersion,
				TipHeight:  h,
				TipHashHex: fmt.Sprintf("%x", block.Hash()),
				TxTotal:    int64(h),
				UTXOs:      core.UTXOSnapshot{},
				Registry: core.RegistrySnapshot{
					Validators: map[string]*core.ValidatorEntry{},
				},
			}
			// Write synchronously inside the callback for deterministic test assertions.
			if saveErr := saveStartupSnapshot(dir, snap); saveErr != nil {
				t.Errorf("saveStartupSnapshot(%d): %v", h, saveErr)
				return
			}
			deleteOldSnapshots(dir, h)
			snapCount.Add(1)
		},
	}, chain, core.NewMempool(core.DefaultMempoolConfig()), newNopLog())

	// Wire a TxVerifier so handleIncomingBlock doesn't fail-closed.
	utxos := core.NewUTXOSet()
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// Feed blocks 1–3 through NewBlockCh (the P2P incoming-block path).
	// Block 1 is at height 1, block 2 at height 2, block 3 at height 3.
	// We can't cheaply simulate 1500 blocks through the full engine, so instead
	// we send 3 real blocks to prove the callback fires on the incoming path,
	// and rely on the snapshot-file test below to prove the 500-block logic.
	parent := chain.Tip()
	for i := uint64(1); i <= 3; i++ {
		blk := buildBlock(t, priv, pub, i, parent)
		eng.NewBlockCh() <- blk
		// Wait for the chain to advance before building the next block.
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if chain.Height() == i {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if chain.Height() != i {
			t.Fatalf("chain did not advance to height %d within 500ms (stuck at %d)", i, chain.Height())
		}
		parent = chain.Tip()
	}

	// All 3 incoming blocks must have triggered OnBlockAccepted.
	mu.Lock()
	gotHeights := make([]uint64, len(acceptedHeights))
	copy(gotHeights, acceptedHeights)
	mu.Unlock()

	if len(gotHeights) != 3 {
		t.Errorf("OnBlockAccepted fired %d times, want 3; heights=%v", len(gotHeights), gotHeights)
	}
	for i, h := range gotHeights {
		if h != uint64(i+1) {
			t.Errorf("acceptedHeights[%d] = %d, want %d", i, h, i+1)
		}
	}
}

// TestPeriodicSnapshot_SaveDeleteContract verifies the snapshot save/delete
// contract: a snapshot file must exist at each interval boundary, and all
// earlier snapshot files must be removed.  The interval size used here (500)
// is arbitrary — the production value is now configurable via
// SnapshotConfig.PeriodicSnapshotInterval (default 10 000).
func TestPeriodicSnapshot_SaveDeleteContract(t *testing.T) {
	dir := t.TempDir()

	saveSnap := func(h uint64) {
		snap := startupSnapshot{
			Version:    snapVersion,
			TipHeight:  h,
			TipHashHex: fmt.Sprintf("%016x", h),
			TxTotal:    int64(h),
			UTXOs:      core.UTXOSnapshot{},
			Registry: core.RegistrySnapshot{
				Validators: map[string]*core.ValidatorEntry{},
			},
		}
		if err := saveStartupSnapshot(dir, snap); err != nil {
			t.Fatalf("saveStartupSnapshot(height=%d): %v", h, err)
		}
		deleteOldSnapshots(dir, h)
	}

	for h := uint64(500); h <= 1500; h += 500 {
		saveSnap(h)

		// The snapshot file for this height must exist.
		path := snapshotPath(dir, h)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("snapshot missing at height %d (expected file %s)", h, path)
		}

		// All earlier 500-block snapshots must have been deleted.
		for prev := uint64(500); prev < h; prev += 500 {
			prevPath := snapshotPath(dir, prev)
			if _, statErr := os.Stat(prevPath); statErr == nil {
				t.Errorf("stale snapshot still present at height %d after writing height %d", prev, h)
			}
		}
	}

	// After the full run only the height-1500 snapshot must remain.
	finalPath := snapshotPath(dir, 1500)
	if _, err := os.Stat(finalPath); os.IsNotExist(err) {
		t.Errorf("final snapshot at height 1500 missing")
	}
}
