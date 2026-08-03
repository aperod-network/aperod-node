package main

// Integration test: partial-snapshot resume after a crash mid-scan.
//
// Covers the four requirements from task #1048:
//  1. A snapshot saved at block N (the "crash point") is found by
//     findLatestSnapshot when the chain tip is > N.
//  2. The startup path (runStartupScan) logs
//     "partial snapshot loaded — resuming scan from checkpoint".
//  3. runStartupScan returns ScanFrom == N+1, not 1.
//  4. The final UTXOSet state matches a node that scanned all blocks from 1.
//
// All main-path assertions are driven through runStartupScan (the production
// function), not through a hand-rolled re-implementation in test code.

import (
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

// ─── helpers ─────────────────────────────────────────────────────────────────

// buildChainResume creates a genesis + nBlocks chain in a fresh SQLite store.
// Returns the open store, all blocks (genesis first), and the validator keys.
func buildChainResume(t *testing.T, dir string, nBlocks int) (*store.DB, []*core.Block, crypto.ValidatorPrivKey, crypto.ValidatorPubKey) {
	t.Helper()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	makeBlk := func(height uint64, prev crypto.Hash32) *core.Block {
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prev,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		}
		if err := hdr.Sign(priv); err != nil {
			t.Fatalf("Sign h=%d: %v", height, err)
		}
		return &core.Block{Header: hdr}
	}

	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var blocks []*core.Block
	genesis := makeBlk(0, crypto.Hash32{})
	blocks = append(blocks, genesis)

	putBlk := func(b *core.Block) {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal h=%d: %v", b.Header.Height, err)
		}
		h := b.Hash()
		if err := db.PutRawBlock(h, b.Header.Height, raw); err != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, err)
		}
	}
	putBlk(genesis)

	parent := genesis
	for i := 1; i <= nBlocks; i++ {
		blk := makeBlk(uint64(i), parent.Hash())
		putBlk(blk)
		blocks = append(blocks, blk)
		parent = blk
	}

	tip := blocks[len(blocks)-1]
	if err := db.PutTip(tip.Hash(), tip.Header.Height); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	return db, blocks, priv, pub
}

// applyBlockRange applies blocks[lo:hi+1] (inclusive, zero-based) to utxos.
func applyBlockRange(t *testing.T, utxos *core.UTXOSet, blocks []*core.Block, lo, hi int) {
	t.Helper()
	for i := lo; i <= hi; i++ {
		if err := utxos.ApplyBlock(blocks[i]); err != nil {
			t.Fatalf("ApplyBlock h=%d: %v", blocks[i].Header.Height, err)
		}
	}
}

// ─── Test 1: end-to-end mid-scan resume via runStartupScan ───────────────────

