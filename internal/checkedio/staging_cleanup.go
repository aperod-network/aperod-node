package checkedio

import (
	"errors"
	"log/slog"
	"os"
)

// LogRemoveStaging removes a caller-owned temporary file without changing an
// already-decided publication result. A successful rename consumes this path,
// so absence is expected. Other failures are reported, not silently discarded.
// This is not suitable for required security deletion or recursive cleanup.
func LogRemoveStaging(log *slog.Logger, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("temporary file cleanup failed", "path", path, "err", err)
	}
}
