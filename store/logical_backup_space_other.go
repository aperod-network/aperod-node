//go:build !linux

package store

import "fmt"

func availableDiskBytes(path string) (uint64, error) {
	return 0, fmt.Errorf("filesystem free-space checks are unsupported on this platform (path %q)", path)
}