// TestPartialSnapshotResume is the main integration test for task #1048.
//
// Scenario:
//   - Chain has 10 blocks (heights 0–10).
//   - The node crashed mid-scan at block 5: a partial checkpoint was written at
//     height 5 but the scan never reached the tip (height 10).
//   - On restart, runStartupScan (the production function) must:
//       (a) find the height-5 checkpoint via findLatestSnapshot,
//       (b) log "partial snapshot loaded — resuming scan from checkpoint",
//       (c) return ScanFrom == 6 (not 1),
//       (d) leave p.UTXOs with the same count as a full scan from block 1.
func TestPartialSnapshotResume(t *testing.T) {
	const totalBlocks = 10 // heights 1–10 plus genesis at 0
	const checkpointAt = 5 // "crash" height: partial checkpoint saved here

	dir := t.TempDir()
	db, blocks, _, pub := buildChainResume(t, dir, totalBlocks)

	tip := blocks[len(blocks)-1]
	tipHeight := tip.Header.Height // 10
	tipHashArr := tip.Hash()
	tipHashHex := fmt.Sprintf("%x", tipHashArr[:])

	// ── Reference: apply all blocks to get the expected UTXO count.
	refUTXOs := core.NewUTXOSet()
	applyBlockRange(t, refUTXOs, blocks, 0, len(blocks)-1)
	refCount := refUTXOs.Count()

	// ── Simulate the crash: scan up to block 5, save a checkpoint, then stop.
	cpUTXOs := core.NewUTXOSet()
	applyBlockRange(t, cpUTXOs, blocks, 0, checkpointAt)
	cpRegistry := core.NewValidatorRegistry()
	cpRegistry.SetUTXOSet(cpUTXOs)
	cpRegistry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	cpBlock := blocks[checkpointAt]
	cpHashArr := cpBlock.Hash()
	cpSnap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  uint64(checkpointAt),
		TipHashHex: fmt.Sprintf("%x", cpHashArr[:]),
		TxTotal:    int64(checkpointAt),
		UTXOs:      cpUTXOs.TakeSnapshot(),
		Registry:   cpRegistry.TakeSnapshot(),
	}
	if err := saveStartupSnapshot(dir, cpSnap); err != nil {
		t.Fatalf("saveStartupSnapshot(checkpoint): %v", err)
	}

	// Sanity: the checkpoint file must exist before we test the resume path.
	if _, err := os.Stat(snapshotPath(dir, uint64(checkpointAt))); os.IsNotExist(err) {
		t.Fatalf("checkpoint snapshot file not created: %s",
			snapshotPath(dir, uint64(checkpointAt)))
	}

	// ── Simulate restart: fresh UTXOSet + registry (nothing pre-loaded).
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	// ── Call the production function under test.
	var wg sync.WaitGroup
	result, err := runStartupScan(startupScanParams{
		DataDir:     dir,
		TipHeight:   tipHeight,
		TipHashHex:  tipHashHex,
		DB:          db,
		UTXOs:       utxos,
		Registry:    registry,
		KiFromIndex: false, // fresh start — no index pre-loaded
		InitTxTotal: 0,
		Log:         log,
		SnapshotWg:  &wg,
	})
	if err != nil {
		t.Fatalf("runStartupScan: %v", err)
	}
	// Wait for all snapshot goroutines to finish before asserting on log output
	// or allowing t.TempDir() cleanup.  Without this the goroutines race with
	// directory removal and write to the log buffer after the test reads it.
	wg.Wait()

	// ── Assertion 1: production code logged the partial-snapshot message.
	if !logContainsMsg(&logBuf, "partial snapshot loaded — resuming scan from checkpoint") {
		t.Errorf("expected log \"partial snapshot loaded — resuming scan from checkpoint\" was not emitted\nlog:\n%s", logBuf.String())
	}

	// ── Assertion 2: scan started from checkpoint+1, not from block 1.
	wantScanFrom := uint64(checkpointAt + 1)
	if result.ScanFrom != wantScanFrom {
		t.Errorf("ScanFrom = %d, want %d (checkpoint height %d + 1)",
			result.ScanFrom, wantScanFrom, checkpointAt)
	}

	// ── Assertion 3: final UTXO count equals the full-scan reference.
	if got := utxos.Count(); got != refCount {
		t.Errorf("UTXOSet.Count() after partial resume = %d, want %d (full-scan reference)",
			got, refCount)
	}
}

// ─── Test 2: no checkpoint → runStartupScan scans from block 1 ───────────────

// TestNoPartialSnapshot_ScanFromOne verifies that when no intermediate
// checkpoint exists, runStartupScan scans from height 1, logs
// "running startup block scan", and does NOT log the partial-snapshot message.
func TestNoPartialSnapshot_ScanFromOne(t *testing.T) {
	const totalBlocks = 5

	dir := t.TempDir() // empty — no checkpoint files at all
	db, blocks, _, pub := buildChainResume(t, dir, totalBlocks)

	tip := blocks[len(blocks)-1]
	tipHeight := tip.Header.Height
	tipHashArr := tip.Hash()
	tipHashHex := fmt.Sprintf("%x", tipHashArr[:])

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	var wg sync.WaitGroup
	result, err := runStartupScan(startupScanParams{
		DataDir:     dir,
		TipHeight:   tipHeight,
		TipHashHex:  tipHashHex,
		DB:          db,
		UTXOs:       utxos,
		Registry:    registry,
		KiFromIndex: false,
		InitTxTotal: 0,
		Log:         log,
		SnapshotWg:  &wg,
	})
	if err != nil {
		t.Fatalf("runStartupScan: %v", err)
	}
	wg.Wait() // ensure all snapshot goroutines finish before asserting

	if logContainsMsg(&logBuf, "partial snapshot loaded — resuming scan from checkpoint") {
		t.Error("partial-snapshot log must NOT appear when no checkpoint exists")
	}
	if !logContainsMsg(&logBuf, "running startup block scan") {
		t.Error("block-scan log must appear when no partial snapshot exists")
	}
	if result.ScanFrom != 1 {
		t.Errorf("ScanFrom = %d, want 1 when no checkpoint exists", result.ScanFrom)
	}
}

