// Command node is the Aperod blockchain node process.
package main

import (
        "bufio"
        "encoding/json"
        "fmt"
        "log/slog"
        "net/http"
        _ "net/http/pprof" // registers /debug/pprof/* handlers on http.DefaultServeMux
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
        strictMemLimit := false
        resetTip := false
        repairDB := false
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
                case "--strict-memlimit":
                        strictMemLimit = true
                case "--reset-tip":
                        resetTip = true
                case "--repair-db":
                        repairDB = true
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

        // Guard: warn (or hard-fail in strict mode) when GOMEMLIMIT is unset or
        // zero.  Without a memory limit the Go runtime can exhaust all available
        // RAM — the exact OOM scenario the drop-in was introduced to prevent.
        // Pass the drop-in path so the function can suggest the canonical fix.
        //
        // If the operator has set memory_limit_bytes in node.yaml and GOMEMLIMIT
        // is absent from the environment, apply the config value in-process now
        // (before the guard runs) so the guard sees it as satisfied.
        configMemLimitApplied := false
        if cfg.MemoryLimitBytes > 0 && os.Getenv("GOMEMLIMIT") == "" {
                debug.SetMemoryLimit(cfg.MemoryLimitBytes)
                configMemLimitApplied = true
                log.Info("memory limit applied from config",
                        "memory_limit_bytes", cfg.MemoryLimitBytes,
                )
        }

        // Apply GC target percentage from config.  The Go runtime default is
        // 100 (GC when heap doubles); 50 keeps peak RSS lower at the cost of
        // ~10-15 % more GC CPU — an acceptable trade-off for a validator node
        // that must stay below GOMEMLIMIT for many hours between restarts.
        gcPct := cfg.GCPercent
        if gcPct == 0 {
                gcPct = 50 // production default; operators can override in node.yaml
        }
        if gcPct > 0 {
                prev := debug.SetGCPercent(gcPct)
                log.Info("GC target percentage set",
                        "gc_percent", gcPct,
                        "previous", prev,
                )
        }
        if err := checkGOMLEMLIMIT(
                os.Getenv("GOMEMLIMIT"),
                configMemLimitApplied,
                strictMemLimit,
                "/etc/systemd/system/aperod-node.service.d/gomemlimit.conf",
                log,
        ); err != nil {
                return err
        }

        // Emit any non-fatal configuration warnings now that the logger is ready.
        for _, w := range cfg.Warnings() {
                log.Warn("config warning", "msg", w)
        }

        // ── 3. Open storage ───────────────────────────────────────────────────────
        if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
                return fmt.Errorf("create data dir: %w", err)
        }

        // Remove any orphaned snapshot .tmp files left by a previous crash
        // regardless of which startup path follows (genesis or resume).
        // It is safe to run here because:
        //   • the data dir now exists (MkdirAll succeeded above)
        //   • no snapshot read or write has started yet
        //   • the genesis branch never called this, so a crash during genesis
        //     initialisation would otherwise accumulate stale .tmp files that
        //     are never removed.
        cleanStaleSnapshotTmpFiles(cfg.DataDir, log)

        dbPath := cfg.DataDir + "/chain.db"
        var db *store.DB
        if repairDB {
                // --repair-db: call leveldb.RecoverFile which rebuilds the on-disk
                // SST index from the WAL, fixing corrupted entries that survive
                // normal Put/putSync writes because the SST has a higher sequence
                // number than the WAL entry in the MANIFEST.
                log.Info("--repair-db: running LevelDB recovery (RecoverFile)", "path", dbPath)
                db, err = store.Recover(dbPath)
                if err != nil {
                        return fmt.Errorf("recover store: %w", err)
                }
        } else {
                db, err = store.Open(dbPath)
                if err != nil {
                        return fmt.Errorf("open store: %w", err)
                }
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

        // Build the initial validator set used to seed the registry and the
        // consensus engine's static fallback list.
        //
        // Non-validator mode: use the public keys declared in the genesis config.
        // This ensures handleIncomingBlock's isKnownValidator() and proposer-slot
        // checks recognise the real network validators so the node can sync.
        // Our own (P2P-identity) key must NOT be in this list or it would be
        // treated as a genesis validator with stake, which could cause it to win
        // a proposer slot even though MyKey is nil in the engine.
        //
        // Validator mode: override with our own key (single-validator / testnet
        // design where this node IS the only proposer).
        validators, err := buildValidatorList(cfg.Consensus.NonValidator, genesisConfig.Validators, myKey)
        if err != nil {
                return fmt.Errorf("build validator list: %w", err)
        }
        if cfg.Consensus.NonValidator {
                log.Info("non-validator mode: registry seeded from genesis config",
                        "genesis_validators", len(validators))
        }

        // ── 6. Initialize chain ───────────────────────────────────────────────────
        chain := core.NewChain()
        mempool := core.NewMempool(core.DefaultMempoolConfig(), log)

        // Create the UTXO set here (before chain loading) so the resume path
        // can populate it from stored blocks, ensuring historical spent key
        // images are known to TxVerifier before the first peer block arrives.
        utxos := core.NewUTXOSet()
        // Wire the spent-UTXO index callback so ApplyBlock (called during the
        // startup scan and live block acceptance) keeps the su/ index current.
        // Non-fatal on DB error — the index is a startup-performance optimisation.
        utxos.OnUTXOSpent = func(txHash crypto.Hash32, outIdx uint32) {
                if spentErr := db.MarkUTXOSpent(txHash, outIdx); spentErr != nil {
                        log.Warn("failed to persist spent UTXO to index",
                                "out_idx", outIdx, "err", spentErr)
                }
        }

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
                apiSrv.SetDataDir(cfg.DataDir)
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
                // Non-validator nodes must never create their own genesis block.
                // CreateGenesisBlock embeds the caller's ValidatorPub, so a node
                // that uses its P2P-identity key here ends up on a different fork
                // than the real network with no visible error.
                // Require an existing chain.db obtained from a validator node instead.
                if cfg.Consensus.NonValidator {
                        return fmt.Errorf(
                                "non_validator mode requires an existing chain; "+
                                        "copy chain.db from a validator node before starting "+
                                        "(expected genesis config: %s)",
                                cfg.Genesis.File,
                        )
                }
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

                // ── Startup integrity check ──────────────────────────────────────────
                // Cross-verify the tip pointer against the canonical height index.
                // db.GetTip() returns the hash that was last written as the chain tip,
                // while h/<tipHeight> in the height index records the canonical block
                // hash accepted at that height. A mismatch means the tip pointer is
                // stale or corrupt (e.g. after a chain-tip reset bug where c.tip was
                // left at genesis while the DB tip had advanced to 973102, causing
                // blocks to be silently overwritten). Hard-fail here so the operator
                // can intervene before any block production begins.
                indexedBlock, idxErr := db.GetBlockByHeight(tipHeight)
                if idxErr != nil {
                        return fmt.Errorf("startup integrity check: read height index at %d: %w", tipHeight, idxErr)
                }
                integrityOK := true
                if indexedBlock == nil {
                        // Always repair: missing height-index entry is safe to fix from
                        // the tip pointer (e.g. rsync bootstrap, OOM-kill SST corruption).
                        // Validators get a more prominent warning; hard-failing here risks
                        // permanent downtime when the SST corruption makes putSync writes
                        // non-persistent across reopens.
                        warnMsg := "startup integrity check: height index has no entry for tip height — repairing from tip pointer"
                        if !cfg.Consensus.NonValidator {
                                warnMsg = "INTEGRITY WARNING (validator): height index missing for tip height — " +
                                        "repairing from tip pointer; run --repair-db if this recurs on every restart"
                        }
                        log.Warn(warnMsg, "tip_height", tipHeight, "tip_hash", fmt.Sprintf("%x", tipHash[:8]))
                        if repErr := db.RepairHeightIndex(tipHeight, tipHash); repErr != nil {
                                log.Warn("startup integrity: height index repair failed — continuing without repair", "err", repErr)
                        } else {
                                log.Info("startup integrity: height index repaired", "height", tipHeight, "hash", fmt.Sprintf("%x", tipHash[:8]))
                        }
                        if resetTip || repairDB {
                                fmt.Printf("aperod-node: height index repaired at height %d — start normally (without --reset-tip / --repair-db)\n", tipHeight)
                                return nil
                        }
                        integrityOK = false
                } else if indexedBlock.Hash != tipHash {
                        // Always repair: hash mismatch means SST entry is stale or
                        // corrupt.  Tip pointer is the authoritative source of truth.
                        warnMsg := "startup integrity check: tip pointer hash does not match height index — repairing from tip pointer"
                        if !cfg.Consensus.NonValidator {
                                warnMsg = "INTEGRITY WARNING (validator): height index hash mismatch at tip height — " +
                                        "repairing from tip pointer; run --repair-db if this recurs on every restart"
                        }
                        log.Warn(warnMsg,
                                "tip_height", tipHeight,
                                "tip_pointer_hash", fmt.Sprintf("%x", tipHash[:8]),
                                "height_index_hash", fmt.Sprintf("%x", indexedBlock.Hash[:8]))
                        if repErr := db.RepairHeightIndex(tipHeight, tipHash); repErr != nil {
                                log.Warn("startup integrity: height index repair failed — continuing without repair", "err", repErr)
                        } else {
                                log.Info("startup integrity: height index repaired", "height", tipHeight, "hash", fmt.Sprintf("%x", tipHash[:8]))
                        }
                        if resetTip || repairDB {
                                fmt.Printf("aperod-node: height index repaired at height %d — start normally (without --reset-tip / --repair-db)\n", tipHeight)
                                return nil
                        }
                        integrityOK = false
                } else if resetTip || repairDB {
                        // Height index is already correct; nothing to repair.
                        fmt.Printf("aperod-node: height index already consistent at height %d — start normally\n", tipHeight)
                        return nil
                }
                if integrityOK {
                        log.Info("startup integrity check passed", "height", tipHeight, "hash", fmt.Sprintf("%x", tipHash[:8]))
                }

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
                // Uses loadRecentBlocksFromStore (resume.go) which continues past
                // any missing heights rather than aborting early.
                recentBlocks := loadRecentBlocksFromStore(db, tipHeight, log)
                startLoad := uint64(1)
                if tipHeight >= core.MaxInMemoryBlocks {
                        startLoad = tipHeight - core.MaxInMemoryBlocks + 1
                }

                // ── Store integrity check (in-memory window) ──────────────────────
                // Scan the same height window for missing LevelDB entries.
                // Missing blocks are non-fatal at this stage — the startup scan has
                // its own MaxMissingBlocks guard — but we emit a structured WARNING
                // immediately so operators know the extent of any damage without
                // waiting for the scan to reach the affected heights hours later.
                // The count is also published on /api/v1/status so the admin panel
                // can surface it without requiring log access.
                {
                        missingCount, firstMissing, lastMissing := countMissingBlocksInWindow(db, tipHeight)
                        if missingCount > 0 {
                                log.Warn("store integrity warning",
                                        "missing_blocks", missingCount,
                                        "first_missing", firstMissing,
                                        "last_missing",  lastMissing,
                                )
                        }
                        if apiSrv != nil {
                                apiSrv.SetStoreMissingBlocks(int64(missingCount))
                        }
                }

                // Fast-forward the in-memory chain.  If the persistent tx-index
                // exists we use FastForwardWithIndex, which skips the expensive
                // tx.Hash() recomputation (saves ~5-6 min of CPU at startup).
                // On the very first restart after upgrade the index is empty and
                // we fall back to FastForward; after that first boot every
                // subsequent start is instant.
                dbTxIdx, txIdxErr := db.LoadTxIndex(startLoad)
                if txIdxErr == nil && dbTxIdx != nil {
                        // Convert store entries to core entries.
                        coreTxIdx := make(map[crypto.Hash32]core.TxIndexEntry, len(dbTxIdx))
                        for h, e := range dbTxIdx {
                                coreTxIdx[h] = core.TxIndexEntry{Height: e.Height, TxIdx: e.TxIdx}
                        }
                        chain.FastForwardWithIndex(recentBlocks, coreTxIdx)
                        log.Info("tx index restored from db (fast path)",
                                "entries", len(coreTxIdx),
                        )
                } else {
                        // Slow path: compute tx.Hash() for every transaction.
                        // Also write index entries to DB so next restart is fast.
                        chain.FastForward(recentBlocks)
                        var backfillErr error
                        for _, blk := range recentBlocks {
                                for i, tx := range blk.Txs {
                                        txHash := tx.Hash()
                                        if err := db.PutTxIdx(txHash, blk.Header.Height, i); err != nil {
                                                backfillErr = err
                                                break
                                        }
                                }
                                if backfillErr != nil {
                                        break
                                }
                        }
                        if backfillErr != nil {
                                log.Warn("tx index backfill failed; next restart will recompute",
                                        "err", backfillErr)
                        } else {
                                log.Info("tx index backfilled for future fast-path restarts",
                                        "blocks", len(recentBlocks))
                        }
                }

                log.Info("chain restored from storage",
                        "tip_height", tipHeight,
                        "blocks_loaded_in_memory", len(recentBlocks),
                )

                // Safety guard: delegate to anchorTipIfNeeded (resume.go) so the
                // logic is unit-tested independently of the full node startup path.
                if err := anchorTipIfNeeded(chain, db, tipHeight, log); err != nil {
                        return err
                }

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
                //
                // Clean up any orphaned .tmp files left by a previous crash
                // BEFORE the load so the directory contains only complete files.
                cleanStaleSnapshotTmpFiles(cfg.DataDir, log)
                snapLoaded := false
                {
                        tipHashHex := fmt.Sprintf("%x", tipHash[:])
                        if snap, snapIsRelaxed, serr := loadStartupSnapshotWithFallback(cfg.DataDir, tipHeight, tipHashHex, log); serr == nil {
                                // ── UTXO count divergence check ──────────────────────────────────
                                // See checkSnapshotUTXOCount in snapshot_utxo_check.go for the
                                // full logic.  isRelaxed widens the tolerance to 100 % when the
                                // snapshot was loaded via the relaxed-hash recovery path.
                                utxoCountOK := checkSnapshotUTXOCount(
                                        db,
                                        len(snap.UTXOs.ActiveUTXOs),
                                        tipHashHex,
                                        cfg.Snapshot.UTXOCountTolerancePct,
                                        snapIsRelaxed,
                                        cfg.Consensus.NonValidator,
                                        log,
                                )

                                if utxoCountOK {
                                        utxos.RestoreFromSnapshot(snap.UTXOs)
                                        registry.RestoreFromSnapshot(snap.Registry)
                                        registry.SetUTXOSet(utxos) // re-wire pointer (not serialised)
                                        if snap.TxTotal > 0 {
                                                initialTxTotal = snap.TxTotal
                                        }
                                        log.Info("startup fast path complete — snapshot loaded",
                                                "tip_height", tipHeight,
                                                "active_utxos", len(snap.UTXOs.ActiveUTXOs),
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
                        } else {
                                // Emit a structured log entry that distinguishes "no snapshot"
                                // (first run / new install) from "corrupt snapshot" (SIGKILL
                                // victim or truncated write).  Operators can filter journalctl
                                // output by startup_reason= to see why a long scan was triggered.
                                logSnapshotStartupReason(serr, tipHeight, log)
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

                // ── DB-index fast path (UTXOFromIndex) ──────────────────────────
                // When the key-image index, spent-UTXO index (su/), and stake-block
                // index (sb/) are all populated, rebuild the active UTXO set from
                // the DB directly — no full block scan required.  On first startup
                // after deploying this feature (no su/ or sb/ entries yet), the
                // normal block scan runs and backfills both indexes for next time.
                var (
                        utxoFromIndex      bool
                        stakeBlockHeights  []uint64
                )
                if kiFromIndex && tipHeight > 0 {
                        suSize, suErr := db.SpentUTXOIndexSize()
                        hasSb, sbErr := db.HasStakeBlockIndex()
                        if suErr == nil && sbErr == nil && suSize > 0 && hasSb {
                                // Load active UTXOs from DB (all u/ entries not in su/).
                                activeCount := 0
                                iterErr := db.IterActiveUTXOs(func(su *store.StoredUTXO) error {
                                        utxos.Add(&core.UTXO{
                                                TxHash:       su.TxHash,
                                                OutputIndex:  su.OutputIndex,
                                                OneTimePub:   su.OneTimePub,
                                                TxPubKey:     su.TxPubKey,
                                                AmountCommit: su.AmountCommit,
                                                EncAmount:    su.EncAmount,
                                                BlockHeight:  su.BlockHeight,
                                        })
                                        activeCount++
                                        return nil
                                })
                                if iterErr == nil && activeCount > 0 {
                                        // Collect stake block heights for registry replay.
                                        sbErr2 := db.IterStakeBlockHeights(func(h uint64) error {
                                                if h <= tipHeight {
                                                        stakeBlockHeights = append(stakeBlockHeights, h)
                                                }
                                                return nil
                                        })
                                        if sbErr2 == nil {
                                                utxoFromIndex = true
                                                log.Info("active UTXO set loaded from db index",
                                                        "active_utxos", activeCount,
                                                        "stake_blocks", len(stakeBlockHeights),
                                                )
                                        }
                                }
                                if !utxoFromIndex {
                                        // Partial load failed — clear and fall back to block scan.
                                        utxos = core.NewUTXOSet()
                                        utxos.OnUTXOSpent = func(txHash crypto.Hash32, outIdx uint32) {
                                                if spentErr := db.MarkUTXOSpent(txHash, outIdx); spentErr != nil {
                                                        log.Warn("failed to persist spent UTXO", "err", spentErr)
                                                }
                                        }
                                        log.Warn("db-index fast path unavailable; falling back to block scan",
                                                "tip_height", tipHeight)
                                }
                        }
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
                        UTXOFromIndex:         utxoFromIndex,
                        StakeBlockHeights:     stakeBlockHeights,
                        InitTxTotal:           initialTxTotal,
                        Log:                   log,
                        UTXOCountTolerancePct: cfg.Snapshot.UTXOCountTolerancePct,
                        CheckpointInterval:    cfg.Snapshot.ScanCheckpointInterval,
                        MaxMissingBlocks:      cfg.Snapshot.MaxMissingBlocks,
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

        // Non-validator mode: pass nil key to the engine so it never produces or
        // signs blocks.  The key is still used for API identity and P2P NodeID.
        consensusMyKey := myKey
        if cfg.Consensus.NonValidator {
                log.Info("non-validator mode — block production disabled; node will sync and relay only")
                consensusMyKey = nil
        }

        engine = consensus.NewEngine(consensus.Config{
                BlockTime:          cfg.Consensus.BlockTime,
                BFTThreshold:       genesisConfig.BFTThreshold,
                Validators:         validators,
                Registry:           registry,
                MyKey:              consensusMyKey,
                RewardAddress:      cfg.Consensus.RewardAddress,
                BlockRewardNAPR:    cfg.Consensus.BlockRewardNAPR,
                StakingPoolNAPR:    cfg.Consensus.StakingPoolNAPR,
                TailRewardNAPR:     cfg.Consensus.TailRewardNAPR,
                Store:              db,
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
                        // Index stake-bearing blocks for the db-index fast-path startup scan.
                        for _, tx := range block.Txs {
                                if tx.IsStake() {
                                        if sbErr := db.PutStakeBlockHeight(block.Header.Height); sbErr != nil {
                                                log.Warn("failed to index stake block",
                                                        "height", block.Header.Height, "err", sbErr)
                                        }
                                        break
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

                        // Draw one block-reward from the staking pool so the balance
                        // tracks the real chain state (runs for local + peer blocks).
                        engine.DecrementPool(h)

                        // Periodically force GC and return freed pages to the OS so that
                        // RSS does not grow unboundedly between snapshot saves.  The Go
                        // runtime only returns heap pages on GC; without an explicit call
                        // during steady-state sync, RSS drifts upward until GOMEMLIMIT
                        // causes GC thrash.  Running in a goroutine keeps the block
                        // acceptance path non-blocking.
                        //
                        // 500 blocks ≈ 25 min at 1 block/3 s.  Previous value (5000)
                        // fired every ~4.2 h, allowing 1–2 GB of RSS drift between
                        // collections on a chain producing one block every 3 seconds.
                        const gcEveryBlocks = uint64(500)
                        if h > 0 && h%gcEveryBlocks == 0 {
                                go func() {
                                        runtime.GC()
                                        debug.FreeOSMemory() // return freed pages to OS so GOMEMLIMIT has headroom
                                }()
                        }

                        // Persist spent key images and stake-block heights for every
                        // accepted canonical block.  OnBlockProduced does the same for
                        // locally-produced blocks; this call covers blocks received via
                        // P2P sync so that pure relay/sync nodes also populate the DB
                        // indexes required by the fast-path startup (IterKeyImages /
                        // HasStakeBlockIndex).  Non-fatal — the block is already committed.
                        for _, tx := range block.Txs {
                                for _, inp := range tx.Inputs {
                                        if kiErr := db.MarkKeyImageSpent(inp.KeyImage); kiErr != nil {
                                                log.Warn("failed to index key image",
                                                        "height", h, "err", kiErr)
                                        }
                                }
                        }
                        for _, tx := range block.Txs {
                                if tx.IsStake() {
                                        if sbErr := db.PutStakeBlockHeight(h); sbErr != nil {
                                                log.Warn("failed to index stake block",
                                                        "height", h, "err", sbErr)
                                        }
                                        break
                                }
                        }

                        interval := cfg.Snapshot.PeriodicSnapshotInterval
                        // interval == 0 means periodic snapshots are disabled (shutdown-only).
                        if interval == 0 || h == 0 || h%interval != 0 {
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
                                periodicSaveStart := time.Now()
                                if saveErr := saveStartupSnapshot(cfg.DataDir, snap); saveErr != nil {
                                        log.Warn("failed to save periodic snapshot",
                                                "height", height, "err", saveErr)
                                } else {
                                        log.Info("periodic snapshot saved",
                                                "height", height,
                                                "save_duration", time.Since(periodicSaveStart).Round(time.Millisecond).String(),
                                        )
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
                                // Explicitly release the deep-copy snapshot data so the GC can
                                // reclaim it immediately rather than waiting for the next
                                // scheduled collection.  The disk write above is complete at this
                                // point, so there is no remaining reference to snap's slices.
                                snap.UTXOs = core.UTXOSnapshot{}
                                snap.Registry = core.RegistrySnapshot{}
                                runtime.GC()
                                debug.FreeOSMemory() // return freed pages to OS so GOMEMLIMIT has headroom
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

        // Atomically clean stale .tmp then restore the mempool — order is enforced
        // by startupLoadMempool so neither step can be accidentally removed.
        if n := startupLoadMempool(cfg.DataDir, mempool, log); n > 0 {
                log.Info("mempool: restored pending transactions from disk", "count", n)
        }

        // ── 9. Start subsystems ───────────────────────────────────────────────────
        stop := make(chan struct{})
        // memPressureCh receives a signal when the memory watchdog detects that
        // RSS is approaching GOMEMLIMIT; the signal loop treats this the same as
        // SIGTERM so the snapshot can be saved while there is still headroom.
        memPressureCh := make(chan struct{}, 1)
        // engineDone is closed when engine.Run returns so the shutdown path can
        // wait for the engine to fully stop before reading the DB tip and saving
        // the snapshot.  Without this wait the engine can produce a block AFTER
        // we read the tip but BEFORE close(stop), writing the snapshot at height H
        // while the DB tip advances to H+1 — causing the next startup to reject
        // the snapshot and fall back to a full block scan.
        engineDone := make(chan struct{})
        go func() {
                engine.Run(stop)
                close(engineDone)
        }()

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

        // Memory watchdog: monitor RSS every 20 s and trigger a proactive
        // graceful restart when memory approaches GOMEMLIMIT.  A graceful
        // restart saves the snapshot while there is still headroom; a
        // SIGKILL (TimeoutStopSec expiry) does not — leading to a growing
        // snapshot/gap cycle that makes each subsequent run shorter.
        //
        // Threshold: 65 % of GOMEMLIMIT.  At that point the UTXO set is
        // small enough that TakeSnapshot() + gzip fits in the remaining 35 %
        // (≈ 2.45 GB on a 7 GB limit) without triggering GC thrash.
        {
                limit := debug.SetMemoryLimit(-1) // -1 = query without changing
                if limit > 0 {
                        threshold := uint64(limit) * 65 / 100
                        go func() {
                                t := time.NewTicker(20 * time.Second)
                                defer t.Stop()
                                var fired bool
                                for {
                                        select {
                                        case <-stop:
                                                return
                                        case <-t.C:
                                                if fired {
                                                        continue
                                                }
                                                rss := readRSSBytes()
                                                if rss <= 0 || uint64(rss) < threshold {
                                                        continue
                                                }
                                                log.Warn("memory watchdog: RSS approaching GOMEMLIMIT — initiating proactive graceful restart",
                                                        "rss_bytes", rss,
                                                        "threshold_bytes", threshold,
                                                        "gomemlimit_bytes", limit,
                                                )
                                                fired = true
                                                select {
                                                case memPressureCh <- struct{}{}:
                                                default:
                                                }
                                        }
                                }
                        }()
                }
        }

        if apiSrv != nil {
                // Wire engine-dependent options now that the consensus engine exists.
                apiSrv.SetRegistry(engine.Registry())
                apiSrv.SetValidatorKey(myKey)
                apiSrv.SetTxTotal(initialTxTotal)
                apiSrv.SetTimestampRejectedCounter(func() int64 { return engine.TimestampRejectedCount() })
                apiSrv.SetStakingPoolFn(func() (uint64, uint64, string) {
                        return engine.StakingPoolRemaining(), engine.StakingPoolInit(), engine.RewardMode()
                })
                // Startup scan is complete — mark the node ready for UTXO queries.
                apiSrv.SetReady()
                log.Info("API server ready (startup scan complete)")
                // Keep utxo_rebuilding=true for 90 s after the scan completes.
                // Wallets see this flag and suppress false "0 balance" / "Потрачен"
                // displays that occur when the UTXO index is still settling.
                go func() {
                        time.Sleep(90 * time.Second)
                        apiSrv.SetUTXOReady()
                        log.Info("UTXO rebuild window closed — live UTXO queries enabled")
                }()
        }

        // ── 9. Start P2P networking ───────────────────────────────────────────────
        if cfg.P2P.ListenAddr != "" {
                tcpAddr := parseP2PAddr(cfg.P2P.ListenAddr)

                // Validate and normalise bootnode addresses at startup.
                // NormalizeBootnodeAddr checks syntax only (no DNS) — multiaddr
                // literals (/ip4/, /ip6/) are converted to host:port; DNS names
                // are kept as-is so the P2P host can re-resolve them periodically.
                // A malformed entry (e.g. /ip6/badaddr with no /tcp/ component)
                // logs a clear warning here rather than producing a confusing
                // OS-level dial error deep inside the P2P layer.
                bootnodes := make([]string, 0, len(cfg.P2P.Bootnodes))
                for _, bn := range cfg.P2P.Bootnodes {
                        normalized, err := p2p.NormalizeBootnodeAddr(bn)
                        if err != nil {
                                log.Warn("bootnode address is invalid and will be skipped — fix node.yaml to restore connectivity",
                                        "bootnode", bn, "err", err)
                                continue
                        }
                        // Skip self-connections (same port as our listener).
                        if normalized != tcpAddr {
                                bootnodes = append(bootnodes, normalized)
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
                        // Default the ban-file path to <data_dir>/p2p_bans.json when
                        // the operator has not set p2p.ban_file in node.yaml.
                        // An explicit "-" disables persistence (useful for unit tests).
                        banFilePath := cfg.P2P.BanFile
                        if banFilePath == "" {
                                banFilePath = filepath.Join(cfg.DataDir, "p2p_bans.json")
                        }
                        // Default the whitelist-file path to <data_dir>/p2p_whitelist.json.
                        // An explicit "-" disables persistence.
                        whitelistFilePath := cfg.P2P.WhitelistFile
                        if whitelistFilePath == "" {
                                whitelistFilePath = filepath.Join(cfg.DataDir, "p2p_whitelist.json")
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
                                SelfFingerprint:      nodeFingerprint,
                                AllowedPeers:         cfg.P2P.AllowedPeers,
                                PeerWhitelist:        cfg.P2P.PeerWhitelist,
                                MaxPendingHandshakes: cfg.P2P.MaxPendingHandshakes,
                                BadBlockHeightLead:   cfg.P2P.BadBlockHeightLead,
                                BadBlockBanThreshold: cfg.P2P.BadBlockBanThreshold,
                                BadBlockBanDuration:  cfg.P2P.BadBlockBanDuration,
                                BanFile:              banFilePath,
                                WhitelistFile:        whitelistFilePath,
                        }, handler, log)
                        if len(cfg.P2P.PeerWhitelist) > 0 {
                                log.Info("peer IP whitelist active — only listed IPs may connect inbound",
                                        "entries", len(cfg.P2P.PeerWhitelist))
                        }
                        if len(cfg.P2P.AllowedPeers) > 0 {
                                log.Info("allow-list active",
                                        "allowed_peers", len(cfg.P2P.AllowedPeers))
                                if len(bootnodes) == 0 {
                                        log.Warn("allow-list is active but no bootnodes are configured — node may not connect to any peers",
                                                "allowed_peers", len(cfg.P2P.AllowedPeers))
                                }
                        }

                        // Wire the chain as the header provider so GetHeaders
                        // requests from syncing peers return real block headers
                        // instead of an empty response.
                        host.SetHeaderProvider(chain)

                        // Wire LevelDB-backed fallbacks so peers that are more
                        // than ringSize blocks behind can still sync: blocks
                        // evicted from the in-memory ring are served from disk.
                        host.SetBlockFetcher(
                                func(hash crypto.Hash32) *core.Block {
                                        raw, err := db.GetRawBlock(hash)
                                        if err != nil || raw == nil {
                                                return nil
                                        }
                                        var b core.Block
                                        if err := json.Unmarshal(raw, &b); err != nil {
                                                return nil
                                        }
                                        return &b
                                },
                                func(height uint64) *core.Block {
                                        raw, err := db.GetRawBlockByHeight(height)
                                        if err != nil || raw == nil {
                                                return nil
                                        }
                                        var b core.Block
                                        if err := json.Unmarshal(raw, &b); err != nil {
                                                return nil
                                        }
                                        return &b
                                },
                        )

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
                                // Wire the live-reload whitelist functions so the Admin
                                // Panel can add/remove entries without a node restart.
                                apiSrv.SetWhitelistGetFunc(host.GetPeerWhitelist)
                                apiSrv.SetWhitelistAddFunc(host.AddToWhitelist)
                                apiSrv.SetWhitelistRemoveFunc(host.RemoveFromWhitelist)
                                // Wire whitelist-exemption event log for the Admin Panel.
                                apiSrv.SetWhitelistExemptFunc(func(since time.Time) []api.WhitelistExemptionEntry {
                                        evts := host.GetWhitelistExemptions(since)
                                        out := make([]api.WhitelistExemptionEntry, len(evts))
                                        for i, e := range evts {
                                                out[i] = api.WhitelistExemptionEntry{
                                                        IP:          e.IP,
                                                        PeerAddr:    e.PeerAddr,
                                                        BlockHeight: e.BlockHeight,
                                                        OurTip:      e.OurTip,
                                                        At:          e.At,
                                                }
                                        }
                                        return out
                                })
                                // Seed the static /api/v1/status display from current live list.
                                apiSrv.SetPeerWhitelist(host.GetPeerWhitelist())
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

        // ── 10. pprof diagnostic endpoint (optional, localhost-only) ──────────────
        // Exposes /debug/pprof/* on a dedicated port so operators can capture CPU
        // and heap profiles without rebuilding the binary.  Disabled by default;
        // set pprof.enabled: true in node.yaml to activate.
        //
        // Security: binds exclusively to the address in pprof.listen_addr, which
        // MUST be a loopback address (127.0.0.1).  The handler is registered on a
        // fresh http.ServeMux so it is entirely isolated from the API server mux.
        if cfg.Pprof.Enabled {
                pprofAddr := cfg.Pprof.ListenAddr
                if pprofAddr == "" {
                        pprofAddr = "127.0.0.1:8546"
                }
                go func() {
                        // The `_ "net/http/pprof"` import registers all /debug/pprof/*
                        // handlers on http.DefaultServeMux as a side effect; we serve
                        // that mux here on its own dedicated port.
                        pprofSrv := &http.Server{
                                Addr:    pprofAddr,
                                Handler: http.DefaultServeMux,
                        }
                        log.Info("pprof endpoint started", "addr", pprofAddr,
                                "hint", "go tool pprof http://"+pprofAddr+"/debug/pprof/profile?seconds=30")
                        if serveErr := pprofSrv.ListenAndServe(); serveErr != nil && serveErr != http.ErrServerClosed {
                                log.Warn("pprof server stopped", "err", serveErr)
                        }
                }()
        }

        // ── 11. Background pruning worker (light mode only) ────────────────────────
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

        // ── 12. Wait for signal ───────────────────────────────────────────────────
        // SIGHUP triggers a live config reload of snapshot.scan_checkpoint_interval
        // so operators can tune memory vs. crash-recovery speed without restarting
        // the node.  SIGINT / SIGTERM initiate a graceful shutdown as before.
        sig := make(chan os.Signal, 1)
        signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
        for {
                select {
                case s := <-sig:
                        if s == syscall.SIGHUP {
                                log.Info("SIGHUP received — reloading scan_checkpoint_interval from config", "config", cfgPath)
                                reloadScanCheckpointInterval(cfgPath, cfg, log)
                                continue
                        }
                        log.Info("signal received — shutting down", "signal", s)
                case <-memPressureCh:
                        log.Warn("memory watchdog triggered — performing graceful restart to preserve snapshot")
                }
                break
        }

        log.Info("shutting down...")

        performShutdown(stop, engineDone, db, utxos, registry, cfg.DataDir, log)

        // Persist pending mempool transactions so they survive the restart.
        if mpErr := mempool.Save(cfg.DataDir); mpErr != nil {
                log.Warn("shutdown: failed to save mempool", "err", mpErr)
        } else {
                log.Info("shutdown: mempool saved", "pending_txs", mempool.Count())
        }

        return nil
}

// performShutdown stops the consensus engine and saves a startup snapshot so
// the next restart can skip the full block scan.
//
// Ordering guarantee (MUST NOT be changed):
//
//  1. close(stop)   — signal engine.Run to return; no new blocks can start
//  2. <-engineDone  — wait until engine.Run has fully returned and written its
//                     last block to the DB
//  3. db.GetTip()   — read the final, stable tip; safe because the engine is
//                     quiescent
//  4. save snapshot — snapshot height == DB tip height is now guaranteed
//
// If the ordering were reversed (GetTip before close+wait), the engine could
// produce block H+1 between GetTip and the snapshot rename, writing a snapshot
// at height H while the DB tip advances to H+1.  The next startup would find no
// snapshot for H+1 and fall back to a multi-hour block scan.
//
// The function is extracted from run() so it can be tested in isolation with a
// controllable fake engine (see snapshot_restart_test.go).
func performShutdown(
        stop chan struct{},
        engineDone <-chan struct{},
        db *store.DB,
        utxos *core.UTXOSet,
        registry *core.ValidatorRegistry,
        dataDir string,
        log *slog.Logger,
) {
        // Step 1 + 2: stop the engine and wait for full quiescence.
        // GetTip MUST NOT be called before this point.
        close(stop)
        <-engineDone

        // Step 3 + 4: read the final tip and save the snapshot.
        saveShutdownSnapshot(db, utxos, registry, dataDir, log)
}

// saveShutdownSnapshot reads the current DB tip and writes a gzip-compressed
// startup snapshot to dataDir.  The caller MUST have already stopped the engine
// (close(stop) + <-engineDone) before calling this function; see performShutdown.
func saveShutdownSnapshot(
        db *store.DB,
        utxos *core.UTXOSet,
        registry *core.ValidatorRegistry,
        dataDir string,
        log *slog.Logger,
) {
        if registry == nil {
                return
        }
        shutTipHash, shutTipHeight, stErr := db.GetTip()
        if stErr != nil || shutTipHeight == 0 {
                return
        }
        shutTxTotal, _ := db.LoadTxTotal()
        shutSnap := startupSnapshot{
                Version:    snapVersion,
                TipHeight:  shutTipHeight,
                TipHashHex: fmt.Sprintf("%x", shutTipHash[:]),
                TxTotal:    shutTxTotal,
                UTXOs:      utxos.TakeSnapshot(),
                Registry:   registry.TakeSnapshot(),
        }
        snapSaveStart := time.Now()
        if saveErr := saveStartupSnapshot(dataDir, shutSnap); saveErr != nil {
                log.Warn("shutdown: failed to save snapshot", "err", saveErr)
                return
        }
        snapSaveDur := time.Since(snapSaveStart).Round(time.Millisecond)
        log.Info("shutdown: snapshot saved",
                "tip_height", shutTipHeight,
                "save_duration", snapSaveDur.String(),
        )
        deleteOldSnapshots(dataDir, shutTipHeight)
        // Persist the active UTXO count keyed by tip hash so the
        // next restart's divergence check has an active-only reference
        // count specific to this snapshot.
        if metaErr := db.StoreActiveUTXOCount(shutSnap.TipHashHex, len(shutSnap.UTXOs.ActiveUTXOs)); metaErr != nil {
                log.Warn("shutdown: failed to persist active_utxo_count metadata", "err", metaErr)
        }
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

// readRSSBytes returns the process Resident Set Size in bytes by reading
// /proc/self/statm (Linux only).  The call does not stop the world, making it
// safe to call from a background goroutine.  Returns 0 when the file is
// unavailable (non-Linux environments, unit tests).
func readRSSBytes() int64 {
        data, err := os.ReadFile("/proc/self/statm")
        if err != nil {
                return 0
        }
        fields := strings.Fields(string(data))
        if len(fields) < 2 {
                return 0
        }
        pages, err := strconv.ParseInt(fields[1], 10, 64)
        if err != nil {
                return 0
        }
        return pages * 4096 // standard 4 KiB page size on Linux amd64/arm64
}

// parseGOMLEMLIMITBytes parses a GOMEMLIMIT string using exactly the grammar
// the Go runtime accepts.  No whitespace trimming or case-folding is applied —
// the runtime itself is strict about these.
//
// Supported forms:
//
//	""           → 0, false  (unset — no limit)
//	"off"        → 0, false  (exactly; runtime disables the limit)
//	"5368709120" → 5368709120, true   (bare byte count)
//	"5368709120B"→ 5368709120, true   (explicit byte suffix)
//	"512MiB"     → 536870912, true
//	"5GiB"       → 5368709120, true
//	"1TiB"       → 1099511627776, true
//
// Returns (bytes, ok).  ok is false when the value is absent, "off",
// parses to zero or negative, overflows int64, or uses an unrecognised format.
//
// Suffixes recognised by the Go 1.21+ runtime: B, KiB, MiB, GiB, TiB.
// PiB and EiB are NOT in the runtime grammar and are rejected here.
func parseGOMLEMLIMITBytes(raw string) (int64, bool) {
        if raw == "" || raw == "off" {
                return 0, false
        }

        // The Go runtime recognises exactly these unit suffixes (longest first
        // so that "KiB" is matched before the bare "B" suffix).
        type unit struct {
                suffix string
                mult   int64
        }
        units := []unit{
                {"TiB", 1 << 40},
                {"GiB", 1 << 30},
                {"MiB", 1 << 20},
                {"KiB", 1 << 10},
                {"B", 1},
        }

        numStr := raw
        mult := int64(1)
        for _, u := range units {
                if strings.HasSuffix(raw, u.suffix) {
                        numStr = raw[:len(raw)-len(u.suffix)]
                        mult = u.mult
                        break
                }
        }

        n, err := strconv.ParseInt(numStr, 10, 64)
        if err != nil || n <= 0 {
                return 0, false
        }

        // Guard against multiplication overflow (e.g. very large TiB values).
        const maxInt64 = 1<<63 - 1
        if mult > 1 && n > maxInt64/mult {
                return 0, false
        }

        return n * mult, true
}

// checkGOMLEMLIMIT inspects the GOMEMLIMIT environment variable and logs a
// prominent warning when it is absent, explicitly disabled ("off"), set to
// zero, or uses an unrecognised format.  If strictMode is true the function
// returns an error so the node exits with a non-zero status, preventing a
// silent start without a memory cap.
//
// Parameters:
//
//	gomlimitEnv          – value of os.Getenv("GOMEMLIMIT") (injected for testability)
//	configLimitApplied   – true when a positive memory_limit_bytes was read from
//	                        node.yaml and applied via debug.SetMemoryLimit before
//	                        this call; treated as equivalent to the env var being set
//	strictMode           – when true, treat a missing/zero limit as a fatal error
//	dropinPath           – canonical path of the systemd drop-in that sets GOMEMLIMIT;
//	                        included in the suggested fix message so operators know
//	                        exactly which file to recreate
//	log                  – structured logger
func checkGOMLEMLIMIT(gomlimitEnv string, configLimitApplied bool, strictMode bool, dropinPath string, log *slog.Logger) error {
        const warnMsg = "GOMEMLIMIT is not set — node may OOM under load"

        // A limit applied via node.yaml memory_limit_bytes satisfies the check
        // in the same way as a positive GOMEMLIMIT environment variable.
        if configLimitApplied {
                return nil
        }

        _, ok := parseGOMLEMLIMITBytes(gomlimitEnv)
        if ok {
                return nil // limit is set and positive — nothing to do
        }

        fix := fmt.Sprintf(
                "set 'memory_limit_bytes: <bytes>' in node.yaml, or recreate the drop-in at %s with 'Environment=\"GOMEMLIMIT=<bytes>\"' and run: systemctl daemon-reload && systemctl restart aperod-node",
                dropinPath,
        )

        if strictMode {
                // Log at Error level so the journal captures the reason before exit.
                log.Error(warnMsg,
                        "gomemlimit_value", gomlimitEnv,
                        "strict_mode", true,
                        "fix", fix,
                )
                return fmt.Errorf("refusing to start without GOMEMLIMIT (--strict-memlimit is set): %s", fix)
        }

        log.Warn(warnMsg,
                "gomemlimit_value", gomlimitEnv,
                "fix", fix,
        )
        return nil
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
        for i, tx := range b.Txs {
                txHash := tx.Hash()
                // Persist tx location so FastForwardWithIndex can restore the
                // in-memory tx index at startup without recomputing tx.Hash().
                if err := db.PutTxIdx(txHash, b.Header.Height, i); err != nil {
                        return fmt.Errorf("put tx index (height %d, tx %x): %w",
                                b.Header.Height, txHash[:4], err)
                }
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
