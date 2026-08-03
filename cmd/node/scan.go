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
	Log         *slog.Logger

	// SetSyncProgress may be nil; when non-nil it is called every 1 000 blocks
	// so that callers can report syncing progress to external consumers.
	SetSyncProgress func(syncing, tip uint64)

	// SnapshotWg, when non-nil, has Add(1)/Done() called around every
	// goroutine that writes a snapshot file.  Tests pass a *sync.WaitGroup
	// and call Wait() before asserting on log output or temp-dir contents;
	// production callers leave it nil.
	SnapshotWg *sync.WaitGroup
}

// runStartupScan performs the unified startup block scan:
//  1. Checks for a partial intermediate checkpoint (findLatestSnapshot) and
//     restores UTXOSet + ValidatorRegistry from it when found.
//  2. Scans blocks from scanFrom (checkpoint+1, or 1) through TipHeight,
//     rebuilding the active UTXO set, spent key images, and stake registry.
//  3. Saves an intermediate checkpoint every 50 000 blocks so that a future
//     crash mid-scan can resume from the checkpoint.
//  4. Saves a full tip-height snapshot after the scan completes.
//
// On return, p.UTXOs and p.Registry reflect the fully-rebuilt state.
// The caller is responsible for loading the key-image index before calling
// this function (when KiFromIndex is true).
func runStartupScan(p startupScanParams) (startupScanResult, error) {
	const syncProgressInterval = uint64(1000)  // report every 1 000 blocks
	const gcInterval           = uint64(10000) // force GC every 10 000 blocks
	const checkpointInterval   = uint64(50000) // save intermediate snapshot every 50 000 blocks

	// ── Partial snapshot resume ───────────────────────────────────────────
	// If an exact-tip snapshot was not found by the caller, look for the most
	// recent intermediate checkpoint saved during a previous scan.  Restoring
	// from it means we only need to replay blocks since that checkpoint.
	scanFrom := uint64(1)
	txTotal := p.InitTxTotal
	if partial := findLatestSnapshot(p.DataDir, p.TipHeight); partial != nil {
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
	kiCount := 0 // only used when !KiFromIndex

	for h := scanFrom; h <= p.TipHeight; h++ {
		if p.SetSyncProgress != nil && h%syncProgressInterval == 0 {
			p.SetSyncProgress(h, p.TipHeight)
		}
		if h%gcInterval == 0 {
			runtime.GC()
		}

		raw, fetchErr := p.DB.GetRawBlockByHeight(h)
		if fetchErr != nil || raw == nil {
			return startupScanResult{}, fmt.Errorf(
				"startup scan: block at height %d missing from store (%v) — "+
					"node cannot start safely; repair the store and restart",
				h, fetchErr)
		}
		var b core.Block
		if parseErr := json.Unmarshal(raw, &b); parseErr != nil {
			return startupScanResult{}, fmt.Errorf(
				"startup scan: cannot decode block at height %d: %w — "+
					"node cannot start safely; repair the store and restart",
				h, parseErr)
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
			if p.SnapshotWg != nil {
				p.SnapshotWg.Add(1)
			}
			go func(s startupSnapshot) {
				if p.SnapshotWg != nil {
					defer p.SnapshotWg.Done()
				}
				if cpErr := saveStartupSnapshot(p.DataDir, s); cpErr != nil {
					p.Log.Warn("scan checkpoint save failed", "height", s.TipHeight, "err", cpErr)
				} else {
					p.Log.Info("scan checkpoint saved", "height", s.TipHeight)
					deleteOldSnapshots(p.DataDir, s.TipHeight)
				}
			}(cpSnap)
		}
	}

	if !p.KiFromIndex {
		if err := p.DB.StoreTxTotal(txCount); err != nil {
			p.Log.Warn("failed to persist tx total after block scan", "err", err)
		}
		txTotal = txCount
		p.Log.Info("spent key-image set rebuilt (full block scan)",
			"key_images_marked", kiCount,
			"blocks_scanned", p.TipHeight,
			"total_txs_counted", txCount,
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
		go func() {
			if p.SnapshotWg != nil {
				defer p.SnapshotWg.Done()
			}
			if saveErr := saveStartupSnapshot(p.DataDir, snapToSave); saveErr != nil {
				p.Log.Warn("failed to save startup snapshot", "err", saveErr)
			} else {
				p.Log.Info("startup snapshot saved", "tip_height", p.TipHeight)
				deleteOldSnapshots(p.DataDir, p.TipHeight)
			}
		}()
	}

	return startupScanResult{ScanFrom: scanFrom, TxTotal: txTotal}, nil
}
