package main

// scan_test.go — verifies that the periodic GC hook in runStartupScan fires at
// the correct cumulative UTXO output boundaries (every gcUTXOInterval = 50 000
// outputs) regardless of how those outputs are distributed across blocks.
//
// Two scenarios are covered:
//
//  1. Varied output counts across blocks (TestStartupScanGCCadenceVariedBlocks)
//     Blocks cycle through [100, 500, 1000] outputs each.  The total is designed
//     to cross the 50 000-UTXO boundary exactly twice (at 50 000 and 100 000),
//     and no single block crosses more than one boundary.  The test asserts that
//     GCHook is called with utxoCount == 50 000 and then 100 000, in that order.
//
//  2. Single block crossing multiple thresholds
//     (TestStartupScanGCCadenceSingleBlockMultipleThresholds)
//     A single block holds 150 001 outputs.  The inner output loop must call
//     GCHook with utxoCount values 50 000, 100 000, and 150 000, in that order.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// buildVariedChain writes a synthetic chain to a fresh LevelDB store.
// outputCounts[i] specifies the number of coinbase outputs in block i+1
// (height 1 … len(outputCounts)).  Block 0 is a signed genesis with no txs.
// Returns the open DB and the tip block.
func buildVariedChain(
	t *testing.T,
	dir string,
	outputCounts []int,
	priv crypto.ValidatorPrivKey,
	pub crypto.ValidatorPubKey,
) (*store.DB, *core.Block) {
	t.Helper()

	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	putBlk := func(b *core.Block) {
		raw, merr := json.Marshal(b)
		if merr != nil {
			t.Fatalf("marshal h=%d: %v", b.Header.Height, merr)
		}
		h := b.Hash()
		if perr := db.PutRawBlock(h, b.Header.Height, raw); perr != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, perr)
		}
	}

	// Genesis: height 0, no transactions.
	genesis := &core.Block{
		Header: core.BlockHeader{
			Height:       0,
			PrevHash:     crypto.Hash32{},
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		},
	}
	if serr := genesis.Header.Sign(priv); serr != nil {
		t.Fatalf("Sign genesis: %v", serr)
	}
	putBlk(genesis)

	parent := genesis
	var tip *core.Block
	for idx, nOut := range outputCounts {
		height := uint64(idx + 1)

		outs := make([]core.Output, nOut)
		for i := range outs {
			// Encode (height, outputIndex) so every output is globally distinct.
			var otp crypto.Point32
			otp[0] = byte(height)
			otp[1] = byte(height >> 8)
			otp[2] = byte(height >> 16)
			otp[3] = byte(height >> 24)
			otp[4] = byte(i)
			otp[5] = byte(i >> 8)
			otp[6] = byte(i >> 16)
			otp[7] = 0x01 // non-zero marker
			outs[i] = core.Output{OneTimePub: otp, TxPubKey: otp}
		}

		coinbase := core.Transaction{
			Version: core.TxVersionBase,
			Outputs: outs,
		}
		txs := []core.Transaction{coinbase}
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     parent.Hash(),
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(txs),
		}
		if serr := hdr.Sign(priv); serr != nil {
			t.Fatalf("Sign h=%d: %v", height, serr)
		}
		blk := &core.Block{Header: hdr, Txs: txs}
		putBlk(blk)
		parent = blk
		tip = blk
	}

	if perr := db.PutTip(tip.Hash(), tip.Header.Height); perr != nil {
		t.Fatalf("PutTip: %v", perr)
	}
	return db, tip
}

// gcTestInterval mirrors the unexported gcUTXOInterval const from scan.go so
// that test assertions can reference the same value without hard-coding magic
// numbers.
const gcTestInterval = uint64(50000)

