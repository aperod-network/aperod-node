// Command node is the Aperod blockchain node process.
package main

import (
        "bufio"
        "encoding/json"
        "fmt"
        "log/slog"
        "os"
        "os/signal"
        "path/filepath"
        "regexp"
        "runtime"
        "runtime/debug"
        "strconv"
        "strings"
        "syscall"
        "time"

        "github.com/aperod/aperod/api"
        "github.com/aperod/aperod/config"
        "github.com/aperod/aperod/consensus"
        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/p2p"
        "github.com/aperod/aperod/store"
)

func main() {
        if err := run(); err != nil {
                fmt.Fprintf(os.Stderr, "aperod-node: %v\n", err)
                os.Exit(1)
        }
}

// multiaddrRe matches /ip4/<host>/tcp/<port> and extracts host and port.
var multiaddrRe = regexp.MustCompile(`/ip4/([\d.]+)/tcp/(\d+)`)

// parseP2PAddr converts a multiaddr like "/ip4/0.0.0.0/tcp/30303" to "0.0.0.0:30303".
// Falls back to addr as-is if it doesn't match (allows plain "host:port" too).
func parseP2PAddr(addr string) string {
        if m := multiaddrRe.FindStringSubmatch(addr); len(m) == 3 {
                return m[1] + ":" + m[2]
        }
        return addr
}

// nodeHandler implements p2p.Handler using the consensus engine and in-memory chain.
type nodeHandler struct {
        engine *consensus.Engine
        chain  *core.Chain
        pool   *core.Mempool
        db     *store.DB
        log    *slog.Logger
}

func (h *nodeHandler) CurrentHeight() uint64 {
        return h.chain.Height()
}

func (h *nodeHandler) CurrentTailHashes(n int) []crypto.Hash32 {
        return h.chain.TailHashes(n)
}

func (h *nodeHandler) GetBlock(hash crypto.Hash32) *core.Block {
        return h.chain.GetByHash(hash)
}

// OnBlock forwards an externally received block to the consensus engine.
func (h *nodeHandler) OnBlock(block *core.Block) {
        select {
        case h.engine.NewBlockCh() <- block:
        default:
                h.log.Warn("p2p: incoming block channel full — dropped", "height", block.Header.Height)
        }
}

// OnTransaction adds an externally received transaction to the mempool.
func (h *nodeHandler) OnTransaction(tx *core.Transaction) {
        if err := h.pool.Add(*tx); err != nil {
                h.log.Debug("p2p: mempool rejected tx", "err", err)
        }
}

// OnVote forwards a p2p vote to the consensus engine.
func (h *nodeHandler) OnVote(vote p2p.VoteMsg) {
        var pub crypto.ValidatorPubKey
        copy(pub[:], vote.ValidatorPub)
        fm := consensus.FinalizeMsg{
                BlockHash:    vote.BlockHash,
                Height:       vote.Height,
                ValidatorPub: pub,
                Signature:    vote.Signature,
        }
        select {
        case h.engine.NewVoteCh() <- fm:
        default:
                h.log.Warn("p2p: vote channel full — dropped")
        }
}

