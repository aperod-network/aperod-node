package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

const (
	logicalBackupBatchBytes       = 4 * 1024 * 1024
	logicalBackupReservedBytes    = 5 * 1024 * 1024 * 1024
	logicalBackupWriteSafetyBytes = 16 * 1024 * 1024
)

type diskFreeBytesFunc func(string) (uint64, error)

// GetSnapshot returns a consistent point-in-time snapshot of the open database.
// The caller must release the snapshot when finished.
func (d *DB) GetSnapshot() (*leveldb.Snapshot, error) {
	if d == nil || d.db == nil {
		return nil, fmt.Errorf("store: database is not open")
	}
	return d.db.GetSnapshot()
}

// ExportLogicalBackup writes a point-in-time logical copy of all key/value pairs
// into a new LevelDB directory. The destination must not already exist. It
// returns the canonical tip recorded in the same snapshot; this does not assert
// that the database contains complete historical block data. Export refuses to
// start or continue unless the destination filesystem has more than 5 GiB of
// available space reserved beyond the estimated write requirement.
func (d *DB) ExportLogicalBackup(ctx context.Context, dst string) (tipHash crypto.Hash32, tipHeight uint64, err error) {
	return d.exportLogicalBackup(ctx, dst, availableDiskBytes)
}

func (d *DB) exportLogicalBackup(ctx context.Context, dst string, freeBytes diskFreeBytesFunc) (tipHash crypto.Hash32, tipHeight uint64, err error) {
	if ctx == nil {
		return tipHash, tipHeight, fmt.Errorf("store: backup context is nil")
	}
	if err := ctx.Err(); err != nil {
		return tipHash, tipHeight, err
	}
	if dst == "" {
		return tipHash, tipHeight, fmt.Errorf("store: backup destination is required")
	}
	if freeBytes == nil {
		return tipHash, tipHeight, fmt.Errorf("store: filesystem space checker is required")
	}
	if d != nil && d.path != "" {
		sourceAbsolute, pathErr := filepath.Abs(d.path)
		if pathErr != nil {
			return tipHash, tipHeight, fmt.Errorf("store: resolve live database path: %w", pathErr)
		}
		sourcePath, pathErr := filepath.EvalSymlinks(sourceAbsolute)
		if pathErr != nil {
			return tipHash, tipHeight, fmt.Errorf("store: resolve live database path: %w", pathErr)
		}
		destinationPath, pathErr := canonicalPathForCreate(dst)
		if pathErr != nil {
			return tipHash, tipHeight, fmt.Errorf("store: resolve backup destination: %w", pathErr)
		}
		relative, pathErr := filepath.Rel(sourcePath, destinationPath)
		if pathErr != nil {
			return tipHash, tipHeight, fmt.Errorf("store: compare database and backup paths: %w", pathErr)
		}
		if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))) {
			return tipHash, tipHeight, fmt.Errorf("store: backup destination must not be inside the live database directory")
		}
	}
	destinationParent := filepath.Dir(dst)
	if err := checkBackupDiskSpace(destinationParent, 0, freeBytes); err != nil {
		return tipHash, tipHeight, err
	}
	// Creating the directory atomically both enforces the absent-destination
	// contract and gives this operation exclusive ownership of cleanup.
	if err := os.Mkdir(dst, 0700); err != nil {
		return tipHash, tipHeight, fmt.Errorf("store: create backup destination (must be absent): %w", err)
	}
	defer func() {
		if err != nil {
			if cleanupErr := os.RemoveAll(dst); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("store: remove partial backup destination: %w", cleanupErr))
			}
		}
	}()

	snapshot, err := d.GetSnapshot()
	if err != nil {
		return tipHash, tipHeight, fmt.Errorf("store: obtain logical backup snapshot: %w", err)
	}
	defer snapshot.Release()

	tipHash, tipHeight, err = validateSnapshotTip(snapshot)
	if err != nil {
		return crypto.Hash32{}, 0, err
	}
	if err = ctx.Err(); err != nil {
		return crypto.Hash32{}, 0, err
	}

	destination, err := leveldb.OpenFile(dst, nil)
	if err != nil {
		return crypto.Hash32{}, 0, fmt.Errorf("store: open logical backup destination: %w", err)
	}
	destinationClosed := false
	defer func() {
		if !destinationClosed {
			closeErr := destination.Close()
			if closeErr != nil {
				closeErr = fmt.Errorf("store: close logical backup destination: %w", closeErr)
				if err == nil {
					err = closeErr
				} else {
					err = errors.Join(err, closeErr)
				}
			}
		}
	}()

	iter := snapshot.NewIterator(nil, nil)
	batch := new(leveldb.Batch)
	batchBytes := 0
	for iter.Next() {
		if err = ctx.Err(); err != nil {
			break
		}
		key := append([]byte(nil), iter.Key()...)
		value := append([]byte(nil), iter.Value()...)
		batch.Put(key, value)
		batchBytes += len(key) + len(value)
		if batchBytes >= logicalBackupBatchBytes {
			if err = writeBackupBatch(ctx, destination, batch, destinationParent, batchBytes, freeBytes); err != nil {
				break
			}
			batch.Reset()
			batchBytes = 0
		}
	}
	iteratorErr := iter.Error()
	iter.Release()
	if err == nil && iteratorErr != nil {
		err = fmt.Errorf("store: iterate logical backup snapshot: %w", iteratorErr)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && batch.Len() > 0 {
		err = writeBackupBatch(ctx, destination, batch, destinationParent, batchBytes, freeBytes)
	}

	closeErr := destination.Close()
	destinationClosed = true
	if closeErr != nil {
		closeErr = fmt.Errorf("store: close logical backup destination: %w", closeErr)
		if err == nil {
			err = closeErr
		} else {
			err = errors.Join(err, closeErr)
		}
	}
	if err != nil {
		return crypto.Hash32{}, 0, err
	}
	verifiedHash, verifiedHeight, verifyErr := ReadTipOnly(dst)
	if verifyErr != nil {
		return crypto.Hash32{}, 0, fmt.Errorf("store: verify closed logical backup read-only: %w", verifyErr)
	}
	if verifiedHash != tipHash || verifiedHeight != tipHeight {
		return crypto.Hash32{}, 0, fmt.Errorf(
			"store: reopened logical backup tip mismatch: got %x/%d, want %x/%d",
			verifiedHash, verifiedHeight, tipHash, tipHeight)
	}
	return tipHash, tipHeight, nil
}

