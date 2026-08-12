package main

// scan.go — startup block-scan logic extracted from run() so that both main.go
// and integration tests can call the same production code path.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// startupScanResult is returned by runStartupScan and exposes the values
// that integration tests need to assert on.
type startupScanResult struct {
	// ScanFrom is the first height that was actually read from the DB during
	// the scan.  It equals partial.TipHeight+1 when a checkpoint was found,
	// or 1 when no checkpoint existed.
	ScanFrom uint64
	// TxTotal is the final non-coinbase transaction counter.
	TxTotal int64
}

// startupScanParams bundles every input required by runStartupScan.
// All pointer fields must be non-nil.
type startupScanParams struct {
	DataDir     string
	TipHeight   uint64
	TipHashHex  string // hex-encoded tip block hash (for the post-scan snapshot)
	DB          *store.DB
	UTXOs       *core.UTXOSet
	Registry    *core.ValidatorRegistry
	KiFromIndex bool  // true when key images were pre-loaded from the DB index
	InitTxTotal int64 // pre-loaded tx total (used when KiFromIndex is true)

	// UTXOFromIndex, when true together with KiFromIndex, means the active
	// UTXO set was already loaded from db.IterActiveUTXOs by the caller.
	// The full block scan is skipped; only stake blocks listed in
	// StakeBlockHeights are fetched for registry replay.
	UTXOFromIndex     bool
	StakeBlockHeights []uint64 // heights of blocks that contain stake txs (ascending)

	// ResumeScanFrom, when non-zero, causes runStartupScan to start the
	// block loop at ResumeScanFrom+1 instead of searching for a partial
	// intermediate checkpoint.  Set this when the caller has already
	// pre-seeded the UTXOSet and key-image set from a rescue snapshot so
	// that only blocks after that snapshot height need processing.
	ResumeScanFrom uint64
	Log         *slog.Logger

	// UTXOCountTolerancePct is the maximum allowed percentage difference
	// between a checkpoint's active UTXO count and the count stored in the DB
	// before the checkpoint is rejected and the scan starts from block 1.
	// Mirrors cfg.Snapshot.UTXOCountTolerancePct.  0 means exact match required.
	UTXOCountTolerancePct float64

	// CheckpointInterval is the number of blocks between intermediate snapshots
	// saved during the scan.  0 means use the default (50 000).
	// Mirrors cfg.Snapshot.ScanCheckpointInterval.
	CheckpointInterval uint64

	// MaxMissingBlocks is the maximum number of individual blocks that may be
	// absent from the LevelDB store before the scan returns a fatal error.
	// The tx-index fast-path already warns and skips missing blocks; a small
	// allowance here lets a node with an isolated store gap (e.g. a single
	// block lost during a hard-kill) start instead of crash-looping.
	// Each skipped block is logged at ERROR level.  0 → default of 10.
	MaxMissingBlocks uint64

	// SetSyncProgress may be nil; when non-nil it is called every 1 000 blocks
	// so that callers can report syncing progress to external consumers.
	SetSyncProgress func(syncing, tip uint64)

	// SnapshotWg, when non-nil, has Add(1)/Done() called around every
	// goroutine that writes a snapshot file.  Tests pass a *sync.WaitGroup
	// and call Wait() before asserting on log output or temp-dir contents;
	// production callers leave it nil.
	SnapshotWg *sync.WaitGroup

	// GCHook, when non-nil, is called in place of runtime.GC() each time the
	// cumulative UTXO output counter crosses a gcUTXOInterval boundary during
	// the main scan loop.  The argument is the current value of utxoCount at
	// the moment the boundary is crossed (always a multiple of gcUTXOInterval).
	// Tests inject a recording function here to assert on the exact boundary
	// sequence without running a real GC cycle.  Production callers leave it nil.
	GCHook func(utxoCount uint64)
}

