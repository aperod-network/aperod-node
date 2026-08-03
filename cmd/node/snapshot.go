package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	// The prev file is consulted by loadStartupSnapshotWithFallback when the
	// primary is corrupt or unreadable, enabling automatic recovery at startup.
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

	// Write a same-height "-prev.json" backup from the validated tmp content
	// before promoting tmp to the primary.  This is a required precondition: if
	// the copy fails we abort the save (remove tmp and return the error) so the
	// caller's old primary — if any — is left intact and the node can still
	// recover on its next start.  Silently skipping this step would leave the
	// fallback absent on the very failure modes where recovery matters most.
	if err := copyFile(tmp, snapshotPrevPath(path)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write same-height prev backup: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename snapshot: %w", err)
	}
	return nil
}

// loadPrevBackupSnapshot reads and validates the "-prev.json" backup file for
// a given primary snapshot path.  It applies the same checks as
// loadStartupSnapshot — schema version, tip height, and tip hash — so a future
// recovery fallback cannot silently bypass any of them.
//
// Returns os.ErrNotExist when the prev file does not exist.
// Returns a descriptive error (not os.ErrNotExist) when the file exists but
// fails a validation check, so callers can distinguish "no backup available"
// from "backup is corrupt or mismatched".
func loadPrevBackupSnapshot(dataDir string, tipHeight uint64, tipHashHex string) (*startupSnapshot, error) {
	primaryPath := snapshotPath(dataDir, tipHeight)
	prevPath := snapshotPrevPath(primaryPath)

	f, err := os.Open(prevPath)
	if err != nil {
		return nil, err // os.IsNotExist check by caller
	}
	defer f.Close()

	var snap startupSnapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode prev snapshot: %w", err)
	}
	if snap.Version != snapVersion {
		return nil, fmt.Errorf("prev snapshot version mismatch: got %d want %d",
			snap.Version, snapVersion)
	}
	if snap.TipHeight != tipHeight {
		return nil, fmt.Errorf("prev snapshot height mismatch: got %d want %d",
			snap.TipHeight, tipHeight)
	}
	if snap.TipHashHex != tipHashHex {
		return nil, fmt.Errorf("prev snapshot hash mismatch at height %d", tipHeight)
	}
	return &snap, nil
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

// loadStartupSnapshotWithFallback is the production entry point for the startup
// fast path.  It first tries the primary snapshot; if that fails with a
// non-NotExist error (corrupt JSON, truncation, version mismatch, …) it
// automatically falls back to the "-prev.json" backup, applying the same
// version + hash validation.  A distinct log line is emitted when the fallback
// is used so operators can see the recovery event in node logs.
//
// Returns os.ErrNotExist when no primary file is present (caller treats this as
// "no fast path available").
// Returns a wrapped descriptive error when the primary exists but is unreadable
// and the prev-backup is also unavailable or corrupt.
func loadStartupSnapshotWithFallback(dataDir string, tipHeight uint64, tipHashHex string, log *slog.Logger) (*startupSnapshot, error) {
	snap, err := loadStartupSnapshot(dataDir, tipHeight, tipHashHex)
	if err == nil {
		return snap, nil
	}
	if os.IsNotExist(err) {
		// No primary snapshot present at all — caller handles this gracefully.
		return nil, err
	}

	// Primary exists but is unreadable — attempt recovery from prev-backup.
	log.Warn("snapshot primary corrupt or unreadable, trying prev-backup", "err", err)

	prevSnap, prevErr := loadPrevBackupSnapshot(dataDir, tipHeight, tipHashHex)
	if prevErr != nil {
		if os.IsNotExist(prevErr) {
			// No prev backup available; surface the original primary error.
			return nil, fmt.Errorf("primary corrupt (%w) and no prev-backup available", err)
		}
		// Prev backup exists but also failed validation.
		return nil, fmt.Errorf("primary corrupt (%v); prev-backup also failed: %w", err, prevErr)
	}

	log.Warn("startup fast path — loaded prev-backup snapshot; primary was unreadable",
		"tip_height", tipHeight)
	return prevSnap, nil
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
