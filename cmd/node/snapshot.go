package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aperod/aperod/core"
)

// snapVersion must be bumped whenever the snapshot schema changes incompatibly.
// v2: gzip-compressed JSON (previously v1 was plain JSON).
const snapVersion = 2

// snapVersionLegacy is the previous uncompressed-JSON format, still accepted on
// first-boot migration but never written by this binary.
const snapVersionLegacy = 1

// startupSnapshot is the on-disk format for the UTXOSet + registry snapshot.
type startupSnapshot struct {
	Version    int                   `json:"v"`
	TipHeight  uint64                `json:"tip_height"`
	TipHashHex string                `json:"tip_hash"`
	TxTotal    int64                 `json:"tx_total"`
	UTXOs      core.UTXOSnapshot     `json:"utxos"`
	Registry   core.RegistrySnapshot `json:"registry"`
}

// snapshotPath returns the canonical file path for a gzip snapshot at height.
func snapshotPath(dataDir string, height uint64) string {
	return filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-%d.json.gz", snapVersion, height))
}

// legacySnapshotPath returns the v1 (uncompressed) primary path for a given
// height.  Used only for one-time migration detection; never written by current
// code.
func legacySnapshotPath(dataDir string, height uint64) string {
	return filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-%d.json", snapVersionLegacy, height))
}

// legacySnapshotPrevPath returns the v1 "-prev.json" backup path that the
// previous binary would have written alongside its primary.
func legacySnapshotPrevPath(dataDir string, height uint64) string {
	return filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-%d-prev.json", snapVersionLegacy, height))
}

// snapshotPrevPath returns the backup path for a primary snapshot file.
// The prev file preserves the last good checkpoint before a new one is written.
func snapshotPrevPath(primaryPath string) string {
	return strings.TrimSuffix(primaryPath, ".json.gz") + "-prev.json.gz"
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

// saveStartupSnapshot writes snap to disk as gzip-compressed JSON atomically
// (temp file + rename).
// Before writing, it promotes the current primary snapshot (if any) to a
// "-prev.json.gz" backup so there is always a recovery floor.
func saveStartupSnapshot(dataDir string, snap startupSnapshot) error {
	path := snapshotPath(dataDir, snap.TipHeight)
	tmp := path + ".tmp"

	// Copy the current primary snapshot to a "-prev.json.gz" backup before
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
			if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, "-prev.json.gz") {
				continue
			}
			if !strings.HasSuffix(name, ".json.gz") {
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
	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	encErr := enc.Encode(snap)
	gzCloseErr := gz.Close()
	fCloseErr := f.Close()
	if encErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("encode snapshot: %w", encErr)
	}
	if gzCloseErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("close gzip writer: %w", gzCloseErr)
	}
	if fCloseErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("close snapshot tmp: %w", fCloseErr)
	}

	// Write a same-height "-prev.json.gz" backup from the validated tmp content
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

// openGzipSnapshotReader opens a gzip-compressed snapshot file and returns both
// the underlying *os.File (for deferred Close) and a *gzip.Reader.
// Caller must close gzr first, then f.
func openGzipSnapshotReader(path string) (f *os.File, gzr *gzip.Reader, err error) {
	f, err = os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	gzr, err = gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("open gzip reader: %w", err)
	}
	return f, gzr, nil
}