// TestStartupScanGCCadenceVariedBlocks confirms that the GC hook is called
// with utxoCount == 50 000 and then 100 000, in that order, when blocks have
// varying output counts that together cross those boundaries.
//
// Chain design:
//   - Blocks cycle through [100, 500, 1000] outputs (1 600 per 3-block cycle).
//   - 63 complete cycles → 189 blocks, 100 800 total outputs.
//   - Max outputs in a single block is 1 000 — well below gcTestInterval —
//     so each boundary is crossed by accumulation across blocks, not within one.
//   - Expected hook sequence: [50 000, 100 000].
func TestStartupScanGCCadenceVariedBlocks(t *testing.T) {
	const numCycles = 63 // 63 × 1 600 = 100 800 outputs → crosses 50 k and 100 k
	outputPattern := []int{100, 500, 1000}

	counts := make([]int, 0, numCycles*len(outputPattern))
	for c := 0; c < numCycles; c++ {
		counts = append(counts, outputPattern...)
	}

	totalOutputs := uint64(0)
	for _, n := range counts {
		totalOutputs += uint64(n)
	}
	// Self-check: the chain design must cross gcTestInterval exactly twice.
	if want := uint64(numCycles * 1600); totalOutputs != want {
		t.Fatalf("chain design error: totalOutputs=%d, want %d", totalOutputs, want)
	}

	wantSequence := []uint64{gcTestInterval, 2 * gcTestInterval} // [50 000, 100 000]

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	dir := t.TempDir()
	db, tip := buildVariedChain(t, dir, counts, priv, pub)
	tipHashHex := fmt.Sprintf("%x", tip.Hash())

	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	// gcSeen records the utxoCount value passed to GCHook at each invocation.
	// The hook is called from a single goroutine (the scan loop), so no mutex
	// is needed.
	var gcSeen []uint64
	var gcMu sync.Mutex // protect gcSeen from the test goroutine
	hook := func(count uint64) {
		gcMu.Lock()
		gcSeen = append(gcSeen, count)
		gcMu.Unlock()
	}

	var snapWg sync.WaitGroup
	_, scanErr := runStartupScan(startupScanParams{
		DataDir:     dir,
		TipHeight:   tip.Header.Height,
		TipHashHex:  tipHashHex,
		DB:          db,
		UTXOs:       utxos,
		Registry:    registry,
		KiFromIndex: false,
		InitTxTotal: 0,
		Log:         discardLog(),
		SnapshotWg:  &snapWg,
		GCHook:      hook,
	})
	snapWg.Wait()

	if scanErr != nil {
		t.Fatalf("runStartupScan: %v", scanErr)
	}

	gcMu.Lock()
	got := make([]uint64, len(gcSeen))
	copy(got, gcSeen)
	gcMu.Unlock()

	// Assert the exact boundary sequence.
	if len(got) != len(wantSequence) {
		t.Fatalf("GCHook called %d time(s), want %d; got utxoCounts=%v, want=%v",
			len(got), len(wantSequence), got, wantSequence)
	}
	for i, want := range wantSequence {
		if got[i] != want {
			t.Errorf("GCHook[%d]: utxoCount=%d, want %d", i, got[i], want)
		}
	}
	t.Logf("GCHook boundary sequence confirmed: %v (total outputs = %d)", got, totalOutputs)
}

// TestStartupScanGCCadenceSingleBlockMultipleThresholds confirms that the GC
// hook fires multiple times — at the correct successive multiples of
// gcUTXOInterval — when a single block's output loop crosses more than one
// boundary.
//
// Chain design:
//   - One block at height 1 with 150 001 outputs.
//   - The inner loop must call GCHook(50 000), GCHook(100 000), GCHook(150 000)
//     in that order, all while processing the outputs of this one block.
func TestStartupScanGCCadenceSingleBlockMultipleThresholds(t *testing.T) {
	const numOutputs = 150001 // 3 × gcTestInterval + 1 → fires at 50k, 100k, 150k

	wantSequence := []uint64{
		1 * gcTestInterval, // 50 000
		2 * gcTestInterval, // 100 000
		3 * gcTestInterval, // 150 000
	}

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	dir := t.TempDir()
	db, tip := buildVariedChain(t, dir, []int{numOutputs}, priv, pub)
	tipHashHex := fmt.Sprintf("%x", tip.Hash())

	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	var gcSeen []uint64
	var gcMu sync.Mutex
	hook := func(count uint64) {
		gcMu.Lock()
		gcSeen = append(gcSeen, count)
		gcMu.Unlock()
	}

	var snapWg sync.WaitGroup
	_, scanErr := runStartupScan(startupScanParams{
		DataDir:     dir,
		TipHeight:   tip.Header.Height,
		TipHashHex:  tipHashHex,
		DB:          db,
		UTXOs:       utxos,
		Registry:    registry,
		KiFromIndex: false,
		InitTxTotal: 0,
		Log:         discardLog(),
		SnapshotWg:  &snapWg,
		GCHook:      hook,
	})
	snapWg.Wait()

	if scanErr != nil {
		t.Fatalf("runStartupScan: %v", scanErr)
	}

	gcMu.Lock()
	got := make([]uint64, len(gcSeen))
	copy(got, gcSeen)
	gcMu.Unlock()

	// Assert the exact count and boundary values.
	if len(got) != len(wantSequence) {
		t.Fatalf("GCHook called %d time(s), want %d; got utxoCounts=%v, want=%v",
			len(got), len(wantSequence), got, wantSequence)
	}
	for i, want := range wantSequence {
		if got[i] != want {
			t.Errorf("GCHook[%d]: utxoCount=%d, want %d (should be %d × gcInterval)",
				i, got[i], want, i+1)
		}
	}
	t.Logf("GCHook boundary sequence confirmed: %v for %d-output single block",
		got, numOutputs)
}

