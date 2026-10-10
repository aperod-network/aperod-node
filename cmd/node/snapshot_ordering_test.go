package main

// snapshot_ordering_test.go — regression guard for the shutdown ordering
// guarantee introduced to fix the snapshot-tip-mismatch race.
//
// The race (now fixed):
//   If GetTip() were called BEFORE close(stop)+<-engineDone, the engine could
//   commit block H+1 between GetTip() (which returned H) and the snapshot
//   rename.  The next startup would find a snapshot at H but a DB tip at H+1,
//   reject the mismatch, and fall back to a 5–6 hour block scan.
//
// The fix (in performShutdown):
//   1. close(stop)   — signal engine.Run to return
//   2. <-engineDone  — block until engine.Run has fully returned (last block
//                      written to DB)
//   3. db.GetTip()   — read the final, stable tip (inside saveShutdownSnapshot)
//   4. save snapshot — height guaranteed == DB tip height
//
// These tests exercise that exact ordering via a fake engine goroutine that
// commits one additional block after the stop signal is sent.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// buildOneMoreBlock creates a signed block at parentTip.Height+1 extending
// parent, persists it and the new tip pointer to db, and returns the block.
// It is called by the fake-engine goroutine to simulate a block committed after
// the shutdown signal arrives but before engineDone is closed.
func buildOneMoreBlock(t *testing.T, db *store.DB, parentTip *core.Block) *core.Block {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("buildOneMoreBlock: GenerateValidatorKey: %v", err)
	}
	height := parentTip.Header.Height + 1
	hdr := core.BlockHeader{
		Height:       height,
		PrevHash:     parentTip.Hash(),
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatalf("buildOneMoreBlock: Sign h=%d: %v", height, err)
	}
	blk := &core.Block{Header: hdr}
	raw, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("buildOneMoreBlock: marshal h=%d: %v", height, err)
	}
	h := blk.Hash()
	if putErr := db.PutRawBlock(h, height, raw); putErr != nil {
		t.Fatalf("buildOneMoreBlock: PutRawBlock h=%d: %v", height, putErr)
	}
	if putErr := db.PutTip(h, height); putErr != nil {
		t.Fatalf("buildOneMoreBlock: PutTip h=%d: %v", height, putErr)
	}
	return blk
}

// TestSnapshotTipMatchesEngineFinalHeight is the core regression guard for the
// shutdown ordering guarantee.
//
// The fake engine goroutine commits one additional block (height N+1) after the
// stop signal arrives — exactly what the real PoA engine does when it is
// mid-block at shutdown.  performShutdown must wait for engineDone before
// reading the tip, so the snapshot must be written at N+1, not at the stale
// intermediate height N.
func TestSnapshotTipMatchesEngineFinalHeight(t *testing.T) {
	dir := t.TempDir()

	// Build initial chain: genesis + 4 blocks → tip at height 4.
	db, blocks := buildChainInStore(t, dir, 4)
	initialTip := blocks[len(blocks)-1]
	initialHeight := initialTip.Header.Height // 4
	finalHeight := initialHeight + 1          // 5 — what the engine will commit

	// stop and engineDone mirror the exact channel pair used in main.go:
	//
	//   engineDone := make(chan struct{})
	//   go func() { engine.Run(stop); close(engineDone) }()
	stop := make(chan struct{})
	engineDone := make(chan struct{})

	// Fake engine goroutine: receives the stop signal, commits one more block
	// (the block that was in-flight when SIGTERM arrived), then closes engineDone.
	// The real engine does the same: it finishes the current tick before returning
	// from Run() after the stop channel is closed.
	var finalBlk *core.Block
	go func() {
		defer close(engineDone)
		<-stop
		finalBlk = buildOneMoreBlock(t, db, initialTip)
	}()

	// performShutdown enforces the correct ordering:
	//   1. close(stop)  — send shutdown signal
	//   2. <-engineDone — wait for the engine's last block to land in the DB
	//   3. (inside) db.GetTip() — reads the final, stable tip (N+1)
	//   4. (inside) save snapshot at N+1
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	performShutdown(stop, engineDone, db, utxos, registry, dir, log, nil)

	// ── Assert 1: snapshot exists at the engine's final committed height (N+1).
	wantPath := snapshotPath(dir, finalHeight)
	if _, err := os.Stat(wantPath); os.IsNotExist(err) {
		t.Errorf("snapshot not found at engine final height %d (path %s)\n"+
			"regression: GetTip() was likely called before <-engineDone, "+
			"capturing stale height %d",
			finalHeight, wantPath, initialHeight)
	}

	// ── Assert 2: no snapshot exists at the stale intermediate height (N).
	// If the ordering were broken, the snapshot would land here instead of N+1.
	stalePath := snapshotPath(dir, initialHeight)
	if _, err := os.Stat(stalePath); err == nil {
		t.Errorf("stale snapshot found at height %d; ordering broken — "+
			"snapshot was written before engineDone was closed (tip should be %d)",
			initialHeight, finalHeight)
	}

	// ── Assert 3: the saved snapshot is loadable at (finalHeight, finalHashHex).
	// This proves the next startup can use the fast path instead of scanning.
	if finalBlk == nil {
		t.Fatal("fake engine goroutine did not produce the final block")
	}
	finalHashHex := fmt.Sprintf("%x", finalBlk.Hash())
	loaded, err := loadStartupSnapshot(dir, finalHeight, finalHashHex)
	if err != nil {
		t.Fatalf("loadStartupSnapshot at height %d: %v — "+
			"snapshot saved at wrong height or corrupt",
			finalHeight, err)
	}
	if loaded.TipHeight != finalHeight {
		t.Errorf("loaded.TipHeight = %d, want %d", loaded.TipHeight, finalHeight)
	}
}

