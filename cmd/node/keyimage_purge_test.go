package main

// keyimage_purge_test.go — verifies that rebuildKeyImagesFromBlocks removes
// phantom "spent" entries from the persistent LevelDB k/ index (Task #1929).
//
// Scenario: the k/ index holds two entries — one for a key image that appears
// in a confirmed transaction, and one phantom entry that never appeared in any
// block (e.g. written for a transaction that was lost in an OOM kill before it
// was confirmed).  After the rebuild:
//
//   - the confirmed key image must remain spent (index + in-memory set)
//   - the phantom entry must be purged from the index and absent from the
//     in-memory set, making the wrongly-blocked UTXO spendable again
//   - a confirmed key image that was MISSING from the index must be
//     re-persisted (index completeness repair)

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestRebuildKeyImagesPurgesPhantomIndexEntries(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var confirmedKI, missingKI, phantomKI crypto.KeyImage
	confirmedKI[0] = 0xC1
	missingKI[0] = 0xC2
	phantomKI[0] = 0xF1

	// Block 1 contains a tx spending confirmedKI and missingKI.
	blk := &core.Block{}
	blk.Header.Height = 1
	blk.Txs = []core.Transaction{{
		Inputs: []core.RingInput{
			{KeyImage: confirmedKI},
			{KeyImage: missingKI},
		},
	}}
	raw, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	if err := db.PutRawBlock(blk.Hash(), 1, raw); err != nil {
		t.Fatalf("PutRawBlock: %v", err)
	}

	// Index state before repair: confirmed + phantom present, missing absent.
	if err := db.MarkKeyImageSpent(confirmedKI); err != nil {
		t.Fatalf("MarkKeyImageSpent(confirmed): %v", err)
	}
	if err := db.MarkKeyImageSpent(phantomKI); err != nil {
		t.Fatalf("MarkKeyImageSpent(phantom): %v", err)
	}

	utxos := core.NewUTXOSet()
	// Simulate the poisoned in-memory state loaded from a stale snapshot.
	utxos.MarkSpent(phantomKI)

	count, _, err := rebuildKeyImagesFromBlocks(db, 1, utxos, false, slog.Default())
	if err != nil {
		t.Fatalf("rebuildKeyImagesFromBlocks: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 confirmed key images, got %d", count)
	}

	// In-memory set: confirmed spent, phantom cleared.
	if !utxos.IsSpent(confirmedKI) {
		t.Error("confirmed key image must remain spent in memory")
	}
	if utxos.IsSpent(phantomKI) {
		t.Error("phantom key image must be cleared from the in-memory set")
	}

	// Persistent k/ index: phantom purged, confirmed kept, missing restored.
	if spent, err := db.IsKeyImageSpent(phantomKI); err != nil || spent {
		t.Errorf("phantom key image must be purged from index (spent=%v err=%v)", spent, err)
	}
	if spent, err := db.IsKeyImageSpent(confirmedKI); err != nil || !spent {
		t.Errorf("confirmed key image must remain in index (spent=%v err=%v)", spent, err)
	}
	if spent, err := db.IsKeyImageSpent(missingKI); err != nil || !spent {
		t.Errorf("missing confirmed key image must be re-persisted (spent=%v err=%v)", spent, err)
	}
}

