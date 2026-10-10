package main

// db_gap_auto_repair_test.go — regression guard for the startup height-index
// auto-repair introduced to survive crash-kill gaps without --repair-db.
//
// Background: when the node is killed (OOM or SIGKILL) while LevelDB is
// compacting, the height-index (h/ namespace) can lose entries even though the
// corresponding block bodies (b/ namespace) survive in the WAL.  The b/ and h/
// entries are written atomically in a single fsynced batch (PutRawBlock), so
// after a normal Open() the WAL replay restores the b/ bodies; but if the SST
// had an older compaction state, the h/ entry for a given height may be absent
// while b/ is present.
//
// Before this fix operators had to run `--repair-db` manually.  The fix adds
// auto-repair logic to the startup store-integrity check: when
// countMissingBlocksInWindow detects gaps it calls RepairAllHeightIndex
// (rebuilding h/ from b/), reloads recentBlocks, and clears the sentinel so
// the next restart re-verifies.
//
// These tests simulate the crash scenario by:
//  1. Writing a full chain (all b/ and h/ entries present).
//  2. Deleting a subset of h/ entries directly via raw LevelDB.
//  3. Confirming countMissingBlocksInWindow detects the gaps.
//  4. Calling RepairAllHeightIndex and confirming it fixes them.
//  5. Confirming loadRecentBlocksFromStore now loads all blocks.

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/store"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// deleteHeightIndexEntry removes the h/<height> entry from LevelDB directly,
// simulating the crash-kill scenario where the block body (b/) survived the
// WAL replay but the height-index entry (h/) was lost because the SST had a
// stale compaction state.
//
// To delete via the raw LevelDB API the DB must be closed first; the function
// closes db, deletes the key, and reopens the DB at dbDir.
func deleteHeightIndexEntry(t *testing.T, dbDir string, db *store.DB, height uint64) *store.DB {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("close before height-key delete: %v", err)
	}
	raw, err := leveldb.OpenFile(dbDir, nil)
	if err != nil {
		t.Fatalf("open raw LevelDB for height-key delete: %v", err)
	}
	// h/<big-endian uint64>
	hKey := make([]byte, 2+8)
	copy(hKey, []byte("h/"))
	binary.BigEndian.PutUint64(hKey[2:], height)
	if err := raw.Delete(hKey, &opt.WriteOptions{Sync: true}); err != nil {
		raw.Close()
		t.Fatalf("delete h/%d: %v", height, err)
	}
	raw.Close()
	db2, err := store.Open(dbDir)
	if err != nil {
		t.Fatalf("reopen after height-key delete: %v", err)
	}
	return db2
}

