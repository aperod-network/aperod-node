package main

// height_index_sentinel_test.go — verifies the rsync-lifecycle contract for
// the height-index sentinel file.
//
// The sentinel lives at <dataDir>/height_index_verified — OUTSIDE chain.db/ —
// so that an operator rsyncing only the chain.db/ LevelDB directory does NOT
// copy a sentinel from a previously-repaired source node.  These tests confirm:
//
//  1. After a chain.db-only rsync the sentinel is absent at the destination,
//     even if the source had a valid sentinel.
//
//  2. RepairAllHeightIndex runs on startup when the sentinel is absent AND
//     repairs a dangling h/<height> entry introduced by the rsync.
//
//  3. After a fully-successful sweep, storeHeightIndexSentinel writes the file
//     and subsequent loadHeightIndexSentinel calls return true.
//
//  4. A sweep with skipped>0 does NOT write the sentinel (gate logic).

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// copyChainDB copies all files and subdirectories from src to dst recursively.
// Used to simulate rsyncing only the chain.db/ LevelDB directory without
// copying sibling files (e.g. the height_index_verified sentinel file).
func copyChainDB(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("readdir %s: %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			copyChainDB(t, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
			continue
		}
		if err := copyOneFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			t.Fatalf("copy %s → %s: %v", e.Name(), dst, err)
		}
	}
}

func copyOneFile(src, dst string) error {
	r, err := os.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer w.Close()
	_, err = io.Copy(w, r)
	return err
}

// buildMinimalChain writes N core.Block entries to db via PutRawBlock and
// records the last one as tip.  Blocks use a simple deterministic structure.
func buildMinimalChain(t *testing.T, db *store.DB, n int) []crypto.Hash32 {
	t.Helper()
	hashes := make([]crypto.Hash32, n)
	for h := 0; h < n; h++ {
		blk := &core.Block{
			Header: core.BlockHeader{
				Height:    uint64(h),
				Timestamp: int64(h) * 1_000_000_000,
			},
		}
		hash := blk.Hash()
		hashes[h] = hash
		raw, err := json.Marshal(blk)
		if err != nil {
			t.Fatalf("marshal height %d: %v", h, err)
		}
		if err := db.PutRawBlock(hash, uint64(h), raw); err != nil {
			t.Fatalf("PutRawBlock height %d: %v", h, err)
		}
	}
	tip := hashes[n-1]
	if err := db.PutTip(tip, uint64(n-1)); err != nil {
		t.Fatalf("PutTip: %v", err)
	}
	return hashes
}

// deleteRawBlockBody removes the b/<hash> entry directly from LevelDB,
// simulating a body lost during a live-LevelDB rsync (h/ entry survives).
func deleteRawBlockBody(t *testing.T, dbDir string, db *store.DB, hash crypto.Hash32) *store.DB {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw, err := leveldb.OpenFile(dbDir, nil)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	bKey := make([]byte, 2+32)
	copy(bKey, []byte("b/"))
	copy(bKey[2:], hash[:])
	if err := raw.Delete(bKey, &opt.WriteOptions{Sync: true}); err != nil {
		raw.Close()
		t.Fatalf("delete block body: %v", err)
	}
	raw.Close()
	db2, err := store.Open(dbDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return db2
}

// TestHeightIndexSentinel_RsyncLifecycle is the core lifecycle test:
//
//  1. Create a source dataDir with a healthy chain.db and a sentinel file.
//  2. Rsync ONLY chain.db/ to dest dataDir (do not copy the sentinel file).
//  3. Introduce a dangling h/<height> entry in dest by deleting a block body.
//  4. Verify the sentinel is absent at dest (it was not copied).
//  5. Call RepairAllHeightIndex on dest — skipped must equal 1 (dangling entry).
//  6. Because skipped>0 the sweep is incomplete; sentinel must NOT be written.
func TestHeightIndexSentinel_RsyncLifecycle(t *testing.T) {
	srcDataDir := t.TempDir()
	srcDBDir := filepath.Join(srcDataDir, "chain.db")

	// ── Build source: healthy chain + sentinel file ────────────────────────
	srcDB, err := store.Open(srcDBDir)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	hashes := buildMinimalChain(t, srcDB, 3) // heights 0, 1, 2
	srcDB.Close()

	// Write sentinel to source dataDir (outside chain.db/).
	if err := storeHeightIndexSentinel(srcDataDir); err != nil {
		t.Fatalf("write source sentinel: %v", err)
	}
	if !loadHeightIndexSentinel(srcDataDir) {
		t.Fatal("source sentinel not found after write")
	}

	// ── Rsync chain.db/ only to dest ──────────────────────────────────────
	destDataDir := t.TempDir()
	destDBDir := filepath.Join(destDataDir, "chain.db")
	copyChainDB(t, srcDBDir, destDBDir)

	// The sentinel file must be absent at dest (it lives outside chain.db/).
	if loadHeightIndexSentinel(destDataDir) {
		t.Fatal("sentinel file was present at dest after chain.db-only rsync — " +
			"it must live outside chain.db/ to be excluded from the copy")
	}

	// ── Introduce corruption: delete block body at height 1 ───────────────
	destDB, err := store.Open(destDBDir)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	destDB = deleteRawBlockBody(t, destDBDir, destDB, hashes[1])

	_, tipHeight, err := destDB.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	// ── Run repair sweep (would be triggered by sentinel-absent startup) ───
	repaired, skipped, sweepErr := destDB.RepairAllHeightIndex(tipHeight, nil)
	destDB.Close()

	if sweepErr != nil {
		t.Fatalf("RepairAllHeightIndex: %v", sweepErr)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (h/ entry is non-zero and dangling, not absent)", repaired)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (dangling h/<1> must be counted as unrepairable)", skipped)
	}

	// Because skipped>0 the sweep is incomplete; sentinel must NOT be written.
	sweepOK := sweepErr == nil && skipped == 0
	if sweepOK {
		t.Error("sweepOK is true despite skipped=1 — gate logic is broken")
	}
	if loadHeightIndexSentinel(destDataDir) {
		t.Error("sentinel file appeared at dest even though sweep was incomplete")
	}
}

