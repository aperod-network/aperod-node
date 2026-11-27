package api

import (
	"errors"
	"fmt"
	"os"
)

// openSnapshotForExport refuses links and pins enumeration metadata to the
// opened descriptor. A snapshot-shaped symlink must not export another file
// (including a key within dataDir); os.Root also prevents escapes from the root.
func openSnapshotForExport(dataDir, name string) (*os.File, error) {
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, err
	}
	observed, err := root.Lstat(name)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	if !observed.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("snapshot export requires a regular non-symlink file"), root.Close())
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	opened, statErr := file.Stat()
	if statErr != nil {
		return nil, errors.Join(statErr, file.Close(), root.Close())
	}
	if !opened.Mode().IsRegular() || !os.SameFile(observed, opened) {
		return nil, errors.Join(fmt.Errorf("snapshot changed while opening export"), file.Close(), root.Close())
	}
	if err := root.Close(); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
