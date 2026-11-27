package avm

import (
	"context"
	"log/slog"
)

type runtimeCloser interface {
	Close(context.Context) error
}

// Runtime/cache cleanup is local housekeeping, not a consensus result.
// Report failures without changing computed effects or retrying execution.
// Durable state-write errors must still propagate through the commit path.
func closeRuntimeResource(ctx context.Context, resource runtimeCloser, kind string, logger *slog.Logger) {
	if err := resource.Close(ctx); err != nil {
		logger.Error("AVM runtime cleanup failed", "resource", kind, "err", err)
	}
}
