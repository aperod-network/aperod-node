package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type ownedSnapshotReader struct {
	io.Reader
	closeErr error
	closes   int
}

func (r *ownedSnapshotReader) Close() error {
	r.closes++
	return r.closeErr
}

func TestSnapshotCopy_SourceCloseFailureNeverPublishes(t *testing.T) {
	for _, failClose := range []bool{false, true} {
		name := "positive"
		if failClose {
			name = "source_close_failure"
		}
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "snapshot-prev.gz")
			if err := os.WriteFile(dst, []byte("old recovery floor"), 0600); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected snapshot source close failure")
			reader := &ownedSnapshotReader{Reader: strings.NewReader("new backup")}
			if failClose {
				reader.closeErr = injected
			}
			err := copySnapshotFromReader(reader, dst)
			if reader.closes != 1 || (failClose && !errors.Is(err, injected)) || (!failClose && err != nil) {
				t.Fatalf("copy close evidence changed: closes=%d err=%v", reader.closes, err)
			}
			body, readErr := os.ReadFile(dst)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := "new backup"
			if failClose {
				want = "old recovery floor"
			}
			if string(body) != want {
				t.Fatalf("published %q, want %q", body, want)
			}
			if _, statErr := os.Stat(dst + ".tmp"); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("temporary copy was not removed: %v", statErr)
			}
		})
	}
}

func TestSnapshotCopy_OutputCreationFailureStillClosesSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "snapshot-prev.gz")
	if err := os.Mkdir(dst+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected source close failure")
	reader := &ownedSnapshotReader{Reader: strings.NewReader("new backup"), closeErr: injected}
	err := copySnapshotFromReader(reader, dst)
	var pathErr *os.PathError
	if reader.closes != 1 || !errors.Is(err, injected) || !errors.As(err, &pathErr) {
		t.Fatalf("creation or cleanup error was lost: closes=%d err=%v", reader.closes, err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed copy published a destination: %v", statErr)
	}
}
