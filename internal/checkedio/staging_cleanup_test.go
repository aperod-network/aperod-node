package checkedio

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestLogRemoveStaging_ConsumesOnlyOwnedPathAndReportsFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owned.tmp")
	if err := os.WriteFile(path, []byte("temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	log := slog.New(slog.NewTextHandler(&output, nil))
	LogRemoveStaging(log, path)
	LogRemoveStaging(log, path) // A rename or earlier cleanup may already consume it.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || output.Len() != 0 {
		t.Fatalf("normal cleanup reported failure or left staging behind: %v %s", err, output.String())
	}
	notAFile := filepath.Join(dir, "not-a-temporary-file")
	if err := os.Mkdir(notAFile, 0700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(notAFile, "preserve")
	if err := os.WriteFile(child, []byte("do not recursively delete"), 0600); err != nil {
		t.Fatal(err)
	}
	LogRemoveStaging(log, notAFile)
	if output.Len() == 0 {
		t.Fatal("cleanup failure was silently lost")
	}
	if body, err := os.ReadFile(child); err != nil || string(body) != "do not recursively delete" {
		t.Fatalf("cleanup traversed a directory: %q %v", body, err)
	}
}
