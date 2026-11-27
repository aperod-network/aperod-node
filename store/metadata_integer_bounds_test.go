package store

import (
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"
)

func TestMetadataIntegerBounds_PreserveDurablePositiveValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if db != nil {
			if err := db.Close(); err != nil {
				t.Errorf("close database: %v", err)
			}
		}
	})
	for _, err := range []error{
		db.StoreTxTotal(12),
		db.StoreSnapshotSaveDuration(34),
		db.StoreActiveUTXOCount("fixture-tip", 56),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, err := range []error{
		db.StoreTxTotal(-1),
		db.StoreSnapshotSaveDuration(-1),
		db.StoreActiveUTXOCount("fixture-tip", -1),
	} {
		if err == nil {
			t.Fatal("negative metadata was accepted")
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := db.LoadTxTotal(); err != nil || n != 12 {
		t.Fatalf("durable count: %d %v", n, err)
	}
	if n, ok, err := db.LoadSnapshotSaveDuration(); err != nil || !ok || n != 34 {
		t.Fatalf("durable duration: %d %v %v", n, ok, err)
	}
	if n, ok, err := db.LoadActiveUTXOCount("fixture-tip"); err != nil || !ok || n != 56 {
		t.Fatalf("durable UTXO count: %d %v %v", n, ok, err)
	}
	var oversized [8]byte
	binary.LittleEndian.PutUint64(oversized[:], math.MaxUint64)
	for _, key := range []string{"tx_total", "last_snap_save_ms", "active_utxo_count/fixture-tip"} {
		if err := db.PutMeta(key, oversized[:]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.LoadTxTotal(); err == nil {
		t.Fatal("oversized count accepted")
	}
	if _, _, err := db.LoadSnapshotSaveDuration(); err == nil {
		t.Fatal("oversized duration accepted")
	}
	if _, _, err := db.LoadActiveUTXOCount("fixture-tip"); err == nil {
		t.Fatal("oversized UTXO count accepted")
	}
}
