package main

// phantom_ki_check_test.go — verifies the startup phantom-key-image detection
// introduced to surface stale snapshot entries before any withdrawal attempt
// fails with "key image already spent".
//
// Properties tested:
//
//  1. UTXOSet.IterKeyImages visits every KI in both internal tiers (sorted
//     historical slice + recent runtime map) without holding the lock during
//     the caller's LevelDB queries.
//
//  2. countPhantomKeyImages correctly counts phantom KIs across all three
//     startup restore paths:
//     a. Exact-tip snapshot + gap-fill (nominal clean path)
//     b. Sub-tip snapshot + gap-fill (OOM-kill: blocks accepted after last snapshot)
//     c. Rescue path (snapshot present but UTXO-count diverged)
//
//  3. Server.SetPhantomKICount / phantom_ki_count on /api/v1/status propagate
//     the count so the API-server monitor can fire a Telegram alert.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// TestIterKeyImages_BothTiers verifies that IterKeyImages visits KIs from
// both the compact sorted-slice tier (populated via CompactKeyImages) and the
// runtime recent-map tier (populated by MarkSpent after compaction).  This is
// the foundation of the bounded-memory phantom check: if either tier were
// missed, confirmed KIs in the wrong tier would be counted as phantoms.
func TestIterKeyImages_BothTiers(t *testing.T) {
	utxos := core.NewUTXOSet()

	// ki1 and ki2 go into the 'recent' map tier initially.
	var ki1, ki2 crypto.KeyImage
	ki1[0] = 0x01
	ki2[0] = 0x02
	utxos.MarkSpent(ki1)
	utxos.MarkSpent(ki2)

	// CompactKeyImages moves recent → sorted slice.
	utxos.CompactKeyImages()

	// ki3 is added after compaction — lives in the recent tier.
	var ki3 crypto.KeyImage
	ki3[0] = 0x03
	utxos.MarkSpent(ki3)

	// IterKeyImages must visit all three regardless of tier.
	seen := make(map[crypto.KeyImage]bool)
	utxos.IterKeyImages(func(ki crypto.KeyImage) {
		seen[ki] = true
	})

	for _, want := range []crypto.KeyImage{ki1, ki2, ki3} {
		canonical, _ := crypto.CanonicalKeyImage(want)
		if !seen[canonical] && !seen[want] {
			t.Errorf("IterKeyImages did not visit %x", want[:4])
		}
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 iterated KIs, got %d", len(seen))
	}
}

// TestPhantomKICheck_CountsCorrectly exercises countPhantomKeyImages with a mix
// of confirmed and phantom KIs — the core algorithm used by all three restore paths.
//
// Setup:
//
//	ki_confirmed — in-memory AND in LevelDB (genuine on-chain spend)
//	ki_phantom1  — in-memory but NOT in LevelDB (OOM-killed mempool tx)
//	ki_phantom2  — in-memory but NOT in LevelDB (second phantom)
//	ki_only_db   — in LevelDB but NOT in-memory (irrelevant to the check)
//
// Expected: phantom=2, checked=3 (ki_only_db is not iterated).
func TestPhantomKICheck_CountsCorrectly(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var kiConfirmed, kiPhantom1, kiPhantom2, kiOnlyDB crypto.KeyImage
	kiConfirmed[0] = 0xC0
	kiPhantom1[0] = 0xF1
	kiPhantom2[0] = 0xF2
	kiOnlyDB[0] = 0xDB

	for _, ki := range []crypto.KeyImage{kiConfirmed, kiOnlyDB} {
		if err := db.MarkKeyImageSpent(ki); err != nil {
			t.Fatalf("MarkKeyImageSpent: %v", err)
		}
	}

	utxos := core.NewUTXOSet()
	utxos.MarkSpent(kiConfirmed)
	utxos.MarkSpent(kiPhantom1)
	utxos.MarkSpent(kiPhantom2)

	phantom, checked := countPhantomKeyImages(db, utxos)
	if phantom != 2 {
		t.Errorf("expected 2 phantom KIs, got %d", phantom)
	}
	if checked != 3 {
		t.Errorf("expected 3 checked KIs, got %d", checked)
	}
}

// TestPhantomKICheck_NoneWhenAllConfirmed verifies that countPhantomKeyImages
// returns zero when every in-memory KI is also present in LevelDB (clean startup).
func TestPhantomKICheck_NoneWhenAllConfirmed(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var ki1, ki2 crypto.KeyImage
	ki1[0] = 0xA1
	ki2[0] = 0xA2

	for _, ki := range []crypto.KeyImage{ki1, ki2} {
		if err := db.MarkKeyImageSpent(ki); err != nil {
			t.Fatalf("MarkKeyImageSpent: %v", err)
		}
	}

	utxos := core.NewUTXOSet()
	utxos.MarkSpent(ki1)
	utxos.MarkSpent(ki2)

	phantom, _ := countPhantomKeyImages(db, utxos)
	if phantom != 0 {
		t.Errorf("expected 0 phantom KIs when all confirmed, got %d", phantom)
	}
}

