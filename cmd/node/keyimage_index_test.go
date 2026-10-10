package main

// Tests for the FAIL-CLOSED gate that decides whether the startup key-image
// fast path (db.IterKeyImages) may replace the full block scan.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestKeyImageIndexTrusted(t *testing.T) {
	iterFail := errors.New("leveldb: corrupted SST")
	cases := []struct {
		name      string
		iterErr   error
		kiCount   int
		tipHeight uint64
		want      bool
	}{
		{"populated index on synced chain", nil, 12345, 800_000, true},
		{"genesis chain with empty index", nil, 0, 0, true},
		{"FAIL-CLOSED: empty index on non-genesis chain (older binary)", nil, 0, 800_000, false},
		{"FAIL-CLOSED: iteration error", iterFail, 0, 800_000, false},
		{"FAIL-CLOSED: iteration error even with partial count", iterFail, 500, 800_000, false},
		{"FAIL-CLOSED: iteration error on genesis", iterFail, 0, 0, false},
		{"single key image at height 1", nil, 1, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keyImageIndexTrusted(tc.iterErr, tc.kiCount, tc.tipHeight); got != tc.want {
				t.Errorf("keyImageIndexTrusted(%v, %d, %d) = %v, want %v",
					tc.iterErr, tc.kiCount, tc.tipHeight, got, tc.want)
			}
		})
	}
}

// TestKeyImageFastPath_LoadsFromDBIndex mirrors the main.go startup loop:
// key images persisted via MarkKeyImageSpent (the OnBlockProduced hook) must
// be fully recoverable through IterKeyImages into a fresh UTXOSet after a
// simulated restart — without reading a single block.
func TestKeyImageFastPath_LoadsFromDBIndex(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chain.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// Persist spent key images the same way OnBlockProduced does.
	var kis []crypto.KeyImage
	for i := 1; i <= 50; i++ {
		var ki crypto.KeyImage
		ki[0] = byte(i >> 8)
		ki[1] = byte(i)
		ki[31] = 0x7f // avoid the all-zero image
		kis = append(kis, ki)
		if err := db.MarkKeyImageSpent(ki); err != nil {
			t.Fatalf("MarkKeyImageSpent(%d): %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Simulated restart: reopen the DB and rebuild the in-memory spent set
	// from the index alone (the main.go fast-path loop).
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db2.Close()

	utxos := core.NewUTXOSet()
	kiCount := 0
	iterErr := db2.IterKeyImages(func(ki crypto.KeyImage) error {
		utxos.MarkSpent(ki)
		kiCount++
		return nil
	})
	if iterErr != nil {
		t.Fatalf("IterKeyImages: %v", iterErr)
	}
	if kiCount != len(kis) {
		t.Errorf("IterKeyImages visited %d images, want %d", kiCount, len(kis))
	}
	if !keyImageIndexTrusted(iterErr, kiCount, 50) {
		t.Error("keyImageIndexTrusted = false for a healthy populated index, want true")
	}
	for i, ki := range kis {
		if !utxos.IsSpent(ki) {
			t.Errorf("key image %d not marked spent after fast-path load", i+1)
		}
	}
}

// TestKeyImageFastPath_EmptyIndexNotTrusted covers the older-binary scenario:
// a DB with blocks (tipHeight > 0) but zero k/ entries must NOT be trusted,
// forcing the caller onto the full block-scan rebuild.
func TestKeyImageFastPath_EmptyIndexNotTrusted(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	kiCount := 0
	iterErr := db.IterKeyImages(func(crypto.KeyImage) error {
		kiCount++
		return nil
	})
	if iterErr != nil {
		t.Fatalf("IterKeyImages on empty db: %v", iterErr)
	}
	if kiCount != 0 {
		t.Fatalf("kiCount = %d, want 0", kiCount)
	}
	if keyImageIndexTrusted(iterErr, kiCount, 808_000) {
		t.Error("empty index trusted on a non-genesis chain — fail-closed guard broken")
	}
}
