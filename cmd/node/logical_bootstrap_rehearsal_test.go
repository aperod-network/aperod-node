package main

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// TestBootstrapSnapshotMustMatchImportedDatabaseTip exercises the local
// bootstrap-pair validation without contacting or modifying a running node.
// The database and startup snapshot are usable together only when they bind to
// the exact same tip hash and height.
func TestBootstrapSnapshotMustMatchImportedDatabaseTip(t *testing.T) {
	rehearsalDir := t.TempDir()
	sourceDB, err := store.Open(filepath.Join(rehearsalDir, "chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sourceDB.Close()

	tipHash := crypto.HashBytes([]byte("isolated-bootstrap-rehearsal-genesis"))
	if err := sourceDB.PutRawBlock(tipHash, 0, []byte("synthetic genesis body")); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.PutTip(tipHash, 0); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.Close(); err != nil {
		t.Fatalf("close isolated fixture database: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	matching := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  0,
		TipHashHex: fmt.Sprintf("%x", tipHash[:]),
		UTXOs:      core.NewUTXOSet().TakeSnapshot(),
		Registry:   core.NewValidatorRegistry().TakeSnapshot(),
	}
	if err := saveStartupSnapshot(rehearsalDir, matching); err != nil {
		t.Fatalf("save matching startup snapshot: %v", err)
	}

	reopenedHash, reopenedHeight, err := store.ReadTipOnly(filepath.Join(rehearsalDir, "chain.db"))
	if err != nil {
		t.Fatalf("read fixture database tip: %v", err)
	}
	if reopenedHash != tipHash || reopenedHeight != 0 {
		t.Fatalf("reopened tip = %x/%d, exported tip = %x/%d",
			reopenedHash, reopenedHeight, tipHash, 0)
	}
	loaded, relaxed, err := tryLoadStartupSnapshot(
		rehearsalDir, reopenedHeight, fmt.Sprintf("%x", reopenedHash[:]), log)
	if err != nil {
		t.Fatalf("load snapshot bound to exported tip: %v", err)
	}
	if loaded == nil || relaxed {
		t.Fatalf("matching snapshot load = (%v, relaxed=%t), want valid strict snapshot", loaded != nil, relaxed)
	}

	mismatchDir := t.TempDir()
	mismatchDB, err := store.Open(filepath.Join(mismatchDir, "chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mismatchDB.PutRawBlock(tipHash, 0, []byte("synthetic genesis body")); err != nil {
		t.Fatal(err)
	}
	if err := mismatchDB.PutTip(tipHash, 0); err != nil {
		t.Fatal(err)
	}
	if err := mismatchDB.Close(); err != nil {
		t.Fatalf("close mismatch control database: %v", err)
	}
	wrongHash := crypto.HashBytes([]byte("different synthetic chain"))
	mismatched := matching
	mismatched.TipHashHex = fmt.Sprintf("%x", wrongHash[:])
	if err := saveStartupSnapshot(mismatchDir, mismatched); err != nil {
		t.Fatalf("save mismatched control snapshot: %v", err)
	}
	if _, _, err := tryLoadStartupSnapshot(
		mismatchDir, 0, fmt.Sprintf("%x", tipHash[:]), log); err == nil {
		t.Fatal("snapshot from another chain was accepted with the copied database")
	}
}