// ─── Test 3: findLatestSnapshot honours limitHeight ──────────────────────────

// TestFindLatestSnapshot_LimitHeight verifies that findLatestSnapshot returns
// the highest checkpoint strictly below limitHeight and never at or above it.
func TestFindLatestSnapshot_LimitHeight(t *testing.T) {
	dir := t.TempDir()

	save := func(h uint64) {
		snap := startupSnapshot{
			Version:    snapVersion,
			TipHeight:  h,
			TipHashHex: fmt.Sprintf("%016x", h),
			TxTotal:    int64(h),
			UTXOs:      core.UTXOSnapshot{},
			Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
		}
		if err := saveStartupSnapshot(dir, snap); err != nil {
			t.Fatalf("saveStartupSnapshot(%d): %v", h, err)
		}
	}

	save(100)
	save(200)
	save(300)

	tests := []struct {
		limit   uint64
		wantH   uint64
		wantNil bool
	}{
		{limit: 400, wantH: 300},
		{limit: 301, wantH: 300},
		{limit: 300, wantH: 200}, // 300 not strictly below 300
		{limit: 200, wantH: 100},
		{limit: 100, wantNil: true},
		{limit: 1, wantNil: true},
	}

	for _, tc := range tests {
		got := findLatestSnapshot(dir, tc.limit, nil)
		if tc.wantNil {
			if got != nil {
				t.Errorf("findLatestSnapshot(%d) = height %d, want nil",
					tc.limit, got.TipHeight)
			}
		} else {
			if got == nil {
				t.Errorf("findLatestSnapshot(%d) = nil, want height %d",
					tc.limit, tc.wantH)
			} else if got.TipHeight != tc.wantH {
				t.Errorf("findLatestSnapshot(%d).TipHeight = %d, want %d",
					tc.limit, got.TipHeight, tc.wantH)
			}
		}
	}
}

// ─── Test 4: corrupt partial snapshot is skipped ─────────────────────────────