// loadPrevBackupSnapshot reads and validates the "-prev.json.gz" backup file for
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

	f, gzr, err := openGzipSnapshotReader(prevPath)
	if err != nil {
		// Preserve os.ErrNotExist so callers can distinguish missing from corrupt.
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("open prev snapshot: %w", err)
	}
	defer f.Close()
	defer gzr.Close()

	var snap startupSnapshot
	if err := json.NewDecoder(gzr).Decode(&snap); err != nil {
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

// loadStartupSnapshot reads and validates a gzip-compressed snapshot for the
// given tip.
// Returns os.ErrNotExist when no snapshot file exists for the height.
func loadStartupSnapshot(dataDir string, tipHeight uint64, tipHashHex string) (*startupSnapshot, error) {
	path := snapshotPath(dataDir, tipHeight)
	f, gzr, err := openGzipSnapshotReader(path)
	if err != nil {
		return nil, err // os.IsNotExist check by caller
	}
	defer f.Close()
	defer gzr.Close()

	var snap startupSnapshot
	if err := json.NewDecoder(gzr).Decode(&snap); err != nil {
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

// loadLegacySnapshot attempts to load a v1 uncompressed JSON snapshot at path.
// It requires snap.Version == snapVersionLegacy and validates height and hash.
// Returns nil on any error so the caller can treat nil as "not available".
func loadLegacySnapshot(path string, tipHeight uint64, tipHashHex string) *startupSnapshot {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var snap startupSnapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return nil
	}
	// Require exactly the legacy version to guard against a corrupt file whose
	// height/hash coincidentally match but whose schema is incompatible.
	if snap.Version != snapVersionLegacy {
		return nil
	}
	if snap.TipHeight != tipHeight || snap.TipHashHex != tipHashHex {
		return nil
	}
	return &snap
}

// loadStartupSnapshotWithFallback is the production entry point for the startup
// fast path.  It first tries the v2 (gzip) primary snapshot; if that fails with
// a non-NotExist error (corrupt data, truncation, version mismatch, …) it
// automatically falls back to the "-prev.json.gz" backup.  If neither v2 file
// exists it checks for a legacy v1 uncompressed JSON snapshot and emits a
// one-time migration warning when found.  A distinct log line is emitted for
// every recovery event so operators can see them in node logs.
//
// Returns os.ErrNotExist when no usable snapshot exists at all (caller treats
// this as "no fast path available").
// Returns a wrapped descriptive error when the primary exists but is unreadable
// and the prev-backup is also unavailable or corrupt.
func loadStartupSnapshotWithFallback(dataDir string, tipHeight uint64, tipHashHex string, log *slog.Logger) (*startupSnapshot, error) {
	snap, err := loadStartupSnapshot(dataDir, tipHeight, tipHashHex)
	if err == nil {
		return snap, nil
	}
	if os.IsNotExist(err) {
		// No v2 primary present — check for a legacy v1 uncompressed snapshot.
		legacyPath := legacySnapshotPath(dataDir, tipHeight)
		if legacySnap := loadLegacySnapshot(legacyPath, tipHeight, tipHashHex); legacySnap != nil {
			log.Warn("loaded legacy uncompressed v1 snapshot; a compressed v2 snapshot will be written on next save",
				"path", legacyPath, "tip_height", tipHeight)
			return legacySnap, nil
		}
		// v1 primary absent or corrupt — try the v1 prev-backup so a node with
		// a damaged v1 primary but valid v1 backup does not lose the fast path
		// on upgrade.
		legacyPrevPath := legacySnapshotPrevPath(dataDir, tipHeight)
		if legacyPrevSnap := loadLegacySnapshot(legacyPrevPath, tipHeight, tipHashHex); legacyPrevSnap != nil {
			log.Warn("loaded legacy uncompressed v1 prev-backup snapshot; primary was absent or invalid; a compressed v2 snapshot will be written on next save",
				"path", legacyPrevPath, "tip_height", tipHeight)
			return legacyPrevSnap, nil
		}
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

// tryLoadSnapshotFile opens path, decodes the gzip-compressed JSON, and
// validates the schema version and recorded tip height against wantHeight.
// Returns nil on any error so callers can unconditionally check for nil without
// error handling.
func tryLoadSnapshotFile(path string, wantHeight uint64) *startupSnapshot {
	f, gzr, err := openGzipSnapshotReader(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	defer gzr.Close()

	var snap startupSnapshot
	if err := json.NewDecoder(gzr).Decode(&snap); err != nil {
		return nil
	}
	if snap.Version != snapVersion || snap.TipHeight != wantHeight {
		return nil
	}
	return &snap
}

// findLatestSnapshot returns the highest-height valid snapshot in dataDir that
// is strictly below limitHeight, or nil if none exists.  Used to resume a
// block scan from the most recent checkpoint instead of starting from block 1.
//
// When the primary snapshot for a candidate height is corrupt or unreadable the
// function falls back to the adjacent "-prev.json.gz" backup before discarding
// that height and trying older checkpoints.  This mirrors the recovery logic in
// loadStartupSnapshotWithFallback so that intermediate checkpoints benefit from
// the same protection.  A warning is logged (when log is non-nil) whenever a
// checkpoint is recovered this way so operators can see the event in node logs.
func findLatestSnapshot(dataDir string, limitHeight uint64, log *slog.Logger) *startupSnapshot {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil
	}
	prefix := fmt.Sprintf("snapshot-v%d-", snapVersion)

	type candidate struct {
		height uint64
		name   string
	}
	var candidates []candidate
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, "-prev.json.gz") {
			continue
		}
		if !strings.HasSuffix(name, ".json.gz") {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		rest = strings.TrimSuffix(rest, ".json.gz")
		h, parseErr := strconv.ParseUint(rest, 10, 64)
		if parseErr != nil || h == 0 || h >= limitHeight {
			continue
		}
		candidates = append(candidates, candidate{h, name})
	}

	// Try candidates from highest to lowest so the most recent checkpoint is
	// preferred and we only fall back to older ones when both the primary and
	// its prev-backup are unreadable at the best height.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].height > candidates[j].height
	})

	for _, c := range candidates {
		primaryPath := filepath.Join(dataDir, c.name)

		if snap := tryLoadSnapshotFile(primaryPath, c.height); snap != nil {
			return snap
		}

		// Primary failed — attempt recovery from the adjacent prev-backup
		// before discarding this height and trying an older checkpoint.
		prevPath := snapshotPrevPath(primaryPath)
		if snap := tryLoadSnapshotFile(prevPath, c.height); snap != nil {
			if log != nil {
				log.Warn("checkpoint recovery — primary corrupt, loaded prev-backup",
					"height", c.height)
			}
			return snap
		}

		// Both primary and prev-backup are unreadable at this height — warn
		// the operator and fall through to the next (older) candidate.
		if log != nil {
			log.Warn("skipping checkpoint — both primary and prev-backup unreadable",
				"height", c.height)
		}
	}
	return nil
}

// deleteOldSnapshots removes snapshot files whose height differs from keep.
// It retains the single most-recent "-prev.json.gz" backup alongside the
// primary so there is always a recovery floor if the newest snapshot is
// unreadable.  Legacy v1 ".json" files at other heights are also cleaned up.
func deleteOldSnapshots(dataDir string, keep uint64) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	prefix := fmt.Sprintf("snapshot-v%d-", snapVersion)
	legacyPrefix := fmt.Sprintf("snapshot-v%d-", snapVersionLegacy)

	// Find the highest-height prev backup to keep.
	var bestPrevHeight uint64
	var bestPrevName string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, "-prev.json.gz") {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		rest = strings.TrimSuffix(rest, "-prev.json.gz")
		h, parseErr := strconv.ParseUint(rest, 10, 64)
		if parseErr != nil {
			continue
		}
		if h > bestPrevHeight {
			bestPrevHeight = h
			bestPrevName = name
		}
	}

	keepPrimary := fmt.Sprintf("snapshot-v%d-%d.json.gz", snapVersion, keep)
	for _, e := range entries {
		name := e.Name()
		// Remove stale v2 files.
		if strings.HasPrefix(name, prefix) {
			if name == keepPrimary {
				continue // always keep the current primary
			}
			if bestPrevName != "" && name == bestPrevName {
				continue // keep the most recent prev backup
			}
			_ = os.Remove(filepath.Join(dataDir, name))
			continue
		}
		// Remove any legacy v1 uncompressed snapshots now that a v2 snapshot
		// has been written (they are no longer needed for migration).
		if strings.HasPrefix(name, legacyPrefix) && strings.HasSuffix(name, ".json") {
			_ = os.Remove(filepath.Join(dataDir, name))
		}
	}
}