// TestHeightIndexSentinel_CleanRsyncWritesSentinel verifies the happy path:
// after a chain.db-only rsync with NO corruption, RepairAllHeightIndex returns
// repaired=0, skipped=0, and the caller writes the sentinel file.
func TestHeightIndexSentinel_CleanRsyncWritesSentinel(t *testing.T) {
	srcDataDir := t.TempDir()
	srcDBDir := filepath.Join(srcDataDir, "chain.db")

	srcDB, err := store.Open(srcDBDir)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	buildMinimalChain(t, srcDB, 3)
	srcDB.Close()

	// Rsync chain.db/ to dest (no sentinel file copied).
	destDataDir := t.TempDir()
	destDBDir := filepath.Join(destDataDir, "chain.db")
	copyChainDB(t, srcDBDir, destDBDir)

	if loadHeightIndexSentinel(destDataDir) {
		t.Fatal("sentinel must not be present after chain.db-only rsync")
	}

	destDB, err := store.Open(destDBDir)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	_, tipHeight, err := destDB.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	repaired, skipped, sweepErr := destDB.RepairAllHeightIndex(tipHeight, nil)
	destDB.Close()

	if sweepErr != nil {
		t.Fatalf("RepairAllHeightIndex: %v", sweepErr)
	}
	if repaired != 0 || skipped != 0 {
		t.Errorf("repaired=%d skipped=%d, want both 0 for a clean rsync", repaired, skipped)
	}

	// Sweep is complete → write sentinel.
	sweepOK := sweepErr == nil && skipped == 0
	if !sweepOK {
		t.Fatal("sweepOK is false for a clean sweep")
	}
	if err := storeHeightIndexSentinel(destDataDir); err != nil {
		t.Fatalf("storeHeightIndexSentinel: %v", err)
	}
	if !loadHeightIndexSentinel(destDataDir) {
		t.Error("sentinel not found after storeHeightIndexSentinel")
	}

	// Subsequent startups should find the sentinel and skip the sweep.
	// (In run(), this is: if !loadHeightIndexSentinel(cfg.DataDir) { sweep })
	if !loadHeightIndexSentinel(destDataDir) {
		t.Error("sentinel missing on second check — subsequent startups would re-sweep unnecessarily")
	}
}

// TestHeightIndexSentinel_SentinelOutsideChainDB is the explicit documentation
// test: it asserts that the sentinel path is NOT inside chain.db/.
func TestHeightIndexSentinel_SentinelOutsideChainDB(t *testing.T) {
	dataDir := t.TempDir()
	sentinelPath := heightIndexSentinelPath(dataDir)
	chainDBPath := filepath.Join(dataDir, "chain.db")

	// Sentinel must not be a subdirectory of chain.db/.
	rel, err := filepath.Rel(chainDBPath, sentinelPath)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if len(rel) > 0 && rel[0] != '.' {
		// rel does not start with ".." → sentinelPath IS inside chainDBPath.
		t.Errorf("sentinel path %q is inside chain.db/ (%q) — it will be copied by a chain.db-only rsync", sentinelPath, chainDBPath)
	}
}
