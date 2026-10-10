package main

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

// ---- helpers ----------------------------------------------------------------

// makeMemDB creates an in-memory LevelDB populated with a fake block at the
// given height/hash and sets m/tip/hash and m/tip/height accordingly.
func makeMemDB(t *testing.T, height uint64, hashBytes []byte) *leveldb.DB {
	t.Helper()
	db, err := leveldb.Open(storage.NewMemStorage(), nil)
	if err != nil {
		t.Fatalf("open mem DB: %v", err)
	}

	// height index
	if err := db.Put(heightKey(height), hashBytes, nil); err != nil {
		t.Fatalf("put height key: %v", err)
	}
	// block data (minimal non-empty value)
	blkKey := append([]byte("b/"), hashBytes...)
	if err := db.Put(blkKey, []byte{0x01}, nil); err != nil {
		t.Fatalf("put block data: %v", err)
	}
	// tip pointers
	var hb [8]byte
	binary.LittleEndian.PutUint64(hb[:], height)
	if err := db.Put(metaKey("tip/height"), hb[:], nil); err != nil {
		t.Fatalf("put tip/height: %v", err)
	}
	if err := db.Put(metaKey("tip/hash"), hashBytes, nil); err != nil {
		t.Fatalf("put tip/hash: %v", err)
	}
	return db
}

// writeGzipSnapshot writes a startupSnapshot as gzip JSON to path.
func writeGzipSnapshot(t *testing.T, path string, snap startupSnapshot) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create snapshot file: %v", err)
	}
	gz := gzip.NewWriter(f)
	if err := json.NewEncoder(gz).Encode(snap); err != nil {
		gz.Close()
		f.Close()
		t.Fatalf("encode snapshot: %v", err)
	}
	if err := gz.Close(); err != nil {
		f.Close()
		t.Fatalf("close gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
}

// readGzipSnapshot reads and decodes a gzip JSON snapshot from path.
func readGzipSnapshot(t *testing.T, path string) startupSnapshot {
	t.Helper()
	snap, err := readSnapshot(path)
	if err != nil {
		t.Fatalf("readSnapshot(%s): %v", path, err)
	}
	return *snap
}

// fileBytes returns the raw bytes of a file.
func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %s: %v", path, err)
	}
	return b
}