// TestFindLatestSnapshot_CorruptFile verifies that findLatestSnapshot skips a
// snapshot file whose JSON is truncated/corrupt and returns nil rather than
// returning a broken struct.
func TestFindLatestSnapshot_CorruptFile(t *testing.T) {
	dir := t.TempDir()

	corruptPath := snapshotPath(dir, 50)
	if err := os.WriteFile(corruptPath, []byte(`{"v":1,"tip_height":50,"tip_hash":"abc`), 0644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	got := findLatestSnapshot(dir, 100, nil)
	if got != nil {
		t.Errorf("findLatestSnapshot returned height=%d for a corrupt file, want nil",
			got.TipHeight)
	}
}

// ─── Test 4b: corrupt primary recovered from prev-backup ─────────────────────

// TestFindLatestSnapshot_CorruptPrimaryRecoveredFromPrev verifies that when a
// candidate primary is corrupt (truncated JSON), findLatestSnapshot falls back
// to the adjacent "-prev.json" backup for the same height instead of discarding
// the checkpoint entirely and returning nil (or an older checkpoint).
//
// Setup:
//   - height-50 snapshot saved normally via saveStartupSnapshot (creates both
//     primary and prev-backup as a side-effect of the production save path).
//   - Primary at height 50 is then overwritten with corrupt bytes.
//   - findLatestSnapshot(dir, 100, log) must return a snapshot at height 50
//     recovered from the prev-backup, not nil.
//   - A warning log must be emitted to notify the operator.
func TestFindLatestSnapshot_CorruptPrimaryRecoveredFromPrev(t *testing.T) {
	dir := t.TempDir()

	// Save a valid snapshot at height 50.  saveStartupSnapshot writes both
	// the primary and a same-height "-prev.json" backup atomically.
	snap50 := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  50,
		TipHashHex: fmt.Sprintf("%016x", uint64(50)),
		TxTotal:    50,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	if err := saveStartupSnapshot(dir, snap50); err != nil {
		t.Fatalf("saveStartupSnapshot(50): %v", err)
	}

	// Verify the prev-backup was created (precondition for the test to be meaningful).
	prevP := snapshotPrevPath(snapshotPath(dir, 50))
	if _, err := os.Stat(prevP); os.IsNotExist(err) {
		t.Fatalf("prev-backup at height 50 not created by saveStartupSnapshot — precondition failed")
	}

	// Corrupt the primary at height 50 (truncated JSON).
	if err := os.WriteFile(snapshotPath(dir, 50), []byte(`{"v":1,"tip_height":50,"tip_hash":"ab`), 0644); err != nil {
		t.Fatalf("corrupt primary at height 50: %v", err)
	}

	// Capture log output so we can assert the recovery warning was emitted.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	got := findLatestSnapshot(dir, 100, log)

	// ── Assertion 1: a snapshot was returned (not nil).
	if got == nil {
		t.Fatal("findLatestSnapshot returned nil — expected recovery from prev-backup at height 50")
	}

	// ── Assertion 2: the recovered snapshot is at the right height.
	if got.TipHeight != 50 {
		t.Errorf("recovered snapshot TipHeight = %d, want 50", got.TipHeight)
	}

	// ── Assertion 3: operator-visible warning was logged.
	if !logContainsMsg(&logBuf, "checkpoint recovery — primary corrupt, loaded prev-backup") {
		t.Errorf("expected recovery warning log was not emitted\nlog:\n%s", logBuf.String())
	}
}

// ─── Test 5: hash mismatch → checkpoint discarded, scan from 1 ───────────────

// TestPartialSnapshot_HashMismatch verifies that when the snapshot's
// TipHashHex does not match the actual block stored in the DB at that height,
// runStartupScan discards the checkpoint, logs a warning, and scans from
// block 1 (ScanFrom == 1) instead of from checkpoint+1.
func TestPartialSnapshot_HashMismatch(t *testing.T) {
	const totalBlocks = 10
	const checkpointAt = 5

	dir := t.TempDir()
	db, blocks, _, pub := buildChainResume(t, dir, totalBlocks)

	tip := blocks[len(blocks)-1]
	tipHeight := tip.Header.Height // 10
	tipHashArr := tip.Hash()
	tipHashHex := fmt.Sprintf("%x", tipHashArr[:])

	// Build a checkpoint snapshot at height 5 but with a deliberately wrong
	// TipHashHex so that the DB cross-check will detect the mismatch.
	cpUTXOs := core.NewUTXOSet()
	applyBlockRange(t, cpUTXOs, blocks, 0, checkpointAt)
	cpRegistry := core.NewValidatorRegistry()
	cpRegistry.SetUTXOSet(cpUTXOs)
	cpRegistry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	wrongHashHex := fmt.Sprintf("%064x", 0xdeadbeef) // bogus hash — will not match DB
	cpSnap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  uint64(checkpointAt),
		TipHashHex: wrongHashHex,
		TxTotal:    int64(checkpointAt),
		UTXOs:      cpUTXOs.TakeSnapshot(),
		Registry:   cpRegistry.TakeSnapshot(),
	}
	if err := saveStartupSnapshot(dir, cpSnap); err != nil {
		t.Fatalf("saveStartupSnapshot(checkpoint): %v", err)
	}

	// Sanity: the checkpoint file exists.
	if _, err := os.Stat(snapshotPath(dir, uint64(checkpointAt))); os.IsNotExist(err) {
		t.Fatalf("checkpoint snapshot file not created")
	}

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	var wg sync.WaitGroup
	result, err := runStartupScan(startupScanParams{
		DataDir:     dir,
		TipHeight:   tipHeight,
		TipHashHex:  tipHashHex,
		DB:          db,
		UTXOs:       utxos,
		Registry:    registry,
		KiFromIndex: false,
		InitTxTotal: 0,
		Log:         log,
		SnapshotWg:  &wg,
	})
	if err != nil {
		t.Fatalf("runStartupScan: %v", err)
	}
	wg.Wait()

	// ── Assertion 1: warning log emitted for the discarded checkpoint.
	if !logContainsMsg(&logBuf, "partial snapshot discarded — hash mismatch against DB block") {
		t.Errorf("expected hash-mismatch warning was not logged\nlog:\n%s", logBuf.String())
	}

	// ── Assertion 2: the partial-snapshot resume log must NOT appear (checkpoint was discarded).
	if logContainsMsg(&logBuf, "partial snapshot loaded — resuming scan from checkpoint") {
		t.Error("partial-snapshot resume log must NOT appear when hash mismatches")
	}

	// ── Assertion 3: scan started from block 1, not from checkpoint+1.
	if result.ScanFrom != 1 {
		t.Errorf("ScanFrom = %d, want 1 when checkpoint hash mismatches", result.ScanFrom)
	}

	// ── Assertion 4: UTXO count matches a full scan from block 1.
	refUTXOs := core.NewUTXOSet()
	applyBlockRange(t, refUTXOs, blocks, 0, len(blocks)-1)
	if got, want := utxos.Count(), refUTXOs.Count(); got != want {
		t.Errorf("UTXOSet.Count() after discarded checkpoint = %d, want %d (full-scan reference)", got, want)
	}
}

