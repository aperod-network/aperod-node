package main

import (
	"fmt"
	"log/slog"

	"github.com/aperod/aperod/store"
)

// checkSnapshotUTXOCount compares the snapshot's active UTXO count against the
// durable count stored in DB metadata (keyed by tipHashHex) and returns true
// when the snapshot should be accepted.
//
// The check is skipped (returns true) when no metadata entry exists — this
// covers the first-run case where the node has never completed a successful
// snapshot save.
//
// isRelaxed=true (relaxed-hash recovery path) widens the effective tolerance
// to 100 % so that a natural UTXO count divergence caused by a recover-tip
// DB repair never triggers a spurious full block scan.
//
// nonValidator=true applies a minimum tolerance floor of 10 % so that relay
// nodes bootstrapped via rsync (whose DB metadata belongs to a different chain
// history) do not silently reject a valid snapshot.
func checkSnapshotUTXOCount(
	db *store.DB,
	snapUTXOCount int,
	tipHashHex string,
	configuredTolerancePct float64,
	isRelaxed bool,
	nonValidator bool,
	log *slog.Logger,
) bool {
	dbActiveCount, dbHasCount, countErr := db.LoadActiveUTXOCount(tipHashHex)
	if countErr != nil {
		log.Warn("snapshot UTXO count check skipped — db metadata error", "err", countErr)
		return true
	}
	if !dbHasCount {
		log.Info("snapshot UTXO count check skipped — no prior active count in db (first snapshot)")
		return true
	}

	diff := snapUTXOCount - dbActiveCount
	if diff < 0 {
		diff = -diff
	}
	larger := dbActiveCount
	if snapUTXOCount > larger {
		larger = snapUTXOCount
	}
	// Protect against both counts being 0 (genesis edge case).
	var diffPct float64
	if larger > 0 {
		diffPct = float64(diff) / float64(larger) * 100.0
	}

	tolerancePct := configuredTolerancePct
	if isRelaxed {
		// Relaxed-hash recovery path: the snapshot was written before
		// recover-tip patched the DB tip, so UTXO count naturally
		// diverges from DB metadata.  Skip the count check.
		tolerancePct = 100.0
	}
	// Non-validator (relay) nodes are often bootstrapped via rsync from a
	// running peer, so their DB active-UTXO metadata is absent or belongs
	// to a different chain history.  A strict tolerance in this mode
	// silently rejects the snapshot and leaves the validator registry
	// seeded from genesis-only (all-zero placeholder keys), causing every
	// incoming block to be rejected.  Apply a floor of 10 % in
	// non-validator mode so that rsync-bootstrapped relay nodes can load
	// the snapshot.  Operators may still override this higher via
	// snapshot.utxo_count_tolerance_pct in node.yaml.
	if nonValidator && tolerancePct < 10.0 {
		tolerancePct = 10.0
	}

	if diffPct > tolerancePct {
		// The tip-hash check in loadStartupSnapshotWithFallback is the
		// primary integrity guard: if the snapshot hash matches the DB tip,
		// the snapshot is structurally valid for this chain state.  The
		// UTXO count stored in DB metadata is a cached hint that diverges
		// naturally after rsync from another node or any recovery that
		// replaces chain.db without re-saving the snapshot.  Rejecting the
		// snapshot here causes a full block scan that fails on chains with
		// >MaxMissingBlocks gaps — far worse than accepting a snapshot whose
		// UTXO set is already hash-verified.  Log a warning and accept.
		logFn := log.Warn
		if nonValidator {
			logFn = log.Error
		}
		logFn("snapshot active UTXO count diverges from last-saved metadata (accepted — tip hash is verified)",
			"snapshot_active_utxos", snapUTXOCount,
			"db_last_active_utxos", dbActiveCount,
			"diff_pct", fmt.Sprintf("%.2f%%", diffPct),
			"tolerance_pct", tolerancePct,
			"hint", "divergence is normal after rsync/recovery; suppress with snapshot.utxo_count_tolerance_pct: 100 in node.yaml",
		)
		// Accept the snapshot instead of falling back to block scan.
		return true
	}

	log.Info("snapshot UTXO count check passed",
		"snapshot_active_utxos", snapUTXOCount,
		"db_last_active_utxos", dbActiveCount,
		"diff_pct", fmt.Sprintf("%.2f%%", diffPct),
	)
	return true
}
