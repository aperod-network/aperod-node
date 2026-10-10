// recover-tip restores the LevelDB tip pointer to a specific block height and
// updates the on-disk snapshot so the next node restart uses the normal fast
// path instead of the relaxed-hash fallback.
//
// Usage:
//
//	cd /opt/aperod/blockchain
//	go run ./cmd/recover-tip/ --db /opt/aperod/data/testnet/chain.db --height 973102
//
// By default the snapshot directory is derived from --db (its parent directory).
// Pass --data-dir explicitly when the snapshot files live elsewhere.
//
// Flags:
//
//	--db        Path to chain.db directory (required)
//	--height    Target block height to restore tip to (required)
//	--data-dir  Directory containing snapshot files (default: parent of --db)
//	--dry-run   Print what would change without writing anything
//
// After repairing m/tip/hash and m/tip/height in LevelDB, recover-tip also
// patches the on-disk snapshot file (snapshot-v2-{height}.json.gz and its
// -prev.json.gz companion) so the next node restart takes the normal fast path
// (exact hash match) instead of the relaxed-hash fallback.
//
// In --dry-run mode the LevelDB is opened read-only (with ErrorIfMissing) so
// no files are created or modified.
//
// The node MUST be stopped before running this tool.
package main

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

var (
	prefixHeight = []byte("h/") // h/<uint64-big-endian> → 32-byte hash
	prefixMeta   = []byte("m/") // m/<key> → value
)

// snapVersion must match the version used by cmd/node/snapshot.go.
const snapVersion = 2

// startupSnapshot mirrors the on-disk format in cmd/node/snapshot.go.
// UTXOs and Registry are decoded as raw JSON so we preserve their content
// exactly — we only patch TipHashHex.
type startupSnapshot struct {
	Version    int             `json:"v"`
	TipHeight  uint64          `json:"tip_height"`
	TipHashHex string          `json:"tip_hash"`
	TxTotal    int64           `json:"tx_total"`
	UTXOs      json.RawMessage `json:"utxos"`
	Registry   json.RawMessage `json:"registry"`
}

func heightKey(h uint64) []byte {
	key := make([]byte, len(prefixHeight)+8)
	copy(key, prefixHeight)
	binary.BigEndian.PutUint64(key[len(prefixHeight):], h)
	return key
}

func metaKey(k string) []byte {
	return append(append([]byte{}, prefixMeta...), []byte(k)...)
}

// snapshotPath returns the canonical primary snapshot path.
func snapshotPath(dataDir string, height uint64) string {
	return filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-%d.json.gz", snapVersion, height))
}

// snapshotPrevPath returns the "-prev.json.gz" backup path for a primary path.
func snapshotPrevPath(primaryPath string) string {
	return strings.TrimSuffix(primaryPath, ".json.gz") + "-prev.json.gz"
}

// safePrefix returns the first n characters of s, or all of s if it is shorter.
// Used to safely truncate potentially-malformed hash hex strings for logging.
func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// readSnapshot reads and decodes a gzip-compressed JSON snapshot from path.
func readSnapshot(path string) (*startupSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("open gzip reader: %w", err)
	}
	defer gzr.Close()

	var snap startupSnapshot
	if err := json.NewDecoder(gzr).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	return &snap, nil
}

// writeSnapshot encodes snap as gzip-compressed JSON to path atomically
// (temp file + rename).
func writeSnapshot(path string, snap *startupSnapshot) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp file: %w", err)
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
		return fmt.Errorf("close tmp file: %w", fCloseErr)
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename tmp to final: %w", err)
	}
	return nil
}

// patchSnapshot reads the snapshot at path, updates tip_hash to newHashHex,
// and writes it back atomically. In dry-run mode it only prints what would
// change. Returns (found, err): found=false means the file does not exist.
func patchSnapshot(path, label, newHashHex string, dryRun bool) (found bool, err error) {
	snap, err := readSnapshot(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return true, fmt.Errorf("read %s snapshot: %w", label, err)
	}

	oldHash := snap.TipHashHex
	if oldHash == newHashHex {
		log.Printf("  %s snapshot: tip_hash already correct (%s) — no change needed",
			label, safePrefix(newHashHex, 16))
		return true, nil
	}

	if dryRun {
		log.Printf("  [dry-run] %s snapshot at %s:", label, path)
		log.Printf("    tip_hash: %s → %s",
			safePrefix(oldHash, 16), safePrefix(newHashHex, 16))
		return true, nil
	}

	snap.TipHashHex = newHashHex
	if err := writeSnapshot(path, snap); err != nil {
		return true, fmt.Errorf("write %s snapshot: %w", label, err)
	}
	log.Printf("  ✓ %s snapshot updated: tip_hash %s → %s",
		label, safePrefix(oldHash, 16), safePrefix(newHashHex, 16))
	return true, nil
}

