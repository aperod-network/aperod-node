package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aperod/aperod/core"
)

// snapVersion must be bumped whenever the snapshot schema changes incompatibly.
const snapVersion = 1

// startupSnapshot is the on-disk format for the UTXOSet + registry snapshot.
type startupSnapshot struct {
	Version    int                  `json:"v"`
	TipHeight  uint64               `json:"tip_height"`
	TipHashHex string               `json:"tip_hash"`
	TxTotal    int64                `json:"tx_total"`
	UTXOs      core.UTXOSnapshot    `json:"utxos"`
	Registry   core.RegistrySnapshot `json:"registry"`
}

// snapshotPath returns the canonical file path for a snapshot at height.
func snapshotPath(dataDir string, height uint64) string {
	return filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-%d.json", snapVersion, height))
}

// snapshotPrevPath returns the backup path for a primary snapshot file.
// The prev file preserves the last good checkpoint before a new one is written.
func snapshotPrevPath(primaryPath string) string {
	return strings.TrimSuffix(primaryPath, ".json") + "-prev.json"
}

// copyFile copies src to dst atomically via a temporary file + rename.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// saveStartupSnapshot writes snap to disk atomically (temp file + rename).
// Before writing, it promotes the current primary snapshot (if any) to a
// "-prev.json" backup so there is always a recovery floor.
func saveStartupSnapshot(dataDir string, snap startupSnapshot) error {
	path := snapshotPath(dataDir, snap.TipHeight)
	tmp := path + ".tmp"

	// Copy the current primary snapshot to a "-prev.json" backup before
	// overwriting it (best-effort). This covers two cases:
	//   • different-height replacement — new tip, old primary backed up
	//   • same-height overwrite — e.g. shutdown snapshot taken at the same tip
	//     as the last periodic checkpoint; the existing file is backed up before
	//     the in-place overwrite.
	// If the copy fails the original primary remains on disk and the save
	// proceeds normally (no worse than the pre-backup behaviour).
	// NOTE: prev files are not consulted by the current load path; automatic
	// fallback to a prev backup on a corrupt primary is intentionally deferred.
	if entries, err := os.ReadDir(dataDir); err == nil {
		prefix := fmt.Sprintf("snapshot-v%d-", snapVersion)
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, "-prev.json") {
				continue
			}
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			full := filepath.Join(dataDir, name)
			_ = copyFile(full, snapshotPrevPath(full))
			break // only one primary should exist at a time
		}
	}

	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create snapshot tmp: %w", err)
	}
	enc := json.NewEncoder(f)
	if encErr := enc.Encode(snap); encErr != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("encode snapshot: %w", encErr)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close snapshot tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename snapshot: %w", err)
	}
	return nil
}

// loadStartupSnapshot reads and validates a snapshot for the given tip.
// Returns os.ErrNotExist when no snapshot file exists for the height.
func loadStartupSnapshot(dataDir string, tipHeight uint64, tipHashHex string) (*startupSnapshot, error) {
	path := snapshotPath(dataDir, tipHeight)
	f, err := os.Open(path)
	if err != nil {
		return nil, err // os.IsNotExist check by caller
	}
	defer f.Close()

	var snap startupSnapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if snap.Version != snapVersion {
		return nil, fmt.Errorf("snapshot version mismatch: got %d want %d",
			snap.Version, snapVersion)
	}
	if snap.TipHeight != tipHeight {
		return nil, fmt.Errorf("snapshot height mismatch: got %d want %d",
			snap.TipHeight, tipHeight)
	}
	if snap.TipHashHex != tipHashHex {
		return nil, fmt.Errorf("snapshot hash mismatch at height %d", tipHeight)
	}
	return &snap, nil
}

// findLatestSnapshot returns the highest-height valid snapshot in dataDir that
// is strictly below limitHeight, or nil if none exists.  Used to resume a
// block scan from the most recent checkpoint instead of starting from block 1.
func findLatestSnapshot(dataDir string, limitHeight uint64) *startupSnapshot {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil
	}
	prefix := fmt.Sprintf("snapshot-v%d-", snapVersion)
	var bestHeight uint64
	var bestName string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, ".tmp") {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		rest = strings.TrimSuffix(rest, ".json")
		h, parseErr := strconv.ParseUint(rest, 10, 64)
		if parseErr != nil || h == 0 || h >= limitHeight {
			continue
		}
		if h > bestHeight {
			bestHeight = h
			bestName = name
		}
	}
	if bestName == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(dataDir, bestName))
	if err != nil {
		return nil
	}
	defer f.Close()
	var snap startupSnapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return nil
	}
	if snap.Version != snapVersion || snap.TipHeight != bestHeight {
		return nil
	}
	return &snap
}

// deleteOldSnapshots removes snapshot files whose height differs from keep.
// It retains the single most-recent "-prev.json" backup alongside the primary
// so there is always a recovery floor if the newest snapshot is unreadable.
func deleteOldSnapshots(dataDir string, keep uint64) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	prefix := fmt.Sprintf("snapshot-v%d-", snapVersion)

	// Find the highest-height prev backup to keep.
	var bestPrevHeight uint64
	var bestPrevName string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, "-prev.json") {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		rest = strings.TrimSuffix(rest, "-prev.json")
		h, parseErr := strconv.ParseUint(rest, 10, 64)
		if parseErr != nil {
			continue
		}
		if h > bestPrevHeight {
			bestPrevHeight = h
			bestPrevName = name
		}
	}

	keepPrimary := fmt.Sprintf("snapshot-v%d-%d.json", snapVersion, keep)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if name == keepPrimary {
			continue // always keep the current primary
		}
		if bestPrevName != "" && name == bestPrevName {
			continue // keep the most recent prev backup
		}
		_ = os.Remove(filepath.Join(dataDir, name))
	}
}
