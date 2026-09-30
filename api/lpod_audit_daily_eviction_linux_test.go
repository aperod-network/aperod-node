//go:build linux

package api

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

// Advisory eviction of the closed synthetic fixture only, never global caches.
// Successful fadvise does not prove reads reached physical storage.
func evictDailyBenchmarkFiles(path string) (int, int64, error) {
	var files int
	var bytes int64
	err := filepath.WalkDir(path, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		f, err := os.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
			return err
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes, err
}

func TestDailyBenchmarkEvictionPreservesFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture")
	want := []byte("synthetic fixture bytes")
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	count, size, err := evictDailyBenchmarkFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) || count != 1 || size != int64(len(want)) {
		t.Fatalf("eviction changed fixture: count=%d size=%d read=%v", count, size, err)
	}
}