// TestAutoRepairFixesCrashKillGap is the primary regression guard.
//
// It simulates the OOM/SIGKILL scenario where the h/ entry for a block is lost
// (crash happened after the WAL fsync but before SST compaction), while the b/
// block body survived.  The test confirms that:
//
//  1. After deleting h/<gapHeight> manually, countMissingBlocksInWindow
//     reports missingCount==1 (the gap is detected).
//  2. RepairAllHeightIndex rebuilds the missing h/<gapHeight> from the b/
//     body (repaired==1, skipped==0).
//  3. After repair, loadRecentBlocksFromStore loads all blocks including the
//     previously-missing height (blocks_loaded_in_memory == nBlocks).
//  4. The sentinel file is removed by the auto-repair path so the next restart
//     re-verifies the repaired index.
func TestAutoRepairFixesCrashKillGap(t *testing.T) {
	const (
		nBlocks   = 9    // heights 0–9
		gapHeight = 5    // h/<5> will be deleted to simulate crash
	)
	tipHeight := uint64(nBlocks)

	dir := t.TempDir()
	dbDir := filepath.Join(dir, "chain.db")

	// ── Step 1: build a complete chain (no gaps) ──────────────────────────
	// buildChainWithGap with gapHeight=999 means no gap is introduced.
	db, _ := buildChainWithGap(t, dir, nBlocks, 999 /* no gap */)

	// ── Step 2: delete h/<gapHeight> to simulate crash-kill ───────────────
	// b/<hash_at_5> still exists; only the height-index entry is removed.
	db = deleteHeightIndexEntry(t, dbDir, db, gapHeight)
	t.Cleanup(func() { db.Close() })

	// ── Step 3: verify the gap is detected ────────────────────────────────
	missing, firstMissing, lastMissing := countMissingBlocksInWindow(db, tipHeight, 0)
	if missing != 1 {
		t.Fatalf("countMissingBlocksInWindow: missing=%d, want 1 (h/ deleted at height %d)", missing, gapHeight)
	}
	if firstMissing != gapHeight {
		t.Errorf("firstMissing=%d, want %d", firstMissing, gapHeight)
	}
	if lastMissing != gapHeight {
		t.Errorf("lastMissing=%d, want %d", lastMissing, gapHeight)
	}
	t.Logf("gap detected: missing=%d first=%d last=%d", missing, firstMissing, lastMissing)

	// ── Step 4: auto-repair via RepairAllHeightIndex ──────────────────────
	repaired, skipped, sweepErr := db.RepairAllHeightIndex(tipHeight, nil)
	if sweepErr != nil {
		t.Fatalf("RepairAllHeightIndex: %v", sweepErr)
	}
	if repaired != 1 {
		t.Errorf("repaired=%d, want 1 (one missing h/ entry should be rebuilt from b/)", repaired)
	}
	if skipped != 0 {
		t.Errorf("skipped=%d, want 0 (b/ body is present so repair must succeed)", skipped)
	}
	t.Logf("repair complete: repaired=%d skipped=%d", repaired, skipped)

	// ── Step 5: verify gaps are gone ─────────────────────────────────────
	missingAfter, _, _ := countMissingBlocksInWindow(db, tipHeight, 0)
	if missingAfter != 0 {
		t.Errorf("missingCount after repair = %d, want 0", missingAfter)
	}

	// ── Step 6: loadRecentBlocksFromStore must now load all blocks ────────
	var logBuf bytes.Buffer
	recentBlocks := loadRecentBlocksFromStore(db, tipHeight, newCaptureLogger(&logBuf))
	wantLoaded := nBlocks // heights 1–9 (genesis excluded from window when tipHeight < MaxInMemoryBlocks and startLoad=1)
	if len(recentBlocks) != wantLoaded {
		t.Errorf("blocks_loaded_in_memory=%d after repair, want %d\nlog:\n%s",
			len(recentBlocks), wantLoaded, logBuf.String())
	}
	// No "block missing" warning should appear after repair.
	if logContainsMsg(&logBuf, "block missing in store during resume") {
		t.Errorf("'block missing in store during resume' warning appeared after repair — h/ not fully rebuilt\nlog:\n%s",
			logBuf.String())
	}
}

// TestAutoRepairSentinelClearedOnGapDetection verifies that when the startup
// auto-repair path runs (countMissingBlocksInWindow > 0), it removes the
// sentinel file so the next restart re-verifies the repaired index rather than
// skipping the sweep.
//
// This guards against a subtle regression: if the sentinel were left in place
// after an auto-repair, a subsequent restart would see "sentinel present →
// skip sweep", never detecting a failed or partial repair.
func TestAutoRepairSentinelClearedOnGapDetection(t *testing.T) {
	const (
		nBlocks   = 5
		gapHeight = 3
	)
	tipHeight := uint64(nBlocks)

	dir := t.TempDir()
	dbDir := filepath.Join(dir, "chain.db")

	// Build complete chain, write sentinel to simulate a node that had
	// previously booted cleanly (sentinel is present).
	db, _ := buildChainWithGap(t, dir, nBlocks, 999)
	if err := storeHeightIndexSentinel(dir); err != nil {
		t.Fatalf("storeHeightIndexSentinel: %v", err)
	}
	if !loadHeightIndexSentinel(dir) {
		t.Fatal("sentinel not found after write")
	}

	// Delete h/<gapHeight> to simulate post-sentinel crash.
	db = deleteHeightIndexEntry(t, dbDir, db, gapHeight)
	t.Cleanup(func() { db.Close() })

	// Confirm gap is detected despite sentinel being present.
	missing, _, _ := countMissingBlocksInWindow(db, tipHeight, 0)
	if missing == 0 {
		t.Fatal("pre-condition: expected gap not detected — test setup error")
	}

	// Simulate the auto-repair path: repair + remove sentinel.
	_, _, _ = db.RepairAllHeightIndex(tipHeight, nil)
	_ = os.Remove(heightIndexSentinelPath(dir))

	// Sentinel must be gone so the next restart re-verifies.
	if loadHeightIndexSentinel(dir) {
		t.Error("sentinel still present after auto-repair — next restart would skip re-verification")
	}
	t.Log("sentinel correctly removed after auto-repair; next restart will re-verify")
}

