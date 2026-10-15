package store

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

func copyMaintenanceDB(t *testing.T, source, dest string) {
	t.Helper()
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			t.Fatal("synthetic fixture expected only native DB files")
		}
		in, err := os.Open(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(filepath.Join(dest, entry.Name()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			in.Close()
			t.Fatal(err)
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			out.Close()
			t.Fatal(err)
		}
		if err := out.Sync(); err != nil {
			out.Close()
			t.Fatal(err)
		}
		out.Close()
	}
}

func TestRingMaintenancePhysicalOlderGenerationImport(t *testing.T) {
	path, state := maintenanceFixture(t)
	backup := filepath.Join(t.TempDir(), "old-chain.db")
	copyMaintenanceDB(t, path, backup)
	l := fixtureLease(t, path, state)
	if err := l.Fence(); err != nil {
		t.Fatal(err)
	}
	if err := l.Invalidate(); err != nil {
		t.Fatal(err)
	}
	legacyPutMember(t, path, 9, false)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	copyMaintenanceDB(t, backup, path) // physically restores old valid ready count
	legacyPutMember(t, path, 8, false) // canonical change invisible to old ordinals
	l.Close()
	if err := checkRingMaintenanceFenceAt(path, state); err == nil {
		t.Fatal("older generation hid external fence")
	}
	l = fixtureLease(t, path, state)
	inventory, err := l.RebuildAndVerify()
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Eligible != 5 {
		t.Fatal("restored marker incorrectly trusted")
	}
	if err := l.Commit(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.VerifyRingSamplingIndex(); err != nil {
		t.Fatal(err)
	}
}

func TestRingMaintenanceBorrowedWorkerAndReceiptFencing(t *testing.T) {
	path, state := maintenanceFixture(t)
	l := fixtureLease(t, path, state)
	if err := l.Fence(); err != nil {
		t.Fatal(err)
	}
	fd, err := l.WorkerFile()
	if err != nil {
		t.Fatal(err)
	}
	borrowed, err := borrowRingMaintenanceAt(path, state, l.WorkerGeneration(), fd, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := borrowed.Invalidate(); err != nil {
		t.Fatal(err)
	}
	inventory, err := borrowed.RebuildAndVerify()
	if err != nil {
		t.Fatal(err)
	}
	borrowed.Close()
	receipt := RingMaintenanceWorkerReceipt{Schema: 1, DBPath: path, Generation: l.WorkerGeneration(), Inventory: inventory}
	stale := receipt
	stale.Generation = "stale"
	if err := l.commitVerifiedWorkerAt(stale, os.Geteuid()); err == nil {
		t.Fatal("stale worker receipt accepted")
	}
	if err := checkRingMaintenanceFenceAt(path, state); err == nil {
		t.Fatal("failed receipt released fence")
	}
	if err := l.commitVerifiedWorkerAt(receipt, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 && l.CommitVerifiedWorker(receipt) == nil {
		t.Fatal("unprivileged worker allowed root commit")
	}
}

func TestRingMaintenanceRejectsCounterfeitLeaseDescriptor(t *testing.T) {
	path, state := maintenanceFixture(t)
	l := fixtureLease(t, path, state)
	fake, err := os.OpenFile(filepath.Join(state, "other.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	if borrowed, err := borrowRingMaintenanceAt(path, state, l.WorkerGeneration(), fake, uint32(os.Geteuid())); err == nil {
		borrowed.Close()
		t.Fatal("unrelated descriptor authorized maintenance")
	}
	other, _ := maintenanceFixture(t)
	fd, err := l.WorkerFile()
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if borrowed, err := borrowRingMaintenanceAt(other, state, l.WorkerGeneration(), fd, uint32(os.Geteuid())); err == nil {
		borrowed.Close()
		t.Fatal("lease authorized different database")
	}
}

func TestRingMaintenanceRejectsConflictingLegacyBackfill(t *testing.T) {
	path, state := maintenanceFixture(t)
	l := fixtureLease(t, path, state)
	if err := l.Fence(); err != nil {
		t.Fatal(err)
	}
	if err := l.Invalidate(); err != nil {
		t.Fatal(err)
	}
	db, err := l.existingFencedDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Delete(append(append([]byte{}, prefixMeta...), []byte("ring-member-index-v1")...), &opt.WriteOptions{Sync: true}); err != nil {
		t.Fatal(err)
	}
	u := sparseRingOutput(1)
	u.ProtocolLocked = true
	raw, _ := json.Marshal(u)
	if err := db.db.Put(ringMemberKey(u.OneTimePub), raw, &opt.WriteOptions{Sync: true}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := l.RebuildAndVerify(); err == nil {
		t.Fatal("legacy backfill overwrote canonical repair")
	}
	db, err = l.existingFencedDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.db.Get(ringMemberKey(u.OneTimePub), nil)
	if err != nil || string(got) != string(raw) {
		t.Fatal("canonical repair lost on failed rebuild")
	}
}

func TestRingMaintenanceReadOnlyNeverCreatesMissingLock(t *testing.T) {
	path, state := maintenanceFixture(t)
	l := fixtureLease(t, path, state)
	lock := filepath.Join(path, "LOCK")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if db, err := l.OpenReadOnly(); err == nil {
		db.Close()
		t.Fatal("missing native lock accepted")
	}
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatal("read-only probe initialized a lock")
	}
}

func maintenanceFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dbPath, state := filepath.Join(root, "chain.db"), filepath.Join(root, "control")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := byte(1); i <= 4; i++ {
		putSamplingOutput(t, db, sparseRingOutput(i))
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Put(append(append([]byte{}, prefixSpentUTXO...), make([]byte, 36)...), []byte{1}, &opt.WriteOptions{Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath, state
}

func fixtureLease(t *testing.T, path, root string) *RingMaintenanceLease {
	t.Helper()
	l, err := acquireRingMaintenanceAt(path, root, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func legacyPutMember(t *testing.T, path string, id byte, locked bool) {
	t.Helper()
	db, err := leveldb.OpenFile(path, &opt.Options{ErrorIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u := sparseRingOutput(id)
	u.ProtocolLocked = locked
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put(ringMemberKey(u.OneTimePub), raw, &opt.WriteOptions{Sync: true}); err != nil {
		t.Fatal(err)
	}
}

func TestRingMaintenanceDowngradeRepairAndRetainedSpent(t *testing.T) {
	path, root := maintenanceFixture(t)
	l := fixtureLease(t, path, root)
	if err := l.Fence(); err != nil {
		t.Fatal(err)
	}
	if err := l.Invalidate(); err != nil {
		t.Fatal(err)
	}
	if err := checkRingMaintenanceFenceAt(path, root); err == nil {
		t.Fatal("fence ignored")
	}
	if err := l.Commit(); err == nil {
		t.Fatal("unaudited commit")
	}
	if other, err := acquireRingMaintenanceAt(path, root, uint32(os.Geteuid())); err == nil {
		other.Close()
		t.Fatal("mixed controllers")
	}
	legacyPutMember(t, path, 7, false)
	legacyPutMember(t, path, 1, true)
	out, err := l.RebuildAndVerify()
	if err != nil || out.Canonical != 5 || out.Eligible != 4 || out.Locked != 1 {
		t.Fatal(out, err)
	}
	db, err := l.existingFencedDB()
	if err != nil {
		t.Fatal(err)
	}
	spent := db.db.NewIterator(nil, nil)
	count := 0
	for spent.Next() {
		if len(spent.Key()) >= len(prefixSpentUTXO) && string(spent.Key()[:len(prefixSpentUTXO)]) == string(prefixSpentUTXO) {
			count++
		}
	}
	if err := spent.Error(); err != nil {
		t.Fatal(err)
	}
	spent.Release()
	db.Close()
	if count != 1 {
		t.Fatal("spent anonymity marker lost")
	}
	if err := l.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := checkRingMaintenanceFenceAt(path, root); err != nil {
		t.Fatal(err)
	}
}

func TestRingMaintenanceRestoreReadyMarkerAndFailureRemainFenced(t *testing.T) {
	path, root := maintenanceFixture(t)
	l := fixtureLease(t, path, root)
	if err := l.Fence(); err != nil {
		t.Fatal(err)
	}
	if err := l.Invalidate(); err != nil {
		t.Fatal(err)
	}
	// Import an older ready-generation marker plus an rm/-only repair.
	raw, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Put(ringSamplingCountKey, ringSamplingUint(4), &opt.WriteOptions{Sync: true}); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	legacyPutMember(t, path, 8, false)
	if err := checkRingMaintenanceFenceAt(path, root); err == nil {
		t.Fatal("imported ready marker bypassed external fence")
	}
	out, err := l.RebuildAndVerify()
	if err != nil || out.Eligible != 5 {
		t.Fatal(out, err)
	}
	// A corrupt canonical repair must revoke readiness and never release.
	raw, err = leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Put(ringMemberKey(sparseRingOutput(9).OneTimePub), []byte("{broken"), nil); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := l.RebuildAndVerify(); err == nil {
		t.Fatal("bad repair accepted")
	}
	if err := l.Commit(); err == nil {
		t.Fatal("failed repair committed")
	}
	if err := checkRingMaintenanceFenceAt(path, root); err == nil {
		t.Fatal("failed repair unlocked")
	}
	raw, err = leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ready, err := ringSamplingCount(raw); err != nil || ready {
		t.Fatal("failed repair left ready", err)
	}
	raw.Close()
}

func TestRingMaintenanceMissingBusyAndRedirectedDatabase(t *testing.T) {
	path, root := maintenanceFixture(t)
	l := fixtureLease(t, path, root)
	if err := l.Fence(); err != nil {
		t.Fatal(err)
	}
	writer, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Invalidate(); err == nil {
		t.Fatal("active legacy writer not rejected")
	}
	writer.Close()
	moved := path + ".held"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := l.RebuildAndVerify(); err == nil {
		t.Fatal("missing DB initialized")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing DB was created", err)
	}
	if err := os.Symlink(moved, path); err != nil {
		t.Fatal(err)
	}
	if err := checkRingMaintenanceFenceAt(path, root); err == nil {
		t.Fatal("symlink redirect hid fence")
	}
	if _, err := l.RebuildAndVerify(); err == nil {
		t.Fatal("redirected database accepted")
	}
}

func TestRingMaintenanceCrashReopen(t *testing.T) {
	if os.Getenv("APEROD_TEST_RING_MAINTENANCE_CRASH") == "1" {
		path, root := os.Getenv("APEROD_TEST_RING_DB"), os.Getenv("APEROD_TEST_RING_STATE")
		// Child is only a test executable and requires its fixture tag.
		if marker, err := os.ReadFile(filepath.Join(root, "synthetic-only")); err != nil || string(marker) != "synthetic\n" {
			t.Fatal("fixture tag required")
		}
		l := fixtureLease(t, path, root)
		if err := l.Fence(); err != nil {
			t.Fatal(err)
		}
		if err := l.Invalidate(); err != nil {
			t.Fatal(err)
		}
		legacyPutMember(t, path, 10, false)
		os.Exit(73)
	}
	path, root := maintenanceFixture(t)
	if err := os.WriteFile(filepath.Join(root, "synthetic-only"), []byte("synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestRingMaintenanceCrashReopen$")
	child.Env = append(os.Environ(), "APEROD_TEST_RING_MAINTENANCE_CRASH=1", "APEROD_TEST_RING_DB="+path, "APEROD_TEST_RING_STATE="+root)
	err := child.Run()
	exited, ok := err.(*exec.ExitError)
	if !ok || exited.ExitCode() != 73 {
		t.Fatal("wrong crash outcome", err)
	}
	if err := checkRingMaintenanceFenceAt(path, root); err == nil {
		t.Fatal("crash lost external fence")
	}
	l := fixtureLease(t, path, root)
	if err := l.Commit(); err == nil {
		t.Fatal("crash recovery trusted old marker")
	}
	out, err := l.RebuildAndVerify()
	if err != nil || out.Eligible != 5 {
		t.Fatal(out, err)
	}
	if err := l.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRingMaintenanceLeaseRejectsUnsafeControlRoot(t *testing.T) {
	path, root := maintenanceFixture(t)
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	if l, err := acquireRingMaintenanceAt(path, root, uint32(os.Geteuid())); err == nil {
		l.Close()
		t.Fatal("writable control root accepted")
	}
	var l *RingMaintenanceLease
	if err := l.Invalidate(); err == nil {
		t.Fatal("nil lease accepted")
	}
}