// fakeHash32 builds a deterministic 32-byte hash from a seed byte.
func fakeHash32(seed byte) []byte {
	h := make([]byte, 32)
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// ---- patchSnapshot tests ----------------------------------------------------

func TestPatchSnapshot_NormalUpdate(t *testing.T) {
	dir := t.TempDir()
	height := uint64(1000)
	oldHash := fmt.Sprintf("%x", fakeHash32(0x01))
	newHash := fmt.Sprintf("%x", fakeHash32(0x02))

	path := snapshotPath(dir, height)
	writeGzipSnapshot(t, path, startupSnapshot{
		Version:   snapVersion,
		TipHeight: height,
		TipHashHex: oldHash,
	})

	found, err := patchSnapshot(path, "primary", newHash, false)
	if err != nil {
		t.Fatalf("patchSnapshot error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}

	got := readGzipSnapshot(t, path)
	if got.TipHashHex != newHash {
		t.Errorf("tip_hash after patch: got %s, want %s", got.TipHashHex, newHash)
	}
}

func TestPatchSnapshot_AlreadyCorrect(t *testing.T) {
	dir := t.TempDir()
	height := uint64(1000)
	hash := fmt.Sprintf("%x", fakeHash32(0x01))

	path := snapshotPath(dir, height)
	writeGzipSnapshot(t, path, startupSnapshot{
		Version:    snapVersion,
		TipHeight:  height,
		TipHashHex: hash,
	})

	before := fileBytes(t, path)

	found, err := patchSnapshot(path, "primary", hash, false)
	if err != nil {
		t.Fatalf("patchSnapshot error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}

	after := fileBytes(t, path)
	if string(before) != string(after) {
		t.Error("file was modified even though tip_hash was already correct")
	}
}

func TestPatchSnapshot_DryRunDoesNotModify(t *testing.T) {
	dir := t.TempDir()
	height := uint64(1000)
	oldHash := fmt.Sprintf("%x", fakeHash32(0x01))
	newHash := fmt.Sprintf("%x", fakeHash32(0x02))

	path := snapshotPath(dir, height)
	writeGzipSnapshot(t, path, startupSnapshot{
		Version:    snapVersion,
		TipHeight:  height,
		TipHashHex: oldHash,
	})

	before := fileBytes(t, path)

	found, err := patchSnapshot(path, "primary", newHash, true /* dryRun */)
	if err != nil {
		t.Fatalf("patchSnapshot dry-run error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true in dry-run")
	}

	after := fileBytes(t, path)
	if string(before) != string(after) {
		t.Error("dry-run modified the snapshot file — it must not write anything")
	}
}

func TestPatchSnapshot_NotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot-v2-9999.json.gz")

	found, err := patchSnapshot(path, "primary", "abcd", false)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if found {
		t.Error("expected found=false for non-existent file")
	}
}

// TestPatchSnapshot_MalformedHash verifies that a snapshot whose tip_hash
// field is shorter than 16 characters does not cause patchSnapshot to panic.
// (safePrefix is used in log lines that would otherwise slice past the end.)
func TestPatchSnapshot_MalformedTipHash(t *testing.T) {
	dir := t.TempDir()
	height := uint64(1000)
	shortHash := "ab" // much shorter than 16 chars
	newHash := fmt.Sprintf("%x", fakeHash32(0x02))

	path := snapshotPath(dir, height)
	writeGzipSnapshot(t, path, startupSnapshot{
		Version:    snapVersion,
		TipHeight:  height,
		TipHashHex: shortHash,
	})

	// Must not panic even though shortHash is only 2 characters.
	found, err := patchSnapshot(path, "primary", newHash, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	got := readGzipSnapshot(t, path)
	if got.TipHashHex != newHash {
		t.Errorf("tip_hash after patch: got %s, want %s", got.TipHashHex, newHash)
	}
}

// TestPatchSnapshot_MalformedTipHash_DryRun verifies no panic in dry-run mode
// either, where the old hash is still logged via safePrefix.
func TestPatchSnapshot_MalformedTipHash_DryRun(t *testing.T) {
	dir := t.TempDir()
	height := uint64(1000)
	shortHash := "x" // single character
	newHash := fmt.Sprintf("%x", fakeHash32(0x02))

	path := snapshotPath(dir, height)
	writeGzipSnapshot(t, path, startupSnapshot{
		Version:    snapVersion,
		TipHeight:  height,
		TipHashHex: shortHash,
	})

	before := fileBytes(t, path)

	// Must not panic and must not modify the file.
	found, err := patchSnapshot(path, "primary", newHash, true /* dryRun */)
	if err != nil {
		t.Fatalf("unexpected error in dry-run: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}

	after := fileBytes(t, path)
	if string(before) != string(after) {
		t.Error("dry-run modified the snapshot file")
	}
}

// ---- safePrefix tests -------------------------------------------------------

func TestSafePrefix(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"abcdefghijklmnop", 16, "abcdefghijklmnop"},
		{"abcdefghijklmnop", 8, "abcdefgh"},
		{"short", 16, "short"},
		{"", 16, ""},
		{"abc", 0, ""},
	}
	for _, c := range cases {
		got := safePrefix(c.s, c.n)
		if got != c.want {
			t.Errorf("safePrefix(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

// ---- snapshot path helpers --------------------------------------------------

func TestSnapshotPaths(t *testing.T) {
	primary := snapshotPath("/data", 1000)
	want := "/data/snapshot-v2-1000.json.gz"
	if primary != want {
		t.Errorf("snapshotPath: got %s, want %s", primary, want)
	}

	prev := snapshotPrevPath(primary)
	wantPrev := "/data/snapshot-v2-1000-prev.json.gz"
	if prev != wantPrev {
		t.Errorf("snapshotPrevPath: got %s, want %s", prev, wantPrev)
	}
}

// ---- integration: both primary and prev updated atomically ------------------

func TestPatchBothPrimaryAndPrev(t *testing.T) {
	dir := t.TempDir()
	height := uint64(2000)
	oldHash := fmt.Sprintf("%x", fakeHash32(0x10))
	newHash := fmt.Sprintf("%x", fakeHash32(0x20))

	primaryPath := snapshotPath(dir, height)
	prevPath := snapshotPrevPath(primaryPath)

	// Write both files with the old hash.
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  height,
		TipHashHex: oldHash,
	}
	writeGzipSnapshot(t, primaryPath, snap)
	writeGzipSnapshot(t, prevPath, snap)

	// Patch both.
	foundP, errP := patchSnapshot(primaryPath, "primary", newHash, false)
	foundV, errV := patchSnapshot(prevPath, "prev-backup", newHash, false)

	if errP != nil {
		t.Fatalf("primary patch error: %v", errP)
	}
	if errV != nil {
		t.Fatalf("prev-backup patch error: %v", errV)
	}
	if !foundP || !foundV {
		t.Fatalf("both files should be found: primary=%v prev=%v", foundP, foundV)
	}

	gotP := readGzipSnapshot(t, primaryPath)
	gotV := readGzipSnapshot(t, prevPath)

	if gotP.TipHashHex != newHash {
		t.Errorf("primary tip_hash: got %s, want %s", gotP.TipHashHex, newHash)
	}
	if gotV.TipHashHex != newHash {
		t.Errorf("prev tip_hash: got %s, want %s", gotV.TipHashHex, newHash)
	}
}

// TestDryRunLeavesFilesBytIdentical patches both files in dry-run mode and
// verifies the raw file bytes are completely unchanged.
func TestDryRunLeavesBytesIdentical(t *testing.T) {
	dir := t.TempDir()
	height := uint64(3000)
	oldHash := fmt.Sprintf("%x", fakeHash32(0x30))
	newHash := fmt.Sprintf("%x", fakeHash32(0x40))

	primaryPath := snapshotPath(dir, height)
	prevPath := snapshotPrevPath(primaryPath)

	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  height,
		TipHashHex: oldHash,
	}
	writeGzipSnapshot(t, primaryPath, snap)
	writeGzipSnapshot(t, prevPath, snap)

	beforeP := fileBytes(t, primaryPath)
	beforeV := fileBytes(t, prevPath)

	patchSnapshot(primaryPath, "primary", newHash, true)
	patchSnapshot(prevPath, "prev-backup", newHash, true)

	afterP := fileBytes(t, primaryPath)
	afterV := fileBytes(t, prevPath)

	if string(beforeP) != string(afterP) {
		t.Error("dry-run modified primary snapshot bytes")
	}
	if string(beforeV) != string(afterV) {
		t.Error("dry-run modified prev-backup snapshot bytes")
	}
}
