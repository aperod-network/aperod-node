package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
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
	// SavedAt records the wall-clock time when this snapshot was written.
	// Used at startup to determine whether the snapshot falls within the OOM
	// window (i.e., was saved close enough to an unclean shutdown that the
	// in-memory state it captured might have been partially corrupted).
	// Omitted from snapshots written by older binaries; treated as zero-value.
	SavedAt    time.Time             `json:"saved_at,omitempty"`
	UTXOs      core.UTXOSnapshot     `json:"utxos"`
	Registry   core.RegistrySnapshot `json:"registry"`
}

// ── Clean-shutdown marker ─────────────────────────────────────────────────────
//
// A small sentinel file written by the SIGTERM shutdown path after the snapshot
// is saved.  Its presence on the next startup means the last shutdown was
// graceful (no OOM).  Its absence means the node was killed without a chance to
// run the shutdown handler — the canonical OOM scenario.
//
// The file lives next to the snapshot files (not inside chain.db/) so that an
// operator who rsyncs only chain.db/ does not accidentally carry over a marker
// from a different machine, which would cause the relay to skip validation.

// cleanShutdownMarkerPath returns the path of the clean-shutdown sentinel file.
func cleanShutdownMarkerPath(dataDir string) string {
	return filepath.Join(dataDir, "clean_shutdown")
}

// writeCleanShutdownMarker records that the last shutdown was graceful so the
// next startup can skip the OOM-window AmountCommit validation.  Non-fatal on
// error: an absent marker causes validation to run (fail-closed behaviour).
func writeCleanShutdownMarker(dataDir string) error {
	return os.WriteFile(cleanShutdownMarkerPath(dataDir), []byte("1\n"), 0644)
}