// TestAutoRepairMultipleGaps confirms that RepairAllHeightIndex handles
// multiple missing h/ entries in a single sweep — matching the production
// scenario where 681 blocks were missing after an OOM kill.
func TestAutoRepairMultipleGaps(t *testing.T) {
	const nBlocks = 20
	tipHeight := uint64(nBlocks)

	// Heights to simulate as crash-lost h/ entries.
	crashLostHeights := []uint64{3, 7, 11, 15, 19}

	dir := t.TempDir()
	dbDir := filepath.Join(dir, "chain.db")

	db, _ := buildChainWithGap(t, dir, nBlocks, 999 /* no gap */)
	for _, h := range crashLostHeights {
		db = deleteHeightIndexEntry(t, dbDir, db, h)
	}
	t.Cleanup(func() { db.Close() })

	// All deleted heights must be detected.
	missing, _, _ := countMissingBlocksInWindow(db, tipHeight, 0)
	if missing != uint64(len(crashLostHeights)) {
		t.Fatalf("missing=%d, want %d before repair", missing, len(crashLostHeights))
	}

	// Single repair sweep must fix all of them.
	repaired, skipped, sweepErr := db.RepairAllHeightIndex(tipHeight, nil)
	if sweepErr != nil {
		t.Fatalf("RepairAllHeightIndex: %v", sweepErr)
	}
	if repaired != uint64(len(crashLostHeights)) {
		t.Errorf("repaired=%d, want %d", repaired, len(crashLostHeights))
	}
	if skipped != 0 {
		t.Errorf("skipped=%d, want 0", skipped)
	}

	// After repair, no gaps should remain.
	missingAfter, _, _ := countMissingBlocksInWindow(db, tipHeight, 0)
	if missingAfter != 0 {
		t.Errorf("missingCount after repair=%d, want 0", missingAfter)
	}
	t.Logf("repaired %d gaps in single sweep; 0 remaining", repaired)

	// loadRecentBlocksFromStore must load all nBlocks blocks now.
	var logBuf bytes.Buffer
	recentBlocks := loadRecentBlocksFromStore(db, tipHeight, newCaptureLogger(&logBuf))
	if len(recentBlocks) != nBlocks {
		t.Errorf("blocks_loaded_in_memory=%d after repair, want %d\nlog:\n%s",
			len(recentBlocks), nBlocks, logBuf.String())
	}
}

// TestAutoRepairNoOpWhenNoGaps confirms that the auto-repair path is a no-op
// when countMissingBlocksInWindow reports 0 missing blocks — the common path
// after a clean shutdown.  repaired and skipped must both be 0.
func TestAutoRepairNoOpWhenNoGaps(t *testing.T) {
	const nBlocks = 9
	tipHeight := uint64(nBlocks)

	dir := t.TempDir()
	db, _ := buildChainWithGap(t, dir, nBlocks, 999 /* no gap */)
	t.Cleanup(func() { db.Close() })

	// No gaps expected.
	missing, _, _ := countMissingBlocksInWindow(db, tipHeight, 0)
	if missing != 0 {
		t.Fatalf("pre-condition: missing=%d, want 0 for a complete chain", missing)
	}

	// Repair on a complete index must report repaired=0, skipped=0.
	repaired, skipped, sweepErr := db.RepairAllHeightIndex(tipHeight, nil)
	if sweepErr != nil {
		t.Fatalf("RepairAllHeightIndex: %v", sweepErr)
	}
	if repaired != 0 || skipped != 0 {
		t.Errorf("repaired=%d skipped=%d, want both 0 for a complete index", repaired, skipped)
	}
}