// TestPhantomKICheck_SubTipPath simulates the sub-tip snapshot + gap-fill restore
// path: a snapshot below the chain tip is loaded, gap-fill replays confirmed
// blocks (which adds their KIs to both memory and LevelDB), then
// countPhantomKeyImages is called.  Phantom KIs from the snapshot that were
// never confirmed on-chain must be counted.
func TestPhantomKICheck_SubTipPath(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// kiConfirmed: in snapshot + in LevelDB (confirmed spend from a block
	// that was replayed by gap-fill).
	// kiPhantom: in snapshot but never confirmed — not in LevelDB.
	var kiConfirmed, kiPhantom crypto.KeyImage
	kiConfirmed[0] = 0xC0
	kiPhantom[0] = 0xF3

	if err := db.MarkKeyImageSpent(kiConfirmed); err != nil {
		t.Fatalf("MarkKeyImageSpent: %v", err)
	}

	// Simulate UTXOSet state after sub-tip restore + gap-fill:
	// both confirmed and phantom KIs are in the in-memory set.
	utxos := core.NewUTXOSet()
	utxos.MarkSpent(kiConfirmed)
	utxos.MarkSpent(kiPhantom)

	phantom, _ := countPhantomKeyImages(db, utxos)
	if phantom != 1 {
		t.Errorf("sub-tip path: expected 1 phantom KI, got %d", phantom)
	}
}

// TestPhantomKICheck_RescuePath simulates the rescue snapshot path: a snapshot
// with a diverged UTXO count seeds KIs into memory; some are confirmed on-chain,
// one is a phantom.  countPhantomKeyImages must surface the phantom.
func TestPhantomKICheck_RescuePath(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var kiReal1, kiReal2, kiGhost crypto.KeyImage
	kiReal1[0] = 0xA1
	kiReal2[0] = 0xA2
	kiGhost[0] = 0xE1

	for _, ki := range []crypto.KeyImage{kiReal1, kiReal2} {
		if err := db.MarkKeyImageSpent(ki); err != nil {
			t.Fatalf("MarkKeyImageSpent: %v", err)
		}
	}

	// After rescue-snapshot restore, in-memory set has all three.
	utxos := core.NewUTXOSet()
	utxos.MarkSpent(kiReal1)
	utxos.MarkSpent(kiReal2)
	utxos.MarkSpent(kiGhost)

	phantom, _ := countPhantomKeyImages(db, utxos)
	if phantom != 1 {
		t.Errorf("rescue path: expected 1 phantom KI, got %d", phantom)
	}
}

// TestPhantomKICheck_ExactTipCleanPath verifies the exact-tip snapshot path
// when all KIs are confirmed — countPhantomKeyImages must return 0.
func TestPhantomKICheck_ExactTipCleanPath(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var ki1, ki2 crypto.KeyImage
	ki1[0] = 0xB1
	ki2[0] = 0xB2
	for _, ki := range []crypto.KeyImage{ki1, ki2} {
		if err := db.MarkKeyImageSpent(ki); err != nil {
			t.Fatalf("MarkKeyImageSpent: %v", err)
		}
	}

	utxos := core.NewUTXOSet()
	utxos.MarkSpent(ki1)
	utxos.MarkSpent(ki2)

	phantom, _ := countPhantomKeyImages(db, utxos)
	if phantom != 0 {
		t.Errorf("exact-tip clean path: expected 0 phantom KIs, got %d", phantom)
	}
}

// TestPhantomKICheck_StatusField verifies that SetPhantomKICount propagates the
// count to /api/v1/status as phantom_ki_count so the API-server monitor can read
// it without a second round-trip.
func TestPhantomKICheck_StatusField(t *testing.T) {
	chain := core.NewChain()
	mempool := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()
	srv := api.NewServer(":0", chain, mempool, utxos, discardLogger())

	// Before any phantom is reported the field must be zero.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status GET failed: %d", rec.Code)
	}
	var beforeBody map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&beforeBody); err != nil {
		t.Fatalf("decode before: %v", err)
	}
	if v, ok := beforeBody["phantom_ki_count"]; ok {
		if count, _ := v.(float64); count != 0 {
			t.Errorf("phantom_ki_count before set: want 0, got %v", count)
		}
	}

	// Simulate the goroutine finding 3 phantom KIs.
	srv.SetPhantomKICount(3)

	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	var afterBody map[string]interface{}
	if err := json.NewDecoder(rec2.Body).Decode(&afterBody); err != nil {
		t.Fatalf("decode after: %v", err)
	}
	count, ok := afterBody["phantom_ki_count"]
	if !ok {
		t.Fatal("phantom_ki_count missing from /api/v1/status after SetPhantomKICount(3)")
	}
	if count.(float64) != 3 {
		t.Errorf("phantom_ki_count: want 3, got %v", count)
	}
}
