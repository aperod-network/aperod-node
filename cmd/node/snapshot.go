package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aperod/aperod/core"
)

// staleTmpMaxAge is the age after which a leftover .tmp file from an atomic
// write is considered orphaned (i.e. not in progress) and safe to delete.
// Five minutes is long enough to never race a healthy write, and short enough
// to catch files left behind by an OOM-kill or power-loss crash.
const snapshotStaleTmpMaxAge = 5 * time.Minute

// cleanStaleTmpFile removes the file at path if it exists and is older than
// maxAge.  It is intentionally non-fatal: a stat or remove failure is logged
// and the function returns so startup can continue.  Call it before loading any
// file produced by an atomic-rename write to avoid indefinite disk accumulation
// across many crash cycles.
func cleanStaleTmpFile(path string, maxAge time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	info, err := os.Stat(path)
	if err != nil {
		// File does not exist or is not accessible — nothing to clean up.
		return
	}
	age := time.Since(info.ModTime())
	if age < maxAge {
		// Recent enough that it might belong to a concurrent write in progress.
		return
	}
	if err := os.Remove(path); err != nil {
		log.Warn("snapshot: failed to remove stale tmp file (ignoring)",
			"path", path,
			"age", age.Round(time.Second).String(),
			"err", err,
		)
		return
	}
	log.Info("snapshot: removed stale tmp file from previous crash",
		"path", path,
		"age", age.Round(time.Second).String(),
	)
}

