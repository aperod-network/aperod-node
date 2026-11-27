package checkedio

import (
	"errors"
	"fmt"
	"os"
)

// OpenRootedRegular opens an existing regular file without accepting a symlink
// or a different inode substituted after inspection. root confines resolution
// of intermediate components; the caller retains ownership of root.
func OpenRootedRegular(root *os.Root, name string) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("regular non-symlink file required: %q", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.Join(fmt.Errorf("file changed during open: %q", name), file.Close())
	}
	return file, nil
}
