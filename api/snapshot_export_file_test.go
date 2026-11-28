package api

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotExport_RejectsSnapshotNamedLinks(t *testing.T) {
	dir := t.TempDir()
	name := "snapshot-v2-1.json.gz"
	want := []byte("synthetic snapshot bytes")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openSnapshotForExport(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	actual, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(actual, want) {
		t.Fatalf("regular snapshot control failed: %q %v %v", actual, readErr, closeErr)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		filepath.Join(dir, "private-fixture"),
		filepath.Join(t.TempDir(), "outside-fixture"),
	} {
		if err := os.WriteFile(target, []byte("must not be exported"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if unexpected, err := openSnapshotForExport(dir, name); err == nil {
			if closeErr := unexpected.Close(); closeErr != nil {
				t.Errorf("close unexpected file: %v", closeErr)
			}
			t.Fatal("snapshot-shaped symlink was accepted")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}