// cleanStaleSnapshotTmpFiles scans dataDir for any orphaned .tmp files left
// behind by a crashed saveStartupSnapshot or copyFile call and removes those
// older than snapshotStaleTmpMaxAge.
//
// Affected patterns (both use atomic rename):
//   - snapshot-v2-<height>.json.gz.tmp  (primary snapshot write)
//   - snapshot-v2-<height>-prev.json.gz.tmp  (prev-backup copy)
//
// Must be called BEFORE loadStartupSnapshotWithFallback so the load path never
// encounters a partially-written .tmp alongside the final .json.gz files.
func cleanStaleSnapshotTmpFiles(dataDir string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		log.Warn("snapshot: cannot read data dir for stale-tmp scan (ignoring)",
			"dir", dataDir,
			"err", err,
		)
		return
	}
	prefix := fmt.Sprintf("snapshot-v%d-", snapVersion)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		cleanStaleTmpFile(filepath.Join(dataDir, name), snapshotStaleTmpMaxAge, log)
	}
}

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
	// Guard against mid-write truncation: a valid compressed snapshot always
	// exceeds 100 bytes (the JSON header alone — version, height, hash — is
	// larger than that when gzip-compressed).  A smaller file means the writer
	// was interrupted before flushing, e.g. by a power loss or OOM-kill.
	// Catching this here prevents json.Decode from blocking on an empty reader
	// and produces a clear, actionable error message.  Task #1019.
	if info, statErr := f.Stat(); statErr == nil && info.Size() < 100 {
		f.Close()
		return nil, nil, fmt.Errorf(
			"snapshot file is truncated or empty (size=%d bytes, path=%s): "+
				"likely interrupted mid-write — node will fall back to scan or prev backup",
			info.Size(), path)
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

// loadPrevBackupSnapshotRelaxed reads the "-prev.json.gz" backup, validating
// only the schema version and tip height — not the tip hash.  This is used as
// an emergency recovery path when the primary snapshot is absent and the DB tip
// hash may have been repaired by an out-of-band tool (e.g. recover-tip).  The
// caller is responsible for emitting a prominent warning and relying on the UTXO
// count divergence check as the secondary trust signal.
//
// Returns os.ErrNotExist when the prev file does not exist.
func loadPrevBackupSnapshotRelaxed(dataDir string, tipHeight uint64) (*startupSnapshot, error) {
	primaryPath := snapshotPath(dataDir, tipHeight)
	prevPath := snapshotPrevPath(primaryPath)

	f, gzr, err := openGzipSnapshotReader(prevPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("open prev snapshot (relaxed): %w", err)
	}
	defer f.Close()
	defer gzr.Close()

	var snap startupSnapshot
	if err := json.NewDecoder(gzr).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode prev snapshot (relaxed): %w", err)
	}
	if snap.Version != snapVersion {
		return nil, fmt.Errorf("prev snapshot (relaxed) version mismatch: got %d want %d",
			snap.Version, snapVersion)
	}
	if snap.TipHeight != tipHeight {
		return nil, fmt.Errorf("prev snapshot (relaxed) height mismatch: got %d want %d",
			snap.TipHeight, tipHeight)
	}
	// Hash not checked here — caller logs a warning and relies on UTXO count
	// divergence check as the secondary integrity signal.
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
// loadStartupSnapshotWithFallback tries to load the best available snapshot
// for tipHeight.  The second return value (isRelaxed) is true when the
// snapshot was loaded via the relaxed-hash recovery path — i.e. the primary
// was absent and the prev-backup hash does not match because recover-tip
// patched the DB tip after the snapshot was written.  Callers should widen
// or skip any UTXO-count divergence check when isRelaxed is true.
func loadStartupSnapshotWithFallback(dataDir string, tipHeight uint64, tipHashHex string, log *slog.Logger) (*startupSnapshot, bool, error) {
	snap, err := loadStartupSnapshot(dataDir, tipHeight, tipHashHex)
	if err == nil {
		return snap, false, nil
	}
	if os.IsNotExist(err) {
		// No v2 primary present — try fallbacks in priority order.
		//
		// firstFallbackErr records the first non-NotExist error from any
		// candidate that EXISTS on disk but cannot be decoded or validated.
		// If all fallbacks are exhausted and firstFallbackErr is set we return
		// it instead of the original os.ErrNotExist so that the caller
		// (logSnapshotStartupReason) can correctly emit startup_reason=corrupt_snapshot
		// rather than startup_reason=no_snapshot.
		var firstFallbackErr error

		// 1. v2 prev-backup with exact hash.
		if prevSnap, prevErr := loadPrevBackupSnapshot(dataDir, tipHeight, tipHashHex); prevErr == nil {
			log.Warn("loaded v2 prev-backup snapshot (primary absent)",
				"tip_height", tipHeight)
			return prevSnap, false, nil
		} else if !os.IsNotExist(prevErr) {
			// Backup file exists but failed validation or decoding.
			firstFallbackErr = fmt.Errorf("v2 prev-backup corrupt or unreadable: %w", prevErr)
		}

		// 2. v2 prev-backup with relaxed hash (emergency recovery: primary
		//    absent, DB tip hash was patched by recover-tip after the snapshot
		//    was written, so the hashes naturally differ).
		if relaxedSnap, relaxedErr := loadPrevBackupSnapshotRelaxed(dataDir, tipHeight); relaxedErr == nil {
			log.Warn("RECOVERY: loaded v2 prev-backup snapshot with relaxed hash check "+
				"(primary absent; DB tip hash may differ after recover-tip repair)",
				"tip_height", tipHeight)
			return relaxedSnap, true, nil
		} else if !os.IsNotExist(relaxedErr) && firstFallbackErr == nil {
			firstFallbackErr = fmt.Errorf("v2 prev-backup (relaxed) corrupt or unreadable: %w", relaxedErr)
		}

		// 3. Legacy v1 uncompressed primary.
		legacyPath := legacySnapshotPath(dataDir, tipHeight)
		if legacySnap := loadLegacySnapshot(legacyPath, tipHeight, tipHashHex); legacySnap != nil {
			log.Warn("loaded legacy uncompressed v1 snapshot; a compressed v2 snapshot will be written on next save",
				"path", legacyPath, "tip_height", tipHeight)
			return legacySnap, false, nil
		} else if _, statErr := os.Stat(legacyPath); statErr == nil && firstFallbackErr == nil {
			// File exists but loadLegacySnapshot returned nil (version/height/hash
			// mismatch or decode error).
			firstFallbackErr = fmt.Errorf("legacy v1 snapshot invalid or corrupt: %s", legacyPath)
		}

		// 4. Legacy v1 prev-backup.
		legacyPrevPath := legacySnapshotPrevPath(dataDir, tipHeight)
		if legacyPrevSnap := loadLegacySnapshot(legacyPrevPath, tipHeight, tipHashHex); legacyPrevSnap != nil {
			log.Warn("loaded legacy uncompressed v1 prev-backup snapshot; primary was absent or invalid; a compressed v2 snapshot will be written on next save",
				"path", legacyPrevPath, "tip_height", tipHeight)
			return legacyPrevSnap, false, nil
		} else if _, statErr := os.Stat(legacyPrevPath); statErr == nil && firstFallbackErr == nil {
			firstFallbackErr = fmt.Errorf("legacy v1 prev-backup invalid or corrupt: %s", legacyPrevPath)
		}

		// If any candidate was found on disk but unreadable, return that error
		// so the caller can distinguish corruption from a clean first-run.
		if firstFallbackErr != nil {
			return nil, false, firstFallbackErr
		}

		// No snapshot files exist at all — clean first-run or all were deleted.
		return nil, false, err
	}

	// Primary exists but is unreadable — attempt recovery from prev-backup.
	log.Warn("snapshot primary corrupt or unreadable, trying prev-backup", "err", err)

	prevSnap, prevErr := loadPrevBackupSnapshot(dataDir, tipHeight, tipHashHex)
	if prevErr != nil {
		if os.IsNotExist(prevErr) {
			return nil, false, fmt.Errorf("primary corrupt (%w) and no prev-backup available", err)
		}
		return nil, false, fmt.Errorf("primary corrupt (%v); prev-backup also failed: %w", err, prevErr)
	}

	log.Warn("startup fast path — loaded prev-backup snapshot; primary was unreadable",
		"tip_height", tipHeight)
	return prevSnap, false, nil
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
	covered := map[uint64]bool{} // heights already covered by a primary checkpoint
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
		covered[h] = true
	}
	// Also consider orphaned shutdown prev-backup files (snapshot-v2-{height}-prev.json.gz)
	// at heights not already covered by a primary scan checkpoint.  These are written on
	// clean shutdown and are often the highest-height valid snapshot available after a
	// crash that wiped later scan checkpoints.
	const prevSuffix = "-prev.json.gz"
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, prevSuffix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		rest = strings.TrimSuffix(rest, prevSuffix)
		h, parseErr := strconv.ParseUint(rest, 10, 64)
		if parseErr != nil || h == 0 || h >= limitHeight || covered[h] {
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

// tryLoadStartupSnapshot calls loadStartupSnapshotWithFallback and, when the
// load fails, immediately calls logSnapshotStartupReason so the structured
// startup_reason= journal entry is always emitted from the same production
// code path.  Extracting this two-call sequence into its own function creates
// a unit-testable seam: tests call tryLoadStartupSnapshot directly, so if
// logSnapshotStartupReason is ever removed from this function the tests fail.
func tryLoadStartupSnapshot(dataDir string, tipHeight uint64, tipHashHex string, log *slog.Logger) (*startupSnapshot, bool, error) {
	snap, isRelaxed, err := loadStartupSnapshotWithFallback(dataDir, tipHeight, tipHashHex, log)
	if err != nil {
		logSnapshotStartupReason(err, tipHeight, log)
	}
	return snap, isRelaxed, err
}

// logSnapshotStartupReason emits the appropriate structured log entry explaining
// why the full block scan is required.  It is called after
// loadStartupSnapshotWithFallback returns an error so journalctl output clearly
// distinguishes two distinct situations:
//
//   - startup_reason=no_snapshot — no snapshot file existed at all (first run,
//     new install, or the file was manually deleted).  This is expected on a
//     fresh node and requires only one full scan to create the first snapshot.
//
//   - startup_reason=corrupt_snapshot — a snapshot file was found but could not
//     be decoded or failed a validation check (version mismatch, height/hash
//     mismatch, truncated gzip).  This is the signature of a SIGKILL arriving
//     mid-write; the atomic rename should have prevented it, but it is logged
//     at Warn level so operators see it in monitoring.
//
// Extracting the decision into its own function makes it directly unit-testable
// without running the full node startup path.
func logSnapshotStartupReason(serr error, tipHeight uint64, log *slog.Logger) {
	if errors.Is(serr, os.ErrNotExist) {
		log.Info("no snapshot found — full block scan required",
			"tip_height", tipHeight,
			"startup_reason", "no_snapshot",
		)
	} else {
		log.Warn("snapshot corrupt or unreadable — falling back to full block scan",
			"err", serr,
			"startup_reason", "corrupt_snapshot",
		)
	}
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
			if strings.HasSuffix(name, ".tmp") {
				continue // skip in-progress temp files written by concurrent goroutines
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
