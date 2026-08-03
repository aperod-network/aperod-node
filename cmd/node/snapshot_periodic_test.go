package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/aperod/aperod/core"
)

// TestPeriodicSnapshot_EveryFiveHundredBlocks simulates a 1500-block run and
// verifies that:
//   - A snapshot file is written at each 500-block boundary (500, 1000, 1500).
//   - Old snapshot files are removed after a newer one is written.
func TestPeriodicSnapshot_EveryFiveHundredBlocks(t *testing.T) {
	dir := t.TempDir()

	// saveSnap writes a dummy snapshot for height h and then removes stale ones.
	saveSnap := func(h uint64) {
		snap := startupSnapshot{
			Version:    snapVersion,
			TipHeight:  h,
			TipHashHex: fmt.Sprintf("%016x", h),
			TxTotal:    int64(h),
			UTXOs:      core.UTXOSnapshot{},
			Registry: core.RegistrySnapshot{
				Validators: map[string]*core.ValidatorEntry{},
			},
		}
		if err := saveStartupSnapshot(dir, snap); err != nil {
			t.Fatalf("saveStartupSnapshot(height=%d): %v", h, err)
		}
		deleteOldSnapshots(dir, h)
	}

	// Simulate 1500 consecutive blocks; call the snapshot logic at each
	// 500-block boundary — exactly as the wired OnBlockProduced callback does.
	for h := uint64(1); h <= 1500; h++ {
		if h%500 != 0 {
			continue
		}

		saveSnap(h)

		// The snapshot file for this height must exist.
		path := snapshotPath(dir, h)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("snapshot missing at height %d (expected file %s)", h, path)
		}

		// All earlier 500-block snapshots must have been deleted.
		for prev := uint64(500); prev < h; prev += 500 {
			prevPath := snapshotPath(dir, prev)
			if _, err := os.Stat(prevPath); err == nil {
				t.Errorf("stale snapshot still present at height %d after writing height %d", prev, h)
			}
		}
	}

	// After the full 1500-block run only the height-1500 snapshot must remain.
	finalPath := snapshotPath(dir, 1500)
	if _, err := os.Stat(finalPath); os.IsNotExist(err) {
		t.Errorf("final snapshot at height 1500 missing: %v", err)
	}
}
