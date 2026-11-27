package checkedio

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRootedRegular_ConfinesFilesAndRejectsLinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close root: %v", err)
		}
	})
	file, err := OpenRootedRegular(root, "data")
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(data) != "fixture" {
		t.Fatalf("positive regular-file control: %q %v %v", data, readErr, closeErr)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "data"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"same-root-link": "data",
		"outside-link":   filepath.Join(outside, "data"),
		"outside-dir":    outside,
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"same-root-link", "outside-link", "outside-dir/data", "../escape", ".", "missing"} {
		if unexpected, err := OpenRootedRegular(root, name); err == nil {
			if err := unexpected.Close(); err != nil {
				t.Errorf("close unexpected file: %v", err)
			}
			t.Errorf("unsafe/nonexistent regular file was accepted: %s", name)
		}
	}
}