func run() error {
        // ── 1. Load configuration ─────────────────────────────────────────────────
        cfgPath := "config/testnet.yaml"
        resetP2PIdentity := false
        validateOnly := false
        for i, arg := range os.Args[1:] {
                switch arg {
                case "--config":
                        if i+2 < len(os.Args) {
                                cfgPath = os.Args[i+2]
                        }
                case "--reset-p2p-identity":
                        resetP2PIdentity = true
                case "--validate-config":
                        validateOnly = true
                }
        }
        _ = resetP2PIdentity // used below in P2P startup

        cfg, err := config.Load(cfgPath)
        if err != nil {
                return fmt.Errorf("load config: %w", err)
        }
        if err := cfg.Validate(); err != nil {
                return fmt.Errorf("invalid config: %w", err)
        }

        // --validate-config: exit 0 after a successful parse+validate so
        // operators (and node-config.sh) can verify node.yaml without starting
        // the node.  Prints the config path and network name so the caller can
        // confirm which file was checked.
        if validateOnly {
                fmt.Fprintf(os.Stdout, "config OK: %s (network=%s)\n", cfgPath, cfg.Network)
                return nil
        }

        // ── 2. Setup logger ───────────────────────────────────────────────────────
        level := slog.LevelInfo
        switch cfg.LogLevel {
        case "debug":
                level = slog.LevelDebug
        case "warn":
                level = slog.LevelWarn
        case "error":
                level = slog.LevelError
        }
        log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
        slog.SetDefault(log)

        log.Info("starting Aperod node", "network", cfg.Network, "data_dir", cfg.DataDir)

        // Guard: warn early if the effective systemd TimeoutStopSec is below the
        // safe threshold.  A value below 240 s risks a SIGKILL arriving before
        // saveStartupSnapshot finishes, forcing a multi-hour block scan on the
        // next restart and potentially triggering an OOM loop.
        //
        // The drop-in file is checked first (it takes precedence over the main
        // unit), then the main unit itself.  When systemd appears to be running
        // but no TimeoutStopSec is configured anywhere, a warning is emitted
        // because the systemd default is 90 s.
        checkSystemdTimeout(
                "/etc/systemd/system/aperod-node.service.d/timeout.conf",
                "/etc/systemd/system/aperod-node.service",
                "/run/systemd/system",
                log,
        )

        // Emit any non-fatal configuration warnings now that the logger is ready.
        for _, w := range cfg.Warnings() {
                log.Warn("config warning", "msg", w)
        }

        // ── 3. Open storage ───────────────────────────────────────────────────────
        if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
                return fmt.Errorf("create data dir: %w", err)
        }
        db, err := store.Open(cfg.DataDir + "/chain.db")
        if err != nil {
                return fmt.Errorf("open store: %w", err)
        }
        defer db.Close()

        // ── 4. Load or generate a persistent validator key ────────────────────────
        myKey, err := loadOrGenerateValidatorKey(cfg, log)
        if err != nil {
                return fmt.Errorf("validator key: %w", err)
        }

        // ── 5. Load genesis config ────────────────────────────────────────────────
        genesisConfig, err := core.LoadGenesis(cfg.Genesis.File)
        if err != nil {
                return fmt.Errorf("load genesis: %w", err)
        }

        // Override validator set with our own key so the node can always propose.
        validators := []crypto.ValidatorPubKey{myKey.Public()}

        // ── 6. Initialize chain ───────────────────────────────────────────────────
        chain := core.NewChain()
        mempool := core.NewMempool(core.DefaultMempoolConfig(), log)

        // Create the UTXO set here (before chain loading) so the resume path
        // can populate it from stored blocks, ensuring historical spent key
        // images are known to TxVerifier before the first peer block arrives.
        utxos := core.NewUTXOSet()

        tipHash, tipHeight, err := db.GetTip()
        if err != nil {
                return fmt.Errorf("get tip: %w", err)
        }

        // Populated during the key-image rebuild scan below; stays 0 for genesis.
        var initialTxTotal int64

        // Declared here so both the genesis and resume branches can assign it.
        // The genesis path leaves it nil; NewEngine creates a fresh registry.
        // The resume path initialises it inside the startup scan below.
        var registry *core.ValidatorRegistry

        // ── Early API server (syncing phase) ─────────────────────────────────────
        // Create and start the API server before the startup block scan so that
        // operators can query /api/v1/status and see syncing progress while the
        // node replays blocks from disk (which can take hours on large chains).
        // The server starts with syncing=1 (default); SetReady() is called after
        // the scan and engine setup so UTXO queries remain blocked until then.
        // The remaining configuration (SetRegistry, SetValidatorKey, etc.) that
        // depends on the consensus engine is applied later in step 9 below.
        var apiSrv *api.Server
        if cfg.API.Enabled && cfg.API.ListenAddr != "" {
                apiSrv = api.NewServer(cfg.API.ListenAddr, chain, mempool, utxos, log)
                apiSrv.SetAllowedOrigins(cfg.API.CORS)
                apiSrv.SetNodeViewKey(cfg.Consensus.ViewKey)
                apiSrv.SetStore(db)
                apiSrv.SetPruningMode(cfg.Pruning.Mode)
                apiSrv.SetKeepBlocks(cfg.Pruning.KeepBlocks)
                if cfg.API.Key != "" {
                        apiSrv.SetAPIKey(cfg.API.Key)
                }
                // Seed tip so the first /api/v1/status call has a meaningful tip_height.
                if tipHeight > 0 {
                        apiSrv.SetSyncProgress(0, tipHeight)
                }
                go func() {
                        if err := apiSrv.Start(); err != nil {
                                log.Error("API server stopped", "err", err)
                        }
                }()
                log.Info("API server started (syncing phase)", "addr", cfg.API.ListenAddr)
        }

        if tipHash == (crypto.Hash32{}) {
                log.Info("initializing genesis block", "chain_id", genesisConfig.ChainID)
                genesis, err := core.CreateGenesisBlock(genesisConfig, myKey.PrivKey())
                if err != nil {
                        return fmt.Errorf("create genesis: %w", err)
                }
                if err := chain.SetGenesis(genesis); err != nil {
                        return fmt.Errorf("set genesis: %w", err)
                }
                // F-7 fix: populate the in-memory UTXO set with genesis outputs so
                // TxVerifier.VerifyTx can validate ring commitments that reference
                // genesis UTXOs from the very first block onwards.
                if err := utxos.ApplyBlock(genesis); err != nil {
                        return fmt.Errorf("apply genesis UTXOs: %w", err)
                }
                if err := storeBlock(db, genesis); err != nil {
                        return fmt.Errorf("store genesis: %w", err)
                }
                if err := db.PutTip(genesis.Hash(), 0); err != nil {
                        return fmt.Errorf("store genesis tip: %w", err)
                }
                h := genesis.Hash()
                log.Info("genesis block created", "hash", fmt.Sprintf("%x", h[:8]))
        } else {
                log.Info("resuming from stored chain", "height", tipHeight, "tip", fmt.Sprintf("%x", tipHash[:8]))

                // Always load genesis (height 0).
                genesisRaw, err := db.GetRawBlockByHeight(0)
                if err != nil || genesisRaw == nil {
                        return fmt.Errorf("genesis block missing in store")
                }
                var genesisBlk core.Block
                if err := json.Unmarshal(genesisRaw, &genesisBlk); err != nil {
                        return fmt.Errorf("unmarshal genesis: %w", err)
                }
                if err := chain.SetGenesis(&genesisBlk); err != nil {
                        return fmt.Errorf("restore genesis: %w", err)
                }
                // F-7 fix: apply genesis UTXOs to the in-memory set on resume so
                // genesis-era ring members pass the C-0 commitment binding check.
                if err := utxos.ApplyBlock(&genesisBlk); err != nil {
                        return fmt.Errorf("apply genesis UTXOs on resume: %w", err)
                }

                // Load only the most recent maxInMemoryBlocks blocks.
                // Older blocks are kept in SQLite; the in-memory window is bounded.
                const maxLoad = core.MaxInMemoryBlocks
                startLoad := uint64(1)
                if tipHeight >= maxLoad {
                        startLoad = tipHeight - maxLoad + 1
                }
                recentBlocks := make([]*core.Block, 0, maxLoad)
                for h := startLoad; h <= tipHeight; h++ {
                        raw, err := db.GetRawBlockByHeight(h)
                        if err != nil || raw == nil {
                                log.Warn("block missing in store during resume", "height", h)
                                break
                        }
                        var b core.Block
                        if err := json.Unmarshal(raw, &b); err != nil {
                                log.Warn("block unmarshal failed", "height", h, "err", err)
                                break
                        }
                        recentBlocks = append(recentBlocks, &b)
                }
                chain.FastForward(recentBlocks)
                log.Info("chain restored from storage",
                        "tip_height", tipHeight,
                        "blocks_loaded_in_memory", len(recentBlocks),
                )

                // ── Unified startup scan ─────────────────────────────────────────────
                //
                // Goals (all complete before any peer or API traffic arrives):
                //  1. Rebuild the spent key-image set (double-spend prevention).
                //  2. Rebuild the active UTXO set (ring-member lookup for C-0 check).
                //  3. Restore the ValidatorRegistry from historical stake transactions.
                //  4. Restore UTXOSet.stakedUTXOs so burned stake collateral cannot be
                //     reused as a ring decoy or re-staked after a restart.
                //
                // Key-image fast path: load from the persistent LevelDB index when
                // available, falling back to a block scan only when the index is missing.
                // UTXO rebuild and stake replay always require a block scan, so there is
                // one scan regardless of which key-image path is taken.
                //
                // Ordering: the registry must be seeded with genesis validators BEFORE
                // the scan so that historical withdrawals by genesis validators replay
                // correctly (applyWithdraw would fail "not registered" otherwise).
                //
                // FAIL-CLOSED: any missing or corrupt block causes the node to refuse to
                // start for both key-image and stake replay.

                // Registry setup must happen before the block scan so that the scan can
                // replay stake txs (including withdrawals by genesis validators).
                registry = core.NewValidatorRegistry()
                registry.SetUTXOSet(utxos)
                // InitFromGenesis is idempotent (!exists guard), so NewEngine's later
                // call is a safe no-op for genesis validators already in the registry.
                genesisStakeForReplay := core.MinStakeNAPR * 10 // must match consensus.NewEngine
                registry.InitFromGenesis(validators, genesisStakeForReplay)

                // ── Snapshot fast path ───────────────────────────────────────
                // If a valid UTXOSet snapshot exists at the exact current tip,
                // restore from it and skip both the key-image load and the
                // full block scan.  Falls back gracefully when the snapshot is
                // absent, stale, or corrupt.
                snapLoaded := false
                {
                        tipHashHex := fmt.Sprintf("%x", tipHash[:])
                        if snap, serr := loadStartupSnapshot(cfg.DataDir, tipHeight, tipHashHex); serr == nil {
                                // ── UTXO count divergence check ──────────────────────────────────
                                // Before restoring, compare the snapshot's active (unspent) UTXO
                                // count against the durable active count written to DB metadata the
                                // last time a snapshot was saved.  A mismatch beyond the configured
                                // tolerance indicates a stale or partially-written snapshot (e.g.
                                // written mid-crash, or produced by a binary with a different UTXO
                                // schema).
                                //
                                // The check is gated on the metadata value being present.  On a
                                // node that has never completed a successful snapshot save the key
                                // does not exist and the check is skipped rather than treating a
                                // missing count as zero.
                                utxoCountOK := true
                                snapUTXOCount := len(snap.UTXOs.ActiveUTXOs)
                                dbActiveCount, dbHasCount, countErr := db.LoadActiveUTXOCount(tipHashHex)
                                if countErr != nil {
                                        log.Warn("snapshot UTXO count check skipped — db metadata error", "err", countErr)
                                } else if !dbHasCount {
                                        log.Info("snapshot UTXO count check skipped — no prior active count in db (first snapshot)")
                                } else {
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
                                        tolerancePct := cfg.Snapshot.UTXOCountTolerancePct
                                        if diffPct > tolerancePct {
                                                log.Warn("snapshot rejected — active UTXO count diverges from last-saved count; falling back to block scan",
                                                        "snapshot_active_utxos", snapUTXOCount,
                                                        "db_last_active_utxos", dbActiveCount,
                                                        "diff_pct", fmt.Sprintf("%.2f%%", diffPct),
                                                        "tolerance_pct", tolerancePct,
                                                )
                                                utxoCountOK = false
                                        } else {
                                                log.Info("snapshot UTXO count check passed",
                                                        "snapshot_active_utxos", snapUTXOCount,
                                                        "db_last_active_utxos", dbActiveCount,
                                                        "diff_pct", fmt.Sprintf("%.2f%%", diffPct),
                                                )
                                        }
                                }

                                if utxoCountOK {
                                        utxos.RestoreFromSnapshot(snap.UTXOs)
                                        registry.RestoreFromSnapshot(snap.Registry)
                                        registry.SetUTXOSet(utxos) // re-wire pointer (not serialised)
                                        if snap.TxTotal > 0 {
                                                initialTxTotal = snap.TxTotal
                                        }
                                        log.Info("startup fast path complete — snapshot loaded",
                                                "tip_height", tipHeight,
                                                "active_utxos", snapUTXOCount,
                                                "spent_decoys", len(snap.UTXOs.SpentDecoys),
                                                "key_images", len(snap.UTXOs.KeyImages),
                                        )
                                        snapLoaded = true
                                        // Release the raw snapshot struct now that its contents have been
                                        // copied into the UTXOSet maps.  Without an explicit GC the
                                        // deserialised snapshot and the in-memory maps coexist until the
                                        // next automatic collection, doubling peak RSS on load.
                                        runtime.GC()
                                        debug.FreeOSMemory() // return freed pages to OS immediately so GOMEMLIMIT has headroom
                                }
                        } else if !os.IsNotExist(serr) {
                                log.Warn("snapshot load error, falling back to block scan", "err", serr)
                        }
                }

                if !snapLoaded {
                // Try the fast path for spent key images first.
                log.Info("loading spent key-image set from database index",
                        "tip_height", tipHeight)
                kiCount := 0
                kiIterErr := db.IterKeyImages(func(ki crypto.KeyImage) error {
                        utxos.MarkSpent(ki)
                        kiCount++
                        return nil
                })
                kiFromIndex := kiIterErr == nil && (kiCount > 0 || tipHeight == 0)
                if kiFromIndex {
                        storedTotal, loadErr := db.LoadTxTotal()
                        if loadErr != nil {
                                log.Warn("could not read stored tx total — counter starts from 0",
                                        "err", loadErr)
                        }
                        initialTxTotal = storedTotal
                        log.Info("spent key-image set loaded from index",
                                "key_images", kiCount,
                                "tx_total_restored", initialTxTotal)
                } else {
                        if kiIterErr != nil {
                                log.Warn("key-image index error; rebuilding from block scan",
                                        "err", kiIterErr, "tip_height", tipHeight)
                        } else {
                                log.Warn("key-image index empty on non-genesis chain; rebuilding from block scan",
                                        "tip_height", tipHeight)
                        }
                        kiCount = 0
                }

                var setSyncProgress func(uint64, uint64)
                if apiSrv != nil {
                        setSyncProgress = apiSrv.SetSyncProgress
                }
                scanResult, scanErr := runStartupScan(startupScanParams{
                        DataDir:               cfg.DataDir,
                        TipHeight:             tipHeight,
                        TipHashHex:            fmt.Sprintf("%x", tipHash[:]),
                        DB:                    db,
                        UTXOs:                 utxos,
                        Registry:              registry,
                        KiFromIndex:           kiFromIndex,
                        InitTxTotal:           initialTxTotal,
                        Log:                   log,
                        UTXOCountTolerancePct: cfg.Snapshot.UTXOCountTolerancePct,
                        SetSyncProgress:       setSyncProgress,
                })
                if scanErr != nil {
                        return scanErr
                }
                initialTxTotal = scanResult.TxTotal
                _ = kiCount // already logged above; kept for the key-image index path
                } // end !snapLoaded
        }

        // Release all block objects decoded during the startup scan.  The GC is
        // not triggered automatically between the last scan iteration and engine
        // startup, so without this call the decoded block tree stays live and
        // inflates RSS until the first scheduled collection.
        runtime.GC()
        debug.FreeOSMemory() // aggressively return freed pages to OS so GOMEMLIMIT has headroom for normal operation

        // initialTxTotal is populated by the scan above (genesis path stays 0).

        // Safety invariant: the UTXO set (active outputs + byPubKey index +
        // spent key-image set + stakedUTXOs) is fully populated from the chain
        // store.  The consensus engine, P2P host, and API server are all wired
        // AFTER this point so no external transaction can reach the mempool
        // before ring-member verification is active.  This prevents the
        // "startup window" attack where a ring tx referencing unknown decoys is
        // accepted during the startup phase.
        log.Info("UTXO set ready — ring-member verification active",
                "unspent_outputs", utxos.Count(),
        )

        // ── 7. Setup consensus engine ─────────────────────────────────────────────
        // host is declared here so OnBlockProduced can reference it
        // (assigned after engine creation; closure captures by reference).
        // apiSrv was declared above and started in the early API server section.
        var host *p2p.Host

        // engine is pre-declared with var so that the OnBlockProduced closure
        // can reference it (e.g. engine.Registry()) via a captured pointer.
        // By the time any block is produced, engine is fully initialised.
        var engine *consensus.Engine

        // For the resume path, registry is already created and seeded inside the
        // startup scan above.  For genesis, it is nil here; NewEngine creates it.

        engine = consensus.NewEngine(consensus.Config{
                BlockTime:          cfg.Consensus.BlockTime,
                BFTThreshold:       genesisConfig.BFTThreshold,
                Validators:         validators,
                Registry:           registry,
                MyKey:              myKey,
                RewardAddress:      cfg.Consensus.RewardAddress,
                BlockRewardNAPR:    cfg.Consensus.BlockRewardNAPR,
                OracleURL:          cfg.Consensus.OracleURL,
                OracleMaxDeviation: cfg.Consensus.OracleMaxDeviation,
                OnBlockProduced: func(block *core.Block) {
                        if err := storeBlock(db, block); err != nil {
                                log.Error("failed to persist block", "height", block.Header.Height, "err", err)
                        } else {
                                hash := block.Hash()
                                if err := db.PutTip(hash, block.Header.Height); err != nil {
                                        log.Error("failed to update tip", "height", block.Header.Height, "err", err)
                                }
                        }
                        // Increment cached tx counter (skip index-0 coinbase reward).
                        if apiSrv != nil {
                                var delta int64
                                for txIdx, tx := range block.Txs {
                                        if txIdx == 0 && tx.IsCoinbase() {
                                                continue
                                        }
                                        delta++
                                }
                                if delta > 0 {
                                        apiSrv.AddTxCount(delta)
                                        // Persist updated total so a fast restart can
                                        // restore the counter without a full block scan.
                                        if storeErr := db.StoreTxTotal(apiSrv.TxTotal()); storeErr != nil {
                                                log.Warn("failed to persist tx total",
                                                        "height", block.Header.Height, "err", storeErr)
                                        }
                                }
                        }
                        // Broadcast newly produced block to P2P peers (non-blocking).
                        if host != nil {
                                host.BroadcastBlock(block)
                        }
                        // Persist spent key images to the LevelDB key-image index so
                        // that future restarts can use db.IterKeyImages() instead of
                        // scanning every raw block.  Non-fatal on error — the block is
                        // already accepted; the index is a startup-performance optimisation.
                        for _, tx := range block.Txs {
                                for _, inp := range tx.Inputs {
                                        if kiErr := db.MarkKeyImageSpent(inp.KeyImage); kiErr != nil {
                                                log.Warn("failed to persist key image",
                                                        "height", block.Header.Height, "err", kiErr)
                                        }
                                }
                        }
                },
                // OnBlockAccepted fires for every canonical block regardless of
                // whether it was produced locally or received from a P2P peer.
                // This is the correct place for the periodic snapshot: it guards
                // against OOM kills during normal operation on any node type,
                // including pure syncing nodes that never call OnBlockProduced.
                OnBlockAccepted: func(block *core.Block) {
                        h := block.Header.Height
                        if h == 0 || h%500 != 0 {
                                return
                        }
                        var txTot int64
                        if apiSrv != nil {
                                txTot = apiSrv.TxTotal()
                        }
                        hash := block.Hash()
                        periodicSnap := startupSnapshot{
                                Version:    snapVersion,
                                TipHeight:  h,
                                TipHashHex: fmt.Sprintf("%x", hash[:]),
                                TxTotal:    txTot,
                                UTXOs:      utxos.TakeSnapshot(),
                                Registry:   engine.Registry().TakeSnapshot(),
                        }
                        periodicActive := len(periodicSnap.UTXOs.ActiveUTXOs)
                        go func(snap startupSnapshot, height uint64, activeCount int) {
                                if saveErr := saveStartupSnapshot(cfg.DataDir, snap); saveErr != nil {
                                        log.Warn("failed to save periodic snapshot",
                                                "height", height, "err", saveErr)
                                } else {
                                        log.Info("periodic snapshot saved", "height", height)
                                        deleteOldSnapshots(cfg.DataDir, height)
                                        // Persist the active UTXO count keyed by tip hash so the
                                        // next restart's divergence check has an active-only
                                        // reference count that is specific to this snapshot and
                                        // cannot be overwritten by a concurrent goroutine saving
                                        // a snapshot at a different height.
                                        if metaErr := db.StoreActiveUTXOCount(snap.TipHashHex, activeCount); metaErr != nil {
                                                log.Warn("failed to persist active_utxo_count metadata",
                                                        "height", height, "err", metaErr)
                                        }
                                }
                        }(periodicSnap, h, periodicActive)
                },
        }, chain, mempool, log)

        // ── 8. Wire TxVerifier BEFORE starting the engine goroutine ──────────────
        // engine.Run launches handleIncomingBlock immediately on incoming P2P
        // blocks.  If SetTxVerifier is called after Run, there is a startup window
        // where blocks are accepted without cryptographic verification (nil verifier
        // = fail-open).  Wiring first closes that window entirely.
        txVerifier := core.NewTxVerifier(utxos)
        // Wire vesting enforcement so the verifier rejects spending of still-locked
        // genesis allocations at the protocol level (not just display-only).
        // Use the actual persisted genesis block timestamp (nanoseconds ÷ 1e9) rather
        // than genesisConfig.Timestamp which may be zero (meaning "use current time")
        // and would cause all allocations to appear fully vested since Unix epoch.
        if genesisBlock := chain.Genesis(); genesisBlock != nil {
                genesisTimeSec := genesisBlock.Header.Timestamp / 1e9
                vl, vlErr := core.BuildVestingLock(genesisConfig, genesisTimeSec)
                if vlErr != nil {
                        // A BuildVestingLock failure means the genesis config is
                        // malformed (e.g. duplicate spend keys).  Starting with
                        // enforcement disabled would be a silent security bypass —
                        // fail hard so the operator must fix the genesis config.
                        log.Error("vesting lock build failed — refusing to start without enforcement", "err", vlErr)
                        os.Exit(1)
                }
                txVerifier.SetVestingLock(vl)
                log.Info("vesting lock loaded", "locked_allocs", vl.LockedAllocsCount(), "genesis_time", genesisTimeSec)
        }
        engine.SetTxVerifier(txVerifier, utxos)
        // Wire the same verifier into the mempool so Add() runs full RingCT checks
        // (C-0 / C-1 fix: prevents inflation via forged commitments or unbound stake).
        mempool.SetVerifier(txVerifier)

        // ── 9. Start subsystems ───────────────────────────────────────────────────
        stop := make(chan struct{})
        go engine.Run(stop)

        // Drain ProducedCh so the consensus engine never blocks on a full channel.
        go func() {
                for {
                        select {
                        case <-stop:
                                return
                        case <-engine.ProducedCh():
                                // Consumed; broadcasting is handled in OnBlockProduced above.
                        }
                }
        }()

        if apiSrv != nil {
                // Wire engine-dependent options now that the consensus engine exists.
                apiSrv.SetRegistry(engine.Registry())
                apiSrv.SetValidatorKey(myKey)
                apiSrv.SetTxTotal(initialTxTotal)
                apiSrv.SetTimestampRejectedCounter(func() int64 { return engine.TimestampRejectedCount() })
                // Startup scan is complete — mark the node ready for UTXO queries.
                apiSrv.SetReady()
                log.Info("API server ready (startup scan complete)")
        }

        // ── 9. Start P2P networking ───────────────────────────────────────────────
        if cfg.P2P.ListenAddr != "" {
                tcpAddr := parseP2PAddr(cfg.P2P.ListenAddr)

                // Convert bootnode multiaddrs to plain TCP addrs.
                bootnodes := make([]string, 0, len(cfg.P2P.Bootnodes))
                for _, bn := range cfg.P2P.Bootnodes {
                        parsed := parseP2PAddr(bn)
                        // Skip self-connections (same port as our listener).
                        if parsed != tcpAddr {
                                bootnodes = append(bootnodes, parsed)
                        }
                }

                // Load or generate a persistent Ed25519 TLS identity.
                // The key file path is configurable via p2p.identity_key in
                // node.yaml; if not set it defaults to <data_dir>/p2p_identity.key.
                // Pass --reset-p2p-identity on the command line to force
                // regeneration (e.g. after a key compromise).  Every connection
                // is encrypted (TLS 1.3) and mutually authenticated — plaintext
                // or unauthenticated peers are rejected at the handshake stage
                // (security finding F-030).
                identityKeyPath := cfg.P2P.IdentityKey
                if identityKeyPath == "" {
                        identityKeyPath = filepath.Join(cfg.DataDir, "p2p_identity.key")
                }
                tlsCfg, nodeFingerprint, isNewIdentity, tlsErr := p2p.LoadOrSaveP2PIdentity(identityKeyPath, resetP2PIdentity)
                if tlsErr != nil {
                        log.Error("p2p tls identity load/generate failed — aborting p2p startup", "err", tlsErr)
                } else {
                        switch {
                        case resetP2PIdentity:
                                log.Info("p2p tls identity reset and regenerated",
                                        "fingerprint", nodeFingerprint, "path", identityKeyPath)
                        case isNewIdentity:
                                log.Warn("p2p tls identity generated for the first time — record this fingerprint for peer allow-lists",
                                        "fingerprint", nodeFingerprint, "path", identityKeyPath)
                        default:
                                log.Info("p2p tls identity loaded",
                                        "fingerprint", nodeFingerprint, "path", identityKeyPath)
                        }

                        // Expose fingerprint, P2P listen address, and node ID via REST.
                        if apiSrv != nil {
                                apiSrv.SetNodeIdentity(nodeFingerprint, tcpAddr, myKey.Public().ID())
                        }

                        handler := &nodeHandler{
                                engine: engine,
                                chain:  chain,
                                pool:   mempool,
                                db:     db,
                                log:    log,
                        }
                        host = p2p.NewHost(p2p.Config{
                                ListenAddr:           tcpAddr,
                                Bootnodes:            bootnodes,
                                MaxPeers:             cfg.P2P.MaxPeers,
                                MinPeers:             cfg.P2P.MinPeers,
                                MaxPeersPerIP:        cfg.P2P.MaxPeersPerIP,
                                MinOutbound:          cfg.P2P.MinOutbound,
                                NodeID:               myKey.Public().ID(),
                                UserAgent:            "aperod-node/1.0",
                                TLSConfig:            tlsCfg,
                                AllowedPeers:         cfg.P2P.AllowedPeers,
                                MaxPendingHandshakes: cfg.P2P.MaxPendingHandshakes,
                        }, handler, log)
                        if len(cfg.P2P.AllowedPeers) > 0 {
                                log.Info("allow-list active",
                                        "allowed_peers", len(cfg.P2P.AllowedPeers))
                                if len(bootnodes) == 0 {
                                        log.Warn("allow-list is active but no bootnodes are configured — node may not connect to any peers",
                                                "allowed_peers", len(cfg.P2P.AllowedPeers))
                                }
                        }

                        if err := host.Start(); err != nil {
                                log.Error("p2p failed to start", "err", err)
                                // Non-fatal: node runs standalone if P2P fails.
                        } else {
                                log.Info("p2p started", "listen", tcpAddr, "bootnodes", len(bootnodes))
                                defer host.Stop()
                                if apiSrv != nil {
                                        apiSrv.SetPeerCounter(host.PeerCount)
                                apiSrv.SetPendingHandshakeCounter(host.PendingHandshakes)
                                apiSrv.SetBanListFunc(func() []api.BanEntry {
                                        bans := host.ListBans()
                                        out := make([]api.BanEntry, len(bans))
                                        for i, b := range bans {
                                                out[i] = api.BanEntry{Addr: b.Addr, Reason: b.Reason, ExpiresAt: b.ExpiresAt}
                                        }
                                        return out
                                })
                                apiSrv.SetBanLiftFunc(host.LiftBan)
                                apiSrv.SetBanAddFunc(host.BanPeer)
                                }
                                // Background goroutine: if an allow-list is active and no peers
                                // connect after 2×block_time, the list may be misconfigured
                                // (e.g. bootnode fingerprints missing).  Fire a WARN on every
                                // check interval so the operator notices quickly.
                                if len(cfg.P2P.AllowedPeers) > 0 {
                                        checkDelay := 2 * cfg.Consensus.BlockTime
                                        if checkDelay <= 0 {
                                                checkDelay = 6 * time.Second
                                        }
                                        go func() {
                                                // Wait the initial 2×block_time before first check.
                                                timer := time.NewTimer(checkDelay)
                                                defer timer.Stop()
                                                select {
                                                case <-stop:
                                                        return
                                                case <-timer.C:
                                                }
                                                // Then repeat every checkDelay until a peer appears or node stops.
                                                ticker := time.NewTicker(checkDelay)
                                                defer ticker.Stop()
                                                for {
                                                        if host.PeerCount() == 0 {
                                                                log.Warn("allow-list may be misconfigured — no peers connected",
                                                                        "allowed_peers", len(cfg.P2P.AllowedPeers),
                                                                        "hint", "ensure bootnode TLS fingerprints are in allowed_peers")
                                                        }
                                                        select {
                                                        case <-stop:
                                                                return
                                                        case <-ticker.C:
                                                        }
                                                }
                                        }()
                                }
                        }
                }
        }

        log.Info("node is running",
                "validators", len(validators),
                "my_pub", myKey.Public().ID(),
                "api", cfg.API.ListenAddr,
                "p2p", cfg.P2P.ListenAddr,
        )

        // ── 10. Background pruning worker (light mode only) ────────────────────────
        // Strips RingCT/Bulletproof tx data from blocks older than KeepBlocks.
        // Fires once per epoch (~100 blocks × block_time).
        if cfg.Pruning.Mode == "light" {
                epochDur := cfg.Consensus.BlockTime * 100
                if epochDur <= 0 {
                        epochDur = 5 * 60 * 1000000000 // 5 min default
                }
                go func() {
                        t := time.NewTicker(epochDur)
                        defer t.Stop()
                        for {
                                select {
                                case <-stop:
                                        return
                                case <-t.C:
                                        n, err := chain.PruneOldData(db, cfg.Pruning.KeepBlocks)
                                        if err != nil {
                                                log.Warn("pruning error", "err", err)
                                        } else if n > 0 {
                                                log.Info("pruned old block tx data",
                                                        "blocks", n,
                                                        "keep_blocks", cfg.Pruning.KeepBlocks)
                                        }
                                }
                        }
                }()
                log.Info("light pruning enabled", "keep_blocks", cfg.Pruning.KeepBlocks)
        }

        // ── 11. Wait for signal ───────────────────────────────────────────────────
        sig := make(chan os.Signal, 1)
        signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
        <-sig

        log.Info("shutting down...")
        // Save a snapshot at the current tip so the next restart is instant.
        if registry != nil {
                if shutTipHash, shutTipHeight, stErr := db.GetTip(); stErr == nil && shutTipHeight > 0 {
                        shutTxTotal, _ := db.LoadTxTotal()
                        shutSnap := startupSnapshot{
                                Version:    snapVersion,
                                TipHeight:  shutTipHeight,
                                TipHashHex: fmt.Sprintf("%x", shutTipHash[:]),
                                TxTotal:    shutTxTotal,
                                UTXOs:      utxos.TakeSnapshot(),
                                Registry:   registry.TakeSnapshot(),
                        }
                        if saveErr := saveStartupSnapshot(cfg.DataDir, shutSnap); saveErr != nil {
                                log.Warn("shutdown: failed to save snapshot", "err", saveErr)
                        } else {
                                log.Info("shutdown: snapshot saved", "tip_height", shutTipHeight)
                                deleteOldSnapshots(cfg.DataDir, shutTipHeight)
                                // Persist the active UTXO count keyed by tip hash so the
                                // next restart's divergence check has an active-only reference
                                // count specific to this snapshot.
                                if metaErr := db.StoreActiveUTXOCount(shutSnap.TipHashHex, len(shutSnap.UTXOs.ActiveUTXOs)); metaErr != nil {
                                        log.Warn("shutdown: failed to persist active_utxo_count metadata", "err", metaErr)
                                }
                        }
                }
        }
        close(stop)
        return nil
}