// ─── Test 6: multiple checkpoints — highest eligible is chosen ───────────────

// ─── Test 7: corrupt primary → runStartupScan accepts prev-backup via DB hash cross-check ──

// TestCorruptCheckpointRunStartupScan_PrevBackupAccepted is the end-to-end
// integration test for task #1075.
//
// It confirms that the two recovery layers compose correctly:
//
//  1. findLatestSnapshot (inner layer) falls back to the "-prev.json" backup
//     and returns a snapshot struct for the corrupt-primary height.
//  2. runStartupScan (outer layer) then cross-checks that struct's TipHashHex
//     against the actual DB block at that height.  Because the prev-backup was
//     written from the same valid checkpoint data, the hashes match and the
//     resume path is accepted — not discarded.
//
// Scenario:
//   - Chain: heights 0–10 (genesis + 10 blocks).
//   - Checkpoint saved at height 5 via saveStartupSnapshot (writes both the
//     primary file and a same-height "-prev.json" backup atomically).
//   - Primary at height 5 is then overwritten with truncated/corrupt bytes.
//   - runStartupScan is called with tip == height 10.
//
// Expected outcome:
//   - "checkpoint recovery — primary corrupt, loaded prev-backup" warning is logged.
//   - "partial snapshot loaded — resuming scan from checkpoint" is logged.
//   - result.ScanFrom == 6 (prev-backup checkpoint accepted by both layers).
//   - Final UTXOSet count matches the full-scan reference.
func TestCorruptCheckpointRunStartupScan_PrevBackupAccepted(t *testing.T) {
	const totalBlocks = 10 // heights 1–10 plus genesis at 0
	const checkpointAt = 5 // height at which the checkpoint is saved

	dir := t.TempDir()
	db, blocks, _, pub := buildChainResume(t, dir, totalBlocks)

	tip := blocks[len(blocks)-1]
	tipHeight := tip.Header.Height // 10
	tipHashArr := tip.Hash()
	tipHashHex := fmt.Sprintf("%x", tipHashArr[:])

	// ── Reference: apply all blocks to get the expected UTXO count.
	refUTXOs := core.NewUTXOSet()
	applyBlockRange(t, refUTXOs, blocks, 0, len(blocks)-1)
	refCount := refUTXOs.Count()

	// ── Save a valid checkpoint at height 5.
	// saveStartupSnapshot writes both the primary and a same-height "-prev.json"
	// backup atomically.  The prev-backup will be the recovery target.
	cpUTXOs := core.NewUTXOSet()
	applyBlockRange(t, cpUTXOs, blocks, 0, checkpointAt)
	cpRegistry := core.NewValidatorRegistry()
	cpRegistry.SetUTXOSet(cpUTXOs)
	cpRegistry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	cpBlock := blocks[checkpointAt]
	cpHashArr := cpBlock.Hash()
	cpSnap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  uint64(checkpointAt),
		TipHashHex: fmt.Sprintf("%x", cpHashArr[:]),
		TxTotal:    int64(checkpointAt),
		UTXOs:      cpUTXOs.TakeSnapshot(),
		Registry:   cpRegistry.TakeSnapshot(),
	}
	if err := saveStartupSnapshot(dir, cpSnap); err != nil {
		t.Fatalf("saveStartupSnapshot(checkpoint): %v", err)
	}

	// ── Precondition: prev-backup must exist before we corrupt the primary.
	prevP := snapshotPrevPath(snapshotPath(dir, uint64(checkpointAt)))
	if _, err := os.Stat(prevP); os.IsNotExist(err) {
		t.Fatalf("prev-backup at height %d not created by saveStartupSnapshot — precondition failed", checkpointAt)
	}

	// ── Corrupt the primary at height 5 (truncated JSON).
	if err := os.WriteFile(snapshotPath(dir, uint64(checkpointAt)),
		[]byte(`{"v":1,"tip_height":5,"tip_hash":"bad`), 0644); err != nil {
		t.Fatalf("corrupt primary at height %d: %v", checkpointAt, err)
	}

	// ── Simulate restart: fresh UTXOSet + registry.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	// ── Call the production function under test.
	var wg sync.WaitGroup
	result, err := runStartupScan(startupScanParams{
		DataDir:     dir,
		TipHeight:   tipHeight,
		TipHashHex:  tipHashHex,
		DB:          db,
		UTXOs:       utxos,
		Registry:    registry,
		KiFromIndex: false,
		InitTxTotal: 0,
		Log:         log,
		SnapshotWg:  &wg,
	})
	if err != nil {
		t.Fatalf("runStartupScan: %v", err)
	}
	wg.Wait() // ensure all snapshot goroutines finish before asserting

	// ── Assertion 1: findLatestSnapshot logged the recovery warning.
	if !logContainsMsg(&logBuf, "checkpoint recovery — primary corrupt, loaded prev-backup") {
		t.Errorf("expected recovery warning log was not emitted\nlog:\n%s", logBuf.String())
	}

	// ── Assertion 2: runStartupScan accepted the prev-backup and logged resume.
	if !logContainsMsg(&logBuf, "partial snapshot loaded — resuming scan from checkpoint") {
		t.Errorf("expected partial-resume log was not emitted (DB hash cross-check may have rejected the prev-backup)\nlog:\n%s", logBuf.String())
	}

	// ── Assertion 3: scan started from checkpoint+1 (prev-backup accepted).
	wantScanFrom := uint64(checkpointAt + 1)
	if result.ScanFrom != wantScanFrom {
		t.Errorf("ScanFrom = %d, want %d (prev-backup checkpoint height %d + 1)\nlog:\n%s",
			result.ScanFrom, wantScanFrom, checkpointAt, logBuf.String())
	}

	// ── Assertion 4: final UTXO count equals the full-scan reference.
	if got := utxos.Count(); got != refCount {
		t.Errorf("UTXOSet.Count() after corrupt-primary recovery = %d, want %d (full-scan reference)",
			got, refCount)
	}
}

