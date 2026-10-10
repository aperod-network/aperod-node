package main

// startupLoadMempool performs the two-step mempool startup sequence that
// main.go must execute in this exact order:
//
//  1. core.CleanStaleTmpFiles — removes any stale mempool.json.tmp left by a
//     previous OOM-kill crash (must run BEFORE Load so the final path is clean).
//  2. pool.Load — restores transactions that were pending when the node last
//     stopped (must run AFTER SetVerifier so full RingCT re-verification is
//     active; invalid or double-spent entries are silently dropped).
//
// Keeping both calls in this single function means a regression that removes
// either step is caught by the integration tests in mempool_tmp_cleanup_test.go.
// Do not inline these calls back into run() — preserve the seam.
//
// Returns the number of transactions successfully restored.

import (
	"log/slog"

	"github.com/aperod/aperod/core"
)

func startupLoadMempool(dataDir string, pool *core.Mempool, log *slog.Logger) int {
	// Step 1: remove any stale .tmp left by a previous OOM-kill crash.
	// Must run BEFORE Load so the final path is clean when Load reads it.
	core.CleanStaleTmpFiles(dataDir, log)

	// Step 2: restore pending transactions.
	// Must run AFTER SetVerifier so full RingCT re-verification is active.
	return pool.Load(dataDir, log)
}
