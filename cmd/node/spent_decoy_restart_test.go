package main

// Integration test: confirms that the startup scan (runStartupScan) correctly
// rebuilds the spent-decoy pool (UTXOSet.spentPubKeys) for every block added
// after the last snapshot checkpoint.
//
// Background
// ----------
// TestSpentDecoyPool_SurvivesRestartReplay (core/c0_c1_fix_test.go) proves
// that ApplyBlockForSpentDecoys works in isolation.  This file adds a
// higher-level integration test that exercises the ACTUAL startup path in
// scan.go — the same runStartupScan call that main.go invokes — to confirm
// that spentPubKeys is correctly rebuilt for blocks [checkpoint+1 .. tipHeight].
//
// Startup scan — spent-decoy rebuild path
// ----------------------------------------
// main.go calls runStartupScan (in scan.go) after applying the genesis block to
// the UTXOSet.  runStartupScan iterates blocks scanFrom..tipHeight and calls
// ApplyBlock for each one.  ApplyBlock populates spentPubKeys (the decoy pool)
// as a side effect: for each spending input it searches byPubKey for the ring
// member whose AmountCommit matches the input's AmountCommit and moves that
// UTXO to spentPubKeys.
//
// When a partial checkpoint is present (saved by a previous interrupted scan or
// the periodic snapshot timer), runStartupScan restores from it via
// RestoreFromSnapshot — which includes the serialised SpentDecoys — and then
// applies only the blocks AFTER the checkpoint via ApplyBlock.  The total
// spentPubKeys after the scan therefore equals:
//
//   (decoys from checkpoint) + (decoys from blocks checkpoint+1..tipHeight)
//
// Test scenario
// -------------
//  Phase 1 (blocks 0..snapHeight):
//    • Block 0 creates a UTXO for ring1[0].
//    • Block 1 spends ring1[0] via a ring input → ring1[0] moves to spentPubKeys.
//    • A UTXOSet snapshot is taken at height 1 and saved as an intermediate
//      checkpoint (exactly what the production checkpointInterval path does).
//
//  Phase 2 (blocks snapHeight+1..tipHeight):
//    • Block 2 creates a UTXO for ring2[0].
//    • Block 3 spends ring2[0] via a ring input.
//    • These blocks are stored in LevelDB but NO snapshot is saved for tipHeight=3.
//
//  Restart simulation:
//    • A fresh UTXOSet (empty spentPubKeys) and ValidatorRegistry are created.
//    • runStartupScan is called with tipHeight=3.
//      – findLatestSnapshot finds the checkpoint at height 1.
//      – The checkpoint is restored (SpentDecoys from block 1 are in place).
//      – ApplyBlock is called for blocks 2 and 3.
//      – After block 3, ring2[0] is moved to spentPubKeys.
//    • Assertions:
//       1. ScanFrom == snapHeight+1 (confirms the checkpoint was used).
//       2. SpentDecoyCount() == 2 (one from the snapshot, one from the scan).
//       3. The log contains the "partial snapshot loaded" message.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// validKeyImage derives a deterministic, cryptographically valid key image from
// a seed byte.  It generates a canonical scalar via ScalarFromUint64(seed+1)
// (never zero), multiplies by G to get a valid public key, then calls
// ComputeKeyImage.  The result is always a valid compressed Edwards25519 point
// suitable for use in ApplyBlock's CanonicalKeyImage check.
func validKeyImage(t *testing.T, seed uint64) crypto.KeyImage {
	t.Helper()
	priv := crypto.ScalarFromUint64(seed + 1) // seed+1 avoids the zero scalar
	pub, err := crypto.ScalarMulBase(priv)
	if err != nil {
		t.Fatalf("ScalarMulBase(seed=%d): %v", seed, err)
	}
	ki, err := crypto.ComputeKeyImage(priv, pub)
	if err != nil {
		t.Fatalf("ComputeKeyImage(seed=%d): %v", seed, err)
	}
	return ki
}