// ─── Test 8: multiple checkpoints — highest eligible is chosen ───────────────

// TestFindLatestSnapshot_MultipleCheckpoints saves several checkpoints and
// confirms findLatestSnapshot always picks the highest one strictly below
// limitHeight, which also sets the correct scanFrom.
func TestFindLatestSnapshot_MultipleCheckpoints(t *testing.T) {
	dir := t.TempDir()

	heights := []uint64{50_000, 100_000, 150_000}
	for _, h := range heights {
		snap := startupSnapshot{
			Version:    snapVersion,
			TipHeight:  h,
			TipHashHex: fmt.Sprintf("%016x", h),
			TxTotal:    int64(h),
			UTXOs:      core.UTXOSnapshot{},
			Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
		}
		if err := saveStartupSnapshot(dir, snap); err != nil {
			t.Fatalf("saveStartupSnapshot(%d): %v", h, err)
		}
	}

	// Chain crashed mid-scan at height 180 000.
	got := findLatestSnapshot(dir, 180_000, nil)
	if got == nil {
		t.Fatal("findLatestSnapshot(180000) = nil, want height 150000")
	}
	if got.TipHeight != 150_000 {
		t.Errorf("findLatestSnapshot(180000).TipHeight = %d, want 150000", got.TipHeight)
	}

	// scanFrom would be 150 001.
	if scanFrom := got.TipHeight + 1; scanFrom != 150_001 {
		t.Errorf("scanFrom = %d, want 150001", scanFrom)
	}
}
