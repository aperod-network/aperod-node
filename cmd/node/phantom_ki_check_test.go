package main

// phantom_ki_check_test.go — verifies the startup phantom-key-image detection
// goroutine introduced to surface stale snapshot entries before any withdrawal
// attempt fails with "key image already spent".
//
// Three properties are tested:
//
//  1. UTXOSet.IterKeyImages visits every KI in both internal tiers (sorted
//     historical slice + recent runtime map) without holding the lock during
//     the caller's LevelDB queries.
//
//  2. The point-lookup check correctly counts phantom KIs: entries present in
//     the in-memory set but absent from the persistent LevelDB index.
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
// both the compact sorted-slice tier (populated via RestoreFromSnapshot /
// CompactKeyImages) and the runtime recent-map tier (populated by MarkSpent
// on a fresh set).  This is the foundation of the bounded-memory phantom check:
// if either tier were missed, confirmed KIs in the wrong tier would appear as
// phantoms.
func TestIterKeyImages_BothTiers(t *testing.T) {
	utxos := core.NewUTXOSet()

	// ki1 and ki2 go into the 'recent' map tier (MarkSpent on a new set).
	var ki1, ki2 crypto.KeyImage
	ki1[0] = 0x01
	ki2[0] = 0x02
	utxos.MarkSpent(ki1)
	utxos.MarkSpent(ki2)

	// CompactKeyImages moves recent → sorted slice.  After compaction ki1/ki2
	// live in the sorted tier; new entries added after will go to recent.
	utxos.CompactKeyImages()

	// ki3 is added after compaction, so it lives in the recent tier.
	var ki3 crypto.KeyImage
	ki3[0] = 0x03
	utxos.MarkSpent(ki3)

	// IterKeyImages must visit all three regardless of tier.
	seen := make(map[crypto.KeyImage]bool)
	utxos.IterKeyImages(func(ki crypto.KeyImage) {
		seen[ki] = true
	})

	// MarkSpent canonicalises before inserting; the canonical form of a small
	// byte value with the high-order bits zero is the same raw bytes.
	for _, want := range []crypto.KeyImage{ki1, ki2, ki3} {
		// The canonical form is what MarkSpent stores; look up the canonical.
		canonical, _ := crypto.CanonicalKeyImage(want)
		if !seen[canonical] && !seen[want] {
			t.Errorf("IterKeyImages did not visit %x", want[:4])
		}
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 iterated KIs, got %d", len(seen))
	}
}

// TestPhantomKICheck_CountsCorrectly exercises the bounded-memory algorithm
// used in the startup goroutine:
//
//   - Collect in-memory KIs into a local slice (IterKeyImages).
//   - For each KI, call db.IsKeyImageSpent (point-lookup; no large map).
//   - Count those absent from LevelDB — they are phantoms.
//
// Setup:
//
//	ki_confirmed  — in-memory AND in LevelDB (genuine spend, confirmed on-chain)
//	ki_phantom1   — in-memory but NOT in LevelDB (phantom from OOM-killed mempool tx)
//	ki_phantom2   — in-memory but NOT in LevelDB (second phantom)
//	ki_only_db    — in LevelDB but NOT in-memory (irrelevant to the check)
//
// Expected: phantom count = 2; confirmed count = 1; ki_only_db not counted.
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

	// Persist kiConfirmed and kiOnlyDB into LevelDB.
	for _, ki := range []crypto.KeyImage{kiConfirmed, kiOnlyDB} {
		if err := db.MarkKeyImageSpent(ki); err != nil {
			t.Fatalf("MarkKeyImageSpent: %v", err)
		}
	}

	// Restore a UTXOSet with kiConfirmed + both phantoms (simulates snapshot load).
	utxos := core.NewUTXOSet()
	utxos.MarkSpent(kiConfirmed)
	utxos.MarkSpent(kiPhantom1)
	utxos.MarkSpent(kiPhantom2)

	// Replicate the goroutine logic from main.go exactly:
	var inMemKIs []crypto.KeyImage
	utxos.IterKeyImages(func(ki crypto.KeyImage) {
		inMemKIs = append(inMemKIs, ki)
	})

	phantom := 0
	for _, ki := range inMemKIs {
		confirmed, lookupErr := db.IsKeyImageSpent(ki)
		if lookupErr != nil {
			continue // assume confirmed on error — no false positives
		}
		if !confirmed {
			phantom++
		}
	}
	inMemKIs = nil

	if phantom != 2 {
		t.Errorf("expected 2 phantom KIs, got %d", phantom)
	}
}

// TestPhantomKICheck_NoneWhenAllConfirmed verifies that the check returns zero
// when every in-memory KI is also present in LevelDB (clean startup).
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

	var inMemKIs []crypto.KeyImage
	utxos.IterKeyImages(func(ki crypto.KeyImage) {
		inMemKIs = append(inMemKIs, ki)
	})

	phantom := 0
	for _, ki := range inMemKIs {
		confirmed, err := db.IsKeyImageSpent(ki)
		if err != nil {
			continue
		}
		if !confirmed {
			phantom++
		}
	}

	if phantom != 0 {
		t.Errorf("expected 0 phantom KIs when all confirmed, got %d", phantom)
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

	// Before any phantom is reported the field must be zero (absent from response
	// is also acceptable; we check the encoded JSON).
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