// checkKeyFilePermissions returns an error if the key file has group- or
// world-readable/writable/executable bits set (mode & 0o077 != 0).
// A compromised key file is a critical security risk — the node refuses to boot.
func checkKeyFilePermissions(path string) error {
        info, err := os.Stat(path)
        if err != nil {
                return nil // not found — handled by caller
        }
        if info.Mode().Perm()&0o077 != 0 {
                return fmt.Errorf(
                        "validator key file %q has unsafe permissions %s — "+
                                "other users may read it; fix with: chmod 600 %q",
                        path, info.Mode().Perm(), path)
        }
        return nil
}

// loadOrGenerateValidatorKey returns the node's validator private key.
func loadOrGenerateValidatorKey(cfg *config.Config, log *slog.Logger) (*crypto.LockedValidatorKey, error) {
        if cfg.Consensus.ValidatorKey != "" {
                if err := checkKeyFilePermissions(cfg.Consensus.ValidatorKey); err != nil {
                        return nil, err
                }
                privBytes, err := os.ReadFile(cfg.Consensus.ValidatorKey)
                if err != nil {
                        return nil, fmt.Errorf("read validator key file: %w", err)
                }
                lk, err := crypto.NewLockedValidatorKey(privBytes, func(mlockErr error) {
                        log.Warn("mlock validator key bytes failed (non-fatal)", "err", mlockErr)
                })
                crypto.ZeroBytes(privBytes) // zero transient copy immediately after derivation
                if err != nil {
                        return nil, fmt.Errorf("parse validator private key: %w", err)
                }
                log.Info("loaded validator key from config", "pub", lk.Public().ID())
                return lk, nil
        }

        keyPath := filepath.Join(cfg.DataDir, "validator.key")
        if err := checkKeyFilePermissions(keyPath); err != nil {
                return nil, err
        }
        if data, err := os.ReadFile(keyPath); err == nil {
                if err := crypto.MlockBytes(data); err != nil {
                        log.Warn("mlock validator key bytes failed (non-fatal)", "err", err)
                }
                lk, err := crypto.NewLockedValidatorKey(data, func(mlockErr error) {
                        log.Warn("mlock validator key bytes failed (non-fatal)", "err", mlockErr)
                })
                crypto.ZeroBytes(data) // zero transient copy immediately after derivation
                if err != nil {
                        return nil, fmt.Errorf("parse persisted validator key: %w", err)
                }
                log.Info("loaded persisted validator key", "pub", lk.Public().ID())
                return lk, nil
        }

        priv, _, err := crypto.GenerateValidatorKey()
        if err != nil {
                return nil, fmt.Errorf("generate validator key: %w", err)
        }
        if err := os.WriteFile(keyPath, priv.Bytes(), 0600); err != nil {
                return nil, fmt.Errorf("save validator key: %w", err)
        }
        lk, err := crypto.NewLockedValidatorKey(priv.Bytes(), func(mlockErr error) {
                log.Warn("mlock validator key bytes failed (non-fatal)", "err", mlockErr)
        })
        crypto.ZeroBytes(priv.Bytes()) // zero the plain-text key after locking
        if err != nil {
                return nil, fmt.Errorf("lock generated validator key: %w", err)
        }
        log.Info("generated new validator key", "pub", lk.Public().ID(), "saved", keyPath)
        return lk, nil
}