// TestSpentDecoyPool_RebuildAfterSnapshot is the primary integration test.
// It confirms that runStartupScan (the startup path in main.go) correctly
// rebuilds the spent-decoy pool for all blocks between the last snapshot
// checkpoint and the chain tip.
func TestSpentDecoyPool_RebuildAfterSnapshot(t *testing.T) {
	dir := t.TempDir()

	// ── Open the DB ────────────────────────────────────────────────────────
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// ── Validator key for signing block headers ────────────────────────────
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// ── Commitment reused by all test UTXOs ────────────────────────────────
	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(1000, blind)

	// makeRing builds a ring of RingSize distinct pub keys, with the first
	// element (base) being the "real" UTXO that will be spent.
	makeRing := func(base byte) []crypto.Point32 {
		ring := make([]crypto.Point32, crypto.RingSize)
		for i := range ring {
			ring[i][0] = base + byte(i)
		}
		return ring
	}

	// makeBlock builds a signed block with the given transactions.
	makeBlock := func(height uint64, prevHash crypto.Hash32, txs []core.Transaction) *core.Block {
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prevHash,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(txs),
		}
		if err := hdr.Sign(priv); err != nil {
			t.Fatalf("Sign block h=%d: %v", height, err)
		}
		return &core.Block{Header: hdr, Txs: txs}
	}

	// storeBlock persists a block in LevelDB and updates the tip pointer.
	storeBlock := func(b *core.Block) {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal block h=%d: %v", b.Header.Height, err)
		}
		h := b.Hash()
		if err := db.PutRawBlock(h, b.Header.Height, raw); err != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, err)
		}
		if err := db.PutTip(h, b.Header.Height); err != nil {
			t.Fatalf("PutTip h=%d: %v", b.Header.Height, err)
		}
	}

	// ──────────────────────────────────────────────────────────────────────
	// Phase 1: build blocks 0..1 (pre-snapshot) and save a checkpoint.
	//
	//   Block 0: output at ring1[0] with commit.
	//   Block 1: spends ring1[0] (valid key image ki1).
	//
	// After ApplyBlock(blk0) + ApplyBlock(blk1), ring1[0] is in spentPubKeys.
	// We take a snapshot and save it so findLatestSnapshot can find it.
	// ──────────────────────────────────────────────────────────────────────
	const snapHeight = uint64(1)

	ring1 := makeRing(0x10) // ring1[0] = {0x10, 0, …}
	ki1 := validKeyImage(t, 1)

	// Block 0: creates a UTXO at ring1[0].
	blk0 := makeBlock(0, crypto.Hash32{}, []core.Transaction{
		{
			Version: core.TxVersionBase,
			Outputs: []core.Output{
				{OneTimePub: ring1[0], AmountCommit: commit},
			},
		},
	})
	storeBlock(blk0)

	// Block 1: spends the UTXO at ring1[0].
	blk1 := makeBlock(1, blk0.Hash(), []core.Transaction{
		{
			Version: core.TxVersionBase,
			Inputs: []core.RingInput{
				{Ring: ring1, AmountCommit: commit, KeyImage: ki1},
			},
		},
	})
	storeBlock(blk1)

	// Build the UTXOSet state after blocks 0..1 to take the snapshot.
	utxosPhase1 := core.NewUTXOSet()
	if err := utxosPhase1.ApplyBlock(blk0); err != nil {
		t.Fatalf("Phase1 ApplyBlock blk0: %v", err)
	}
	if err := utxosPhase1.ApplyBlock(blk1); err != nil {
		t.Fatalf("Phase1 ApplyBlock blk1: %v", err)
	}

	// Sanity: the snapshot must already contain at least one spent decoy.
	if utxosPhase1.SpentDecoyCount() == 0 {
		t.Fatal("pre-condition failed: Phase 1 UTXOSet must have spent decoys after block 1")
	}
	snapDecoys := utxosPhase1.SpentDecoyCount()
	t.Logf("Phase 1 snapshot contains %d spent decoy(s)", snapDecoys)

	// Save the intermediate checkpoint at snapHeight so findLatestSnapshot
	// can discover it during the restart simulation.
	blk1Hash := blk1.Hash()
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  snapHeight,
		TipHashHex: fmt.Sprintf("%x", blk1Hash[:]),
		UTXOs:      utxosPhase1.TakeSnapshot(),
		Registry:   core.NewValidatorRegistry().TakeSnapshot(),
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	// ──────────────────────────────────────────────────────────────────────
	// Phase 2: add blocks 2..3 (post-snapshot) and advance the tip.
	//
	//   Block 2: output at ring2[0] with commit.
	//   Block 3: spends ring2[0] (valid key image ki2).
	//
	// These blocks are stored in LevelDB but NO snapshot is saved for
	// tipHeight=3 — simulating a crash or graceful stop before the next
	// periodic snapshot.
	// ──────────────────────────────────────────────────────────────────────
	const tipHeight = uint64(3)

	ring2 := makeRing(0x20) // ring2[0] = {0x20, 0, …}
	ki2 := validKeyImage(t, 2)

	// Block 2: creates a UTXO at ring2[0].
	blk2 := makeBlock(2, blk1.Hash(), []core.Transaction{
		{
			Version: core.TxVersionBase,
			Outputs: []core.Output{
				{OneTimePub: ring2[0], AmountCommit: commit},
			},
		},
	})
	storeBlock(blk2)

	// Block 3: spends the UTXO at ring2[0].
	blk3 := makeBlock(3, blk2.Hash(), []core.Transaction{
		{
			Version: core.TxVersionBase,
			Inputs: []core.RingInput{
				{Ring: ring2, AmountCommit: commit, KeyImage: ki2},
			},
		},
	})
	storeBlock(blk3)

	// ──────────────────────────────────────────────────────────────────────
	// Restart simulation: call runStartupScan with fresh state.
	//
	// The snapshot for tipHeight=3 does NOT exist, so
	// loadStartupSnapshotWithFallback (called in main.go before
	// runStartupScan) would have returned an error.  runStartupScan then
	// uses findLatestSnapshot, which finds the checkpoint at height 1,
	// restores from it, and scans blocks 2..3.
	//
	// Expected outcome after the scan:
	//   • SpentDecoys from the checkpoint (block 1) are restored via
	//     RestoreFromSnapshot.
	//   • ApplyBlock(blk2) adds ring2[0] to byPubKey (output, no inputs).
	//   • ApplyBlock(blk3) moves ring2[0] to spentPubKeys (spending input).
	//   • Final SpentDecoyCount() == snapDecoys + 1.
	// ──────────────────────────────────────────────────────────────────────
	utxosRestart := core.NewUTXOSet()
	registryRestart := core.NewValidatorRegistry()

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	blk3Hash := blk3.Hash()
	var wg sync.WaitGroup
	result, err := runStartupScan(startupScanParams{
		DataDir:     dir,
		TipHeight:   tipHeight,
		TipHashHex:  fmt.Sprintf("%x", blk3Hash[:]),
		DB:          db,
		UTXOs:       utxosRestart,
		Registry:    registryRestart,
		KiFromIndex: false, // no pre-loaded key-image index; scan builds them incrementally
		Log:         log,
		SnapshotWg:  &wg,
		CheckpointInterval: 50000,
		})
	wg.Wait() // ensure snapshot goroutine finishes before assertions

	if err != nil {
		t.Fatalf("runStartupScan: %v", err)
	}

	// ── Assert 1: scan resumed from the checkpoint, not from block 1 ─────
	// This confirms that findLatestSnapshot found the checkpoint at height 1
	// and runStartupScan scanned only the gap (blocks 2..3).
	if result.ScanFrom != snapHeight+1 {
		t.Errorf("ScanFrom = %d, want %d (checkpoint height + 1); "+
			"the checkpoint was not used — runStartupScan may have ignored the snapshot",
			result.ScanFrom, snapHeight+1)
	}

	// ── Assert 2: spent decoy pool contains decoys from BOTH phases ──────
	// snapDecoys decoys come from the checkpoint (block 1 spend).
	// +1 comes from the post-checkpoint scan (block 3 spend).
	wantDecoys := snapDecoys + 1
	gotDecoys := utxosRestart.SpentDecoyCount()
	if gotDecoys != wantDecoys {
		t.Errorf("SpentDecoyCount() = %d, want %d; "+
			"startup scan did not correctly rebuild the spent-decoy pool "+
			"for blocks [%d..%d]",
			gotDecoys, wantDecoys, snapHeight+1, tipHeight)
	} else {
		t.Logf("SpentDecoyCount() = %d (%d from snapshot + 1 from post-snapshot scan) ✓",
			gotDecoys, snapDecoys)
	}

	// ── Assert 3: log must confirm the checkpoint was used ────────────────
	if !logContainsMsg(&logBuf, "partial snapshot loaded — resuming scan from checkpoint") {
		t.Error("expected log message \"partial snapshot loaded — resuming scan from checkpoint\" not found; " +
			"the checkpoint may not have been discovered by findLatestSnapshot")
	}
}

