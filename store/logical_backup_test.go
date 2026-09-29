package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
)

func TestExportLogicalBackupConcurrentAtomicWrites(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := writeBackupTestTip(source, 0, make([]byte, 64*1024)); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "backup")
	start := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		<-start
		for height := uint64(1); height <= 120; height++ {
			if err := writeBackupTestTip(source, height, make([]byte, 64*1024)); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()
	close(start)
	tipHash, tipHeight, exportErr := source.exportLogicalBackup(context.Background(), dst, testBackupFreeBytes)
	if err := <-writerDone; err != nil {
		t.Fatalf("concurrent source writes: %v", err)
	}
	if exportErr != nil {
		t.Fatalf("ExportLogicalBackup: %v", exportErr)
	}
	reopenedHash, reopenedHeight, err := ReadTipOnly(dst)
	if err != nil {
		t.Fatalf("ReadTipOnly on exported destination: %v", err)
	}
	if reopenedHash != tipHash || reopenedHeight != tipHeight {
		t.Fatalf("read-only reopened tip = %x/%d, returned %x/%d",
			reopenedHash, reopenedHeight, tipHash, tipHeight)
	}

	copyDB, err := leveldb.OpenFile(dst, nil)
	if err != nil {
		t.Fatalf("open exported database: %v", err)
	}
	defer copyDB.Close()
	copiedTipHash, err := copyDB.Get(append(append([]byte(nil), prefixMeta...), []byte("tip/hash")...), nil)
	if err != nil {
		t.Fatal(err)
	}
	copiedTipHeight, err := copyDB.Get(append(append([]byte(nil), prefixMeta...), []byte("tip/height")...), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(copiedTipHash) != string(tipHash[:]) || len(copiedTipHeight) != 8 ||
		binary.LittleEndian.Uint64(copiedTipHeight) != tipHeight {
		t.Fatalf("exported tip metadata disagrees with returned tip: hash=%x height=%x, returned %x/%d",
			copiedTipHash, copiedTipHeight, tipHash, tipHeight)
	}

	// Every write updates the tip, index, block, and marker in one DB batch.
	// Their copied generation values must therefore agree even if export ran
	// concurrently with many commits.
	marker, err := copyDB.Get([]byte("test/atomic-generation"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(marker) != 8 || binary.LittleEndian.Uint64(marker) != tipHeight {
		t.Fatalf("copied atomic marker = %x, want generation %d", marker, tipHeight)
	}
	indexed, err := copyDB.Get(heightKey(tipHeight), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(indexed) != string(tipHash[:]) {
		t.Fatalf("copied height index does not match returned tip")
	}
	body, err := copyDB.Get(append(append([]byte(nil), prefixBlock...), tipHash[:]...), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 8 || binary.LittleEndian.Uint64(body[:8]) != tipHeight {
		t.Fatalf("copied tip body = %x, want generation %d", body, tipHeight)
	}
}

func TestExportLogicalBackupRejectsInvalidSnapshotTipAndCleansDestination(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*DB) error
	}{
		{
			name: "missing indexed tip",
			setup: func(db *DB) error {
				if err := writeBackupTestTip(db, 3, []byte("block")); err != nil {
					return err
				}
				return db.db.Delete(heightKey(3), nil)
			},
		},
		{
			name: "malformed tip hash",
			setup: func(db *DB) error {
				return db.db.Put(append(append([]byte(nil), prefixMeta...), []byte("tip/hash")...), []byte{1, 2}, nil)
			},
		},
		{
			name: "malformed tip height",
			setup: func(db *DB) error {
				return db.db.Put(append(append([]byte(nil), prefixMeta...), []byte("tip/height")...), []byte{1}, nil)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := Open(filepath.Join(t.TempDir(), "source"))
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if err := writeBackupTestTip(source, 0, []byte("initial block")); err != nil {
				t.Fatal(err)
			}
			if err := tt.setup(source); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "partial-backup")
			if hash, height, err := source.exportLogicalBackup(context.Background(), dst, testBackupFreeBytes); err == nil {
				t.Fatalf("ExportLogicalBackup unexpectedly succeeded with tip %x/%d", hash, height)
			}
			if _, err := os.Stat(dst); !os.IsNotExist(err) {
				t.Fatalf("partial destination still exists or could not be checked: %v", err)
			}
		})
	}
}

func TestExportLogicalBackupRequiresAbsentDestination(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	dst := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.exportLogicalBackup(context.Background(), dst, testBackupFreeBytes); err == nil {
		t.Fatal("ExportLogicalBackup succeeded with an existing destination")
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("existing destination was unexpectedly removed: %v", err)
	}
}

func TestExportLogicalBackupCancelledRemovesPartialDestination(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := writeBackupTestTip(source, 0, []byte("tip body")); err != nil {
		t.Fatal(err)
	}

	baseContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelAfterErrChecks{Context: baseContext, cancel: cancel, cancelAt: 4}
	dst := filepath.Join(t.TempDir(), "cancelled-backup")
	if _, _, err := source.exportLogicalBackup(ctx, dst, testBackupFreeBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExportLogicalBackup error = %v, want context cancellation", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("partial destination still exists or could not be checked: %v", err)
	}
}

func TestExportLogicalBackupRejectsDestinationInsideLiveDatabase(t *testing.T) {
	livePath := filepath.Join(t.TempDir(), "live")
	source, err := Open(livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	dst := filepath.Join(livePath, "nested-backup")
	if _, _, err := source.exportLogicalBackup(context.Background(), dst, testBackupFreeBytes); err == nil {
		t.Fatal("ExportLogicalBackup succeeded inside the live database directory")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("nested destination was created or could not be checked: %v", err)
	}
}

func TestExportLogicalBackupRejectsLowFreeSpaceAndCleansDestination(t *testing.T) {
	tests := []struct {
		name    string
		lowCall int
	}{
		{name: "initial preflight", lowCall: 1},
		{name: "before first batch", lowCall: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := Open(filepath.Join(t.TempDir(), "source"))
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if err := writeBackupTestTip(source, 0, []byte("tip body")); err != nil {
				t.Fatal(err)
			}

			calls := 0
			statfs := func(string) (uint64, error) {
				calls++
				if calls == tt.lowCall {
					return logicalBackupReservedBytes - 1, nil
				}
				return logicalBackupReservedBytes + 64*1024*1024, nil
			}
			dst := filepath.Join(t.TempDir(), "insufficient-space-backup")
			if _, _, err := source.exportLogicalBackup(context.Background(), dst, statfs); err == nil {
				t.Fatal("ExportLogicalBackup succeeded below reserved free-space headroom")
			}
			if calls < tt.lowCall {
				t.Fatalf("space checker called %d times, want at least %d", calls, tt.lowCall)
			}
			if _, err := os.Stat(dst); !os.IsNotExist(err) {
				t.Fatalf("partial destination still exists or could not be checked: %v", err)
			}
		})
	}
}

type cancelAfterErrChecks struct {
	context.Context
	cancel   context.CancelFunc
	cancelAt int
	calls    int
}

func testBackupFreeBytes(string) (uint64, error) {
	return logicalBackupReservedBytes + 64*1024*1024, nil
}

func (c *cancelAfterErrChecks) Err() error {
	c.calls++
	if c.calls == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

func writeBackupTestTip(db *DB, height uint64, blockBody []byte) error {
	hash := backupTestHash(height)
	var heightBytes [8]byte
	binary.LittleEndian.PutUint64(heightBytes[:], height)
	batch := new(leveldb.Batch)
	batch.Put(append(append([]byte(nil), prefixMeta...), []byte("tip/hash")...), hash[:])
	batch.Put(append(append([]byte(nil), prefixMeta...), []byte("tip/height")...), heightBytes[:])
	batch.Put(heightKey(height), hash[:])
	batch.Put(append(append([]byte(nil), prefixBlock...), hash[:]...), append(heightBytes[:], blockBody...))
	batch.Put([]byte("test/atomic-generation"), heightBytes[:])
	batch.Put([]byte(fmt.Sprintf("test/history/%020d", height)), make([]byte, 64*1024))
	return db.db.Write(batch, nil)
}

func backupTestHash(height uint64) crypto.Hash32 {
	var hash crypto.Hash32
	binary.BigEndian.PutUint64(hash[:8], height+1)
	hash[len(hash)-1] = 0xa5
	return hash
}