// readAndDeleteCleanShutdownMarker returns true when the clean-shutdown marker
// file exists AND is successfully removed.  It returns false (treating the
// shutdown as unclean) in all other cases:
//
//   - file absent → previous shutdown was unclean (OOM / SIGKILL)
//   - stat error → treat as unclean (fail-closed)
//   - remove fails → marker left on disk; treat as unclean and log so the
//     operator knows validation is running and why
//
// The remove-fail-closed rule is critical: if os.Remove fails (e.g. permissions
// changed) but we return true, the stale marker survives and every subsequent
// restart skips AmountCommit validation even after OOM kills.
func readAndDeleteCleanShutdownMarker(dataDir string) (wasClean bool) {
	path := cleanShutdownMarkerPath(dataDir)
	if _, err := os.Stat(path); err != nil {
		return false // absent → previous shutdown was unclean
	}
	if err := os.Remove(path); err != nil {
		// Cannot consume the marker — treat as unclean (fail-closed).
		// Log to stderr (logger not yet initialised at call site) so the
		// operator can diagnose the permission issue.
		fmt.Fprintf(os.Stderr,
			"aperod-node: warning: clean-shutdown marker exists but could not be removed (%v); "+
				"treating previous shutdown as unclean — AmountCommit validation will run\n", err)
		return false
	}
	return true
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

// snapshotChecksumPath returns the sidecar file path holding the SHA-256
// checksum (hex) of the snapshot file at path.  Task #964.
func snapshotChecksumPath(path string) string {
	return path + ".sha256"
}

// writeSnapshotChecksum atomically writes the hex-encoded SHA-256 digest of a
// snapshot file to its sidecar (temp file + rename, same pattern as the
// snapshot itself so a crash can never leave a half-written sidecar).
func writeSnapshotChecksum(path string, digest []byte) error {
	chkPath := snapshotChecksumPath(path)
	tmp := chkPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(hex.EncodeToString(digest)+"\n"), 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, chkPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// copySnapshotChecksum keeps a snapshot's checksum sidecar in sync when the
// snapshot file itself is copied (primary → prev-backup promotion).
//
// Fail-closed contract (task #964 review):
//   - Source sidecar ABSENT (os.IsNotExist only): the source snapshot was
//     written by a pre-checksum binary.  Any stale sidecar at the destination
//     is removed — a leftover digest from an older promotion would make the
//     fresh, valid copy fail verification.
//   - Source sidecar stat fails for any OTHER reason (permissions, I/O): the
//     error is returned.  Treating it as "absent" would silently convert a
//     checksum-protected snapshot into an unchecksummed backup.
//   - Copy fails: the error is returned and any existing destination sidecar
//     is LEFT IN PLACE.  A stale digest forces a verification failure and a
//     fallback — fail-closed — whereas removing it would skip verification
//     entirely on a backup whose integrity is now unknown.
func copySnapshotChecksum(src, dst string) error {
	srcChk := snapshotChecksumPath(src)
	dstChk := snapshotChecksumPath(dst)
	if _, err := os.Stat(srcChk); err != nil {
		if os.IsNotExist(err) {
			_ = os.Remove(dstChk)
			return nil
		}
		return fmt.Errorf("stat source checksum sidecar %s: %w", srcChk, err)
	}
	if err := copyFile(srcChk, dstChk); err != nil {
		return fmt.Errorf("copy checksum sidecar %s -> %s: %w", srcChk, dstChk, err)
	}
	return nil
}

// verifySnapshotChecksum compares the SHA-256 of the ALREADY-OPEN snapshot
// file f against the sidecar at snapshotChecksumPath(path), then seeks f back
// to offset 0 so the caller can deserialise it.  Hashing the same descriptor
// that will be deserialised (rather than re-opening the pathname) closes the
// verify/deserialise race: a file replaced on disk between the two operations
// cannot pass verification as one file and load as another.
//
// Only an ABSENT sidecar (os.IsNotExist) skips verification — that is the
// backward-compatible path for snapshots written by pre-checksum binaries;
// the schema version / height / hash checks still apply.  A sidecar that
// exists but is unreadable or malformed is reported as corruption: silently
// skipping verification in those cases would let a structurally valid but
// altered snapshot be deserialised, which is exactly what this check exists
// to prevent.  Task #964.
func verifySnapshotChecksum(f *os.File, path string) error {
	raw, err := os.ReadFile(snapshotChecksumPath(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no sidecar — backward compatible, verification skipped
		}
		return fmt.Errorf("snapshot checksum sidecar unreadable (path=%s): %w — "+
			"treating snapshot as corrupt and falling back", path, err)
	}
	want := strings.TrimSpace(string(raw))
	if len(want) != sha256.Size*2 {
		return fmt.Errorf("snapshot checksum sidecar malformed (path=%s, len=%d, want %d hex chars): "+
			"treating snapshot as corrupt and falling back",
			path, len(want), sha256.Size*2)
	}
	if _, err := hex.DecodeString(want); err != nil {
		return fmt.Errorf("snapshot checksum sidecar is not valid hex (path=%s): %w — "+
			"treating snapshot as corrupt and falling back", path, err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash snapshot for checksum verification: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf(
			"snapshot checksum mismatch (path=%s, want=%s, got=%s): "+
				"file is corrupt, truncated mid-write, or was replaced on disk — "+
				"falling back instead of serving wrong chain state",
			path, want, got)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind snapshot after checksum verification: %w", err)
	}
	return nil
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

	// Same-height overwrite guard: when a primary already exists at this exact
	// height (e.g. shutdown snapshot taken at the same tip as the last periodic
	// checkpoint), copy the EXISTING PRIMARY to its "-prev.json.gz" backup
	// BEFORE writing the new tmp.  A failure here aborts the save so the
	// original primary is left intact and the node can recover on its next start.
	//
	// When no primary exists at this height yet (first checkpoint save at this
	// height during a genesis scan), skip the copy entirely — there is nothing
	// to preserve.  The previous code attempted an unconditional copy from the
	// tmp file at this point, which failed with "no such file or directory" on
	// every first-time checkpoint because no prior backup source existed,
	// forcing a full rescan after any crash mid genesis scan.
	if _, statErr := os.Stat(path); statErr == nil {
		// Staged publication (fail-closed): promote the SIDECAR first, then
		// the snapshot file.  If the sidecar copy fails, nothing has been
		// touched — the old prev backup (and its sidecar) remain intact and
		// the save aborts with the original primary still on disk.  If the
		// file copy then fails, the prev sidecar already holds the NEW digest
		// while the prev file is still the OLD content — verification fails
		// and loaders fall back, rather than accepting a prev backup whose
		// integrity can no longer be proven.
		if err := copySnapshotChecksum(path, snapshotPrevPath(path)); err != nil {
			return fmt.Errorf("promote checksum sidecar for same-height prev backup: %w", err)
		}
		if err := copyFile(path, snapshotPrevPath(path)); err != nil {
			return fmt.Errorf("write same-height prev backup: %w", err)
		}
	}

	// Best-effort: copy any existing primary at a DIFFERENT height to its own
	// prev backup so older checkpoints get a recovery floor before being
	// superseded.  Errors are ignored — the old primary remains on disk
	// regardless, so this step is strictly additive.
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
			if full == path {
				continue // same height already handled by the guard above
			}
			// Staged publication, same order as the same-height guard: sidecar
			// first, then the file.  A sidecar failure skips the file copy
			// entirely (old prev state intact); a file-copy failure after the
			// sidecar leaves a digest that does not match the old prev content,
			// forcing verification failure and fallback — never an unverified
			// prev backup from a checksum-protected source.
			if chkErr := copySnapshotChecksum(full, snapshotPrevPath(full)); chkErr == nil {
				_ = copyFile(full, snapshotPrevPath(full))
			}
			break // only one other primary should exist at a time
		}
	}

	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create snapshot tmp: %w", err)
	}
	// Hash the exact bytes written to disk (compressed stream) so the sidecar
	// checksum can later detect truncation or partial writes.  Task #964.
	hasher := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(f, hasher))
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

	// Staged publication (fail-closed): write the SHA-256 sidecar BEFORE
	// renaming the snapshot into place.  A sidecar write failure aborts the
	// save while the previous primary (and its matching sidecar, when the
	// heights differ) is still on disk — a current-binary snapshot is never
	// published without its checksum, so it can never be mistaken for an
	// unchecked legacy file on restart.  If the rename below then fails, the
	// sidecar already holds the NEW digest while path still has the OLD
	// content — verification fails and loaders fall back to the prev backup
	// promoted above, rather than serving a file of unproven integrity.
	if chkErr := writeSnapshotChecksum(path, hasher.Sum(nil)); chkErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("write snapshot checksum sidecar: %w", chkErr)
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
	// Verify the SHA-256 sidecar BEFORE deserialising (task #964).  The check
	// hashes THIS descriptor and rewinds it, so the verified bytes are the
	// exact bytes handed to the gzip reader below.  A mismatch is returned as
	// a descriptive (non-NotExist) error so every caller — primary,
	// prev-backup, and checkpoint loaders — treats the file as corrupt and
	// falls back rather than silently serving wrong chain state.  Snapshots
	// without a sidecar (pre-checksum binaries) skip verification.
	if chkErr := verifySnapshotChecksum(f, path); chkErr != nil {
		f.Close()
		return nil, nil, chkErr
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
			if name == keepPrimary || name == keepPrimary+".sha256" {
				continue // always keep the current primary and its checksum sidecar
			}
			if bestPrevName != "" && (name == bestPrevName || name == bestPrevName+".sha256") {
				continue // keep the most recent prev backup and its checksum sidecar
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