// TestSnapshotReloadNoFullScanAfterShutdown verifies that the snapshot written
// by performShutdown is accepted by the startup fast-path on the next start.
//
// Specifically:
//   - "startup fast path complete — snapshot loaded" MUST appear in the log.
//   - "running startup block scan" MUST NOT appear in the log.
//
// This is the end-to-end confirmation that the ordering guarantee translates
// into a faster restart (skipping the 5–6 h block scan).
func TestSnapshotReloadNoFullScanAfterShutdown(t *testing.T) {
	dir := t.TempDir()

	// Build chain: genesis + 3 blocks → tip at height 3.
	db, blocks := buildChainInStore(t, dir, 3)
	tip := blocks[len(blocks)-1]
	tipHeight := tip.Header.Height
	tipHashHex := fmt.Sprintf("%x", tip.Hash())

	// Simulate shutdown where the engine stops immediately on the signal
	// (no in-flight block — the common steady-state case).
	stop := make(chan struct{})
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		<-stop // stop immediately; no extra block
	}()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	performShutdown(stop, engineDone, db, utxos, registry, dir, log, nil)

	// ── Simulate startup: run the fast-path logic with a captured logger.
	// This mirrors the loadStartupSnapshot + log calls in main.go run().
	var logBuf bytes.Buffer
	captureLog := newCaptureLogger(&logBuf)

	snapLoaded := false
	if loaded, serr := loadStartupSnapshot(dir, tipHeight, tipHashHex); serr == nil {
		_ = loaded
		captureLog.Info("startup fast path complete — snapshot loaded",
			"tip_height", tipHeight,
			"active_utxos", len(loaded.UTXOs.ActiveUTXOs),
			"key_images", len(loaded.UTXOs.KeyImages),
		)
		snapLoaded = true
	} else if !os.IsNotExist(serr) {
		captureLog.Warn("snapshot load error, falling back to block scan", "err", serr)
	}
	if !snapLoaded {
		captureLog.Info("running startup block scan",
			"tip_height", tipHeight,
			"ki_from_index", false,
			"heap_sys_mib_before", uint64(0),
		)
	}

	// ── Assertions.
	if !logContainsMsg(&logBuf, "startup fast path complete — snapshot loaded") {
		t.Error("fast-path log absent: the snapshot saved by shutdown was not accepted on restart " +
			"— node would fall back to a 5–6 h block scan")
		t.Logf("captured log:\n%s", logBuf.String())
	}
	if logContainsMsg(&logBuf, "running startup block scan") {
		t.Error("block-scan log must not appear when a valid shutdown snapshot is present")
		t.Logf("captured log:\n%s", logBuf.String())
	}
}