// checkSystemdTimeout validates the effective TimeoutStopSec for the
// aperod-node service and logs a warning when it is below the safe threshold.
//
// Parameters (all overridable in tests via temp-dir paths):
//
//	dropinPath   – drop-in override file, e.g.
//	               /etc/systemd/system/aperod-node.service.d/timeout.conf
//	               (takes precedence over the main unit when present)
//	servicePath  – main unit file, e.g.
//	               /etc/systemd/system/aperod-node.service
//	systemdDir   – runtime directory that exists only under systemd, e.g.
//	               /run/systemd/system
//
// Logic:
//  1. Scan the drop-in file for TimeoutStopSec= (highest precedence).
//  2. If absent, scan the main unit file.
//  3. If neither contains TimeoutStopSec and systemd appears to be active
//     (systemdDir exists), warn that the 90-second default is in effect.
//
// A value below minSec (240 s) risks SIGKILL before saveStartupSnapshot
// finishes, forcing a multi-hour block scan on the next restart.
func checkSystemdTimeout(dropinPath, servicePath, systemdDir string, log *slog.Logger) {
        const minSec = 240
        const warnMsg = "systemd TimeoutStopSec is below safe threshold — snapshot may not save on restart"

        // scanForTimeout reads a unit/drop-in file and returns (seconds, found, error).
        // "infinity" maps to a very large number so the ≥ minSec check passes.
        scanForTimeout := func(path string) (float64, bool, error) {
                f, err := os.Open(path)
                if err != nil {
                        return 0, false, err
                }
                defer f.Close()
                sc := bufio.NewScanner(f)
                for sc.Scan() {
                        line := strings.TrimSpace(sc.Text())
                        after, ok := strings.CutPrefix(line, "TimeoutStopSec=")
                        if !ok {
                                continue
                        }
                        val := strings.TrimSpace(after)
                        if strings.EqualFold(val, "infinity") {
                                return 1e18, true, nil // effectively unlimited — safe
                        }
                        secs, parseErr := strconv.ParseFloat(val, 64)
                        if parseErr != nil {
                                return 0, true, fmt.Errorf("cannot parse TimeoutStopSec=%q in %s", val, path)
                        }
                        return secs, true, nil
                }
                return 0, false, nil // file exists but no TimeoutStopSec line
        }

        // 1. Try the drop-in first.
        if secs, found, err := scanForTimeout(dropinPath); err != nil && !os.IsNotExist(err) {
                log.Warn("systemd TimeoutStopSec: could not read drop-in", "path", dropinPath, "err", err)
                return
        } else if found {
                if secs < minSec {
                        log.Warn(warnMsg,
                                "source", dropinPath,
                                "current_sec", secs,
                                "minimum_sec", minSec,
                                "fix", fmt.Sprintf("set TimeoutStopSec=%d in %s and run: systemctl daemon-reload", minSec, dropinPath),
                        )
                }
                return
        }

        // 2. Try the main unit file.
        if secs, found, err := scanForTimeout(servicePath); err != nil && !os.IsNotExist(err) {
                log.Warn("systemd TimeoutStopSec: could not read service file", "path", servicePath, "err", err)
                return
        } else if found {
                if secs < minSec {
                        log.Warn(warnMsg,
                                "source", servicePath,
                                "current_sec", secs,
                                "minimum_sec", minSec,
                                "fix", fmt.Sprintf("set TimeoutStopSec=%d in %s and run: systemctl daemon-reload", minSec, servicePath),
                        )
                }
                return
        }

        // 3. No explicit TimeoutStopSec found anywhere.  Warn only when systemd
        // is active so we don't produce noise in non-systemd environments.
        if _, statErr := os.Stat(systemdDir); statErr == nil {
                log.Warn(warnMsg,
                        "source", "systemd default (90 s) — no TimeoutStopSec found in unit or drop-in",
                        "current_sec", 90,
                        "minimum_sec", minSec,
                        "fix", fmt.Sprintf(
                                "add TimeoutStopSec=%d to %s or %s and run: systemctl daemon-reload",
                                minSec, dropinPath, servicePath,
                        ),
                )
        }
}

// storeBlock serialises a block to JSON and writes it via PutRawBlock.
// It also persists a UTXO record for every transaction output so that
// api.utxoMissingReason can later distinguish "never existed" from "already
// spent or burned" from "originated in a now-pruned block".  The records are
// intentionally never deleted (no DeleteUTXO call) so that spent and staked
// outputs remain queryable after they leave the active set.
func storeBlock(db *store.DB, b *core.Block) error {
        data, err := json.Marshal(b)
        if err != nil {
                return fmt.Errorf("marshal block: %w", err)
        }
        hash := b.Hash()
        if err := db.PutRawBlock(hash, b.Header.Height, data); err != nil {
                return err
        }
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
                        if err := db.PutUTXO(txHash, uint32(i), su); err != nil {
                                return fmt.Errorf("put utxo (height %d, tx %x, idx %d): %w",
                                        b.Header.Height, txHash[:4], i, err)
                        }
                }
        }
        return nil
}
