package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestFilterSnapshotKeyImagesLeavesEmptyIndexUnchanged(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	var ki crypto.KeyImage
	ki[0] = 1
	snap := startupSnapshot{UTXOs: core.UTXOSnapshot{KeyImages: []crypto.KeyImage{ki}}}
	filterSnapshotKeyImages(&snap, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(snap.UTXOs.KeyImages) != 1 || snap.UTXOs.KeyImages[0] != ki {
		t.Fatalf("empty index changed snapshot key images: %v", snap.UTXOs.KeyImages)
	}
}

func TestFilterSnapshotKeyImagesUsesPointLookupsAndPurgesUnconfirmed(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	var confirmed, unconfirmed crypto.KeyImage
	confirmed[0] = 0xC1
	unconfirmed[0] = 0xF1
	if err := db.MarkKeyImageSpent(confirmed); err != nil {
		t.Fatalf("mark confirmed key image: %v", err)
	}
	snap := startupSnapshot{UTXOs: core.UTXOSnapshot{
		KeyImages: []crypto.KeyImage{confirmed, unconfirmed},
	}}
	filterSnapshotKeyImages(&snap, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(snap.UTXOs.KeyImages) != 1 || snap.UTXOs.KeyImages[0] != confirmed {
		t.Fatalf("filtered key images = %v, want confirmed key image only", snap.UTXOs.KeyImages)
	}
}

func TestFilterSnapshotKeyImagesLeavesSnapshotUnchangedOnIndexReadError(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var indexed crypto.KeyImage
	indexed[0] = 0xC1
	if err := db.MarkKeyImageSpent(indexed); err != nil {
		t.Fatalf("mark key image: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	var first, second crypto.KeyImage
	first[0], second[0] = 0xC1, 0xF1
	snap := startupSnapshot{UTXOs: core.UTXOSnapshot{
		KeyImages: []crypto.KeyImage{first, second},
	}}
	filterSnapshotKeyImages(&snap, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(snap.UTXOs.KeyImages) != 2 ||
		snap.UTXOs.KeyImages[0] != first ||
		snap.UTXOs.KeyImages[1] != second {
		t.Fatalf("index read error changed snapshot key images: %v", snap.UTXOs.KeyImages)
	}
}