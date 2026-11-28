package checkedio

import (
	"io"
	"log/slog"
)

// LogClose records a cleanup error without changing an already-decided result.
// Use it for response bodies or post-effect teardown where returning a new
// failure could invite replay of an action that already succeeded. Durable
// writers must instead be closed and checked before their publication point.
func LogClose(log *slog.Logger, resource io.Closer, operation string) {
	if err := resource.Close(); err != nil {
		log.Warn("resource cleanup failed", "operation", operation, "err", err)
	}
}