// TestSpentDecoyPool_FullScanRebuildsDecoys tests the complementary scenario:
// no checkpoint exists, so runStartupScan scans all blocks from height 1.
// ApplyBlock must still populate spentPubKeys for spending blocks encountered
// during the full scan.
//
// This mirrors the main.go startup path:
//   - The genesis block (height 0) is applied to the UTXOSet before
//     runStartupScan is called (line in main.go: utxos.ApplyBlock(&genesisBlk)).
//   - runStartupScan scans from height 1 (scanFrom=1 when no checkpoint).
func TestSpentDecoyPool_FullScanRebuildsDecoys(t *testing.T) {
	dir := t.TempDir()

	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(500, blind)

	// Valid key image for the spending transaction.
	ki := validKeyImage(t, 3)

	makeBlock := func(height uint64, prevHash crypto.Hash32, txs []core.Transaction) *core.Block {
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prevHash,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(txs),
		}
		if err := hdr.Sign(priv); err != nil {
			t.Fatalf("Sign block h=%d: %v", height, err)
		}
		return &core.Block{Header: hdr, Txs: txs}
	}

	storeBlock := func(b *core.Block) {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal block h=%d: %v", b.Header.Height, err)
		}
		h := b.Hash()
		if err := db.PutRawBlock(h, b.Header.Height, raw); err != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, err)
		}
		if err := db.PutTip(h, b.Header.Height); err != nil {
			t.Fatalf("PutTip h=%d: %v", b.Header.Height, err)
		}
	}

	// Build a ring of 16 UTXOs; ring[0] will be the real spent UTXO.
	ring := make([]crypto.Point32, crypto.RingSize)
	for i := range ring {
		ring[i][0] = 0x50 + byte(i)
	}

	// Block 0 (genesis): output at ring[0].
	blk0 := makeBlock(0, crypto.Hash32{}, []core.Transaction{
		{
			Version: core.TxVersionBase,
			Outputs: []core.Output{
				{OneTimePub: ring[0], AmountCommit: commit},
			},
		},
	})
	storeBlock(blk0)

	// Block 1: spends ring[0].  No checkpoint exists — full scan from block 1.
	blk1 := makeBlock(1, blk0.Hash(), []core.Transaction{
		{
			Version: core.TxVersionBase,
			Inputs: []core.RingInput{
				{Ring: ring, AmountCommit: commit, KeyImage: ki},
			},
		},
	})
	storeBlock(blk1)

	// Fresh state for the restart simulation.
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()

	// Mirror main.go's resume path: genesis UTXOs are applied to the UTXOSet
	// BEFORE runStartupScan is called.  The scan starts from height 1;
	// block 0 (genesis) is handled separately by main.go before calling the scan.
	if err := utxos.ApplyBlock(blk0); err != nil {
		t.Fatalf("pre-scan ApplyBlock blk0 (genesis): %v", err)
	}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	blk1Hash := blk1.Hash()
	var wg sync.WaitGroup
	result, err := runStartupScan(startupScanParams{
		DataDir:     dir,
		TipHeight:   1,
		TipHashHex:  fmt.Sprintf("%x", blk1Hash[:]),
		DB:          db,
		UTXOs:       utxos,
		Registry:    registry,
		KiFromIndex: false,
		Log:         log,
		SnapshotWg:  &wg,
		CheckpointInterval: 50000,
		})
	wg.Wait()

	if err != nil {
		t.Fatalf("runStartupScan: %v", err)
	}

	// Full scan starts from block 1 (no checkpoint in the temp dir).
	if result.ScanFrom != 1 {
		t.Errorf("ScanFrom = %d, want 1 (no checkpoint)", result.ScanFrom)
	}

	// The spending block (blk1) must have moved ring[0] to spentPubKeys.
	if utxos.SpentDecoyCount() == 0 {
		t.Fatal("SpentDecoyCount() == 0; full scan did not rebuild the spent-decoy pool")
	}
	t.Logf("full scan: SpentDecoyCount() = %d ✓", utxos.SpentDecoyCount())
}