// TestRebuildKeyImagesFailClosedOnIncompleteScan verifies that the phantom
// purge is skipped when any block in [1, tip] cannot be read: deleting index
// entries based on an incomplete scan could remove a genuinely confirmed key
// image and re-open a spent output.
func TestRebuildKeyImagesFailClosedOnIncompleteScan(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var confirmedKI, unknownKI crypto.KeyImage
	confirmedKI[0] = 0xC1
	unknownKI[0] = 0xF1

	// Block at height 2 only — height 1 is missing, so the scan is incomplete.
	blk := &core.Block{}
	blk.Header.Height = 2
	blk.Txs = []core.Transaction{{Inputs: []core.RingInput{{KeyImage: confirmedKI}}}}
	raw, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	if err := db.PutRawBlock(blk.Hash(), 2, raw); err != nil {
		t.Fatalf("PutRawBlock: %v", err)
	}

	// unknownKI is not in block 2 — but it MIGHT be in the unreadable block 1,
	// so the purge must not touch it.
	if err := db.MarkKeyImageSpent(unknownKI); err != nil {
		t.Fatalf("MarkKeyImageSpent: %v", err)
	}

	utxos := core.NewUTXOSet()
	if _, _, err := rebuildKeyImagesFromBlocks(db, 2, utxos, false, slog.Default()); err != nil {
		t.Fatalf("rebuildKeyImagesFromBlocks: %v", err)
	}

	if spent, err := db.IsKeyImageSpent(unknownKI); err != nil || !spent {
		t.Errorf("incomplete scan must NOT purge index entries (spent=%v err=%v)", spent, err)
	}
	if spent, err := db.IsKeyImageSpent(confirmedKI); err != nil || !spent {
		t.Errorf("observed confirmed key image must still be re-persisted (spent=%v err=%v)", spent, err)
	}

	// The in-memory set must be restored from the persistent index: unknownKI
	// could have been spent inside the unreadable block, so dropping it from
	// memory (and thus from the rebuilt snapshot) would re-open a double-spend
	// window on pruned nodes.
	if !utxos.IsSpent(unknownKI) {
		t.Errorf("incomplete scan must restore index-known key images into the in-memory set")
	}
	if !utxos.IsSpent(confirmedKI) {
		t.Errorf("scanned key image must be in the in-memory set")
	}
}

// TestRebuildKeyImagesForcePurgeOnIncompleteScan verifies that --force-purge-ki-index
// removes entries from the persistent index that could not be verified from
// the block scan, even when the scan is incomplete (pruned node scenario).
// The in-memory set must NOT be restored from the persistent index — only the
// scan results are trusted.
func TestRebuildKeyImagesForcePurgeOnIncompleteScan(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var confirmedKI, phantomKI crypto.KeyImage
	confirmedKI[0] = 0xC1
	phantomKI[0] = 0xF0 // unverifiable entry in the persistent index

	// Block at height 2 only — height 1 is missing, so the scan is incomplete.
	blk := &core.Block{}
	blk.Header.Height = 2
	blk.Txs = []core.Transaction{{Inputs: []core.RingInput{{KeyImage: confirmedKI}}}}
	raw, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	if err := db.PutRawBlock(blk.Hash(), 2, raw); err != nil {
		t.Fatalf("PutRawBlock: %v", err)
	}

	// phantomKI is in the persistent index but NOT in any readable block.
	if err := db.MarkKeyImageSpent(phantomKI); err != nil {
		t.Fatalf("MarkKeyImageSpent phantom: %v", err)
	}

	utxos := core.NewUTXOSet()
	utxos.MarkSpent(phantomKI) // simulate stale snapshot entry

	if _, _, err := rebuildKeyImagesFromBlocks(db, 2, utxos, true, slog.Default()); err != nil {
		t.Fatalf("rebuildKeyImagesFromBlocks(forcePurge=true): %v", err)
	}

	// Force-purge must remove the unverifiable entry from the persistent index.
	if spent, err := db.IsKeyImageSpent(phantomKI); err != nil || spent {
		t.Errorf("force-purge must remove unverifiable index entry (spent=%v err=%v)", spent, err)
	}
	// Confirmed entry must still be in the index.
	if spent, err := db.IsKeyImageSpent(confirmedKI); err != nil || !spent {
		t.Errorf("confirmed key image must remain in index (spent=%v err=%v)", spent, err)
	}
	// In-memory set must NOT contain the phantom (not restored from index).
	if utxos.IsSpent(phantomKI) {
		t.Error("force-purge must not restore unverifiable entry into memory")
	}
	// In-memory set must contain the confirmed KI from the scan.
	if !utxos.IsSpent(confirmedKI) {
		t.Error("confirmed key image from scan must be in memory")
	}
}
