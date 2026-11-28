package main

import (
	"fmt"
	"io"
	"log/slog"
)

// writeMaintenanceReport is for an already-started, idempotent repair. Broken
// terminal output must neither cancel storage work mid-sweep nor claim it was
// rolled back. Preflight output is checked and returned before mutations start.
func writeMaintenanceReport(out io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(out, format, args...); err != nil {
		slog.Warn("maintenance progress or result output failed; inspect stored state before retrying", "err", err)
	}
}