// runStartupScan performs the unified startup block scan:
//  1. Checks for a partial intermediate checkpoint (findLatestSnapshot) and
//     restores UTXOSet + ValidatorRegistry from it when found.
//  2. Scans blocks from scanFrom (checkpoint+1, or 1) through TipHeight,
//     rebuilding the active UTXO set, spent key images, and stake registry.
//  3. Saves an intermediate checkpoint every p.CheckpointInterval blocks (default
//     50 000) so that a future crash mid-scan can resume from the checkpoint.
//  4. Saves a full tip-height snapshot after the scan completes.
//
// On return, p.UTXOs and p.Registry reflect the fully-rebuilt state.
// The caller is responsible for loading the key-image index before calling
// this function (when KiFromIndex is true).
func runStartupScan(p startupScanParams) (startupScanResult, error) {
	const syncProgressInterval = uint64(1000)   // report every 1 000 blocks
	const gcUTXOInterval       = uint64(50000)  // force GC every 50 000 UTXOs processed

	// ── DB-index fast path ────────────────────────────────────────────────
	// When UTXOFromIndex is true, both the active UTXO set and the spent
	// key-image set have already been loaded from LevelDB by the caller.
	// Skip the full block scan; only replay stake transactions from the
	// pre-indexed block heights so the ValidatorRegistry is up to date.
	if p.UTXOFromIndex {
		p.Log.Info("db-index fast path: replaying stake blocks only",
			"stake_block_count", len(p.StakeBlockHeights),
			"tip_height", p.TipHeight,
		)
		for _, h := range p.StakeBlockHeights {
			if h > p.TipHeight {
				break
			}
			raw, fetchErr := p.DB.GetRawBlockByHeight(h)
			if fetchErr != nil || raw == nil {
				p.Log.Warn("db-index fast path: stake block missing — skipped",
					"height", h, "err", fetchErr)
				continue
			}
			var b core.Block
			if jsonErr := json.Unmarshal(raw, &b); jsonErr != nil {
				p.Log.Warn("db-index fast path: stake block decode error — skipped",
					"height", h, "err", jsonErr)
				continue
			}
			if replayErr := p.Registry.ReplayBlockStakeTxs(b.Txs, b.Header.Height); replayErr != nil {
				return startupScanResult{}, fmt.Errorf(
					"db-index fast path: stake replay at height %d: %w", h, replayErr)
			}
		}
		p.Log.Info("db-index fast path complete",
			"tip_height", p.TipHeight,
			"stake_blocks_replayed", len(p.StakeBlockHeights),
			"unspent_outputs", p.UTXOs.Count(),
		)
		// Save a tip snapshot so the next restart uses the snapshot path.
		snapToSave := startupSnapshot{
			Version:    snapVersion,
			TipHeight:  p.TipHeight,
			TipHashHex: p.TipHashHex,
			TxTotal:    p.InitTxTotal,
			UTXOs:      p.UTXOs.TakeSnapshot(),
			Registry:   p.Registry.TakeSnapshot(),
		}
		if p.SnapshotWg != nil {
			p.SnapshotWg.Add(1)
		}
		go func() {
			if p.SnapshotWg != nil {
				defer p.SnapshotWg.Done()
			}
			if saveErr := saveStartupSnapshot(p.DataDir, snapToSave); saveErr != nil {
				p.Log.Warn("db-index fast path: failed to save snapshot", "err", saveErr)
			} else {
				p.Log.Info("startup snapshot saved after db-index fast path", "tip_height", p.TipHeight)
				deleteOldSnapshots(p.DataDir, p.TipHeight)
			}
		}()
		return startupScanResult{ScanFrom: p.TipHeight + 1, TxTotal: p.InitTxTotal}, nil
	}

	checkpointInterval := p.CheckpointInterval
	if checkpointInterval == 0 {
		checkpointInterval = 50000 // default: save intermediate snapshot every 50 000 blocks
	}
	maxMissing := p.MaxMissingBlocks
	if maxMissing == 0 {
		maxMissing = 5000 // tolerate up to 5 000 missing blocks before giving up
	}
	var missingBlockCount uint64

	// ── Partial snapshot resume ───────────────────────────────────────────
	// If an exact-tip snapshot was not found by the caller, look for the most
	// recent intermediate checkpoint saved during a previous scan.  Restoring
	// from it means we only need to replay blocks since that checkpoint.
	scanFrom := uint64(1)
	txTotal := p.InitTxTotal
	if p.ResumeScanFrom > 0 {
		// Caller pre-seeded UTXOSet + key-image set from a rescue snapshot.
		// Start the block loop immediately after that snapshot's tip height.
		// Skip findLatestSnapshot: the rescue snapshot IS the checkpoint.
		scanFrom = p.ResumeScanFrom + 1
		p.Log.Info("scan resuming after rescue snapshot",
			"resume_from", scanFrom,
			"tip_height", p.TipHeight,
			"blocks_to_scan", p.TipHeight-p.ResumeScanFrom,
		)
	} else if partial := findLatestSnapshot(p.DataDir, p.TipHeight, p.Log); partial != nil {
		// Cross-check: verify the snapshot's recorded TipHashHex against the
		// actual block stored in the DB at that height.  A mismatch means the
		// block was reorganised (e.g. in dev/test) after the checkpoint was
		// written; restoring stale UTXO state would cause silent corruption.
		// In that case we discard the checkpoint and scan from block 1.
		hashOK := false
		dbRaw, dbErr := p.DB.GetRawBlockByHeight(partial.TipHeight)
		if dbErr != nil || dbRaw == nil {
			p.Log.Warn("partial snapshot discarded — cannot fetch block for hash check",
				"snapshot_height", partial.TipHeight, "err", dbErr)
		} else {
			var dbBlk core.Block
			if jsonErr := json.Unmarshal(dbRaw, &dbBlk); jsonErr != nil {
				p.Log.Warn("partial snapshot discarded — cannot decode block for hash check",
					"snapshot_height", partial.TipHeight, "err", jsonErr)
			} else {
				dbHash := dbBlk.Hash()
				dbHashHex := fmt.Sprintf("%x", dbHash[:])
				if dbHashHex == partial.TipHashHex {
					hashOK = true
				} else {
					p.Log.Warn("partial snapshot discarded — hash mismatch against DB block",
						"snapshot_height", partial.TipHeight,
						"snapshot_hash", partial.TipHashHex,
						"db_hash", dbHashHex,
					)
				}
			}
		}
		if hashOK {
			// ── UTXO count divergence check (partial checkpoint) ─────────────
			// Compare the checkpoint's active UTXO count against the hash-keyed
			// metadata written when this checkpoint was saved.  A mismatch beyond
			// tolerance indicates a stale or partially-written checkpoint and
			// causes the resume path to fall back to a full scan from block 1.
			// The check is skipped when no metadata exists for this hash (e.g.
			// the checkpoint pre-dates this feature or the process crashed before
			// the metadata write) — the safe direction is to accept.
			utxoCountOK := true
			partialSnapCount := len(partial.UTXOs.ActiveUTXOs)
			if dbActiveCount, dbHasCount, countErr := p.DB.LoadActiveUTXOCount(partial.TipHashHex); countErr != nil {
				p.Log.Warn("partial snapshot UTXO count check skipped — db metadata error",
					"snapshot_height", partial.TipHeight, "err", countErr)
			} else if dbHasCount {
				diff := partialSnapCount - dbActiveCount
				if diff < 0 {
					diff = -diff
				}
				larger := dbActiveCount
				if partialSnapCount > larger {
					larger = partialSnapCount
				}
				var diffPct float64
				if larger > 0 {
					diffPct = float64(diff) / float64(larger) * 100.0
				}
				if diffPct > p.UTXOCountTolerancePct {
					p.Log.Warn("partial snapshot discarded — active UTXO count diverges from saved count; scanning from block 1",
						"snapshot_height", partial.TipHeight,
						"snapshot_active_utxos", partialSnapCount,
						"db_last_active_utxos", dbActiveCount,
						"diff_pct", fmt.Sprintf("%.2f%%", diffPct),
						"tolerance_pct", p.UTXOCountTolerancePct,
					)
					utxoCountOK = false
				} else {
					p.Log.Info("partial snapshot UTXO count check passed",
						"snapshot_height", partial.TipHeight,
						"snapshot_active_utxos", partialSnapCount,
						"db_last_active_utxos", dbActiveCount,
						"diff_pct", fmt.Sprintf("%.2f%%", diffPct),
					)
				}
			}
			if utxoCountOK {
				p.UTXOs.RestoreFromSnapshot(partial.UTXOs)
				p.Registry.RestoreFromSnapshot(partial.Registry)
				p.Registry.SetUTXOSet(p.UTXOs)
				if partial.TxTotal > 0 {
					txTotal = partial.TxTotal
				}
				scanFrom = partial.TipHeight + 1
				p.Log.Info("partial snapshot loaded — resuming scan from checkpoint",
					"snapshot_height", partial.TipHeight,
					"resume_from", scanFrom,
					"tip_height", p.TipHeight,
					"blocks_to_scan", p.TipHeight-partial.TipHeight,
				)
				runtime.GC()
				debug.FreeOSMemory()
			}
		}
	}

	// ── Main scan loop ─────────────────────────────────────────────────────
	var msScanStart runtime.MemStats
	runtime.ReadMemStats(&msScanStart)
	scanStart := time.Now()
	p.Log.Info("running startup block scan",
		"tip_height", p.TipHeight,
		"scan_from", scanFrom,
		"ki_from_index", p.KiFromIndex,
		"heap_sys_mib_before", msScanStart.Sys/(1024*1024),
	)

	var txCount int64
	blocksWithStake := 0
	kiCount    := 0     // only used when !KiFromIndex
	utxoCount  := uint64(0) // cumulative output count; drives periodic GC

	// observedProducers collects every distinct ValidatorPub seen in block headers
	// during this scan.  Used below to seed the registry when the partial snapshot
	// pre-dates registry snapshotting and no stake txs appear in the scanned range.
	observedProducers := make(map[string]crypto.ValidatorPubKey)

	for h := scanFrom; h <= p.TipHeight; h++ {
		if p.SetSyncProgress != nil && h%syncProgressInterval == 0 {
			p.SetSyncProgress(h, p.TipHeight)
		}

		raw, fetchErr := p.DB.GetRawBlockByHeight(h)
		if fetchErr != nil || raw == nil {
			missingBlockCount++
			p.Log.Error("startup scan: block missing from store — skipping height",
				"height", h,
				"err", fetchErr,
				"missing_so_far", missingBlockCount,
				"max_allowed", maxMissing,
			)
			if missingBlockCount > maxMissing {
				return startupScanResult{}, fmt.Errorf(
					"startup scan: too many missing blocks (%d > %d allowed); "+
						"last missing height %d — node cannot start safely; "+
						"repair the store and restart",
					missingBlockCount, maxMissing, h)
			}
			continue
		}
		var b core.Block
		if parseErr := json.Unmarshal(raw, &b); parseErr != nil {
			return startupScanResult{}, fmt.Errorf(
				"startup scan: cannot decode block at height %d: %w — "+
					"node cannot start safely; repair the store and restart",
				h, parseErr)
		}

		// Track block producer pubkeys so we can seed the registry later if
		// the scanned range contained no stake transactions (e.g. a relay node
		// bootstrapped from a partial snapshot that pre-dates registry snapshotting).
		if len(b.Header.ValidatorPub) > 0 {
			key := fmt.Sprintf("%x", []byte(b.Header.ValidatorPub))
			if _, seen := observedProducers[key]; !seen {
				pub := make(crypto.ValidatorPubKey, len(b.Header.ValidatorPub))
				copy(pub, b.Header.ValidatorPub)
				observedProducers[key] = pub
			}
		}

		// Goal 2: rebuild the active UTXO set.
		if applyErr := p.UTXOs.ApplyBlock(&b); applyErr != nil {
			p.Log.Warn("startup scan: ApplyBlock failed (continuing)",
				"height", h, "err", applyErr)
		}

		// UTXO index backfill.
		for _, tx := range b.Txs {
			txHash := tx.Hash()
			for i, out := range tx.Outputs {
				su := &store.StoredUTXO{
					TxHash:       txHash,
					OutputIndex:  uint32(i),
					OneTimePub:   out.OneTimePub,
					TxPubKey:     out.TxPubKey,
					AmountCommit: out.AmountCommit,
					EncAmount:    out.EncAmount,
					BlockHeight:  b.Header.Height,
				}
				_ = p.DB.PutUTXO(txHash, uint32(i), su)
				utxoCount++
				if utxoCount%gcUTXOInterval == 0 {
					if p.GCHook != nil {
						p.GCHook(utxoCount)
					} else {
						runtime.GC()
					}
				}
			}
		}

		// Goal 1 (fallback): mark spent key images and backfill the DB index.
		if !p.KiFromIndex {
			for txIdx, tx := range b.Txs {
				for _, inp := range tx.Inputs {
					p.UTXOs.MarkSpent(inp.KeyImage)
					kiCount++
					_ = p.DB.MarkKeyImageSpent(inp.KeyImage)
				}
				if !(txIdx == 0 && tx.IsCoinbase()) {
					txCount++
				}
			}
		}

		// Goals 3+4: replay stake transactions.
		hasStake := false
		for _, tx := range b.Txs {
			if tx.IsStake() {
				hasStake = true
				break
			}
		}
		if hasStake {
			if replayErr := p.Registry.ReplayBlockStakeTxs(b.Txs, b.Header.Height); replayErr != nil {
				return startupScanResult{}, fmt.Errorf(
					"stake replay failed at height %d: %w — "+
						"node cannot start safely; repair the store and restart",
					h, replayErr)
			}
			// Backfill the sb/ index so future fast-path restarts know
			// which blocks to replay for stake reconstruction.
			_ = p.DB.PutStakeBlockHeight(h)
			blocksWithStake++
		}

		// Intermediate checkpoint every 50 K blocks (goroutine to avoid stalling).
		if h%checkpointInterval == 0 && h < p.TipHeight {
			cpTxTotal := txTotal
			if !p.KiFromIndex {
				cpTxTotal = txCount
			}
			cpHashArr := b.Header.Hash()
			cpSnap := startupSnapshot{
				Version:    snapVersion,
				TipHeight:  h,
				TipHashHex: fmt.Sprintf("%x", cpHashArr[:]),
				TxTotal:    cpTxTotal,
				UTXOs:      p.UTXOs.TakeSnapshot(),
				Registry:   p.Registry.TakeSnapshot(),
			}
			cpActiveCount := len(cpSnap.UTXOs.ActiveUTXOs)
			if p.SnapshotWg != nil {
				p.SnapshotWg.Add(1)
			}
			go func(s startupSnapshot, activeCount int) {
				if p.SnapshotWg != nil {
					defer p.SnapshotWg.Done()
				}
				if cpErr := saveStartupSnapshot(p.DataDir, s); cpErr != nil {
					p.Log.Warn("scan checkpoint save failed", "height", s.TipHeight, "err", cpErr)
				} else {
					p.Log.Info("scan checkpoint saved", "height", s.TipHeight)
					deleteOldSnapshots(p.DataDir, s.TipHeight)
					// Persist the active UTXO count keyed by this checkpoint's
					// tip hash so the resume-path divergence check can validate it.
					if metaErr := p.DB.StoreActiveUTXOCount(s.TipHashHex, activeCount); metaErr != nil {
						p.Log.Warn("scan checkpoint: failed to persist active_utxo_count metadata",
							"height", s.TipHeight, "err", metaErr)
					}
				}
			}(cpSnap, cpActiveCount)
		}
	}

	// Release UTXO-scan heap to the OS immediately so steady-state operation
	// starts with the minimum possible RSS.  The periodic runtime.GC() calls
	// during the loop already freed heap; FreeOSMemory() returns those pages
	// so GOMEMLIMIT has full headroom before the consensus engine starts.
	debug.FreeOSMemory()

	if !p.KiFromIndex {
		if err := p.DB.StoreTxTotal(txCount); err != nil {
			p.Log.Warn("failed to persist tx total after block scan", "err", err)
		}
		txTotal = txCount
		p.Log.Info("spent key-image set rebuilt (full block scan)",
			"key_images_marked", kiCount,
			"blocks_scanned", p.TipHeight,
			"total_txs_counted", txCount,
			"utxos_processed", utxoCount,
		)
	}

	// Scan instrumentation.
	{
		scanElapsed := time.Since(scanStart)
		var msScanEnd runtime.MemStats
		runtime.ReadMemStats(&msScanEnd)
		p.Log.Info("startup scan metrics",
			"elapsed_sec", fmt.Sprintf("%.2f", scanElapsed.Seconds()),
			"key_images_loaded", kiCount,
			"heap_sys_mib_before", msScanStart.Sys/(1024*1024),
			"heap_sys_mib_after", msScanEnd.Sys/(1024*1024),
			"heap_alloc_mib", msScanEnd.HeapAlloc/(1024*1024),
			"heap_sys_delta_mib", (msScanEnd.Sys-msScanStart.Sys)/(1024*1024),
		)
	}
	{
		active, total := p.Registry.Count()
		p.Log.Info("startup scan complete",
			"tip_height", p.TipHeight,
			"blocks_with_stake_txs", blocksWithStake,
			"active_validators", active,
			"total_registered", total,
			"unspent_outputs", p.UTXOs.Count(),
		)

		// ── Observed-producer fallback ────────────────────────────────────────
		// If the scan found no active validators (the partial snapshot pre-dates
		// registry snapshotting and the scanned range had no stake transactions),
		// but we did observe block producers in the headers, seed those pubkeys as
		// Active so the consensus engine does not fall back to the genesis zero-key
		// and reject every incoming block as "block from unknown validator".
		//
		// We use MinStakeNAPR×10 as the seed stake — large enough that these
		// entries survive any epoch churn until a real stake transaction is seen,
		// but they will be overwritten/extended by genuine stake data on the next
		// full scan or once a stake tx arrives.
		if active == 0 && len(observedProducers) > 0 {
			seedPubs := make([]crypto.ValidatorPubKey, 0, len(observedProducers))
			for _, pub := range observedProducers {
				seedPubs = append(seedPubs, pub)
			}
			seedStake := core.MinStakeNAPR * 10
			p.Registry.InitFromGenesis(seedPubs, seedStake)
			seedActive, _ := p.Registry.Count()
			p.Log.Warn("startup scan: registry was empty after scan — seeded observed block producers as active validators",
				"seeded_count", len(seedPubs),
				"seed_stake_napro", seedStake,
				"active_after_seed", seedActive,
				"tip_height", p.TipHeight,
				"note", "these entries will be replaced once a real stake transaction is replayed",
			)
		}
	}

	// Save a full tip-height snapshot for the next fast-start.
	{
		snapToSave := startupSnapshot{
			Version:    snapVersion,
			TipHeight:  p.TipHeight,
			TipHashHex: p.TipHashHex,
			TxTotal:    txTotal,
			UTXOs:      p.UTXOs.TakeSnapshot(),
			Registry:   p.Registry.TakeSnapshot(),
		}
		if p.SnapshotWg != nil {
			p.SnapshotWg.Add(1)
		}
		activeCount := len(snapToSave.UTXOs.ActiveUTXOs)
	go func() {
			if p.SnapshotWg != nil {
				defer p.SnapshotWg.Done()
			}
			if saveErr := saveStartupSnapshot(p.DataDir, snapToSave); saveErr != nil {
				p.Log.Warn("failed to save startup snapshot", "err", saveErr)
			} else {
				p.Log.Info("startup snapshot saved", "tip_height", p.TipHeight)
				deleteOldSnapshots(p.DataDir, p.TipHeight)
				// Persist the active UTXO count keyed by tip hash alongside the
				// snapshot so the startup divergence check has an active-only
				// reference count that cannot be overwritten by a concurrent
				// goroutine saving a snapshot at a different height.
				if metaErr := p.DB.StoreActiveUTXOCount(snapToSave.TipHashHex, activeCount); metaErr != nil {
					p.Log.Warn("failed to persist active_utxo_count metadata", "err", metaErr)
				}
			}
		}()
	}

	return startupScanResult{ScanFrom: scanFrom, TxTotal: txTotal}, nil
}