func writeBackupBatch(ctx context.Context, destination *leveldb.DB, batch *leveldb.Batch, destinationParent string, batchBytes int, freeBytes diskFreeBytesFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if batchBytes < 0 || uint64(batchBytes) > (^uint64(0)-logicalBackupWriteSafetyBytes)/2 {
		return fmt.Errorf("store: logical backup batch size overflows disk-space accounting")
	}
	requiredForBatch := uint64(batchBytes)*2 + logicalBackupWriteSafetyBytes
	if err := checkBackupDiskSpace(destinationParent, requiredForBatch, freeBytes); err != nil {
		return err
	}
	if err := destination.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
		return fmt.Errorf("store: write logical backup batch: %w", err)
	}
	if err := checkBackupDiskSpace(destinationParent, 0, freeBytes); err != nil {
		return err
	}
	return nil
}

func checkBackupDiskSpace(path string, pendingBytes uint64, freeBytes diskFreeBytesFunc) error {
	available, err := freeBytes(path)
	if err != nil {
		return fmt.Errorf("store: check free space for logical backup: %w", err)
	}
	if pendingBytes > ^uint64(0)-logicalBackupReservedBytes {
		return fmt.Errorf("store: logical backup disk-space requirement overflows")
	}
	required := logicalBackupReservedBytes + pendingBytes
	if available <= required {
		return fmt.Errorf("store: logical backup needs more than %d available bytes including 5 GiB reserved headroom; only %d available",
			required, available)
	}
	return nil
}

func canonicalPathForCreate(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	var missing []string
	for {
		_, statErr := os.Lstat(current)
		if statErr == nil {
			current, err = filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				current = filepath.Join(current, missing[i])
			}
			return filepath.Clean(current), nil
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %q", path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func validateSnapshotTip(snapshot *leveldb.Snapshot) (crypto.Hash32, uint64, error) {
	var tipHash crypto.Hash32
	hashBytes, err := snapshot.Get(append(append([]byte(nil), prefixMeta...), []byte("tip/hash")...), nil)
	if err != nil {
		return tipHash, 0, fmt.Errorf("store: read snapshot tip hash: %w", err)
	}
	if len(hashBytes) != len(tipHash) {
		return tipHash, 0, fmt.Errorf("store: snapshot tip hash malformed: got %d bytes, want %d", len(hashBytes), len(tipHash))
	}
	copy(tipHash[:], hashBytes)
	if tipHash == (crypto.Hash32{}) {
		return crypto.Hash32{}, 0, fmt.Errorf("store: snapshot tip hash is all zeroes")
	}

	heightBytes, err := snapshot.Get(append(append([]byte(nil), prefixMeta...), []byte("tip/height")...), nil)
	if err != nil {
		return crypto.Hash32{}, 0, fmt.Errorf("store: read snapshot tip height: %w", err)
	}
	if len(heightBytes) != 8 {
		return crypto.Hash32{}, 0, fmt.Errorf("store: snapshot tip height malformed: got %d bytes, want 8", len(heightBytes))
	}
	tipHeight := binary.LittleEndian.Uint64(heightBytes)

	indexedHash, err := snapshot.Get(heightKey(tipHeight), nil)
	if err != nil {
		return crypto.Hash32{}, 0, fmt.Errorf("store: read snapshot tip height index at %d: %w", tipHeight, err)
	}
	if len(indexedHash) != len(tipHash) || string(indexedHash) != string(tipHash[:]) {
		return crypto.Hash32{}, 0, fmt.Errorf("store: snapshot tip metadata does not match height index at %d", tipHeight)
	}
	blockKey := append(append([]byte(nil), prefixBlock...), tipHash[:]...)
	if _, err := snapshot.Get(blockKey, nil); err != nil {
		return crypto.Hash32{}, 0, fmt.Errorf("store: read snapshot tip block at %d: %w", tipHeight, err)
	}
	return tipHash, tipHeight, nil
}