// TestRelayNodeOldSnapshotAcceptsNewBlocks confirms the fix introduced in
// task #1485: when a relay node loads from a partial snapshot whose
// Registry.Validators is nil (a snapshot produced before registry snapshotting
// was added), the startup scan seeds the registry from observed block-producer
// pubkeys so the consensus engine can accept new blocks from that validator.
//
// Scenario:
//  1. Build a chain of 10 blocks, all signed by one known validator key.
//  2. Construct a partial snapshot at height 5 with an empty Registry.Validators
//     (simulating a pre-registry-era snapshot file).
//  3. Call runStartupScan — it finds the partial snapshot, restores the empty
//     registry, then scans headers for heights 6–10.
//  4. After the scan the "observed-producer fallback" in scan.go seeds the
//     validator's pubkey as Active (because no stake txs exist and active == 0).
//  5. Assert ≥1 active validator and that its pubkey matches the block producer.
//
// The test would fail on the un-patched code because GetActiveValidators() would
// return an empty slice, leaving the consensus engine with no known validators.
func TestRelayNodeOldSnapshotAcceptsNewBlocks(t *testing.T) {
	const chainHeight = 10   // total blocks produced by the validator
	const snapAtHeight = 5   // intermediate snapshot that pre-dates registry data

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	dir := t.TempDir()

	// Build a 10-block chain; each block has one coinbase output so the DB is
	// non-trivial, but the exact output count does not matter for this test.
	outputCounts := make([]int, chainHeight)
	for i := range outputCounts {
		outputCounts[i] = 2
	}
	db, tip := buildVariedChain(t, dir, outputCounts, priv, pub)

	// Fetch the block at snapAtHeight so we can record its exact hash in the
	// snapshot.  runStartupScan's hash-consistency check requires the snapshot's
	// TipHashHex to match the block stored in LevelDB at that height.
	rawAt5, fetchErr := db.GetRawBlockByHeight(snapAtHeight)
	if fetchErr != nil || rawAt5 == nil {
		t.Fatalf("GetRawBlockByHeight(%d): err=%v, raw=%v", snapAtHeight, fetchErr, rawAt5)
	}
	var blkAt5 core.Block
	if err := json.Unmarshal(rawAt5, &blkAt5); err != nil {
		t.Fatalf("unmarshal block at height %d: %v", snapAtHeight, err)
	}
	hashAt5 := blkAt5.Hash()
	tipHashHex5 := fmt.Sprintf("%x", hashAt5[:])

	// Build the partial snapshot: UTXOs are empty (fine — the full scan will
	// rebuild them), and Registry.Validators is nil to simulate a snapshot that
	// was written before the registry field existed.
	oldSnap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  snapAtHeight,
		TipHashHex: tipHashHex5,
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{}, // empty — scan will rebuild
		Registry: core.RegistrySnapshot{
			Validators: nil, // deliberately nil: pre-registry-era snapshot
		},
	}
	if err := saveStartupSnapshot(dir, oldSnap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	// Set up a fresh UTXOSet and an empty ValidatorRegistry — exactly what the
	// node creates on startup before loading any snapshot.
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	// Do NOT call InitFromGenesis: the registry starts empty, mirroring a node
	// that only has the old snapshot and no in-memory validator state yet.

	tipHashHex := fmt.Sprintf("%x", tip.Hash())

	var snapWg sync.WaitGroup
	_, scanErr := runStartupScan(startupScanParams{
		DataDir:     dir,
		TipHeight:   tip.Header.Height,
		TipHashHex:  tipHashHex,
		DB:          db,
		UTXOs:       utxos,
		Registry:    registry,
		KiFromIndex: false,
		InitTxTotal: 0,
		Log:         discardLog(),
		SnapshotWg:  &snapWg,
	})
	snapWg.Wait()

	if scanErr != nil {
		t.Fatalf("runStartupScan: %v", scanErr)
	}

	// ── Assertion 1: at least one active validator ────────────────────────────
	activeValidators := registry.GetActiveValidators()
	if len(activeValidators) == 0 {
		t.Fatal("registry has no active validators after scan — relay node would reject all new blocks")
	}

	// ── Assertion 2: the seeded pubkey matches the block producer ─────────────
	wantHex := fmt.Sprintf("%x", []byte(pub))
	found := false
	for _, v := range activeValidators {
		if fmt.Sprintf("%x", []byte(v)) == wantHex {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("active validators do not include the known block producer pubkey %s; got %d validator(s)",
			wantHex[:16]+"…", len(activeValidators))
	}

	t.Logf("relay-node old-snapshot bootstrap: %d active validator(s) seeded from observed block producers",
		len(activeValidators))
}
