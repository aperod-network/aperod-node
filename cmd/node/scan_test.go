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

	"github.com/aperod/aperod/consensus"
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
			Round:        uint32(height),
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
// pubkeys — and a relay consensus engine built from that registry then accepts
// a freshly signed block from the producer, advancing the chain.
//
// Scenario:
//  1. Build a chain of 10 blocks, all signed by one known validator key.
//  2. Construct a realistic partial snapshot at height 5: its UTXO set is the
//     genuine state after applying blocks 1–5, but Registry.Validators is nil
//     (simulating a snapshot file written before the registry field existed).
//  3. Call runStartupScan — it finds the partial snapshot, restores the empty
//     registry + snapshot UTXOs, then scans blocks 6–10.
//  4. Assert the observed-producer fallback seeded ≥1 active validator whose
//     pubkey matches the block producer, and that the UTXO set is complete.
//  5. Build a relay consensus.Engine (MyKey=nil, exactly like main.go's
//     non-validator path) from the scanned registry, replay the chain into it,
//     submit a NEW block at height 11 signed by the producer, and assert the
//     relay chain advances to height 11.
//
// Without the fix in scan.go, GetActiveValidators() returns an empty slice
// after step 3, isKnownValidator() fails for every incoming block, and step 5
// times out with the chain stuck at height 10.
func TestRelayNodeOldSnapshotAcceptsNewBlocks(t *testing.T) {
	const chainHeight = 10   // total blocks produced by the validator
	const snapAtHeight = 5   // intermediate snapshot that pre-dates registry data
	const outsPerBlock = 2   // coinbase outputs per block

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	dir := t.TempDir()

	outputCounts := make([]int, chainHeight)
	for i := range outputCounts {
		outputCounts[i] = outsPerBlock
	}
	db, tip := buildVariedChain(t, dir, outputCounts, priv, pub)

	// ── Step 2: build a realistic pre-registry snapshot at height 5 ───────────
	// Apply blocks 1–5 to a scratch UTXO set so the snapshot carries the true
	// UTXO state at that height — exactly what an old binary would have saved.
	// Also collect the blocks for the relay-chain replay in step 5.
	preUTXOs := core.NewUTXOSet()
	blocksByHeight := make([]*core.Block, chainHeight+1) // index = height
	for h := uint64(0); h <= chainHeight; h++ {
		raw, fetchErr := db.GetRawBlockByHeight(h)
		if fetchErr != nil || raw == nil {
			t.Fatalf("GetRawBlockByHeight(%d): err=%v", h, fetchErr)
		}
		var b core.Block
		if uerr := json.Unmarshal(raw, &b); uerr != nil {
			t.Fatalf("unmarshal block at height %d: %v", h, uerr)
		}
		blocksByHeight[h] = &b
		if h >= 1 && h <= snapAtHeight {
			if aerr := preUTXOs.ApplyBlock(&b); aerr != nil {
				t.Fatalf("ApplyBlock h=%d: %v", h, aerr)
			}
		}
	}
	hashAt5 := blocksByHeight[snapAtHeight].Hash()
	tipHashHex5 := fmt.Sprintf("%x", hashAt5[:])

	// The snapshot has genuine UTXOs but a nil Validators map — the exact shape
	// of a checkpoint written before registry snapshotting was added.
	oldSnap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  snapAtHeight,
		TipHashHex: tipHashHex5,
		TxTotal:    0,
		UTXOs:      preUTXOs.TakeSnapshot(),
		Registry: core.RegistrySnapshot{
			Validators: nil, // deliberately nil: pre-registry-era snapshot
		},
	}
	if err := saveStartupSnapshot(dir, oldSnap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	// ── Step 3: startup scan, exactly as a restarting relay node runs it ──────
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	// Do NOT call InitFromGenesis: the registry starts empty, mirroring a node
	// that only has the old snapshot and no in-memory validator state yet.

	tipHashHex := fmt.Sprintf("%x", tip.Hash())

	var snapWg sync.WaitGroup
	res, scanErr := runStartupScan(startupScanParams{
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
	// The scan must have resumed from the partial snapshot, not from block 1.
	if want := uint64(snapAtHeight + 1); res.ScanFrom != want {
		t.Fatalf("scan resumed from %d, want %d (partial snapshot was not used)",
			res.ScanFrom, want)
	}
	// The rebuilt UTXO set must be complete: snapshot state (blocks 1–5) plus
	// the scanned range (blocks 6–10).
	if wantUTXOs := chainHeight * outsPerBlock; utxos.Count() != wantUTXOs {
		t.Fatalf("UTXO count after scan = %d, want %d (snapshot + scanned range)",
			utxos.Count(), wantUTXOs)
	}

	// ── Step 4: the observed-producer fallback seeded the registry ────────────
	activeValidators := registry.GetActiveValidators()
	if len(activeValidators) == 0 {
		t.Fatal("registry has no active validators after scan — relay node would reject all new blocks")
	}
	wantHex := fmt.Sprintf("%x", []byte(pub))
	found := false
	for _, v := range activeValidators {
		if fmt.Sprintf("%x", []byte(v)) == wantHex {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("active validators do not include the known block producer pubkey %s…; got %d validator(s)",
			wantHex[:16], len(activeValidators))
	}

	// ── Step 5: relay engine built from the seeded registry accepts a new block
	// Mirrors main.go's NonValidator=true path: Config.Validators comes from the
	// registry's active set, MyKey is nil (never produces blocks).
	relayChain := core.NewChain()
	if err := relayChain.SetGenesis(blocksByHeight[0]); err != nil {
		t.Fatalf("relay SetGenesis: %v", err)
	}
	for h := uint64(1); h <= chainHeight; h++ {
		if err := relayChain.AddBlock(blocksByHeight[h]); err != nil {
			t.Fatalf("relay AddBlock h=%d: %v", h, err)
		}
	}

	relayMp := core.NewMempool(core.DefaultMempoolConfig())
	relayEng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   activeValidators, // seeded by the observed-producer fallback
		Registry:     registry,
		MyKey:        nil, // non-validator relay: never produces blocks
		RingCTV4ActivationHeight: ^uint64(0),
		OnCanonicalBlock: noopCanonicalPersistence,
	}, relayChain, relayMp, discardLog())
	relayEng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	relayStop := make(chan struct{})
	go relayEng.Run(relayStop)
	defer close(relayStop)

	// A brand-new block at height 11 arrives from the producer after restart.
	newBlk := makeSignedBlk(t, chainHeight+1, tip.Hash(), priv, pub)
	relayEng.NewBlockCh() <- newBlk

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("relay chain height = %d after 2 s, want %d; "+
				"the engine rejected the new block despite the registry being "+
				"seeded from observed block producers (task #1485 regression)",
				relayChain.Height(), chainHeight+1)
		default:
			if relayChain.Height() == chainHeight+1 {
				t.Logf("relay-node old-snapshot bootstrap: %d validator(s) seeded; "+
					"new block %d accepted, chain advanced",
					len(activeValidators), chainHeight+1)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