func main() {
	dbPath := flag.String("db", "", "Path to chain.db directory (required)")
	height := flag.Uint64("height", 0, "Target block height to restore tip to (required)")
	dataDir := flag.String("data-dir", "", "Directory containing snapshot files (default: parent of --db)")
	dryRun := flag.Bool("dry-run", false, "Print what would change without writing anything")
	flag.Parse()

	if *dbPath == "" || *height == 0 {
		fmt.Fprintln(os.Stderr, "Usage: recover-tip --db <chain.db path> --height <block height> [--data-dir <dir>] [--dry-run]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Flags:")
		fmt.Fprintln(os.Stderr, "  --db        Path to chain.db directory (required)")
		fmt.Fprintln(os.Stderr, "  --height    Target block height to restore tip to (required)")
		fmt.Fprintln(os.Stderr, "  --data-dir  Directory containing snapshot-v2-* files (default: parent of --db)")
		fmt.Fprintln(os.Stderr, "  --dry-run   Print what would change without writing anything")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "After repairing m/tip/hash and m/tip/height in LevelDB, recover-tip also")
		fmt.Fprintln(os.Stderr, "patches the on-disk snapshot (snapshot-v2-{height}.json.gz and its")
		fmt.Fprintln(os.Stderr, "-prev.json.gz companion) so the next node restart takes the normal fast")
		fmt.Fprintln(os.Stderr, "path (exact hash match) instead of the relaxed-hash fallback.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "In --dry-run mode the LevelDB is opened read-only so nothing is written.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "The node MUST be stopped before running this tool.")
		os.Exit(1)
	}

	if *dryRun {
		log.Printf("[dry-run mode] no changes will be written to disk")
	}

	// Resolve the data directory (where snapshot files live).
	snapDir := *dataDir
	if snapDir == "" {
		snapDir = filepath.Dir(*dbPath)
	}
	log.Printf("Snapshot directory: %s", snapDir)

	// Open LevelDB. In dry-run mode use read-only + ErrorIfMissing so we
	// cannot create a new DB or trigger any writable recovery work.
	log.Printf("Opening LevelDB at %s ...", *dbPath)
	dbOpts := &opt.Options{
		BlockCacheCapacity: 32 * opt.MiB,
		WriteBuffer:        16 * opt.MiB,
	}
	if *dryRun {
		dbOpts.ReadOnly = true
		dbOpts.ErrorIfMissing = true
	}
	db, err := leveldb.OpenFile(*dbPath, dbOpts)
	if err != nil {
		log.Fatalf("FATAL: cannot open LevelDB: %v", err)
	}
	defer db.Close()

	// Read current tip for display.
	curHashB, _ := db.Get(metaKey("tip/hash"), nil)
	curHtB, _ := db.Get(metaKey("tip/height"), nil)
	if curHashB != nil && len(curHtB) == 8 {
		curHt := binary.LittleEndian.Uint64(curHtB)
		log.Printf("Current tip: height=%d hash=%x", curHt, curHashB[:8])
	} else {
		log.Printf("Current tip: not found / corrupt")
	}

	// Look up the hash for the requested height via the canonical height index.
	hKey := heightKey(*height)
	hashBytes, err := db.Get(hKey, nil)
	if err == leveldb.ErrNotFound || len(hashBytes) == 0 {
		log.Fatalf("FATAL: no height-index entry for block %d — block not in DB", *height)
	}
	if err != nil {
		log.Fatalf("FATAL: reading height index for %d: %v", *height, err)
	}
	if len(hashBytes) != 32 {
		log.Fatalf("FATAL: height index entry for %d is %d bytes, want 32", *height, len(hashBytes))
	}

	log.Printf("Found block %d with hash %x", *height, hashBytes[:8])

	// Confirm the block data itself exists (sanity check).
	blkKey := append([]byte("b/"), hashBytes...)
	blkData, err := db.Get(blkKey, nil)
	if err == leveldb.ErrNotFound || len(blkData) == 0 {
		log.Fatalf("FATAL: block data for height %d (hash %x) not found in b/ namespace", *height, hashBytes[:8])
	}
	if err != nil {
		log.Fatalf("FATAL: reading block data: %v", err)
	}
	log.Printf("Block data found: %d bytes", len(blkData))

	newHashHex := fmt.Sprintf("%x", hashBytes)

	// Write new tip (skip in dry-run mode).
	if *dryRun {
		log.Printf("[dry-run] would write m/tip/hash = %x", hashBytes[:8])
		log.Printf("[dry-run] would write m/tip/height = %d", *height)
	} else {
		var hb [8]byte
		binary.LittleEndian.PutUint64(hb[:], *height)

		batch := new(leveldb.Batch)
		batch.Put(metaKey("tip/hash"), hashBytes)
		batch.Put(metaKey("tip/height"), hb[:])

		if err := db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
			log.Fatalf("FATAL: writing tip: %v", err)
		}
		log.Printf("✓ Tip restored to height=%d hash=%x", *height, hashBytes[:8])
	}

	// Patch snapshot files so the next restart uses the normal fast path.
	//
	// We update both the primary (snapshot-v2-{height}.json.gz) and the
	// prev-backup (snapshot-v2-{height}-prev.json.gz) when they exist at the
	// target height, because either file may be loaded at startup depending on
	// which fast-path branch the node takes.
	log.Printf("Checking snapshot files in %s ...", snapDir)

	primaryPath := snapshotPath(snapDir, *height)
	prevPath := snapshotPrevPath(primaryPath)

	primaryFound, primaryErr := patchSnapshot(primaryPath, "primary", newHashHex, *dryRun)
	prevFound, prevErr := patchSnapshot(prevPath, "prev-backup", newHashHex, *dryRun)

	if primaryErr != nil {
		log.Printf("WARNING: could not patch primary snapshot: %v", primaryErr)
	}
	if prevErr != nil {
		log.Printf("WARNING: could not patch prev-backup snapshot: %v", prevErr)
	}

	if !primaryFound && !prevFound {
		log.Printf("WARNING: no snapshot found for height %d in %s; next restart will use block scan or relaxed path", *height, snapDir)
	}

	if *dryRun {
		log.Printf("[dry-run] done — no files were modified")
	} else {
		log.Printf("You can now restart the node.")
	}
}
