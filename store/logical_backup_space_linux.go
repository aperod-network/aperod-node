//go:build linux

package store

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func availableDiskBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 {
		return 0, fmt.Errorf("filesystem reported invalid block size %d", stat.Bsize)
	}
	blockSize := uint64(stat.Bsize)
	if stat.Bavail > math.MaxUint64/blockSize {
		return 0, fmt.Errorf("filesystem available-byte count overflows")
	}
	return stat.Bavail * blockSize, nil
}
