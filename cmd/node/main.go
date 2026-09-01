// Command node is the Aperod blockchain node process.
package main

import (
        "bufio"
        "crypto/tls"
        "encoding/json"
        "fmt"
        "io"
        "log/slog"
        "math"
        "math/rand/v2"
        "net/http"
        _ "net/http/pprof" // registers /debug/pprof/* handlers on http.DefaultServeMux
        "os"
        "os/signal"
        "path/filepath"
        "regexp"
        "runtime"
        "runtime/debug"
        "strconv"
        "sync/atomic"
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

// buildP2PConfig is the single production mapping from the loaded node.yaml
// configuration (config.Config) to the p2p host configuration (p2p.Config).
// It is factored out of run() so unit tests can invoke the REAL mapping —
// e.g. verifying that a raised p2p.get_block_stall_timeout (60s for slow
// links) actually reaches the p2p host instead of silently falling back to
// the built-in 15s default after a refactor.
func buildP2PConfig(
        cfg *config.Config,
        tcpAddr string,
        bootnodes []string,
        nodeID string,
        tlsCfg *tls.Config,
        nodeFingerprint string,
) p2p.Config {
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
        return p2p.Config{
                ListenAddr:           tcpAddr,
                Bootnodes:            bootnodes,
                MaxPeers:             cfg.P2P.MaxPeers,
                MinPeers:             cfg.P2P.MinPeers,
                MaxPeersPerIP:        cfg.P2P.MaxPeersPerIP,
                MinOutbound:          cfg.P2P.MinOutbound,
                NodeID:               nodeID,
                UserAgent:            "aperod-node/1.0",
                TLSConfig:            tlsCfg,
                SelfFingerprint:      nodeFingerprint,
                AllowedPeers:         cfg.P2P.AllowedPeers,
                PeerWhitelist:        cfg.P2P.PeerWhitelist,
                MaxPendingHandshakes: cfg.P2P.MaxPendingHandshakes,
                BadBlockHeightLead:    cfg.P2P.BadBlockHeightLead,
                BadBlockBanThreshold:  cfg.P2P.BadBlockBanThreshold,
                BadBlockBanDuration:   cfg.P2P.BadBlockBanDuration,
                TimestampBanThreshold: cfg.P2P.TimestampBanThreshold,
                TimestampBanDuration:  cfg.P2P.TimestampBanDuration,
                BanFile:               banFilePath,
                WhitelistFile:        whitelistFilePath,
                KeepaliveInterval:    cfg.P2P.KeepaliveInterval,
                MaxBlockIngestPerSec: cfg.P2P.MaxBlockIngestPerSec,
                TxRateBurst:          cfg.P2P.TxRateBurst,
                TxRateSustained:      cfg.P2P.TxRateSustained,
                TxRateBanThreshold:   cfg.P2P.TxRateBanThreshold,
                TxRateBanDuration:    cfg.P2P.TxRateBanDuration,
                MaxStaleBootnodeAge:  cfg.P2P.MaxStaleBootnodeAge,
                GetBlockStallTimeout: cfg.P2P.GetBlockStallTimeout,
                MaxDialBackoff:       cfg.P2P.MaxDialBackoff,
        }
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

// runCompactDB implements the --compact-db subcommand.
//
// Usage: aperod-node --compact-db --data-dir=<path>
//
// Opens chain.db, runs a full LevelDB compaction across the entire key range,
// and exits.  Compaction reclaims physical disk space freed by Delete/prune
// operations — it is the equivalent of VACUUM for LevelDB.  The node MUST be
// stopped before running this command.
//
// The operation is safe to interrupt (LevelDB stays consistent) but may take
// several minutes on large databases.  It prints before/after size estimates
// to stdout so operators can see how much space was recovered.
func runCompactDB() error {
        dataDir := ""
        args := os.Args[1:]
        for i, arg := range args {
                switch {
                case strings.HasPrefix(arg, "--data-dir="):
                        dataDir = strings.TrimPrefix(arg, "--data-dir=")
                case arg == "--data-dir" && i+1 < len(args):
                        dataDir = args[i+1]
                }
        }
        if dataDir == "" {
                return fmt.Errorf("--compact-db requires --data-dir=<path>")
        }

        absDataDir, err := filepath.Abs(dataDir)
        if err != nil {
                return fmt.Errorf("compact-db: resolve data dir: %w", err)
        }
        dbPath := filepath.Join(absDataDir, "chain.db")
        _, tipHeight, err := store.ReadTipOnly(dbPath)
        if err != nil {
                return fmt.Errorf("compact-db: inspect target before mutation: %w", err)
        }
        if err := maintenancePreflightResolved(
                os.Stderr, "(not used; --data-dir supplied explicitly)", absDataDir, tipHeight,
        ); err != nil {
                return err
        }

        // Measure disk use before compaction.
        sizeBefore := dirSize(dbPath)

        fmt.Fprintf(os.Stdout, "compact-db: opening %s (size before: %s)\n",
                dbPath, formatBytes(sizeBefore))

        db, err := store.Open(dbPath)
        if err != nil {
                return fmt.Errorf("compact-db: open %s: %w", dbPath, err)
        }

        fmt.Fprintf(os.Stdout, "compact-db: running full compaction (this may take several minutes)...\n")
        if err := db.Compact(); err != nil {
                db.Close()
                return fmt.Errorf("compact-db: compaction failed: %w", err)
        }
        db.Close()

        sizeAfter := dirSize(dbPath)
        saved := sizeBefore - sizeAfter
        fmt.Fprintf(os.Stdout,
                "compact-db: done — size after: %s, saved: %s\n",
                formatBytes(sizeAfter), formatBytes(saved))
        return nil
}

// dirSize returns the total byte size of all files under path, ignoring errors.
func dirSize(path string) int64 {
        var total int64
        entries, err := os.ReadDir(path)
        if err != nil {
                return 0
        }
        for _, e := range entries {
                info, err := e.Info()
                if err != nil {
                        continue
                }
                total += info.Size()
        }
        return total
}

// formatBytes returns a human-readable size string (KB / MB / GB).
func formatBytes(b int64) string {
        switch {
        case b >= 1<<30:
                return fmt.Sprintf("%.2f GB", float64(b)/float64(1<<30))
        case b >= 1<<20:
                return fmt.Sprintf("%.2f MB", float64(b)/float64(1<<20))
        case b >= 1<<10:
                return fmt.Sprintf("%.2f KB", float64(b)/float64(1<<10))
        default:
                return fmt.Sprintf("%d B", b)
        }
}

const maintenanceStaleMinGap uint64 = 10_000

func maintenancePaths(cfgPath, dataDir string) (string, string, error) {
        absConfig, err := filepath.Abs(cfgPath)
        if err != nil {
                return "", "", fmt.Errorf("resolve config path %q: %w", cfgPath, err)
        }
        absDataDir, err := filepath.Abs(dataDir)
        if err != nil {
                return "", "", fmt.Errorf("resolve data dir %q: %w", dataDir, err)
        }
        return filepath.Clean(absConfig), filepath.Clean(absDataDir), nil
}

func latestSnapshotHeight(dataDir string) uint64 {
        entries, err := os.ReadDir(dataDir)
        if err != nil {
                return 0
        }
        prefix := fmt.Sprintf("snapshot-v%d-", snapVersion)
        var highest uint64
        for _, entry := range entries {
                name := entry.Name()
                if !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, ".tmp") {
                        continue
                }
                heightText := strings.TrimPrefix(name, prefix)
                heightText = strings.TrimSuffix(heightText, "-prev.json.gz")
                heightText = strings.TrimSuffix(heightText, ".json.gz")
                height, parseErr := strconv.ParseUint(heightText, 10, 64)
                if parseErr == nil && height > highest {
                        highest = height
                }
        }
        return highest
}

// maintenancePreflight prints the exact chain selected for a destructive or
// long-running maintenance command and rejects a database tip that is far
// behind a snapshot stored in the same data directory.
func maintenancePreflight(out io.Writer, cfgPath, dataDir string, tipHeight uint64) error {
        absConfig, absDataDir, err := maintenancePaths(cfgPath, dataDir)
        if err != nil {
                return err
        }
        return maintenancePreflightResolved(out, absConfig, absDataDir, tipHeight)
}

func maintenancePreflightResolved(out io.Writer, resolvedConfig, absDataDir string, tipHeight uint64) error {
        snapshotHeight := latestSnapshotHeight(absDataDir)
        fmt.Fprintf(out,
                "\n================ MAINTENANCE PREFLIGHT ================\n"+
                        "resolved_config: %s\n"+
                        "absolute_data_dir: %s\n"+
                        "tip_height: %d\n"+
                        "latest_snapshot_height: %d\n"+
                        "=======================================================\n",
                resolvedConfig, absDataDir, tipHeight, snapshotHeight)

        if snapshotHeight <= tipHeight {
                return nil
        }
        gap := snapshotHeight - tipHeight
        staleGap := snapshotHeight / 10
        if staleGap < maintenanceStaleMinGap {
                staleGap = maintenanceStaleMinGap
        }
        if gap < staleGap {
                return nil
        }
        return fmt.Errorf(
                "MAINTENANCE REFUSED: database tip %d is %d blocks behind snapshot height %d in %s; "+
                        "this data directory appears stale or internally inconsistent — verify --config and data_dir before retrying",
                tipHeight, gap, snapshotHeight, absDataDir)
}

// runCheckStore implements the --check-store subcommand.
//
// Usage: aperod-node --check-store --data-dir=<path> [--max-missing=<n>]
//
// Opens the chain.db found inside <data-dir>, reads the stored tip height from
// the metadata, then iterates the height index to count missing block entries.
// Exits non-zero (and prints a human-readable error) when the count exceeds
// max-missing (default: 5000 — identical to the scan.go runtime default so
// the pre-flight check and the actual scan use the same threshold).
//
// This is intentionally config-free: operators call it right after an rsync
// when no full node.yaml may yet be present on the relay.
func runCheckStore() error {
        dataDir := ""
        maxMissing := uint64(5000) // mirrors the scan.go hardcoded default

        args := os.Args[1:]
        for i, arg := range args {
                switch {
                case strings.HasPrefix(arg, "--data-dir="):
                        dataDir = strings.TrimPrefix(arg, "--data-dir=")
                case arg == "--data-dir" && i+1 < len(args):
                        dataDir = args[i+1]
                case strings.HasPrefix(arg, "--max-missing="):
                        if v, parseErr := strconv.ParseUint(strings.TrimPrefix(arg, "--max-missing="), 10, 64); parseErr == nil {
                                maxMissing = v
                        }
                }
        }

        if dataDir == "" {
                return fmt.Errorf("--check-store requires --data-dir=<path>")
        }

        dbPath := filepath.Join(dataDir, "chain.db")
        db, err := store.Open(dbPath)
        if err != nil {
                return fmt.Errorf("check-store: open %s: %w", dbPath, err)
        }
        defer db.Close()

        _, tipHeight, err := db.GetTip()
        if err != nil {
                return fmt.Errorf("check-store: read tip: %w", err)
        }
        if tipHeight == 0 {
                fmt.Fprintf(os.Stdout, "check-store OK: empty store (tip_height=0)\n")
                return nil
        }

        missing, firstMissing, err := db.CountMissingHeights(tipHeight)
        if err != nil {
                return fmt.Errorf("check-store: scan height index: %w", err)
        }

        if missing > maxMissing {
                return fmt.Errorf(
                        "check-store: %d missing blocks (first missing: %d, tip: %d) "+
                                "exceeds threshold %d — "+
                                "the rsync'd chain.db has gaps consistent with a live-LevelDB copy; "+
                                "re-run bootstrap to obtain a consistent snapshot",
                        missing, firstMissing, tipHeight, maxMissing,
                )
        }

        fmt.Fprintf(os.Stdout,
                "check-store OK: tip_height=%d missing=%d (threshold=%d)\n",
                tipHeight, missing, maxMissing)
        return nil
}

// runCheckSnapshot implements the --check-snapshot subcommand.
//
// Usage: aperod-node --check-snapshot --data-dir=<path>
//
// Reads the canonical tip from chain.db and validates the matching startup
// snapshot through the same loader used by normal node startup. Missing,
// truncated, corrupt, or tip-mismatched snapshots cause a non-zero exit.
func runCheckSnapshot() error {
        dataDir := ""
        args := os.Args[1:]
        for i, arg := range args {
                switch {
                case strings.HasPrefix(arg, "--data-dir="):
                        dataDir = strings.TrimPrefix(arg, "--data-dir=")
                case arg == "--data-dir" && i+1 < len(args):
                        dataDir = args[i+1]
                }
        }
        if dataDir == "" {
                return fmt.Errorf("--check-snapshot requires --data-dir=<path>")
        }

        dbPath := filepath.Join(dataDir, "chain.db")
        tipHash, tipHeight, err := store.ReadTipOnly(dbPath)
        if err != nil {
                return fmt.Errorf("check-snapshot: read canonical tip from %s: %w", dbPath, err)
        }

        log := slog.New(slog.NewTextHandler(os.Stderr, nil))
        _, isRelaxed, err := tryLoadStartupSnapshot(
                dataDir, tipHeight, fmt.Sprintf("%x", tipHash), log,
        )
        if err != nil {
                return fmt.Errorf(
                        "check-snapshot: snapshot validation failed for tip height %d: %w",
                        tipHeight, err,
                )
        }
        if isRelaxed {
                return fmt.Errorf(
                        "check-snapshot: snapshot validation failed for tip height %d: "+
                                "backup snapshot was accepted only by relaxed hash recovery and does not match chain.db",
                        tipHeight,
                )
        }

        fmt.Fprintf(os.Stdout, "check-snapshot OK: tip_height=%d\n", tipHeight)
        return nil
}

// ─── Rsync-in-progress sentinel ──────────────────────────────────────────────
//
// join-network.sh writes .rsync-in-progress to the data directory before any
// rsync transfer begins and removes it once the transfer finishes.  The sentinel
// lets the node detect when it has been started against a partially-written
// LevelDB and refuse to proceed rather than silently diverge from the canonical
// chain.

// rsyncSentinelPath returns the path of the rsync-in-progress sentinel file.
func rsyncSentinelPath(dataDir string) string {
        return filepath.Join(dataDir, ".rsync-in-progress")
}

// checkRsyncSentinel returns a descriptive error when the rsync sentinel is
// present in dataDir, and nil when it is absent (safe to start).
func checkRsyncSentinel(dataDir string) error {
        sentinelPath := rsyncSentinelPath(dataDir)
        if _, statErr := os.Stat(sentinelPath); statErr == nil {
                return fmt.Errorf(
                        "startup blocked: %s exists\n"+
                                "This file is written by join-network.sh before rsync begins and\n"+
                                "removed only after a successful transfer.  Starting the node now\n"+
                                "would open a half-written LevelDB and produce a divergent chain.\n"+
                                "\n"+
                                "Options:\n"+
                                "  (a) Wait for join-network.sh to finish — it will remove the file.\n"+
                                "  (b) If no rsync is running, remove the file manually and retry:\n"+
                                "        rm %s",
                        sentinelPath, sentinelPath)
        }
        return nil
}

// ─── Height-index sentinel (file-based) ──────────────────────────────────────
//
// The sentinel is stored as a plain file at <dataDir>/height_index_verified,
// deliberately OUTSIDE the chain.db/ LevelDB directory.  Operators who
// bootstrap a relay node typically rsync only chain.db/:
//
//   rsync -a validator:/opt/aperod/data/testnet/chain.db/ relay:/opt/aperod/data/testnet/chain.db/
//
// Because the sentinel lives next to chain.db/ (not inside it) it is NOT
// copied by that rsync command.  The relay therefore starts without a sentinel
// on every fresh bootstrap, unconditionally triggering the repair sweep before
// any blocks are processed.
//
// A sentinel inside chain.db would be copied by the rsync and could cause the
// relay to skip the sweep even when the rsync introduced index corruption.

// heightIndexSentinelPath returns the path of the sentinel marker file.
func heightIndexSentinelPath(dataDir string) string {
        return filepath.Join(dataDir, "height_index_verified")
}

// loadHeightIndexSentinel returns true when the sentinel file exists.
func loadHeightIndexSentinel(dataDir string) bool {
        _, err := os.Stat(heightIndexSentinelPath(dataDir))
        return err == nil
}

// storeHeightIndexSentinel creates (or overwrites) the sentinel marker file.
// Non-fatal on error: the sentinel is absent and the next restart retries.
func storeHeightIndexSentinel(dataDir string) error {
        return os.WriteFile(heightIndexSentinelPath(dataDir), []byte("1\n"), 0644)
}

// runRepairHeightIndex implements the --repair-height-index subcommand.
//
// Usage: aperod-node --repair-height-index --data-dir=<path>
//
// Scans every block in chain.db (b/ namespace), builds a height→hash map from
// the actual block data, then rewrites any h/<height> entry that is absent or
// mismatched.  This fixes the class of corruption that the tip-only startup
// integrity check cannot address: zeroed or missing index entries at heights
// below the tip — the typical symptom of a live-LevelDB rsync.
//
// The node MUST be stopped before running this command.  Unlike --check-store
// (which counts gaps) this command actually repairs them.
//
// On success it writes a sentinel FILE at <data-dir>/height_index_verified so
// the auto-repair path inside run() knows the index has been verified.  The
// file lives outside chain.db/ so it is NOT copied by a chain.db-only rsync.
func runRepairHeightIndex() error {
        dataDir := ""
        args := os.Args[1:]
        for i, arg := range args {
                switch {
                case strings.HasPrefix(arg, "--data-dir="):
                        dataDir = strings.TrimPrefix(arg, "--data-dir=")
                case arg == "--data-dir" && i+1 < len(args):
                        dataDir = args[i+1]
                }
        }
        if dataDir == "" {
                return fmt.Errorf("--repair-height-index requires --data-dir=<path>")
        }

        absDataDir, err := filepath.Abs(dataDir)
        if err != nil {
                return fmt.Errorf("repair-height-index: resolve data dir: %w", err)
        }
        dbPath := filepath.Join(absDataDir, "chain.db")
        _, inspectedTipHeight, err := store.ReadTipOnly(dbPath)
        if err != nil {
                return fmt.Errorf("repair-height-index: inspect target before mutation: %w", err)
        }
        if err := maintenancePreflightResolved(
                os.Stderr, "(not used; --data-dir supplied explicitly)", absDataDir, inspectedTipHeight,
        ); err != nil {
                return err
        }
        db, err := store.Open(dbPath)
        if err != nil {
                return fmt.Errorf("repair-height-index: open %s: %w", dbPath, err)
        }
        defer db.Close()

        _, tipHeight, err := db.GetTip()
        if err != nil {
                return fmt.Errorf("repair-height-index: read tip: %w", err)
        }
        if tipHeight == 0 {
                fmt.Fprintf(os.Stdout, "repair-height-index: empty store (tip_height=0) — nothing to repair\n")
                return nil
        }

        fmt.Fprintf(os.Stdout,
                "repair-height-index: scanning %d stored blocks (tip_height=%d)...\n",
                tipHeight+1, tipHeight)

        lastPct := uint64(101) // sentinel so first progress call always prints
        progress := func(scanned, total uint64) {
                if total == 0 {
                        return
                }
                pct := scanned * 100 / total
                if pct != lastPct {
                        fmt.Fprintf(os.Stdout,
                                "repair-height-index: %d / %d (%d%%)\r",
                                scanned, total, pct)
                        lastPct = pct
                }
        }

        repaired, skipped, repErr := db.RepairAllHeightIndex(tipHeight, progress)
        fmt.Fprintf(os.Stdout, "\n") // newline after \r progress line

        // Only write the sentinel when the sweep completed without I/O errors
        // AND without any unrepairable heights (missing block body AND missing
        // h/ entry).  Silently marking an incomplete sweep as done would allow
        // subsequent starts to skip the only full verification and boot with
        // residual corruption.
        sweepOK := repErr == nil && skipped == 0
        if sweepOK {
                if sentErr := storeHeightIndexSentinel(dataDir); sentErr != nil {
                        fmt.Fprintf(os.Stderr,
                                "repair-height-index: warning: failed to write sentinel file: %v\n", sentErr)
                }
        }

        if skipped > 0 {
                fmt.Fprintf(os.Stderr,
                        "repair-height-index: %d height(s) could not be repaired "+
                                "(block body missing from b/ namespace) — sentinel NOT written; "+
                                "restore from a clean snapshot for those heights\n",
                        skipped)
        }
        if repaired > 0 {
                fmt.Fprintf(os.Stdout,
                        "repair-height-index: repaired %d height-index entries "+
                                "(tip_height=%d, skipped=%d)\n",
                        repaired, tipHeight, skipped)
        } else {
                fmt.Fprintf(os.Stdout,
                        "repair-height-index: height index consistent "+
                                "(tip_height=%d, 0 entries repaired, skipped=%d)\n",
                        tipHeight, skipped)
        }
        if !sweepOK {
                fmt.Fprintf(os.Stdout,
                        "repair-height-index: sweep incomplete — start normally only after resolving the gaps above\n")
        } else {
                fmt.Fprintf(os.Stdout,
                        "repair-height-index: sweep complete — start normally\n")
        }
        if repErr != nil {
                return fmt.Errorf("repair-height-index: %w", repErr)
        }
        if skipped > 0 {
                return fmt.Errorf("repair-height-index: %d height(s) unrepairable (block body missing)", skipped)
        }
        return nil
}

// checkStartupIntegrity cross-verifies the tip pointer against the canonical
// height index at startup and self-heals a missing/zeroed or mismatched
// h/<tipHeight> entry from the authoritative tip pointer.
//
// It returns (done, err):
//   - done==true  means the caller should return err immediately (used for the
//     --reset-tip / --repair-db CLI early-exit paths, where err is nil).
//   - done==false means the caller should continue normal startup; err may still
//     be non-nil for a fatal validator-side failure.
//
// This is the REAL production code path invoked from run(); it is factored out
// as a standalone helper so it can be unit-tested with a slog capture handler
// (verifying, for example, that a successful self-heal is logged at INFO — not
// WARN — so operators filtering alerts by log level are not spuriously paged).
func checkStartupIntegrity(
        db *store.DB,
        tipHeight uint64,
        tipHash crypto.Hash32,
        nonValidator bool,
        resetTip bool,
        repairDB bool,
        log *slog.Logger,
) (done bool, err error) {
        indexedBlock, idxErr := db.GetBlockByHeight(tipHeight)
        if idxErr != nil {
                return false, fmt.Errorf("startup integrity check: read height index at %d: %w", tipHeight, idxErr)
        }
        integrityOK := true
        if indexedBlock == nil {
                // Missing or zeroed height-index entry at the tip height.
                // Common after rsync bootstrap or an OOM-kill that corrupted
                // the LevelDB SST.  Attempt a one-time self-repair from the
                // tip pointer (the authoritative source of truth) before
                // deciding how to proceed.
                repErr := db.RepairHeightIndex(tipHeight, tipHash)
                if repErr == nil {
                        // Repair succeeded.  Log INFO so operators can see
                        // the self-heal in the startup log; subsequent restarts
                        // will find a valid entry and skip this branch entirely.
                        log.Info("startup integrity: height index was missing/zeroed — repaired from tip pointer",
                                "tip_height", tipHeight, "tip_hash", fmt.Sprintf("%x", tipHash[:8]))
                } else {
                        // Repair failed.  Validators must not continue: a broken
                        // height index on a proposer risks block production on a
                        // corrupt state.  Non-validators log a WARN and continue
                        // so the relay node can still serve headers.
                        if !nonValidator {
                                return false, fmt.Errorf(
                                        "startup integrity (validator): height index missing for tip height %d "+
                                                "and repair failed: %w; run --repair-db or restore from a clean snapshot",
                                        tipHeight, repErr)
                        }
                        log.Warn("startup integrity: height index missing and repair failed — continuing",
                                "tip_height", tipHeight, "tip_hash", fmt.Sprintf("%x", tipHash[:8]), "err", repErr)
                        integrityOK = false
                }
                if resetTip || repairDB {
                        if repErr == nil {
                                fmt.Printf("aperod-node: height index repaired at height %d — start normally (without --reset-tip / --repair-db)\n", tipHeight)
                        } else {
                                fmt.Printf("aperod-node: height index repair FAILED at height %d: %v\n", tipHeight, repErr)
                        }
                        return true, nil
                }
        } else if indexedBlock.Hash != tipHash {
                // Hash mismatch: the height-index entry is stale or corrupt
                // while the tip pointer is authoritative.  Attempt repair.
                repErr := db.RepairHeightIndex(tipHeight, tipHash)
                if repErr == nil {
                        log.Info("startup integrity: height index hash mismatch — repaired from tip pointer",
                                "tip_height", tipHeight,
                                "tip_pointer_hash", fmt.Sprintf("%x", tipHash[:8]),
                                "height_index_hash", fmt.Sprintf("%x", indexedBlock.Hash[:8]))
                } else {
                        if !nonValidator {
                                return false, fmt.Errorf(
                                        "startup integrity (validator): height index hash mismatch at tip height %d "+
                                                "and repair failed: %w; run --repair-db or restore from a clean snapshot",
                                        tipHeight, repErr)
                        }
                        log.Warn("startup integrity: height index hash mismatch and repair failed — continuing",
                                "tip_height", tipHeight,
                                "tip_pointer_hash", fmt.Sprintf("%x", tipHash[:8]),
                                "height_index_hash", fmt.Sprintf("%x", indexedBlock.Hash[:8]),
                                "err", repErr)
                        integrityOK = false
                }
                if resetTip || repairDB {
                        if repErr == nil {
                                fmt.Printf("aperod-node: height index repaired at height %d — start normally (without --reset-tip / --repair-db)\n", tipHeight)
                        } else {
                                fmt.Printf("aperod-node: height index repair FAILED at height %d: %v\n", tipHeight, repErr)
                        }
                        return true, nil
                }
        } else if resetTip || repairDB {
                // Height index is already correct; nothing to repair.
                fmt.Printf("aperod-node: height index already consistent at height %d — start normally\n", tipHeight)
                return true, nil
        }
        if integrityOK {
                log.Info("startup integrity check passed", "height", tipHeight, "hash", fmt.Sprintf("%x", tipHash[:8]))
        }
        return false, nil
}

func run() error {
        // ── 0. config-free maintenance subcommands ────────────────────────────────
        // Processed before config loading so operators can verify a chain.db that
        // was just rsync'd without needing a fully-configured node.yaml.
        for _, arg := range os.Args[1:] {
                switch arg {
                case "--check-store":
                        return runCheckStore()
                case "--check-snapshot":
                        return runCheckSnapshot()
                case "--compact-db":
                        return runCompactDB()
                case "--repair-height-index":
                        return runRepairHeightIndex()
                }
        }

        // ── 1. Load configuration ─────────────────────────────────────────────────
        cfgPath := "config/testnet.yaml"
        configFlagExplicit := false
        resetP2PIdentity := false
        validateOnly := false
        strictMemLimit := false
        resetTip := false
        repairDB := false
        rebuildKeyImages  := false
        forcePurgeKIIndex := false
        rebuildSpentIndex := false
        for i, arg := range os.Args[1:] {
                switch arg {
                case "--config":
                        if i+2 < len(os.Args) {
                                cfgPath = os.Args[i+2]
                                configFlagExplicit = true
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
                case "--rebuild-key-images":
                        rebuildKeyImages = true
                case "--force-purge-ki-index":
                        // Must be combined with --rebuild-key-images.
                        // On pruned nodes the normal rebuild cannot verify key images
                        // from unreadable blocks and falls back to the persistent index,
                        // which may contain phantom entries from partial LevelDB writes.
                        // This flag purges all unverifiable entries from the persistent
                        // k/ index and rebuilds in-memory state solely from the readable
                        // block scan.  SAFE because the UTXO set is the primary
                        // double-spend guard — any UTXO whose key image we remove was
                        // already removed from the UTXO set when the block was applied.
                        forcePurgeKIIndex = true
                case "--rebuild-spent-index":
                        // Backfill the su/ spent-UTXO LevelDB index for outputs that
                        // were spent before the index existed.  After the full startup
                        // scan the in-memory UTXOSet is the source of truth; any u/
                        // entry absent from it is spent and gets an su/ record written.
                        // Safe to re-run (idempotent); exits when done.
                        rebuildSpentIndex = true
                }
        }
        _ = resetP2PIdentity // used below in P2P startup

        // ── repair-db / reset-tip safety guard ───────────────────────────────────
        // When a maintenance flag (--repair-db, --reset-tip, --rebuild-key-images,
        // --rebuild-spent-index) is active and the operator did NOT supply an
        // explicit --config flag, the default config path (config/testnet.yaml)
        // points to a stale local copy with a ~105 K-block data directory — NOT the
        // 1 M+ block production chain in /etc/aperod/node.yaml.  Silently scanning
        // the wrong copy has caused multi-hour repair attempts with no effect on the
        // live node.
        //
        // Fix: if the system config exists at the canonical production path and no
        // --config was given, print a clear warning, switch to the system config,
        // and continue.  If the system config does NOT exist this guard is a no-op
        // so the binary still works correctly in development / CI environments.
        const systemConfigPath = "/etc/aperod/node.yaml"
        maintenanceMode := repairDB || resetTip || rebuildKeyImages || rebuildSpentIndex
        if maintenanceMode && !configFlagExplicit {
                if _, statErr := os.Stat(systemConfigPath); statErr == nil {
                        fmt.Fprintf(os.Stderr,
                                "WARNING: --config was not supplied; defaulting to %s\n"+
                                        "  The local default (%s) points to a stale development data directory\n"+
                                        "  and is NOT the production chain.  Switching to system config automatically.\n"+
                                        "  To silence this warning, pass --config %s explicitly.\n",
                                systemConfigPath, cfgPath, systemConfigPath)
                        cfgPath = systemConfigPath
                        configFlagExplicit = true
                } else {
                        return fmt.Errorf(
                                "MAINTENANCE REFUSED: --config was not supplied and %s does not exist; "+
                                        "pass --config <path> explicitly so a repair cannot run against the default data directory",
                                systemConfigPath)
                }
        }

        cfg, err := config.Load(cfgPath)
        if err != nil {
                return fmt.Errorf("load config: %w", err)
        }
        if err := cfg.Validate(); err != nil {
                return fmt.Errorf("invalid config: %w", err)
        }
        if maintenanceMode {
                absConfig, absDataDir, pathErr := maintenancePaths(cfgPath, cfg.DataDir)
                if pathErr != nil {
                        return pathErr
                }
                fmt.Fprintf(os.Stderr,
                        "\n================ MAINTENANCE TARGET ===================\n"+
                                "resolved_config: %s\n"+
                                "absolute_data_dir: %s\n"+
                                "=======================================================\n",
                        absConfig, absDataDir)
        }

        // --validate-config: exit 0 after a successful parse+validate so
        // operators (and node-config.sh) can verify node.yaml without starting
        // the node.  Prints the config path and network name so the caller can
        // confirm which file was checked.
        if validateOnly {
                // Enforce the same fatal validator-key permission check the node
                // applies at startup (mode & 0o077 must be 0).  Running it as part of
                // this dry-run — BEFORE any live service is stopped — lets deploy
                // tooling detect an unsafe-permission key and fix it without ever
                // taking the node offline.  Otherwise the old binary keeps running
                // while the freshly deployed one would refuse to boot.
                keyPath := cfg.Consensus.ValidatorKey
                if keyPath == "" {
                        keyPath = filepath.Join(cfg.DataDir, "validator.key")
                }
                if err := checkKeyFilePermissions(keyPath); err != nil {
                        return err
                }
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
                cfg.MemoryLimitDisabled,
                "/etc/systemd/system/aperod-node.service.d/gomemlimit.conf",
                log,
        ); err != nil {
                return err
        }

        // Emit any non-fatal configuration warnings now that the logger is ready.
        emitConfigWarnings(log, cfg)

        // Guard: warn when a relay (non-validator) node has a snapshot tolerance
        // below 10 %.  Operators who manually rsync chain.db+snapshots outside of
        // join-network.sh --bootstrap-from often forget to raise this value; the
        // result is that every snapshot is rejected on startup and the node falls
        // back to a multi-hour full block scan — with no obvious indication of why.
        //
        // The threshold of 10 % matches the value join-network.sh sets automatically.
        // The warning is intentionally loud (plain stderr line + structured log) so
        // it is visible in `journalctl -u aperod-node` without grepping.
        if cfg.Consensus.NonValidator && cfg.Snapshot.UTXOCountTolerancePct < 10 {
                warnMsg := "[RELAY CONFIG WARNING] snapshot.utxo_count_tolerance_pct is " +
                        fmt.Sprintf("%.1f", cfg.Snapshot.UTXOCountTolerancePct) +
                        " but should be >= 10 on a relay node. " +
                        "Snapshots will be rejected on startup if the UTXO count drifts " +
                        "by more than the configured percentage, forcing a full block scan " +
                        "(potentially hours). Set snapshot.utxo_count_tolerance_pct: 10 " +
                        "in node.yaml and restart."
                fmt.Fprintln(os.Stderr, warnMsg)
                log.Warn("[RELAY CONFIG WARNING] utxo_count_tolerance_pct too low for relay node",
                        "current_value", cfg.Snapshot.UTXOCountTolerancePct,
                        "recommended_minimum", 10,
                        "fix", "set snapshot.utxo_count_tolerance_pct: 10 in node.yaml",
                )
        }

        // ── 3. Open storage ───────────────────────────────────────────────────────
        // Maintenance commands must inspect the selected chain without writes
        // before MkdirAll, Recover, or a normal LevelDB open can alter anything.
        if maintenanceMode {
                _, absDataDir, pathErr := maintenancePaths(cfgPath, cfg.DataDir)
                if pathErr != nil {
                        return pathErr
                }
                dbPath := filepath.Join(absDataDir, "chain.db")
                _, inspectedTipHeight, inspectErr := store.ReadTipOnly(dbPath)
                if inspectErr != nil {
                        return fmt.Errorf(
                                "MAINTENANCE REFUSED: cannot inspect %s read-only before mutation: %w; "+
                                        "verify --config and data_dir, then repair from a verified copy",
                                dbPath, inspectErr)
                }
                if err := maintenancePreflight(os.Stderr, cfgPath, cfg.DataDir, inspectedTipHeight); err != nil {
                        return err
                }
        }
        if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
                return fmt.Errorf("create data dir: %w", err)
        }

        // Guard: refuse to start when join-network.sh left a .rsync-in-progress
        // sentinel in the data directory.  The sentinel is written immediately
        // before rsync begins (push mode and bootstrap mode) and removed only
        // after a successful transfer.  Starting against a half-written LevelDB
        // produces a divergent chain that is hard to recover from — this hard
        // refusal is cheaper than any recovery procedure.
        //
        // Operators can unblock startup by either:
        //   (a) waiting for join-network.sh to finish and remove the file, or
        //   (b) removing the file manually after confirming no rsync is running:
        //         rm <data-dir>/.rsync-in-progress
        if err := checkRsyncSentinel(cfg.DataDir); err != nil {
                return err
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
        // Resolve the configured in-memory block window.  0 means "use default";
        // pass the config value so the chain uses what node.yaml requests.
        chainMaxBlocks := cfg.MaxInMemoryBlocks
        if chainMaxBlocks == 0 {
                chainMaxBlocks = core.MaxInMemoryBlocks
        }
        chain := core.NewChain(chainMaxBlocks)
        mempool := core.NewMempool(core.DefaultMempoolConfig(), log)

        // Create the UTXO set here (before chain loading) so the resume path
        // can populate it from stored blocks, ensuring historical spent key
        // images are known to TxVerifier before the first peer block arrives.
        // NewUTXOSetWithDB wires the LevelDB key-image backend so that
        // IsSpent lookups for historical KIs go to disk rather than a
        // 700 MB in-memory sorted slice, eliminating the main RSS baseline
        // and the ≈768 KB/h growth that accumulated between restarts.
        utxos := core.NewUTXOSetWithDB(db)
        // Wire the spent-UTXO index callback so ApplyBlock (called during the
        // startup scan and live block acceptance) keeps the su/ index current.
        // Non-fatal on DB error — the index is a startup-performance optimisation.
        utxos.OnUTXOSpent = func(txHash crypto.Hash32, outIdx uint32) {
                if spentErr := db.MarkUTXOSpent(txHash, outIdx); spentErr != nil {
                        log.Warn("failed to persist spent UTXO to index",
                                "out_idx", outIdx, "err", spentErr)
                }
        }
        utxos.OnUTXORestored = func(txHash crypto.Hash32, outIdx uint32) error {
                return db.UnmarkUTXOSpent(txHash, outIdx)
        }

        tipHash, tipHeight, err := db.GetTip()
        if err != nil {
                return fmt.Errorf("get tip: %w", err)
        }

        // ── UTXO store rebuild (--repair-db only) ─────────────────────────────────
        // After OOM kill + LevelDB RecoverFile, u/ (UTXO store) entries that were
        // only in SST files at crash time may be absent even though their key-images
        // are unspent.  Scan all blocks now and restore any missing u/ entries so
        // validator-reward and admin-mint UTXOs remain spendable after repair.
        // This must run before the height-index integrity check (which calls return nil
        // for --repair-db) to ensure the rebuild completes even on the early-exit path.
        if repairDB {
                log.Info("--repair-db: scanning blocks to restore missing UTXO store entries",
                        "tip_height", tipHeight)
                restored, rebuildErr := rebuildMissingUTXOs(db, tipHeight, log)
                if rebuildErr != nil {
                        log.Warn("--repair-db: UTXO store rebuild completed with errors",
                                "restored_entries", restored, "err", rebuildErr)
                } else {
                        log.Info("--repair-db: UTXO store rebuild complete",
                                "restored_entries", restored)
                }

                // Verify existing u/ entries against raw block data and overwrite
                // any whose fields (AmountCommit, OneTimePub, TxPubKey, EncAmount)
                // do not match the on-chain output.  Catches store corruption
                // introduced by external writers or bad restore flows that
                // rebuildMissingUTXOs cannot fix (it skips existing entries).
                log.Info("--repair-db: verifying UTXO store entries against raw blocks",
                        "tip_height", tipHeight)
                fixed, verifyErr := verifyUTXOStoreEntries(db, tipHeight, log)
                if verifyErr != nil {
                        log.Warn("--repair-db: UTXO store verification completed with errors",
                                "fixed_entries", fixed, "err", verifyErr)
                } else {
                        log.Info("--repair-db: UTXO store verification complete",
                                "fixed_entries", fixed)
                }

                // ── Key-image rebuild (--repair-db) ────────────────────────────────
                // After an OOM kill the snapshot can contain stale key images for
                // transactions that were hashed but never confirmed in a block.
                // Rebuilding from authoritative block data eliminates phantom entries
                // that make UTXOs appear "already spent" even though they are active
                // on-chain.  Running this here means --repair-db fixes both u/ and ki/
                // in one command; --rebuild-key-images remains available for ki/-only
                // repairs when --repair-db is unnecessary.
                log.Info("--repair-db: rebuilding key-image set from block scan",
                        "tip_height", tipHeight)
                kiBuilt, kiAllOK, kiErr := rebuildKeyImagesFromBlocks(db, tipHeight, utxos, false, log)
                if kiErr != nil || !kiAllOK {
                        // Do NOT patch the snapshot when the rebuild was incomplete or any
                        // k/ write failed.  Replacing the snapshot key-image set with a
                        // partial build could forget genuine spends from unreadable blocks,
                        // re-opening already-spent outputs — a consensus/double-spend
                        // regression.  The k/ index updates that did succeed are still
                        // applied; a subsequent normal startup loads from k/ when no
                        // matching snapshot is found.
                        if kiErr != nil {
                                log.Warn("--repair-db: key-image rebuild failed — snapshot NOT updated",
                                        "key_images_found", kiBuilt, "err", kiErr,
                                        "hint", "re-run --repair-db once the underlying error is resolved")
                        } else {
                                log.Warn("--repair-db: key-image rebuild incomplete (unreadable blocks or index write failures) — snapshot NOT updated",
                                        "key_images_found", kiBuilt,
                                        "hint", "on archive nodes re-run --repair-db; on pruned nodes run --rebuild-key-images --force-purge-ki-index")
                        }
                } else {
                        log.Info("--repair-db: u/ and ki/ both rebuilt",
                                "key_images_found", kiBuilt)

                        // Patch the on-disk snapshot so the next normal startup loads the
                        // corrected key-image set instead of the stale snapshot entries.
                        // Gated on kiAllOK==true: the scan was complete and all k/ writes
                        // succeeded, so the in-memory set is the authoritative replacement.
                        // If no snapshot exists the k/ index update is sufficient — the
                        // next startup loads key images from k/ when no snapshot is found.
                        repairTipHashHex := fmt.Sprintf("%x", tipHash[:])
                        if existingSnap, _, snapLoadErr := tryLoadStartupSnapshot(cfg.DataDir, tipHeight, repairTipHashHex, log); snapLoadErr == nil {
                                existingSnap.UTXOs.KeyImages = utxos.TakeSnapshot().KeyImages
                                existingSnap.SavedAt = time.Now()
                                if snapSaveErr := saveStartupSnapshot(cfg.DataDir, *existingSnap); snapSaveErr != nil {
                                        log.Warn("--repair-db: failed to update snapshot with rebuilt key images",
                                                "err", snapSaveErr)
                                } else {
                                        log.Info("--repair-db: snapshot updated with rebuilt key-image set",
                                                "key_images", len(existingSnap.UTXOs.KeyImages))
                                }
                        } else {
                                log.Info("--repair-db: no existing snapshot to update — key images persisted to k/ index only",
                                        "hint", "start normally after --repair-db to rebuild the full snapshot")
                        }
                }
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
                apiSrv.SetRSSStatsFn(readRSSBytes)
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

        // ── Load persisted snapshot save duration ─────────────────────────────────
        // StoreSnapshotSaveDuration is written at the end of every successful
        // shutdown snapshot.  Loading it here lets us:
        //   1. Pre-populate /api/v1/status (snapshot_save_duration_ms) from the
        //      previous run so the Admin Panel shows a meaningful value immediately
        //      after restart, before the next shutdown snapshot is taken.
        //   2. Proactively warn when the observed duration already exceeds 50 % /
        //      80 % of the current TimeoutStopSec — without waiting for the next
        //      shutdown to discover the problem.
        if savedSnapMs, found, loadErr := db.LoadSnapshotSaveDuration(); loadErr != nil {
                log.Warn("startup: failed to load last_snap_save_ms from DB", "err", loadErr)
        } else if found && savedSnapMs > 0 {
                log.Info("startup: last snapshot save duration loaded from DB",
                        "last_snap_save_ms", savedSnapMs,
                )
                // Feed the persisted timing into the API server so /api/v1/status
                // returns a non-zero snapshot_save_duration_ms on the very first
                // request after restart, stamped as "from previous shutdown".
                // Prefer the persisted TimeoutStopSec from the previous shutdown
                // (matches the environment that produced the save duration); fall
                // back to the current systemd reading if no persisted value exists.
                if apiSrv != nil {
                        const dropinDir = "/etc/systemd/system/aperod-node.service.d"
                        const svcPath   = "/etc/systemd/system/aperod-node.service"
                        timeoutSec, timeoutFound, timeoutLoadErr := db.LoadSnapshotTimeoutSec()
                        if timeoutLoadErr != nil {
                                log.Warn("startup: failed to load last_snap_timeout_sec from DB", "err", timeoutLoadErr)
                        }
                        if !timeoutFound || timeoutSec == 0 {
                                // No persisted timeout — fall back to current systemd reading.
                                timeoutSec, _ = readEffectiveTimeoutStopSec(dropinDir, svcPath)
                        }
                        apiSrv.SetSnapshotTimingsFromPreviousShutdown(
                                time.Duration(savedSnapMs)*time.Millisecond,
                                timeoutSec,
                        )
                }
                // Proactive startup warning: if the observed save duration already
                // exceeds a dangerous fraction of TimeoutStopSec, warn now — before
                // the next shutdown — so operators can act while the node is running.
                checkStartupSnapshotTiming(
                        savedSnapMs,
                        "/etc/systemd/system/aperod-node.service.d",
                        "/etc/systemd/system/aperod-node.service",
                        log,
                )
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
                //
                // The logic lives in checkStartupIntegrity so it can be unit-tested
                // in isolation (see cmd/node/startup_integrity_test.go); done==true
                // means one of the --reset-tip / --repair-db CLI paths has finished
                // and run() should return immediately.
                done, err := checkStartupIntegrity(
                        db, tipHeight, tipHash,
                        cfg.Consensus.NonValidator, resetTip, repairDB, log,
                )
                if err != nil {
                        return err
                }
                if done {
                        return nil
                }

                // ── Validator startup full height-index check ─────────────────────────
                // Validators must be bootstrapped from a clean snapshot.  Any corrupt
                // h/ entry below the tip — whether absent, zeroed, or pointing at a
                // missing block body (dangling) — is a hard-fail: producing a block
                // on a node with a broken height index risks silent chain divergence
                // that cannot be automatically recovered.
                //
                // CheckAllHeightIndex detects both classes of height-index
                // inconsistency:
                //   • absent/zeroed h/ keys
                //   • dangling h/ entries whose b/<hash> block body is missing
                //
                // For nodes that have ever run --repair-db (RecoverFile) after an OOM,
                // or that use light pruning, some b/ block bodies may be absent while
                // the corresponding h/ entries survive.  Missing b/ bodies do NOT
                // affect block production: the validator only needs the current tip and
                // UTXO snapshot to propose new blocks.  A hard-fail here crash-loops the
                // node on databases that are otherwise fully operational.
                //
                // Policy: log a WARNING for every broken entry so operators can spot
                // genuine corruption, but do NOT abort startup.  Run
                // --repair-height-index to attempt recovery or restore from a clean
                // snapshot if strict consistency is required.
                if !cfg.Consensus.NonValidator && !repairDB && tipHeight > 0 {
                        checkFrom := uint64(0)
                        if cfg.Pruning.Mode == "light" && cfg.Pruning.KeepBlocks > 0 && tipHeight > cfg.Pruning.KeepBlocks {
                                checkFrom = tipHeight - cfg.Pruning.KeepBlocks
                        }
                        broken, firstBroken, chkErr := db.CheckAllHeightIndex(tipHeight, checkFrom)
                        if chkErr != nil {
                                // I/O error during the scan — log and continue; do not hard-fail.
                                log.Warn("startup integrity (validator): height-index scan error — continuing",
                                        "err", chkErr)
                        } else if broken > 0 {
                                // Some h/ entries have no corresponding b/ body.  This is
                                // expected after --repair-db recovery or light pruning.
                                // Block production is not affected; log for operator awareness.
                                log.Warn("startup integrity (validator): broken height-index entries detected",
                                        "broken", broken,
                                        "first_broken", firstBroken,
                                        "tip", tipHeight,
                                        "note", "missing b/ bodies do not affect block production; run --repair-height-index if unexpected")
                        } else {
                                log.Info("startup integrity (validator): full height-index check passed",
                                        "tip_height", tipHeight)
                        }
                }

                // ── Startup height-index auto-repair (non-validator nodes) ────────────
                // On the first start after an rsync bootstrap the
                // height_index_sentinel metadata key is absent.  Run a full sweep of
                // the b/ namespace and repair any h/ entry that is missing or zeroed
                // at heights below the tip — gaps that the tip-only integrity check
                // above cannot fix.  After a successful sweep the sentinel is written
                // so subsequent restarts skip this scan.
                //
                // The sentinel represents "I've verified and corrected the initial
                // rsync data".  Blocks added after the sentinel was written use the
                // atomic PutRawBlock (b/ + h/ in a single fsynced batch), so ordinary
                // chain growth does NOT require a new sweep — the sentinel remains
                // valid regardless of how many blocks accumulate after it.
                //
                // The sentinel is only written when the sweep completes with no I/O
                // errors AND no heights that needed repair but had no block body.
                // A partial sweep leaves the sentinel absent so the next restart
                // retries rather than skipping on corrupted data.
                if cfg.Consensus.NonValidator && !repairDB {
                        // Only sweep when the sentinel FILE is absent.  The sentinel is
                        // stored at <dataDir>/height_index_verified — OUTSIDE chain.db/ —
                        // so it is never copied by a chain.db-only rsync bootstrap.  A
                        // relay that just received a fresh chain.db copy will therefore
                        // always find the sentinel absent and always run the sweep, even
                        // if the source node had previously written the sentinel in its
                        // own data directory.
                        sentinelFound := loadHeightIndexSentinel(cfg.DataDir)
                        if !sentinelFound {
                                log.Info("startup: height-index sentinel absent — running repair sweep",
                                        "tip_height", tipHeight,
                                        "sentinel_path", heightIndexSentinelPath(cfg.DataDir),
                                )
                                repaired, skipped, sweepErr := db.RepairAllHeightIndex(tipHeight, nil)
                                sweepComplete := sweepErr == nil && skipped == 0
                                if sweepErr != nil {
                                        log.Warn("startup: height-index sweep completed with I/O errors",
                                                "repaired", repaired, "skipped", skipped, "err", sweepErr)
                                } else if skipped > 0 {
                                        log.Warn("startup: height-index sweep incomplete — some heights have no block body",
                                                "repaired", repaired, "skipped", skipped,
                                                "tip_height", tipHeight)
                                } else if repaired > 0 {
                                        log.Info("startup: height-index sweep repaired missing entries",
                                                "repaired", repaired, "tip_height", tipHeight)
                                } else {
                                        log.Info("startup: height-index sweep complete — index consistent",
                                                "tip_height", tipHeight)
                                }
                                // Write sentinel file only after a fully successful sweep so
                                // that a partial repair is retried on the next restart.
                                if sweepComplete {
                                        if sentErr2 := storeHeightIndexSentinel(cfg.DataDir); sentErr2 != nil {
                                                log.Warn("startup: failed to write height-index sentinel",
                                                        "err", sentErr2)
                                        }
                                }
                        }
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

                // Load only the most recent chainMaxBlocks blocks.
                // Older blocks are kept on disk; the in-memory window is bounded.
                // Uses loadRecentBlocksFromStore (resume.go) which continues past
                // any missing heights rather than aborting early.
                recentBlocks := loadRecentBlocksFromStore(db, tipHeight, log, chainMaxBlocks)
                // startLoad is the first height in the loaded window.  It is used
                // below to seed the tx-index fast path with the correct height range.
                startLoad := uint64(1)
                if tipHeight >= chainMaxBlocks {
                        startLoad = tipHeight - chainMaxBlocks + 1
                }

                // ── Store integrity check (in-memory window) ──────────────────────
                // Scan the same height window for missing LevelDB entries.
                // Missing blocks are non-fatal at this stage — the startup scan has
                // its own MaxMissingBlocks guard — but we emit a structured WARNING
                // immediately so operators know the extent of any damage without
                // waiting for the scan to reach the affected heights hours later.
                // The count is also published on /api/v1/status so the admin panel
                // can surface it without requiring log access.
                //
                // Auto-repair: when gaps are detected we attempt to rebuild the
                // height index (h/ entries) from the block bodies (b/ namespace)
                // that were written atomically in the same fsynced batch.  This is
                // the typical crash-kill symptom: the WAL was fsynced but LevelDB
                // had not yet compacted the h/ entries into the SST when the
                // process was killed.  RepairAllHeightIndex fixes those h/ entries
                // in-place without closing and reopening the DB (no --repair-db
                // required).  After repair recentBlocks is reloaded so FastForward
                // uses the now-complete height index.  The sentinel file is removed
                // so the next restart re-verifies the repaired state.
                {
                        missingCount, firstMissing, lastMissing := countMissingBlocksInWindow(db, tipHeight, chainMaxBlocks)
                        if missingCount > 0 {
                                log.Warn("store integrity warning",
                                        "missing_blocks", missingCount,
                                        "first_missing", firstMissing,
                                        "last_missing",  lastMissing,
                                )
                                // Attempt automatic height-index repair before giving up.
                                log.Info("startup: auto-repairing height index to fix detected gaps",
                                        "missing_count", missingCount,
                                        "first_missing", firstMissing,
                                )
                                repaired, skipped, sweepErr := db.RepairAllHeightIndex(tipHeight, nil)
                                switch {
                                case sweepErr != nil:
                                        log.Warn("startup: height-index auto-repair completed with I/O errors",
                                                "repaired", repaired, "skipped", skipped, "err", sweepErr)
                                case skipped > 0:
                                        log.Warn("startup: height-index auto-repair: some heights unrepairable (block body absent)",
                                                "repaired", repaired, "skipped", skipped,
                                                "hint", "run --repair-db to attempt WAL-level recovery")
                                default:
                                        log.Info("startup: height-index auto-repair complete",
                                                "repaired", repaired, "tip_height", tipHeight)
                                }
                                // Clear the sentinel so the next restart re-verifies the
                                // repaired index rather than skipping the sweep.
                                _ = os.Remove(heightIndexSentinelPath(cfg.DataDir))
                                // Reload recentBlocks with the repaired height index.
                                recentBlocks = loadRecentBlocksFromStore(db, tipHeight, log, chainMaxBlocks)
                                // Re-count to see whether repair resolved all gaps.
                                missingCount, firstMissing, lastMissing = countMissingBlocksInWindow(db, tipHeight, chainMaxBlocks)
                                if missingCount > 0 {
                                        log.Warn("startup: gaps remain after auto-repair — block bodies may be unrecoverable",
                                                "remaining_missing", missingCount,
                                                "first_missing", firstMissing,
                                                "hint", "run --repair-db to recover data still in the LevelDB WAL",
                                        )
                                }
                        }
                        if apiSrv != nil {
                                apiSrv.SetStoreMissingBlocks(int64(missingCount))
                                apiSrv.SetStoreMissingFirstBlock(int64(firstMissing))
                                apiSrv.SetStoreMissingLastBlock(int64(lastMissing))
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

                // ── Unclean-shutdown detection ───────────────────────────────────────
                // The SIGTERM handler writes a clean_shutdown marker file after saving the
                // snapshot.  If the file is absent the previous run did not reach that code
                // path — the canonical OOM / SIGKILL scenario.  Consuming the marker here
                // means the CURRENT run is considered unverified until its own marker is
                // written at shutdown.
                wasOOMRestart := !readAndDeleteCleanShutdownMarker(cfg.DataDir)
                if wasOOMRestart {
                        log.Info("startup: no clean-shutdown marker — previous shutdown may have been unclean (OOM/SIGKILL); " +
                                "AmountCommit validation will run if snapshot is recent")
                } else {
                        log.Info("startup: clean-shutdown marker found — previous shutdown was graceful")
                }

                // rescueSnap is set when a snapshot is readable but the UTXO-count check
                // fails.  It is consumed below (before the !snapLoaded block) to seed the
                // in-memory UTXO + key-image state so the block scan covers only the few
                // hundred blocks from rescueSnap.TipHeight+1 to tipHeight instead of
                // scanning from block 1.
                var rescueSnap *startupSnapshot
                var rescueSnapHeight uint64
                {
                        tipHashHex := fmt.Sprintf("%x", tipHash[:])
                        if snap, snapIsRelaxed, serr := tryLoadStartupSnapshot(cfg.DataDir, tipHeight, tipHashHex, log); serr == nil {
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

                                        // ── Snapshot ↔ disk-store reconciliation ─────────────────────
                                        // A snapshot can carry corrupted UTXO fields indefinitely: a bad
                                        // write into the in-memory set is persisted at the next snapshot
                                        // save and reloaded on every restart, while the u/ disk store
                                        // (written from consensus-verified raw block data) stays correct.
                                        // Cross-check every active/staked UTXO against the disk store and
                                        // patch divergent fields, then persist the corrected snapshot so
                                        // the next restart starts clean.  Observed in production: an
                                        // admin restore flow inserted commitments recomputed from wrong
                                        // amounts, making the UTXOs unspendable until reconciled.
                                        recChecked, recFixed, recPubMism := utxos.ReconcileWithStore(
                                                func(txHash crypto.Hash32, outIdx uint32) (*core.UTXO, bool) {
                                                        su, gerr := db.GetUTXO(txHash, outIdx)
                                                        if gerr != nil || su == nil {
                                                                return nil, false
                                                        }
                                                        return &core.UTXO{
                                                                TxHash:       su.TxHash,
                                                                OutputIndex:  su.OutputIndex,
                                                                OneTimePub:   su.OneTimePub,
                                                                TxPubKey:     su.TxPubKey,
                                                                AmountCommit: su.AmountCommit,
                                                                EncAmount:    su.EncAmount,
                                                                BlockHeight:  su.BlockHeight,
                                                        }, true
                                                },
                                                func(u *core.UTXO, disk *core.UTXO) {
                                                        log.Warn("snapshot reconcile: in-memory UTXO diverges from disk store — patching",
                                                                "tx_hash", fmt.Sprintf("%x", u.TxHash[:]),
                                                                "out_idx", u.OutputIndex,
                                                                "mem_commit", fmt.Sprintf("%x", u.AmountCommit[:]),
                                                                "disk_commit", fmt.Sprintf("%x", disk.AmountCommit[:]))
                                                })
                                        if recFixed > 0 || recPubMism > 0 {
                                                log.Warn("snapshot reconcile: divergent entries patched from disk store",
                                                        "checked", recChecked, "fixed", recFixed,
                                                        "one_time_pub_mismatches", recPubMism)
                                                txTotRec, _ := db.LoadTxTotal()
                                                recSnap := startupSnapshot{
                                                        Version:    snapVersion,
                                                        TipHeight:  snap.TipHeight,
                                                        TipHashHex: snap.TipHashHex,
                                                        TxTotal:    txTotRec,
                                                        SavedAt:    time.Now(),
                                                        UTXOs:      utxos.TakeSnapshot(),
                                                        Registry:   registry.TakeSnapshot(),
                                                }
                                                if saveErr := saveStartupSnapshot(cfg.DataDir, recSnap); saveErr != nil {
                                                        log.Warn("snapshot reconcile: failed to save corrected snapshot",
                                                                "err", saveErr)
                                                } else {
                                                        log.Info("snapshot reconcile: corrected snapshot saved")
                                                }
                                        } else {
                                                log.Info("snapshot reconcile: in-memory UTXO set matches disk store",
                                                        "checked", recChecked)
                                        }

                                        // ── OOM-window AmountCommit raw-block validation ──────────────
                                        // ReconcileWithStore above cross-checks each UTXO against the
                                        // u/ disk store, but after an OOM kill the u/ store can itself
                                        // carry the corrupted value that was written from the same
                                        // partially-corrupted in-memory state.  When both the snapshot
                                        // and the u/ store agree on a wrong AmountCommit the reconcile
                                        // pass silently passes, leaving the UTXO unspendable until a
                                        // user hits the "forged commitment" error (C-0 failure).
                                        //
                                        // The raw block data is the authoritative source of truth:
                                        // block bytes are written atomically during consensus and are
                                        // never updated in-place.  We only pay the cost of reading
                                        // every block when a snapshot was saved within oomWindow of
                                        // this restart (the "OOM window"), i.e. when the in-memory
                                        // state that was snapshotted could plausibly have been
                                        // captured during or shortly before the OOM.  If the snapshot
                                        // is hours old, any in-memory corruption happened after the
                                        // snapshot was taken and is already absent from it.
                                        const oomWindow = 30 * time.Minute
                                        if wasOOMRestart && !snap.SavedAt.IsZero() &&
                                                time.Since(snap.SavedAt) < oomWindow {
                                                log.Info("startup: OOM window — validating snapshot AmountCommits against raw block data",
                                                        "snap_saved_at", snap.SavedAt.UTC().Format(time.RFC3339),
                                                        "age", time.Since(snap.SavedAt).Round(time.Second).String())
                                                acChecked, acFixed := validateAmountCommitsFromBlocks(utxos, db, log)
                                                if acFixed > 0 {
                                                        log.Warn("startup: OOM AmountCommit validation: mismatches patched from raw blocks",
                                                                "checked", acChecked, "fixed", acFixed)
                                                        // Persist corrected snapshot so subsequent restarts start clean.
                                                        oomTxTot, _ := db.LoadTxTotal()
                                                        oomSnap := startupSnapshot{
                                                                Version:    snapVersion,
                                                                TipHeight:  snap.TipHeight,
                                                                TipHashHex: snap.TipHashHex,
                                                                TxTotal:    oomTxTot,
                                                                SavedAt:    time.Now(),
                                                                UTXOs:      utxos.TakeSnapshot(),
                                                                Registry:   registry.TakeSnapshot(),
                                                        }
                                                        if saveErr := saveStartupSnapshot(cfg.DataDir, oomSnap); saveErr != nil {
                                                                log.Warn("startup: OOM validate: failed to save corrected snapshot", "err", saveErr)
                                                        } else {
                                                                log.Info("startup: OOM validate: corrected snapshot saved")
                                                        }
                                                } else {
                                                        log.Info("startup: OOM AmountCommit validation: all values match raw blocks",
                                                                "checked", acChecked)
                                                }
                                        } else if wasOOMRestart && snap.SavedAt.IsZero() {
                                                log.Info("startup: OOM restart detected but snapshot has no SavedAt timestamp " +
                                                        "(written by older binary) — skipping raw-block AmountCommit validation; " +
                                                        "run --rebuild-key-images if transfers fail with 'forged commitment'")
                                        }

                                        // ── --rebuild-key-images: fix stale snapshot key-image set ──
                                        // When a transaction is lost mid-flight (OOM kill after the
                                        // ring inputs were hashed but before the block was confirmed),
                                        // its key images can end up in the snapshot without ever
                                        // appearing on-chain.  The Go node then rejects the UTXO as
                                        // "already spent" even though the UTXO is active on-chain.
                                        // This flag scans all blocks, rebuilds key images from actual
                                        // confirmed transactions, saves the corrected snapshot, and
                                        // exits.  Start normally (without the flag) after it completes.
                                        if rebuildKeyImages {
                                                log.Info("--rebuild-key-images: clearing stale entries and rebuilding from block scan",
                                                        "tip_height", tipHeight,
                                                        "stale_key_images_in_snapshot", len(snap.UTXOs.KeyImages))
                                                kiBuilt, kiAllOK, kiErr := rebuildKeyImagesFromBlocks(db, tipHeight, utxos, forcePurgeKIIndex, log)
                                                if kiErr != nil {
                                                        return fmt.Errorf("--rebuild-key-images: %w", kiErr)
                                                }
                                                if !kiAllOK {
                                                        // Scan was incomplete or some k/ index writes failed.
                                                        // Do NOT save the snapshot: replacing it with a partial
                                                        // key-image set could forget genuine spends from blocks
                                                        // we could not read, re-opening spent outputs.
                                                        // The k/ index updates that did succeed are persisted.
                                                        log.Warn("--rebuild-key-images: rebuild incomplete — snapshot NOT saved",
                                                                "key_images_found", kiBuilt,
                                                                "hint", "on archive nodes run --repair-db first; on pruned nodes use --force-purge-ki-index")
                                                        fmt.Printf(
                                                                "aperod-node: key-image rebuild incomplete (%d entries from readable blocks) — snapshot NOT saved; see log for details\n",
                                                                kiBuilt)
                                                        return nil
                                                }
                                                log.Info("--rebuild-key-images: rebuild complete", "key_images_found", kiBuilt)
                                                txTotKI, _ := db.LoadTxTotal()
                                                kiFixSnap := startupSnapshot{
                                                        Version:    snapVersion,
                                                        TipHeight:  snap.TipHeight,
                                                        TipHashHex: snap.TipHashHex,
                                                        TxTotal:    txTotKI,
                                                        SavedAt:    time.Now(),
                                                        UTXOs:      utxos.TakeSnapshot(),
                                                        Registry:   registry.TakeSnapshot(),
                                                }
                                                if saveErr := saveStartupSnapshot(cfg.DataDir, kiFixSnap); saveErr != nil {
                                                        return fmt.Errorf("--rebuild-key-images: save snapshot: %w", saveErr)
                                                }
                                                fmt.Printf(
                                                        "aperod-node: key-image set rebuilt (%d entries) — start normally (without --rebuild-key-images)\n",
                                                        kiBuilt)
                                                return nil
                                        }

                                        // ── --rebuild-spent-index: backfill su/ for old spends ───────
                                        // After the full startup scan the in-memory UTXOSet is the
                                        // authoritative source of truth.  Any record in the u/ prefix
                                        // that is absent from the in-memory set was spent at some
                                        // point before the su/ index was introduced and never received
                                        // an su/ entry.  Write those missing entries now so the REST
                                        // endpoint correctly returns 404 for spent UTXOs.
                                        if rebuildSpentIndex {
                                                log.Info("--rebuild-spent-index: scanning u/ index", "tip_height", tipHeight)
                                                var marked, alreadyDone int
                                                iterErr := db.IterAllUTXOKeys(func(txHash crypto.Hash32, outIdx uint32) {
                                                        if utxos.Get(txHash, outIdx) != nil {
                                                                return // still active — skip
                                                        }
                                                        if db.IsUTXOSpent(txHash, outIdx) {
                                                                alreadyDone++
                                                                return // already in su/
                                                        }
                                                        if mErr := db.MarkUTXOSpent(txHash, outIdx); mErr != nil {
                                                                log.Warn("--rebuild-spent-index: failed to mark spent",
                                                                        "tx_hash", fmt.Sprintf("%x", txHash[:]),
                                                                        "out_idx", outIdx, "err", mErr)
                                                                return
                                                        }
                                                        marked++
                                                })
                                                if iterErr != nil {
                                                        return fmt.Errorf("--rebuild-spent-index: %w", iterErr)
                                                }
                                                log.Info("--rebuild-spent-index: done",
                                                        "newly_marked_spent", marked,
                                                        "already_in_index",   alreadyDone)
                                                fmt.Printf(
                                                        "aperod-node: spent-UTXO index rebuilt (%d entries added, %d already present) — start normally (without --rebuild-spent-index)\n",
                                                        marked, alreadyDone)
                                                return nil
                                        }

                                        // ── Post-snapshot block-replay gap-fill ──────────────────────
                                        // The snapshot captures the UTXO set at snap.TipHeight.
                                        // When the node was stopped without a clean SIGTERM (e.g.
                                        // OOM-kill or SIGKILL) after blocks were accepted beyond
                                        // that height, those post-snapshot blocks are in LevelDB
                                        // but their UTXOs are absent from the snapshot.
                                        // Transparent admin-mint outputs are the most visible case:
                                        // they disappear from address scans and circulating supply
                                        // shows 0 after restart.
                                        //
                                        // replayPostSnapshotGap replays each block from
                                        // snap.TipHeight+1 through tipHeight using raw LevelDB
                                        // block data (GetRawBlockByHeight → core.Block) — no
                                        // reliance on the su/ spent-UTXO index.  When
                                        // snap.TipHeight == tipHeight (normal clean shutdown) the
                                        // function is a no-op.
                                        if snap.TipHeight < tipHeight {
                                                gfAdded, gfSpent, gfOK := replayPostSnapshotGap(
                                                        db, utxos, registry, snap.TipHeight, tipHeight, log)
                                                if gfAdded > 0 || gfSpent > 0 || !gfOK {
                                                        log.Info("snapshot gap-fill: replayed post-snapshot blocks",
                                                                "snap_height",             snap.TipHeight,
                                                                "tip_height",              tipHeight,
                                                                "outputs_added",           gfAdded,
                                                                "key_images_marked_spent", gfSpent,
                                                                "complete",                gfOK)
                                                }
                                                if !gfOK {
                                                        // Gap-fill incomplete — cannot trust UTXO/registry state.
                                                        // Reset snapLoaded and hand the loaded snapshot to the
                                                        // rescue path so the startup scan covers only the gap.
                                                        snapLoaded = false
                                                        rescueSnap = snap
                                                }
                                        }
                                        // ── Phantom key-image detection (exact-tip path) ─────────────
                                        // Run after gap-fill so the KI set reflects its final state.
                                        // When gap-fill failed snapLoaded is false; the rescue path will
                                        // run its own check so we skip here to avoid a duplicate alert.
                                        if snapLoaded {
                                                checkPhantomKeyImages(db, utxos, apiSrv, log)
                                        }
                                }
                                // Snapshot was readable but failed the UTXO-count divergence check.
                                // Save a pointer so the rescue path below can seed key-images +
                                // UTXOs and start the scan only from this height onwards.
                                if !utxoCountOK && len(snap.UTXOs.KeyImages) > 0 {
                                        rescueSnap = snap
                                }
                        }
                }

                // ── Sub-tip snapshot + block-replay gap-fill ──────────────────────────
                // When tryLoadStartupSnapshot above found no snapshot at the exact chain
                // tip (e.g. after an unclean shutdown where blocks were accepted after the
                // last periodic snapshot was saved), find the highest valid snapshot below
                // tipHeight and replay only the missing blocks.  This is the correct fix
                // for the admin-mint UTXO loss bug: a transparent mint at height H is
                // in LevelDB but absent from the snapshot at height H-1, so after a
                // SIGKILL the UTXO disappears from address scans and circulating supply.
                //
                // If the gap-fill completes cleanly, snapLoaded is set true and the
                // (expensive) full startup scan is skipped entirely.  If any block in the
                // gap is missing or corrupt, the sub-tip snapshot is handed to rescueSnap
                // so the startup scan covers only the remaining gap instead of the full
                // chain — limiting the scan to a few blocks rather than millions.
                if !snapLoaded {
                        if gapSnap := findLatestSnapshot(cfg.DataDir, tipHeight, log); gapSnap != nil {
                                log.Info("found sub-tip snapshot — attempting gap-fill",
                                        "snap_height", gapSnap.TipHeight,
                                        "tip_height",  tipHeight)

                                // Validate UTXO count against the snapshot's own block hash
                                // (the count was stored when the snapshot was saved, keyed by
                                // the snapshot's tip hash, not the current chain tip hash).
                                gapCountOK := checkSnapshotUTXOCount(
                                        db,
                                        len(gapSnap.UTXOs.ActiveUTXOs),
                                        gapSnap.TipHashHex,
                                        cfg.Snapshot.UTXOCountTolerancePct,
                                        false, // not relaxed — we know the exact snapshot height
                                        cfg.Consensus.NonValidator,
                                        log,
                                )

                                if gapCountOK {
                                        utxos.RestoreFromSnapshot(gapSnap.UTXOs)
                                        registry.RestoreFromSnapshot(gapSnap.Registry)
                                        registry.SetUTXOSet(utxos)
                                        if gapSnap.TxTotal > 0 {
                                                initialTxTotal = gapSnap.TxTotal
                                        }
                                        runtime.GC()
                                        debug.FreeOSMemory()

                                        gfAdded, gfSpent, gfOK := replayPostSnapshotGap(
                                                db, utxos, registry, gapSnap.TipHeight, tipHeight, log)

                                        if gfOK {
                                                log.Info("startup gap-fill complete — sub-tip snapshot + replay",
                                                        "snap_height",             gapSnap.TipHeight,
                                                        "tip_height",              tipHeight,
                                                        "outputs_added",           gfAdded,
                                                        "key_images_marked_spent", gfSpent)
                                                snapLoaded = true
                                                // ── Phantom key-image detection (sub-tip path) ──────────
                                                // Gap-fill has applied all confirmed blocks; the KI set is
                                                // now in its final state for this startup path.
                                                checkPhantomKeyImages(db, utxos, apiSrv, log)
                                        } else {
                                                // Gap-fill incomplete — block missing or corrupt in range.
                                                // Seed the startup scan from the sub-tip snapshot so it
                                                // only covers the gap (gapSnap.TipHeight+1..tipHeight)
                                                // rather than the full chain.
                                                log.Warn("gap-fill incomplete — falling back to startup "+
                                                        "scan seeded from sub-tip snapshot",
                                                        "snap_height", gapSnap.TipHeight,
                                                        "tip_height",  tipHeight)
                                                rescueSnap = gapSnap
                                        }
                                } else if len(gapSnap.UTXOs.KeyImages) > 0 {
                                        // UTXO count diverged but snapshot has key-image data.
                                        // Use it as a rescue seed to shorten the scan.
                                        rescueSnap = gapSnap
                                }
                        }
                }

                // ── Snapshot rescue path ──────────────────────────────────────────────
                // When the tip-height snapshot failed the UTXO-count check but was
                // otherwise readable, use it to seed the in-memory UTXO + key-image
                // state.  The startup scan then covers only blocks from
                // rescueSnap.TipHeight+1 to tipHeight — typically a few hundred blocks
                // instead of scanning from block 1 (which can take hours on a 1M-block
                // chain after an OOM-corrupted LevelDB key-image index).
                if !snapLoaded && rescueSnap != nil {
                        utxos.RestoreFromSnapshot(rescueSnap.UTXOs)
                        registry.RestoreFromSnapshot(rescueSnap.Registry)
                        registry.SetUTXOSet(utxos)
                        if rescueSnap.TxTotal > 0 {
                                initialTxTotal = rescueSnap.TxTotal
                        }
                        log.Warn("snapshot rescue: seeding UTXO + key-image state from snapshot "+
                                "despite UTXO count divergence — scan covers remaining blocks only",
                                "snapshot_height", rescueSnap.TipHeight,
                                "key_images", len(rescueSnap.UTXOs.KeyImages),
                                "active_utxos", len(rescueSnap.UTXOs.ActiveUTXOs),
                        )
                        rescueSnapHeight = rescueSnap.TipHeight
                        // Structured alert log so operators / log aggregators can detect
                        // the rescue path without parsing the free-text message above.
                        log.Warn("startup: rescue path activated",
                                "snapshot_height", rescueSnapHeight,
                                "scan_from",       rescueSnapHeight+1,
                        )
                        // Expose the rescue flag on /api/v1/status for the lifetime of
                        // this process so the API-server system monitor can alert once.
                        if apiSrv != nil {
                                apiSrv.SetStartupRescue()
                        }
                        rescueSnap = nil // allow GC
                        runtime.GC()
                        debug.FreeOSMemory()
                        // ── Phantom key-image detection (rescue path) ────────────────
                        // KIs from the rescue snapshot are now in memory; the startup
                        // scan (below) will add confirmed KIs for blocks above the rescue
                        // height.  Run the check here so phantom KIs from an OOM-kill
                        // are surfaced immediately rather than silently blocking sends.
                        checkPhantomKeyImages(db, utxos, apiSrv, log)
                }

                if !snapLoaded {
                // When a rescue snapshot was used, key-images up to rescueSnapHeight are
                // already in memory.  Skip the LevelDB key-image index load; the startup
                // scan will collect key-images only for blocks rescueSnapHeight+1..tipHeight.
                kiCount := 0
                kiFromIndex := rescueSnapHeight > 0
                if !kiFromIndex {
                // Try the fast path for spent key images first.
                log.Info("loading spent key-image set from database index",
                        "tip_height", tipHeight)
                kiIterErr := db.IterKeyImages(func(ki crypto.KeyImage) error {
                        // With the LevelDB-backed UTXOSet, historical key images are
                        // not pre-loaded into RAM — IsSpent queries kiDB on demand.
                        // We iterate only to validate index health and get the count
                        // for keyImageIndexTrusted; no MarkSpent call is needed.
                        kiCount++
                        return nil
                })
                // FAIL-CLOSED: only trust the index when iteration succeeded and
                // the count is consistent with the chain height (see
                // keyImageIndexTrusted for the full contract).
                kiFromIndex = keyImageIndexTrusted(kiIterErr, kiCount, tipHeight)
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
                } // end !kiFromIndex (ki index load block)

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
                // Skip the UTXOFromIndex fast path when the rescue snapshot already
                // seeded the UTXO set: iterating db.IterActiveUTXOs would double-add
                // entries on top of the snapshot UTXOs.
                if kiFromIndex && tipHeight > 0 && rescueSnapHeight == 0 {
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
                                        utxos = core.NewUTXOSetWithDB(db)
                                        utxos.OnUTXOSpent = func(txHash crypto.Hash32, outIdx uint32) {
                                                if spentErr := db.MarkUTXOSpent(txHash, outIdx); spentErr != nil {
                                                        log.Warn("failed to persist spent UTXO", "err", spentErr)
                                                }
                                        }
                                        utxos.OnUTXORestored = func(txHash crypto.Hash32, outIdx uint32) error {
                                                return db.UnmarkUTXOSpent(txHash, outIdx)
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
                        ResumeScanFrom:        rescueSnapHeight,
                })
                if scanErr != nil {
                        return scanErr
                }
                initialTxTotal = scanResult.TxTotal
                _ = kiCount // already logged above; kept for the key-image index path
                } // end !snapLoaded

                // ── Background tx-hash index backfill (snapshot fast path) ────────────
                // When the snapshot fast path skips the full startup block scan, blocks
                // outside the in-memory chain window may lack t/ entries if they were
                // accepted before PutTxIdx was introduced or if their SST entries were
                // lost to an OOM-kill + RecoverFile cycle.  Without t/ entries,
                // getTransactionFromDisk (LookupTxIdx) cannot resolve those txs and
                // falls through to the slower u/ GetUTXO and in-memory UTXO fallbacks.
                //
                // This goroutine scans pre-window blocks (heights 1 … startLoad-1) in
                // the background and writes any missing t/ entries.  Progress is tracked
                // by the txidx_complete_height marker so each restart resumes from where
                // the previous run stopped — the first run after upgrading does the heavy
                // scan; subsequent boots that are already complete are instant.
                //
                // Blocks inside the chain window (≥ startLoad) already have t/ entries
                // written by storeBlock at acceptance time and are excluded.
                if snapLoaded && startLoad > 1 {
                        completeH, _, _ := db.LoadTxIdxCompleteHeight()
                        backfillTarget := startLoad - 1
                        if completeH < backfillTarget {
                                log.Info("tx index background backfill queued",
                                        "from_height", completeH+1,
                                        "to_height", backfillTarget,
                                )
                                go backfillTxIdxRange(completeH, backfillTarget, db, log)
                        }
                }
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

        // ── Startup UTXO store gap detection ─────────────────────────────────────
        // Sample the most recent 100 blocks and cross-reference every unspent
        // output against the u/ LevelDB prefix.  If any u/ entries are missing
        // the operator must run --repair-db before a withdrawal attempt triggers
        // the "Balance temporarily unavailable" error.  The sample keeps startup
        // overhead below 1 second; rebuildMissingUTXOs in --repair-db mode scans
        // the full chain.  Skip this on genesis (tipHeight == 0) and in repair
        // mode (rebuildMissingUTXOs has already re-populated all missing entries).
        if tipHeight > 0 && !repairDB {
                const sampleTail = uint64(100)
                gapCount, gapErr := sampleUTXOStoreGaps(db, tipHeight, sampleTail, log)
                if gapErr != nil {
                        log.Warn("startup UTXO store gap sample: read error — continuing",
                                "err", gapErr)
                }
                if gapCount > 0 {
                        log.Warn("UTXO store gap detected — u/ entries missing; run --repair-db before any transfer",
                                "missing_entries", gapCount,
                                "sampled_blocks", sampleTail,
                                "tip_height", tipHeight,
                                "hint", "aperod-node --repair-db")
                } else {
                        log.Info("startup UTXO store gap sample: no gaps detected",
                                "sampled_blocks", sampleTail,
                                "tip_height", tipHeight)
                }
                if apiSrv != nil {
                        apiSrv.SetUTXOStoreMissing(gapCount)
                }
        }

        // ── 7. Setup consensus engine ─────────────────────────────────────────────
        // host is declared here so OnBlockProduced can reference it
        // (assigned after engine creation; closure captures by reference).
        // apiSrv was declared above and started in the early API server section.
        var host *p2p.Host

        // engine is pre-declared with var so that the OnBlockProduced closure
        // can reference it (e.g. engine.Registry()) via a captured pointer.
        // By the time any block is produced, engine is fully initialised.
        var engine *consensus.Engine

        // gcInFlight guards the periodic CompactKeyImages+GC+FreeOSMemory
        // goroutine so at most one is active at any time.  Without this guard,
        // rapid block acceptance (catch-up sync or multi-block bursts) can
        // queue an unbounded backlog of goroutines that each serialise on the
        // UTXOSet write lock, then each force a stop-the-world GC, worsening
        // exactly the performance problem this maintenance is meant to solve.
        // Both the periodic-GC path and the post-snapshot cleanup path use
        // this same gate so they cannot overlap each other either.
        var gcInFlight atomic.Bool

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
			RingCTV4ActivationHeight: cfg.Consensus.RingCTV4ActivationHeight,
RewardAuthorizationActivationHeight: cfg.Consensus.RewardAuthorizationActivationHeight,
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

                        // Periodically compact key images, force GC, and return freed
                        // pages to the OS so RSS does not grow unboundedly between
                        // snapshot saves.
                        //
                        // CompactKeyImages flushes the 'recent' map (≈150 B/entry
                        // Go-map overhead) to the persistent LevelDB key-image index
                        // and clears the map.  After the flush, IsSpent lookups for
                        // those KIs go directly to LevelDB (OS page cache, sub-ms)
                        // rather than the Go map, keeping RSS proportional to blocks
                        // seen since the last flush rather than to total chain history.
                        //
                        // 100 blocks ≈ 5 min at 1 block/3 s.  The previous value (500)
                        // fired every ~25 min, allowing up to 540 MB of RSS growth between
                        // FreeOSMemory calls when live data was accumulating.  Lowering to
                        // 100 caps the oscillation window to ~100 MB, keeping peak RSS
                        // well below the watchdog threshold.
                        const gcEveryBlocks = uint64(100)
                        if h > 0 && h%gcEveryBlocks == 0 {
                                // Single-flight guard: skip if a prior GC goroutine is still
                                // running.  Without this, rapid block acceptance during catch-up
                                // sync queues an unbounded backlog of goroutines that all
                                // serialise on the UTXOSet write lock, then each force a
                                // stop-the-world GC — worsening the problem this task solves.
                                if gcInFlight.CompareAndSwap(false, true) {
                                        go func(height uint64) {
                                                defer gcInFlight.Store(false)
                                                rssBefore := readRSSBytes()
                                                recentMoved := utxos.CompactKeyImages()
                                                runtime.GC()
                                                debug.FreeOSMemory() // return freed pages to OS so GOMEMLIMIT has headroom
                                                rssAfter := readRSSBytes()
                                                var ms runtime.MemStats
                                                runtime.ReadMemStats(&ms)
                                                log.Info("periodic GC complete",
                                                        "height", height,
                                                        "ki_recent_compacted", recentMoved,
                                                        "rss_before_mb", rssBefore>>20,
                                                        "rss_after_mb", rssAfter>>20,
                                                        "rss_freed_mb", (rssBefore-rssAfter)>>20,
                                                        "heap_alloc_mb", int64(ms.HeapAlloc)>>20,
                                                        "heap_sys_mb", int64(ms.HeapSys)>>20,
                                                        "heap_idle_mb", int64(ms.HeapIdle)>>20,
                                                        "num_gc", ms.NumGC,
                                                )
                                        }(h)
                                } else {
                                        log.Debug("periodic GC skipped — previous cycle still in flight", "height", h)
                                }
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
                                SavedAt:    time.Now(),
                                UTXOs:      utxos.TakeSnapshot(),
                                Registry:   engine.Registry().TakeSnapshot(),
                        }
                        // Purge any mempool KIs that were never confirmed on-chain.
                        // Confirmed KIs are always in the persistent index; mempool-only
                        // KIs are not.  Without this filter, an OOM kill can freeze a
                        // valid UTXO permanently (phantom key-image cycle).
                        filterSnapshotKeyImages(&periodicSnap, db, log)
                        periodicActive := len(periodicSnap.UTXOs.ActiveUTXOs)
                        go func(snap startupSnapshot, height uint64, activeCount int) {
                                periodicSaveStart := time.Now()
                                if saveErr := saveStartupSnapshot(cfg.DataDir, snap); saveErr != nil {
                                        log.Warn("failed to save periodic snapshot",
                                                "height", height, "err", saveErr)
                                        if apiSrv != nil {
                                                apiSrv.SetSnapshotFailed(saveErr)
                                        }
                                } else {
                                        log.Info("periodic snapshot saved",
                                                "height", height,
                                                "save_duration", time.Since(periodicSaveStart).Round(time.Millisecond).String(),
                                        )
                                        if apiSrv != nil {
                                                apiSrv.SetSnapshotSaved(height)
                                        }
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
                                // Compact key images after the snapshot: TakeSnapshot already
                                // serialised them, so we can now merge the 'recent' map
                                // (≈150 B/entry Go-map overhead) into the 'sorted' slice
                                // (32 B/entry).  GC reclaims the old map buckets immediately
                                // after this call.
                                // Use the same single-flight guard as the periodic GC path so
                                // the two goroutines can never overlap and queue concurrent
                                // stop-the-world GC events.
                                if gcInFlight.CompareAndSwap(false, true) {
                                        defer gcInFlight.Store(false)
                                        utxos.CompactKeyImages()
                                        runtime.GC()
                                        debug.FreeOSMemory() // return freed pages to OS so GOMEMLIMIT has headroom
                                } else {
                                        log.Debug("post-snapshot GC skipped — periodic GC cycle still in flight", "height", height)
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
                                runMemoryWatchdogLoop(t.C, stop, threshold, limit, readRSSBytes, memPressureCh, log)
                        }()
                }
        }

        // Mempool eviction: remove expired/old transactions every
        // MempoolEvictIntervalSec seconds (default 300 = 5 minutes) so
        // low-fee transactions do not accumulate indefinitely in RAM.
        mempoolEvictInterval := config.ResolveMempoolEvictInterval(cfg.MempoolEvictIntervalSec)
        go func() {
                t := time.NewTicker(mempoolEvictInterval)
                defer t.Stop()
                for {
                        select {
                        case <-stop:
                                return
                        case <-t.C:
                                n := mempool.Evict()
                                if n > 0 {
                                        log.Info("mempool: evicted expired transactions", "count", n)
                                }
                        }
                }
        }()

        // Nightly restart scheduler: at maintenance.restart_at (UTC, HH:MM) the
        // node sends SIGTERM to itself for a graceful RAM-reclaim restart.  Go
        // heap fragmentation grows ~1.3 GB/h in production; a nightly SIGTERM
        // triggers a clean snapshot-save → restart before memory pressure builds.
        // The scheduler respects the stop channel so a concurrent external
        // SIGTERM (operator or watchdog) simply cancels the pending self-restart.
        if cfg.Maintenance.RestartAt != "" {
                h, m, _ := config.ParseRestartAt(cfg.Maintenance.RestartAt) // already validated by Validate()
                go runNightlyRestartScheduler(stop, h, m, time.Now, time.After, func() {
                        log.Info("nightly restart: sending SIGTERM to self for scheduled RAM-reclaim restart",
                                "restart_at", fmt.Sprintf("%02d:%02d UTC", h, m))
                        if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
                                log.Warn("nightly restart: failed to send SIGTERM to self", "err", err)
                        }
                }, log)
        }

        if apiSrv != nil {
                // Wire engine-dependent options now that the consensus engine exists.
                apiSrv.SetRegistry(engine.Registry())
                apiSrv.SetValidatorKey(myKey)
                apiSrv.SetTxTotal(initialTxTotal)
                apiSrv.SetTimestampRejectedCounter(func() int64 { return engine.TimestampRejectedCount() })
                // Admin mints are built at block-production time so every mint gets a
                // unique one-time pub (spend_pub + height*G) → unique key image.
                apiSrv.SetMintScheduler(func(addr string, amountNAPR uint64, timeout time.Duration) (string, uint64, error) {
                        h, height, err := engine.ScheduleAdminMint(addr, amountNAPR, timeout)
                        if err != nil {
                                return "", 0, err
                        }
                        return fmt.Sprintf("%x", h[:]), height, nil
                })
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
                // Background UTXO-store integrity audit: sampled u/ entries +
                // recent blocks vs. raw block data, exposed on
                // /api/v1/admin/utxo-audit for the api-server Telegram monitor.
                startUTXOAuditLoop(db, apiSrv, log)
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
                        host = p2p.NewHost(
                                buildP2PConfig(cfg, tcpAddr, bootnodes, myKey.Public().ID(), tlsCfg, nodeFingerprint),
                                handler, log)
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
                                // Expose reconnect_backoff_active so monitoring can
                                // distinguish "relay stuck in dial back-off with 0 peers"
                                // from a healthy just-started node.
                                apiSrv.SetReconnectBackoffFlag(host.ReconnectBackoffActive)
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
                                // Wire peer-list snapshot for /api/v1/network/peers so
                                // external monitoring can detect relay-node lag.
                                apiSrv.SetPeerListFunc(func() []api.PeerListEntry {
                                        peers := host.GetPeerList()
                                        out := make([]api.PeerListEntry, len(peers))
                                        for i, p := range peers {
                                                out[i] = api.PeerListEntry{Addr: p.Addr, Height: p.Height, Direction: p.Direction}
                                        }
                                        return out
                                })
                                // Wire live keepalive-interval tuning so the Admin Panel
                                // can adjust it without a node restart.
                                apiSrv.SetP2PKeepaliveGetFunc(host.GetKeepaliveInterval)
                                apiSrv.SetP2PKeepaliveSetFunc(host.SetKeepaliveInterval)
                                // Task #1910 — persist keepalive tuning to node.yaml
                                // (atomic tmp+rename) so it survives a node restart,
                                // and expose the persisted yaml value so the Admin
                                // Panel can flag live≠yaml drift.
                                apiSrv.SetP2PKeepalivePersistFunc(func(d time.Duration) error {
                                        return persistKeepaliveInterval(cfgPath, d)
                                })
                                apiSrv.SetP2PKeepaliveYAMLFunc(func() (time.Duration, error) {
                                        return readYAMLKeepaliveInterval(cfgPath)
                                })
                                // Wire static rogue-fork ban parameters so the Admin Panel
                                // can display the values configured in node.yaml.
                                apiSrv.SetP2PBanConfig(
                                        cfg.P2P.BadBlockBanThreshold,
                                        int64(cfg.P2P.BadBlockBanDuration.Seconds()),
                                        cfg.P2P.BadBlockHeightLead,
                                )
                                // Task #1922 — wire LIVE wrong-fork ban tuning so operators
                                // can tighten the threshold from the Admin Panel without a
                                // node restart.
                                apiSrv.SetP2PBanConfigGetFunc(host.GetBanConfig)
                                apiSrv.SetP2PBanConfigSetFunc(host.SetBanConfig)
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
                                // Wire peer-ban event log for admin Telegram notifications.
                                apiSrv.SetBanEventFunc(func(since time.Time) []api.BanEventEntry {
                                        evts := host.GetBanEvents(since)
                                        out := make([]api.BanEventEntry, len(evts))
                                        for i, e := range evts {
                                                out[i] = api.BanEventEntry{
                                                        IP:              e.IP,
                                                        PeerAddr:        e.PeerAddr,
                                                        PeerID:          e.PeerID,
                                                        Reason:          e.Reason,
                                                        Violations:      e.Violations,
                                                        BanDurationSecs: e.BanDurationSecs,
                                                        At:              e.At,
                                                }
                                        }
                                        return out
                                })
                                // Wire block-fetch stall event log for the Admin Panel notification log.
                                apiSrv.SetStallEventFunc(func(since time.Time) []api.StallEventEntry {
                                        evts := host.GetStallEvents(since)
                                        out := make([]api.StallEventEntry, len(evts))
                                        for i, e := range evts {
                                                out[i] = api.StallEventEntry{
                                                        PeerAddr:    e.PeerAddr,
                                                        StalledCount: e.StalledCount,
                                                        At:          e.At,
                                                }
                                        }
                                        return out
                                })
                                // Wire duplicate-identity conflict events for the Admin Panel notification log.
                                apiSrv.SetDuplicateIdentityEventFunc(func(since time.Time) []api.DuplicateIdentityEntry {
                                        evts := host.GetDuplicateIdentityEvents(since)
                                        out := make([]api.DuplicateIdentityEntry, len(evts))
                                        for i, e := range evts {
                                                out[i] = api.DuplicateIdentityEntry{
                                                        Addr:        e.Addr,
                                                        Fingerprint: e.Fingerprint,
                                                        At:          e.At,
                                                }
                                        }
                                        return out
                                })
                                // Wire malformed/stale bootnode warning events for the Admin Panel.
                                apiSrv.SetBootnodeWarnEventFunc(func(since time.Time) []api.BootnodeWarnEntry {
                                        evts := host.GetBootnodeWarnEvents(since)
                                        out := make([]api.BootnodeWarnEntry, len(evts))
                                        for i, e := range evts {
                                                out[i] = api.BootnodeWarnEntry{
                                                        Bootnode: e.Bootnode,
                                                        Err:      e.Err,
                                                        AgeSecs:  e.AgeSecs,
                                                        At:       e.At,
                                                }
                                        }
                                        return out
                                })
                                // Wire live stale-bootnode status for the /health endpoint so
                                // the Admin Panel health widget can surface degraded DNS without SSH.
                                apiSrv.SetStaleBootnodeFn(func() []api.StaleBootnodeEntry {
                                        nodes := host.GetStaleBootnodes()
                                        out := make([]api.StaleBootnodeEntry, len(nodes))
                                        for i, n := range nodes {
                                                out[i] = api.StaleBootnodeEntry{
                                                        Bootnode:   n.Bootnode,
                                                        AgeSeconds: n.AgeSeconds,
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

        performShutdown(stop, engineDone, db, utxos, registry, cfg.DataDir, log, apiSrv)

        // Persist pending mempool transactions so they survive the restart.
        saveMempoolOnShutdown(mempool, cfg.DataDir, log)

        return nil
}

// saveMempoolOnShutdown persists the mempool to dataDir during a graceful
// shutdown and reports the outcome to the log.  A Save() failure MUST be
// surfaced at WARN level (task #1625): losing the mempool silently on shutdown
// makes it impossible to tell — from the journal alone — whether pending
// transactions were preserved across a restart.  On success it logs the number
// of persisted transactions at INFO level.
//
// This is the exact code path the SIGTERM/SIGINT handler in run() invokes; it
// is extracted into a named function so it can be tested in isolation with a
// non-writable data directory (see shutdown_save_log_test.go).
func saveMempoolOnShutdown(mempool *core.Mempool, dataDir string, log *slog.Logger) {
        if mpErr := mempool.Save(dataDir); mpErr != nil {
                log.Warn("shutdown: failed to save mempool", "err", mpErr)
        } else {
                log.Info("shutdown: mempool saved", "pending_txs", mempool.Count())
        }
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
        apiSrv *api.Server,
) {
        // Step 1 + 2: stop the engine and wait for full quiescence.
        // GetTip MUST NOT be called before this point.
        close(stop)
        <-engineDone

        // Step 3: reclaim as much memory as possible before the snapshot save so
        // that TakeSnapshot() itself — which serialises the full UTXO set — does
        // not push RSS above GOMEMLIMIT and trigger an OOM-kill mid-write.  This
        // mirrors the post-snapshot-load GC already performed on startup.
        rssBeforeGC := readRSSBytes()
        runtime.GC()
        debug.FreeOSMemory()
        rssAfterGC := readRSSBytes()
        log.Info("shutdown: GC before snapshot save",
                "rss_before_mb", rssBeforeGC>>20,
                "rss_after_mb", rssAfterGC>>20,
                "rss_freed_mb", (rssBeforeGC-rssAfterGC)>>20,
        )

        // Step 4: read the final tip and save the snapshot.
        saveShutdownSnapshot(db, utxos, registry, dataDir, log, apiSrv)
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
        apiSrv *api.Server,
) {
        const snapshotDropinDir  = "/etc/systemd/system/aperod-node.service.d"
        const snapshotServicePath = "/etc/systemd/system/aperod-node.service"
        saveShutdownSnapshotWithPaths(db, utxos, registry, dataDir, log, apiSrv,
                snapshotDropinDir, snapshotServicePath, 0)
}

// filterSnapshotKeyImages removes key images from snap.UTXOs that are absent
// from the persistent LevelDB spent-key-image index (k/ entries).  Confirmed
// key images are persisted synchronously on block acceptance; unconfirmed ones
// (e.g. from a mempool transaction that was lost after an OOM-kill or a crash
// before the block was mined) are only in the in-memory set and must not be
// written to the snapshot — if saved they permanently mark a valid UTXO as
// spent, requiring a manual re-mint to recover the balance.
//
// Safe fallbacks:
//   - If IterKeyImages fails, filtering is skipped (all KIs kept).
//   - If the index is empty (genesis or index not yet built), filtering is
//     skipped so genesis nodes don't lose their initial KI state.
func filterSnapshotKeyImages(snap *startupSnapshot, db *store.DB, log *slog.Logger) {
        confirmed := make(map[crypto.KeyImage]bool)
        if iterErr := db.IterKeyImages(func(ki crypto.KeyImage) error {
                confirmed[ki] = true
                return nil
        }); iterErr != nil {
                log.Warn("snapshot: could not read key-image index — snapshot KIs unfiltered", "err", iterErr)
                return
        }
        if len(confirmed) == 0 {
                return // genesis node or index not yet built — nothing to filter
        }
        before := len(snap.UTXOs.KeyImages)
        kept := snap.UTXOs.KeyImages[:0]
        for _, ki := range snap.UTXOs.KeyImages {
                if confirmed[ki] {
                        kept = append(kept, ki)
                }
        }
        snap.UTXOs.KeyImages = kept
        if purged := before - len(kept); purged > 0 {
                log.Warn("snapshot: purged unconfirmed key images before save",
                        "purged", purged,
                        "remaining", len(kept),
                        "hint", "mempool KIs that were never confirmed on-chain (e.g. from an OOM kill)")
        }
}

// countPhantomKeyImages returns the number of key images present in the in-memory
// UTXOSet but absent from the persistent LevelDB index.  Such entries are phantoms:
// they were saved into a snapshot from in-flight mempool transactions that were
// lost (e.g. OOM kill) before confirming on-chain.  Each phantom marks a live
// UTXO as "spent", blocking withdrawals until the operator runs --rebuild-key-images.
//
// Algorithm: collect in-memory KIs into a local slice (read lock held only for
// the fast in-memory traversal), then release the lock and do per-KI point-lookups
// via IsKeyImageSpent.  Peak transient allocation is 32 B/entry for the slice —
// released after the LevelDB pass — rather than the ~150 B/entry that a Go map
// representation of the full index would require.  This bounded-memory approach
// is safe for OOM-restart scenarios where RSS is already elevated.
func countPhantomKeyImages(db *store.DB, utxos *core.UTXOSet) (phantom, checked int) {
        // Phase 1: collect in-memory KIs without I/O under the read lock.
        var inMemKIs []crypto.KeyImage
        utxos.IterKeyImages(func(ki crypto.KeyImage) {
                inMemKIs = append(inMemKIs, ki)
        })
        checked = len(inMemKIs)
        // Phase 2: per-KI point-lookup against the persistent LevelDB index.
        for _, ki := range inMemKIs {
                confirmed, lookupErr := db.IsKeyImageSpent(ki)
                if lookupErr != nil {
                        continue // assume confirmed on error — no false positives
                }
                if !confirmed {
                        phantom++
                }
        }
        inMemKIs = nil // release the temporary slice
        return phantom, checked
}

// checkPhantomKeyImages launches a goroutine that calls countPhantomKeyImages and,
// when phantoms are found, logs a warning and exposes the count on /api/v1/status
// via srv.SetPhantomKICount so the API-server monitor can fire a Telegram alert.
//
// Call after any snapshot restore path has reached its final key-image state
// (including successful gap-fill).  srv may be nil.
func checkPhantomKeyImages(db *store.DB, utxos *core.UTXOSet, srv *api.Server, log *slog.Logger) {
        go func() {
                if utxos.KeyImagesCount() == 0 {
                        // With the LevelDB-backed UTXOSet, key images are not
                        // pre-loaded into RAM at startup — only the small 'recent'
                        // map is in memory.  Phantom KIs (mempool entries saved into
                        // old snapshots that falsely blocked UTXOs) cannot occur
                        // because restoreFromSlice is a no-op and kiDB holds only
                        // confirmed on-chain key images.
                        log.Info("startup: phantom-ki-check skipped — key images are LevelDB-backed (no RAM pre-load)")
                        return
                }
                phantom, checked := countPhantomKeyImages(db, utxos)
                if phantom > 0 {
                        log.Warn("startup: phantom key images detected — active UTXOs may be blocked",
                                "phantom_count", phantom,
                                "checked",       checked,
                                "hint",          "run --rebuild-key-images to clear stale entries and restore affected UTXOs")
                        if srv != nil {
                                srv.SetPhantomKICount(phantom)
                        }
                } else {
                        log.Info("startup: phantom-ki-check passed — all snapshot key images confirmed on-chain",
                                "checked", checked)
                }
        }()
}

// saveShutdownSnapshotWithPaths is the real implementation of saveShutdownSnapshot.
// dropinDir and servicePath are injectable so tests can supply synthetic systemd
// config files without touching real /etc paths.
//
// saveDurOverride, when non-zero, is used as the snapshot save duration for the
// ratio check instead of the real measured wall time.  Pass 0 in production; pass
// a synthetic value in tests to exercise the ratio-warning path deterministically
// without depending on wall-clock write speed.
func saveShutdownSnapshotWithPaths(
        db *store.DB,
        utxos *core.UTXOSet,
        registry *core.ValidatorRegistry,
        dataDir string,
        log *slog.Logger,
        apiSrv *api.Server,
        dropinDir, servicePath string,
        saveDurOverride time.Duration,
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
                SavedAt:    time.Now(),
                UTXOs:      utxos.TakeSnapshot(),
                Registry:   registry.TakeSnapshot(),
        }
        // Purge any mempool KIs that were never confirmed on-chain before
        // persisting so that an OOM-kill followed by a graceful restart does
        // not carry forward phantom entries from a previous dirty snapshot.
        filterSnapshotKeyImages(&shutSnap, db, log)
        snapSaveStart := time.Now()
        if saveErr := saveStartupSnapshot(dataDir, shutSnap); saveErr != nil {
                log.Warn("shutdown: failed to save snapshot", "err", saveErr)
                if apiSrv != nil {
                        apiSrv.SetSnapshotFailed(saveErr)
                }
                return
        }
        // Record a clean-shutdown marker so the next startup knows this
        // shutdown was graceful and can skip the OOM-window AmountCommit
        // validation.  Non-fatal: an absent marker causes validation to run
        // (fail-closed).
        if markerErr := writeCleanShutdownMarker(dataDir); markerErr != nil {
                log.Warn("shutdown: failed to write clean-shutdown marker (next startup will validate AmountCommits)",
                        "err", markerErr)
        }
        if apiSrv != nil {
                apiSrv.SetSnapshotSaved(shutTipHeight)
        }
        snapSaveDur := time.Since(snapSaveStart).Round(time.Millisecond)
        log.Info("shutdown: snapshot saved",
                "tip_height", shutTipHeight,
                "save_duration", snapSaveDur.String(),
        )

        // Warn when the save duration is approaching the systemd stop timeout.
        // If the ratio keeps creeping up (e.g. due to a CPU quota or a growing
        // UTXO set), the next SIGKILL will arrive before the rename completes
        // and the node will fall back to a multi-hour block scan on the next
        // restart — with no advance notice.
        //
        //   > 50 % of TimeoutStopSec → Warn  (early notice; tune now)
        //   > 80 % of TimeoutStopSec → Error (critical; increase immediately)
        //
        // saveDurOverride, when non-zero, replaces the real measured duration for
        // the ratio check so that tests can trigger the warning path deterministically
        // without relying on wall-clock write speed.
        warnDur := snapSaveDur
        if saveDurOverride > 0 {
                warnDur = saveDurOverride
        }
        warnIfSnapshotSlowRelativeToTimeout(
                warnDur,
                dropinDir,
                servicePath,
                log,
        )
        // Expose timing via /api/v1/status so the Admin Panel can display the
        // timeout-ratio risk indicator without log access.
        if apiSrv != nil {
                timeoutSec, _ := readEffectiveTimeoutStopSec(dropinDir, servicePath)
                apiSrv.SetSnapshotTimings(snapSaveDur, timeoutSec)
        }

        deleteOldSnapshots(dataDir, shutTipHeight)
        // Persist the active UTXO count keyed by tip hash so the
        // next restart's divergence check has an active-only reference
        // count specific to this snapshot.
        if metaErr := db.StoreActiveUTXOCount(shutSnap.TipHashHex, len(shutSnap.UTXOs.ActiveUTXOs)); metaErr != nil {
                log.Warn("shutdown: failed to persist active_utxo_count metadata", "err", metaErr)
        }
        // Persist the save duration so the next boot can compare it against the
        // configured TimeoutStopSec before the next shutdown (proactive warning).
        if durErr := db.StoreSnapshotSaveDuration(snapSaveDur.Milliseconds()); durErr != nil {
                log.Warn("shutdown: failed to persist last_snap_save_ms metadata", "err", durErr)
        }
        // Persist the effective TimeoutStopSec alongside the save duration so
        // the next startup can restore the full timing context (duration + timeout)
        // without re-reading systemd config — and mark the values as
        // "from previous shutdown" in /api/v1/status immediately on boot.
        {
                timeoutSec, _ := readEffectiveTimeoutStopSec(dropinDir, servicePath)
                if toutErr := db.StoreSnapshotTimeoutSec(timeoutSec); toutErr != nil {
                        log.Warn("shutdown: failed to persist last_snap_timeout_sec metadata", "err", toutErr)
                }
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

// runMemoryWatchdogLoop is the body of the memory-watchdog goroutine.
// It is extracted from the anonymous goroutine in run() so it can be
// exercised in unit tests with an injectable ticker channel and RSS reader.
//
// Parameters:
//
//	ticks       — receives time.Time values at the poll interval (use t.C in
//	              production; send manually in tests)
//	stop        — closed when the node is shutting down
//	threshold   — RSS bytes at which a proactive restart is triggered
//	limit       — the GOMEMLIMIT value (logged only)
//	readRSS     — returns current RSS in bytes; injectable for testing
//	memPressureCh — receives a struct{}{} when threshold is exceeded
//	log         — structured logger
//
// The function blocks until stop is closed.  It fires at most once (fired flag)
// so a sustained high-RSS period does not send redundant signals.
func runMemoryWatchdogLoop(
        ticks <-chan time.Time,
        stop <-chan struct{},
        threshold uint64,
        limit int64,
        readRSS func() int64,
        memPressureCh chan<- struct{},
        log *slog.Logger,
) {
        var fired bool
        for {
                select {
                case <-stop:
                        return
                case <-ticks:
                        if fired {
                                continue
                        }
                        rss := readRSS()
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
// emitConfigWarnings iterates over the non-fatal warnings returned by
// cfg.Warnings() and logs each one at WARN level.  Extracting the loop into
// this helper makes it directly testable without spinning up a full node.
func emitConfigWarnings(log *slog.Logger, cfg *config.Config) {
        for _, w := range cfg.Warnings() {
                log.Warn("config warning", "msg", w)
        }
}

func checkGOMLEMLIMIT(gomlimitEnv string, configLimitApplied bool, strictMode bool, memLimitDisabled bool, dropinPath string, log *slog.Logger) error {
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

        // The operator has declared that running uncapped is intentional
        // (memory_limit_disabled: true in node.yaml).  Downgrade the warning to
        // DEBUG so the journal is not spammed with a WARN on every startup while
        // still leaving a trace for anyone who enables debug logging.
        if memLimitDisabled {
                log.Debug(warnMsg,
                        "gomemlimit_value", gomlimitEnv,
                        "memory_limit_disabled", true,
                        "fix", fix,
                )
                return nil
        }

        log.Warn(warnMsg,
                "gomemlimit_value", gomlimitEnv,
                "fix", fix,
        )
        return nil
}

// parseSystemdDuration parses a systemd TimeoutStopSec value and returns the
// equivalent number of seconds.  Accepted forms (matching systemd grammar):
//
//   - plain number: "900"          (bare seconds)
//   - with suffix:  "900s", "15min", "1h", "1d", "1w"
//   - "infinity"                   (mapped to 1e18 — effectively unlimited)
//
// The function handles the most common single-unit forms used in real drop-in
// files.  Returns (seconds, true) on success; (0, false) on parse failure so
// callers can silently skip unknown values without crashing.
func parseSystemdDuration(val string) (float64, bool) {
        val = strings.TrimSpace(val)
        if strings.EqualFold(val, "infinity") {
                return 1e18, true
        }
        // Plain bare seconds — the most common form: "900".
        if secs, err := strconv.ParseFloat(val, 64); err == nil {
                return secs, true
        }
        // Single-unit suffixes.  Listed longest-first so "min" and "ms" are
        // matched before their prefix "m" / "s" could be.
        type unit struct {
                sfx string
                mul float64
        }
        units := []unit{
                {"month", 30 * 24 * 3600},
                {"min", 60},
                {"ms", 0.001},
                {"us", 0.000001},
                {"ns", 1e-9},
                {"h", 3600},
                {"d", 24 * 3600},
                {"w", 7 * 24 * 3600},
                {"s", 1},
        }
        for _, u := range units {
                if strings.HasSuffix(val, u.sfx) {
                        numStr := strings.TrimSpace(val[:len(val)-len(u.sfx)])
                        if n, err := strconv.ParseFloat(numStr, 64); err == nil {
                                return n * u.mul, true
                        }
                }
        }
        return 0, false
}

// readEffectiveTimeoutStopSec returns the effective TimeoutStopSec value (in
// seconds) for the aperod-node service.  It scans every *.conf file in
// dropinDir in lexicographic order (matching systemd drop-in precedence —
// later files override earlier ones) and returns the LAST TimeoutStopSec
// value found.  If none is found in the directory it falls back to
// servicePath.
//
// Returns (seconds, true) when a value is found and parseable; (0, false)
// when no TimeoutStopSec line is present anywhere or all files are absent.
// Parse errors are silently ignored so callers do not need to handle an error
// value — the worst case is that no threshold check runs.
func readEffectiveTimeoutStopSec(dropinDir, servicePath string) (float64, bool) {
        // scanFile reads all TimeoutStopSec= directives in path and returns the
        // LAST parseable value found.  systemd unit files allow a directive to be
        // reassigned multiple times; the final assignment takes effect.  Returning
        // the first match is wrong when a drop-in overrides an earlier setting in
        // the same file.  Whitespace around "=" is trimmed to match systemd's
        // lenient unit-file parser.
        scanFile := func(path string) (float64, bool) {
                f, err := os.Open(path)
                if err != nil {
                        return 0, false
                }
                defer f.Close()
                var last float64
                found := false
                sc := bufio.NewScanner(f)
                for sc.Scan() {
                        line := strings.TrimSpace(sc.Text())
                        after, ok := strings.CutPrefix(line, "TimeoutStopSec")
                        if !ok {
                                continue
                        }
                        after = strings.TrimSpace(after)
                        if !strings.HasPrefix(after, "=") {
                                continue
                        }
                        val := strings.TrimSpace(after[1:])
                        if secs, ok := parseSystemdDuration(val); ok {
                                last = secs
                                found = true
                                // Do NOT return early — a later directive in the same file overrides.
                        }
                }
                return last, found
        }

        // Scan all *.conf files in the drop-in directory in lex order;
        // keep the LAST TimeoutStopSec seen (later drop-ins win in systemd).
        if dropinDir != "" {
                if entries, err := os.ReadDir(dropinDir); err == nil {
                        var last float64
                        found := false
                        for _, e := range entries {
                                if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
                                        continue
                                }
                                if secs, ok := scanFile(filepath.Join(dropinDir, e.Name())); ok {
                                        last = secs
                                        found = true
                                }
                        }
                        if found {
                                return last, true
                        }
                }
        }

        // Fall back to the main unit file.
        if servicePath != "" {
                return scanFile(servicePath)
        }
        return 0, false
}

// warnIfSnapshotSlowRelativeToTimeout emits a structured log entry when the
// snapshot save duration represents a significant fraction of the effective
// systemd TimeoutStopSec.  It is called immediately after every shutdown
// snapshot save so operators receive advance notice before the ratio crosses
// the dangerous threshold.
//
//   - dur > 80 % of TimeoutStopSec → log.Error  (critical; act now)
//   - dur > 50 % of TimeoutStopSec → log.Warn   (early warning; plan ahead)
//
// dropinDir is the drop-in directory (e.g. /etc/systemd/system/aperod-node.service.d)
// whose *.conf files are scanned in lex order.  servicePath is the main unit
// file consulted when no drop-in value is found.  Both are injectable for
// unit tests; on non-systemd hosts neither path exists and the function is a
// no-op (readEffectiveTimeoutStopSec returns (0, false)).
func warnIfSnapshotSlowRelativeToTimeout(dur time.Duration, dropinDir, servicePath string, log *slog.Logger) {
        timeoutSec, found := readEffectiveTimeoutStopSec(dropinDir, servicePath)
        if !found || timeoutSec <= 0 || timeoutSec >= 1e17 { // 1e17 = "infinity"
                return
        }
        ratio := dur.Seconds() / timeoutSec
        fix := fmt.Sprintf("increase TimeoutStopSec in a file under %s/ and run: systemctl daemon-reload", dropinDir)
        switch {
        case ratio > 0.80:
                log.Error("snapshot save time is dangerously close to TimeoutStopSec — increase it immediately to avoid losing the snapshot on next shutdown",
                        "save_duration", dur.String(),
                        "timeout_stop_sec", timeoutSec,
                        "ratio_pct", fmt.Sprintf("%.0f%%", ratio*100),
                        "fix", fix,
                )
        case ratio > 0.50:
                log.Warn("snapshot save time is approaching TimeoutStopSec — consider increasing it before it causes a missed snapshot",
                        "save_duration", dur.String(),
                        "timeout_stop_sec", timeoutSec,
                        "ratio_pct", fmt.Sprintf("%.0f%%", ratio*100),
                        "fix", fix,
                )
        }
}

// checkStartupSnapshotTiming compares the persisted last snapshot save duration
// against the effective systemd TimeoutStopSec and emits a structured log
// warning when the ratio is already concerning — on startup, before the next
// shutdown.  This lets operators act proactively rather than discovering the
// problem only when the next SIGKILL arrives mid-save.
//
// Thresholds:
//
//	> 80 % of TimeoutStopSec → Error (critical; increase immediately)
//	> 50 % of TimeoutStopSec → Warn  (early notice; tune now)
//
// The function is a no-op when no TimeoutStopSec can be determined
// (readEffectiveTimeoutStopSec returns (0, false)).
func checkStartupSnapshotTiming(savedSnapMs int64, dropinDir, servicePath string, log *slog.Logger) {
        timeoutSec, found := readEffectiveTimeoutStopSec(dropinDir, servicePath)
        if !found || timeoutSec <= 0 {
                return
        }
        ratio := float64(savedSnapMs) / 1000.0 / timeoutSec
        const (
                warnThreshold  = 0.50
                errorThreshold = 0.80
        )
        switch {
        case ratio > errorThreshold:
                log.Error("startup: last snapshot save duration exceeds 80% of systemd stop timeout — SIGKILL risk on next shutdown",
                        "last_snap_save_ms", savedSnapMs,
                        "timeout_stop_sec", timeoutSec,
                        "ratio_pct", math.Round(ratio*1000)/10,
                        "fix", fmt.Sprintf("increase TimeoutStopSec in %s or reduce UTXO set size", dropinDir),
                )
        case ratio > warnThreshold:
                log.Warn("startup: last snapshot save duration exceeds 50% of systemd stop timeout — consider increasing TimeoutStopSec",
                        "last_snap_save_ms", savedSnapMs,
                        "timeout_stop_sec", timeoutSec,
                        "ratio_pct", math.Round(ratio*1000)/10,
                        "fix", fmt.Sprintf("increase TimeoutStopSec in %s or reduce UTXO set size", dropinDir),
                )
        }
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

        // Use the drop-in directory (derived from the canonical drop-in file path)
        // so that ALL .conf files in the directory are checked, not just one.
        // This matches real systemd semantics: every .conf file in the drop-in
        // directory is applied in lex order; later files take precedence.
        dropinDir := filepath.Dir(dropinPath)

        // 1. Try all drop-in .conf files first (directory scan, lex order).
        if secs, found := readEffectiveTimeoutStopSec(dropinDir, ""); found {
                if secs < minSec {
                        log.Warn(warnMsg,
                                "source", dropinDir,
                                "current_sec", secs,
                                "minimum_sec", minSec,
                                "fix", fmt.Sprintf("set TimeoutStopSec=%d in %s and run: systemctl daemon-reload", minSec, dropinPath),
                        )
                }
                return
        }

        // 2. Try the main unit file.
        if secs, found := readEffectiveTimeoutStopSec("", servicePath); found {
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

// rebuildKeyImagesFromBlocks scans every block from height 1 to tipHeight and
// rebuilds the spent key-image set from key images embedded in confirmed
// transactions.  It replaces the in-memory UTXOSet key-image index with the
// authoritative on-chain data, excluding any spurious entries that were added
// by lost transactions (e.g. after an OOM kill before the tx was confirmed in
// a block).  This makes UTXOs whose key images were erroneously marked "spent"
// spendable again.
// forcePurge controls behaviour when the block scan is incomplete (pruned
// node).  When false the function falls back to the persistent k/ index so
// no confirmed key image is lost.  When true the persistent index is purged
// of all entries that could not be verified from the readable blocks; this
// eliminates phantom entries on pruned nodes at the cost of forgetting key
// images for txes that were confirmed inside the missing block range (safe
// because those UTXOs are also absent from the UTXO set).
//
// Returns (count, allOK, err).  allOK is true only when the block scan
// was complete (no unreadable or undecodable blocks) AND every k/ index
// write succeeded.  Callers that gate further actions on a clean rebuild —
// such as patching an existing snapshot — MUST check allOK in addition to
// err.  A nil error with allOK==false means the in-memory set was restored
// via the k/ index fallback (expected on pruned nodes) and is not safe to
// treat as an authoritative replacement for the existing snapshot.
func rebuildKeyImagesFromBlocks(blockStore *store.DB, tipHeight uint64, utxos *core.UTXOSet, forcePurge bool, log *slog.Logger) (int, bool, error) {
        utxos.ClearKeyImages()
        count := 0
        // valid holds every key image that appears in a confirmed transaction,
        // in both raw and canonical form, so the LevelDB k/ index purge below
        // recognises entries written via either MarkKeyImageSpent path.
        valid := make(map[crypto.KeyImage]bool)
        // scanComplete guards the purge below: deleting index entries based on an
        // INCOMPLETE block scan could remove a genuinely confirmed key image and
        // re-open a spent output.  Any unreadable or undecodable block therefore
        // disables the purge (fail closed); re-persisting confirmed key images is
        // additive and stays safe either way.
        scanComplete := true
        // On pruned nodes millions of historical blocks are missing by design;
        // log only the first few occurrences plus a final summary instead of one
        // WARN line per block.
        const maxUnreadableLogs = 5
        unreadable := 0
        for h := uint64(1); h <= tipHeight; h++ {
                if h%100000 == 0 {
                        log.Info("--rebuild-key-images: scanning blocks",
                                "height", h, "tip_height", tipHeight, "key_images_so_far", count)
                }
                raw, err := blockStore.GetRawBlockByHeight(h)
                if err != nil || raw == nil {
                        if unreadable < maxUnreadableLogs {
                                log.Warn("--rebuild-key-images: block unreadable — phantom purge disabled (fail closed)",
                                        "height", h, "err", err)
                        }
                        unreadable++
                        scanComplete = false
                        continue
                }
                var blk core.Block
                if jsonErr := json.Unmarshal(raw, &blk); jsonErr != nil {
                        if unreadable < maxUnreadableLogs {
                                log.Warn("--rebuild-key-images: block undecodable — phantom purge disabled (fail closed)",
                                        "height", h, "err", jsonErr)
                        }
                        unreadable++
                        scanComplete = false
                        continue
                }
                for _, tx := range blk.Txs {
                        for _, inp := range tx.Inputs {
                                utxos.MarkSpent(inp.KeyImage)
                                valid[inp.KeyImage] = true
                                if canonical, cErr := crypto.CanonicalKeyImage(inp.KeyImage); cErr == nil {
                                        valid[canonical] = true
                                }
                                count++
                        }
                }
        }

        // ── Purge phantom entries from the persistent k/ index (Task #1929) ──
        // The in-memory rebuild above fixes the snapshot, but startups WITHOUT a
        // snapshot load key images from the LevelDB k/ index — if phantom
        // entries stay there, the same UTXOs become unspendable again after the
        // next snapshot loss (exactly the OOM-kill scenario this flag repairs).
        removed := 0
        if !scanComplete {
                log.Error("--rebuild-key-images: block scan incomplete",
                        "unreadable_blocks", unreadable,
                        "hint", "expected on pruned nodes; on archive nodes run --repair-db and re-run")

                if forcePurge {
                        // --force-purge-ki-index: purge the persistent index using only
                        // the block scan as truth.  Phantom entries from partial LevelDB
                        // writes are deleted even though we cannot verify them against
                        // pruned blocks.  Safe because the UTXO set is the primary
                        // double-spend guard — any UTXO whose KI we remove was already
                        // removed from the UTXO set when its spending block was applied.
                        // The in-memory set (utxos) already holds every KI observed in
                        // the block scan (via MarkSpent above); we do NOT restore from
                        // the persistent index so phantom entries stay out of memory too.
                        log.Warn("--rebuild-key-images: force-purging persistent k/ index — unverifiable entries will be removed",
                                "verified_from_scan", count, "unreadable_blocks", unreadable)
                        purged := 0
                        if fpErr := blockStore.IterKeyImages(func(ki crypto.KeyImage) error {
                                if valid[ki] {
                                        return nil
                                }
                                if canonical, cErr := crypto.CanonicalKeyImage(ki); cErr == nil && valid[canonical] {
                                        return nil
                                }
                                if delErr := blockStore.DeleteKeyImage(ki); delErr != nil {
                                        log.Warn("--rebuild-key-images: force-purge: failed to delete key image",
                                                "key_image", fmt.Sprintf("%x", ki[:8]), "err", delErr)
                                        return nil
                                }
                                purged++
                                return nil
                        }); fpErr != nil {
                                log.Warn("--rebuild-key-images: force-purge iteration incomplete", "err", fpErr)
                        }
                        // Re-persist verified KIs so the index matches the in-memory set.
                        for ki := range valid {
                                if kiErr := blockStore.MarkKeyImageSpent(ki); kiErr != nil {
                                        log.Warn("--rebuild-key-images: force-purge: failed to persist verified key image",
                                                "key_image", fmt.Sprintf("%x", ki[:8]), "err", kiErr)
                                }
                        }
                        log.Info("--rebuild-key-images: force-purge complete",
                                "verified_from_scan", count, "phantom_purged", purged)
                        // forcePurge runs on incomplete scans (pruned nodes); the result is
                        // not safe to use as an authoritative snapshot replacement.
                        return count, false, nil
                }

                // Normal (safe) path: re-persist confirmed KIs we observed and
                // restore the rest from the persistent index so no confirmed spend
                // from a pruned block is forgotten.
                for ki := range valid {
                        if kiErr := blockStore.MarkKeyImageSpent(ki); kiErr != nil {
                                log.Warn("--rebuild-key-images: failed to persist key image",
                                        "key_image", fmt.Sprintf("%x", ki[:8]), "err", kiErr)
                        }
                }
                restoredFromIndex := 0
                if iterErr := blockStore.IterKeyImages(func(ki crypto.KeyImage) error {
                        utxos.MarkSpent(ki)
                        restoredFromIndex++
                        return nil
                }); iterErr != nil {
                        return count, false, fmt.Errorf("restore key images from index after incomplete scan: %w", iterErr)
                }
                log.Info("--rebuild-key-images: in-memory set restored from persistent index (scan incomplete)",
                        "restored_from_index", restoredFromIndex, "from_block_scan", count)
                // Scan was incomplete: not safe to use this set as an authoritative
                // replacement for an existing snapshot.
                return count, false, nil
        }
        // ── Complete-scan branch ──────────────────────────────────────────────────
        // Track whether all k/ index writes succeeded; a failure here means the
        // persistent index and the snapshot cannot both be safely updated.
        allOK := true
        purgeErr := blockStore.IterKeyImages(func(ki crypto.KeyImage) error {
                if valid[ki] {
                        return nil
                }
                if canonical, cErr := crypto.CanonicalKeyImage(ki); cErr == nil && valid[canonical] {
                        return nil
                }
                if delErr := blockStore.DeleteKeyImage(ki); delErr != nil {
                        log.Warn("--rebuild-key-images: failed to delete phantom key image from index",
                                "key_image", fmt.Sprintf("%x", ki[:8]), "err", delErr)
                        allOK = false // phantom entry remains; index is not fully reconciled
                        return nil
                }
                removed++
                return nil
        })
        if purgeErr != nil {
                log.Warn("--rebuild-key-images: k/ index purge incomplete", "err", purgeErr)
                allOK = false
        }
        // Re-persist every confirmed key image so the index is complete even if
        // some entries were missing before the repair.
        for ki := range valid {
                if kiErr := blockStore.MarkKeyImageSpent(ki); kiErr != nil {
                        log.Warn("--rebuild-key-images: failed to persist key image",
                                "key_image", fmt.Sprintf("%x", ki[:8]), "err", kiErr)
                        allOK = false
                }
        }
        log.Info("--rebuild-key-images: persistent index reconciled",
                "phantom_entries_removed", removed, "confirmed_key_images", count, "all_writes_ok", allOK)
        return count, allOK, nil
}

// sampleUTXOStoreGaps samples the most recent sampleBlocks blocks from the chain
// and counts outputs that are absent from the u/ (UTXO store) LevelDB prefix
// despite not being marked as spent in the su/ index.  A non-zero return value
// means the operator should run --repair-db to restore the missing entries before
// any withdrawal attempt triggers the "Balance temporarily unavailable" error.
//
// The function is intentionally capped at sampleBlocks so startup overhead stays
// well under 1 second on production chains.  rebuildMissingUTXOs performs the
// same check across the ENTIRE chain when --repair-db is requested.
func sampleUTXOStoreGaps(blockStore *store.DB, tipHeight uint64, sampleBlocks uint64, log *slog.Logger) (int64, error) {
        if tipHeight == 0 {
                return 0, nil
        }
        startHeight := uint64(0)
        if tipHeight >= sampleBlocks {
                startHeight = tipHeight - sampleBlocks + 1
        }

        var missing int64
        var firstErr error
        for h := startHeight; h <= tipHeight; h++ {
                raw, err := blockStore.GetRawBlockByHeight(h)
                if err != nil || raw == nil {
                        if err != nil && firstErr == nil {
                                firstErr = fmt.Errorf("height %d: %w", h, err)
                        }
                        continue
                }
                var b core.Block
                if err := json.Unmarshal(raw, &b); err != nil {
                        if firstErr == nil {
                                firstErr = fmt.Errorf("unmarshal height %d: %w", h, err)
                        }
                        continue
                }
                for _, tx := range b.Txs {
                        txHash := tx.Hash()
                        for outIdx := range tx.Outputs {
                                if blockStore.IsUTXOSpent(txHash, uint32(outIdx)) {
                                        continue // su/ entry present — output was consumed
                                }
                                u, _ := blockStore.GetUTXO(txHash, uint32(outIdx))
                                if u == nil {
                                        missing++
                                }
                        }
                }
        }
        if missing > 0 {
                log.Warn("startup UTXO store gap sample: found missing u/ entries",
                        "missing", missing,
                        "sampled_from", startHeight,
                        "sampled_to", tipHeight,
                )
        }
        return missing, firstErr
}

// rebuildMissingUTXOs scans every stored block from height 0 to tipHeight and
// re-populates two LevelDB prefix families that are lost together during an
// OOM-kill + RecoverFile cycle:
//
//   - u/  (UTXO store)    — needed by apr_walletSend to build ring inputs
//   - t/  (tx-hash index) — needed by apr_walletSend to look up the full TX
//
// Both prefixes live in SST files and are lost simultaneously when the Go
// runtime is OOM-killed mid-compaction.  Restoring only u/ leaves t/ broken,
// so apr_walletSend still fails with "tx not found on chain or mempool" even
// though the UTXO shows as active.
//
// The rebuild is safe to run multiple times: entries already present are
// skipped; already-spent outputs (su/ entry present) skip the u/ restore.
// Missing blocks are silently skipped with a non-fatal error.
func rebuildMissingUTXOs(blockStore *store.DB, tipHeight uint64, log *slog.Logger) (int, error) {
        rebuiltUTXO := 0
        rebuiltTxIdx := 0
        var firstErr error
        for h := uint64(0); h <= tipHeight; h++ {
                raw, err := blockStore.GetRawBlockByHeight(h)
                if err != nil || raw == nil {
                        if err != nil && firstErr == nil {
                                firstErr = fmt.Errorf("height %d: %w", h, err)
                        }
                        continue
                }
                var b core.Block
                if err := json.Unmarshal(raw, &b); err != nil {
                        if firstErr == nil {
                                firstErr = fmt.Errorf("unmarshal height %d: %w", h, err)
                        }
                        continue
                }
                for txPos, tx := range b.Txs {
                        txHash := tx.Hash()

                        // ── t/ tx-hash index ──────────────────────────────────────────
                        // apr_walletSend looks up the full TX by hash; if the t/ entry
                        // is missing it returns "tx not found on chain or mempool" even
                        // when the u/ UTXO entry is intact.
                        if existing, _ := blockStore.LookupTxIdx(txHash); existing == nil {
                                if putErr := blockStore.PutTxIdx(txHash, h, txPos); putErr != nil {
                                        if firstErr == nil {
                                                firstErr = putErr
                                        }
                                } else {
                                        rebuiltTxIdx++
                                }
                        }

                        // ── u/ UTXO store ─────────────────────────────────────────────
                        for outIdx, out := range tx.Outputs {
                                if blockStore.IsUTXOSpent(txHash, uint32(outIdx)) {
                                        continue // spent — su/ entry present, skip
                                }
                                existing, _ := blockStore.GetUTXO(txHash, uint32(outIdx))
                                if existing != nil {
                                        continue // already in store, skip
                                }
                                su := &store.StoredUTXO{
                                        TxHash:       txHash,
                                        OutputIndex:  uint32(outIdx),
                                        OneTimePub:   out.OneTimePub,
                                        TxPubKey:     out.TxPubKey,
                                        AmountCommit: out.AmountCommit,
                                        EncAmount:    out.EncAmount,
                                        BlockHeight:  h,
                                }
                                if putErr := blockStore.PutUTXO(txHash, uint32(outIdx), su); putErr != nil {
                                        if firstErr == nil {
                                                firstErr = putErr
                                        }
                                        continue
                                }
                                rebuiltUTXO++
                        }
                }
                if h%10_000 == 0 && h > 0 {
                        log.Info("repair: store rebuild progress",
                                "height", h, "tip", tipHeight,
                                "restored_utxo", rebuiltUTXO, "restored_txidx", rebuiltTxIdx)
                }
        }
        if rebuiltTxIdx > 0 || rebuiltUTXO > 0 {
                log.Warn("repair: store gaps detected and repaired",
                        "restored_utxo", rebuiltUTXO, "restored_txidx", rebuiltTxIdx)
        }
        return rebuiltUTXO + rebuiltTxIdx, firstErr
}

// verifyUTXOStoreEntries compares every existing (unspent) u/ store entry
// against the authoritative output data in the raw block store and overwrites
// entries whose fields diverge.  rebuildMissingUTXOs cannot catch this class
// of corruption because it skips entries that already exist.
//
// Divergence has been observed in production: after an OOM crash + manual
// restore, u/ entries carried AmountCommit values recomputed from incorrect
// database amounts instead of the on-chain commitments, making the UTXOs
// unspendable (ring construction used the wrong commitment).  Raw blocks are
// the source of truth — consensus verified them at acceptance time.
//
// Every fix is logged with the tx hash and both commitments so operators can
// audit exactly what was repaired.
func verifyUTXOStoreEntries(blockStore *store.DB, tipHeight uint64, log *slog.Logger) (int, error) {
        fixed := 0
        var firstErr error
        for h := uint64(0); h <= tipHeight; h++ {
                raw, err := blockStore.GetRawBlockByHeight(h)
                if err != nil || raw == nil {
                        if err != nil && firstErr == nil {
                                firstErr = fmt.Errorf("height %d: %w", h, err)
                        }
                        continue
                }
                var b core.Block
                if err := json.Unmarshal(raw, &b); err != nil {
                        if firstErr == nil {
                                firstErr = fmt.Errorf("unmarshal height %d: %w", h, err)
                        }
                        continue
                }
                for _, tx := range b.Txs {
                        txHash := tx.Hash()
                        for outIdx, out := range tx.Outputs {
                                existing, _ := blockStore.GetUTXO(txHash, uint32(outIdx))
                                if existing == nil {
                                        continue // absent — rebuildMissingUTXOs handles these
                                }
                                if existing.AmountCommit == out.AmountCommit &&
                                        existing.OneTimePub == out.OneTimePub &&
                                        existing.TxPubKey == out.TxPubKey &&
                                        existing.EncAmount == out.EncAmount {
                                        // Matches raw block — OK.  BlockHeight is
                                        // deliberately NOT compared: identical
                                        // deterministic mint txs (same address +
                                        // amount + height param) share one tx hash
                                        // and can be included at multiple chain
                                        // heights, so a height-only mismatch is
                                        // expected, not corruption.
                                        continue
                                }
                                log.Warn("repair: UTXO store entry diverges from raw block — overwriting",
                                        "tx_hash", fmt.Sprintf("%x", txHash[:]),
                                        "out_idx", outIdx,
                                        "height", h,
                                        "store_commit", fmt.Sprintf("%x", existing.AmountCommit[:]),
                                        "block_commit", fmt.Sprintf("%x", out.AmountCommit[:]))
                                su := &store.StoredUTXO{
                                        TxHash:       txHash,
                                        OutputIndex:  uint32(outIdx),
                                        OneTimePub:   out.OneTimePub,
                                        TxPubKey:     out.TxPubKey,
                                        AmountCommit: out.AmountCommit,
                                        EncAmount:    out.EncAmount,
                                        BlockHeight:  h,
                                }
                                if putErr := blockStore.PutUTXO(txHash, uint32(outIdx), su); putErr != nil {
                                        if firstErr == nil {
                                                firstErr = putErr
                                        }
                                        continue
                                }
                                fixed++
                        }
                }
                if h%100_000 == 0 && h > 0 {
                        log.Info("repair: UTXO store verification progress",
                                "height", h, "tip", tipHeight, "fixed", fixed)
                }
        }
        return fixed, firstErr
}

// storeBlock serialises a block to JSON and writes it via PutRawBlock.
// It also persists a UTXO record for every transaction output so that
// api.utxoMissingReason can later distinguish "never existed" from "already
// spent or burned" from "originated in a now-pruned block".  The records are
// intentionally never deleted (no DeleteUTXO call) so that spent and staked
// outputs remain queryable after they leave the active set.
// replayPostSnapshotGap replays blocks in (snapTipHeight, chainTipHeight] from
// raw LevelDB bytes (GetRawBlockByHeight → core.Block) and applies three
// effects to the in-memory state, matching exactly what the startup scan does:
//
//  1. Add every tx output to utxos (so address scans include post-snapshot mints)
//  2. Mark every tx input's key image as spent (double-spend guard)
//  3. Replay stake/delegation/withdrawal txs into registry
//     (via ReplayBlockStakeTxs) so validator eligibility is correct
//
// All three must succeed for every block; if any block is missing, unreadable,
// or causes a registry replay error the function halts and returns
// complete=false.  The caller must fall back to the full startup scan (via
// rescueSnap) rather than setting snapLoaded=true.
//
// When snapTipHeight == chainTipHeight (normal clean shutdown) the loop body
// never executes and the function returns (0, 0, true) with no overhead.
func replayPostSnapshotGap(
        db *store.DB,
        utxos *core.UTXOSet,
        registry *core.ValidatorRegistry,
        snapTipHeight, chainTipHeight uint64,
        log *slog.Logger,
) (added, spent int, complete bool) {
        complete = true
        for h := snapTipHeight + 1; h <= chainTipHeight; h++ {
                raw, err := db.GetRawBlockByHeight(h)
                if err != nil || raw == nil {
                        log.Warn("snapshot gap-fill: block not found — halting replay",
                                "height", h, "err", err)
                        complete = false
                        break
                }
                var blk core.Block
                if err := json.Unmarshal(raw, &blk); err != nil {
                        log.Warn("snapshot gap-fill: block unmarshal failed — halting replay",
                                "height", h, "err", err)
                        complete = false
                        break
                }
                // Apply the full UTXO state transition for this block — same
                // semantics as the startup scan's ApplyBlock call.  This:
                //   • inserts new outputs into the primary and byPubKey indexes
                //   • removes real consumed UTXOs from the active/byPubKey indexes
                //   • moves them into the spent-decoy pool
                //   • invokes OnUTXOSpent for the persistent su/ index
                //   • inserts canonical key images into the key-image set
                //
                // Using ApplyBlock (rather than manual MarkSpent+Add) is
                // required so spent snapshot UTXOs are removed from the active
                // index — otherwise they remain eligible as ring members and
                // appear erroneously in address scans and the UTXO supply count.
                if err := utxos.ApplyBlock(&blk); err != nil {
                        log.Warn("snapshot gap-fill: ApplyBlock failed — halting replay",
                                "height", h, "err", err)
                        complete = false
                        break
                }
                // Count for logging only.
                for _, tx := range blk.Txs {
                        added += len(tx.Outputs)
                        spent += len(tx.Inputs)
                }
                // Replay stake/delegation/withdrawal txs so validator registry
                // state advances to chainTipHeight.  Matches the startup scan's
                // ReplayBlockStakeTxs call.  A replay error means the gap block
                // cannot be trusted; halt and fall back to the full scan.
                if replayErr := registry.ReplayBlockStakeTxs(blk.Txs, blk.Header.Height); replayErr != nil {
                        log.Warn("snapshot gap-fill: registry replay failed — halting",
                                "height", h, "err", replayErr)
                        complete = false
                        break
                }
        }
        return
}

// backfillTxIdxRange writes t/ LevelDB entries for every transaction in every
// block from fromH+1 through toH.
//
// It halts at the first unrecoverable error — block missing from the store,
// JSON decode failure, or PutTxIdx write failure — to avoid advancing the
// txidx_complete_height marker past heights whose t/ entries were not actually
// written.  Only the last successfully indexed height is saved to the marker,
// so the next restart retries the failed range rather than treating it as done.
//
// This function is called in a background goroutine by the snapshot fast-path
// startup; it is package-level (not inlined) so tests can exercise it directly.
func backfillTxIdxRange(fromH, toH uint64, db *store.DB, log *slog.Logger) {
        const progressInterval = uint64(10_000)
        lastGood := fromH
        for h := fromH + 1; h <= toH; h++ {
                raw, fetchErr := db.GetRawBlockByHeight(h)
                if fetchErr != nil {
                        // Real LevelDB read error — halt and retry the whole range next restart.
                        log.Warn("tx index background backfill: block fetch error — halting; will retry next restart",
                                "height", h, "err", fetchErr)
                        _ = db.StoreTxIdxCompleteHeight(lastGood)
                        return
                }
                if raw == nil {
                        // Height entry exists but raw-block data is absent (e.g. post-repair
                        // gap or pruned block).  Nothing to index; skip and continue so all
                        // subsequent blocks are not permanently blocked by this gap.
                        log.Warn("tx index background backfill: block data missing — skipping height",
                                "height", h)
                        lastGood = h // mark as processed so we don't retry this gap forever
                        continue
                }
                var b core.Block
                if jsonErr := json.Unmarshal(raw, &b); jsonErr != nil {
                        log.Warn("tx index background backfill: block decode failed — halting; will retry next restart",
                                "height", h, "err", jsonErr)
                        _ = db.StoreTxIdxCompleteHeight(lastGood)
                        return
                }
                for i, tx := range b.Txs {
                        txHash := tx.Hash()
                        if putErr := db.PutTxIdx(txHash, b.Header.Height, i); putErr != nil {
                                log.Warn("tx index background backfill: PutTxIdx failed — halting; will retry next restart",
                                        "height", h, "err", putErr)
                                _ = db.StoreTxIdxCompleteHeight(lastGood)
                                return
                        }
                }
                lastGood = h
                if h%progressInterval == 0 {
                        _ = db.StoreTxIdxCompleteHeight(h)
                        log.Info("tx index background backfill progress",
                                "height", h, "target", toH)
                }
        }
        _ = db.StoreTxIdxCompleteHeight(toH)
        log.Info("tx index background backfill complete",
                "from_height", fromH+1,
                "to_height", toH,
        )
}

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


// validateAmountCommitsFromBlocks validates the AmountCommit field of every
// active and staked UTXO in utxos against the authoritative raw block bytes
// on disk.  For each mismatch found it:
//
//  1. Patches the in-memory UTXO set so transfers succeed on THIS startup.
//  2. Persists the corrected AmountCommit to the u/ LevelDB record so that
//     on the next graceful startup ReconcileWithStore does not overwrite the
//     repaired value with the still-corrupt disk record.
//
// A mismatch is counted as "fixed" only when BOTH the in-memory patch AND the
// store write succeed.  If the store write fails the in-memory patch is still
// applied (transfers work for this run) but the log records an explicit warning
// that the repair is not durable — the operator must investigate the LevelDB
// error or rerun with --repair-db.
//
// Called during the OOM-window startup check when a snapshot was saved within
// 30 minutes of an unclean shutdown.  Raw block bytes are the only source
// never modified in-place after acceptance.
//
// For efficiency UTXOs are grouped by BlockHeight so each block is fetched at
// most once.  Heights whose block is missing (pruned) are skipped silently.
//
// Returns (checked, fixed): UTXOs compared and fully-durable mismatches repaired.
func validateAmountCommitsFromBlocks(
        utxos *core.UTXOSet,
        db *store.DB,
        log *slog.Logger,
) (checked, fixed int) {
        // Take a read-only snapshot so we can iterate without holding the mutex
        // while performing potentially slow block reads from disk.
        snap := utxos.TakeSnapshot()
        if len(snap.ActiveUTXOs) == 0 && len(snap.StakedUTXOs) == 0 {
                return
        }

        // isStaked distinguishes which in-memory patch call to use.
        type utxoRef struct {
                txHash      crypto.Hash32
                outputIndex uint32
                oneTimePub  crypto.Point32 // used only for active UTXOs
                commit      crypto.Commitment
                staked      bool
        }

        // Group by BlockHeight so each block is fetched at most once.
        byHeight := make(map[uint64][]utxoRef, 64)
        for _, u := range snap.ActiveUTXOs {
                byHeight[u.BlockHeight] = append(byHeight[u.BlockHeight], utxoRef{
                        txHash:      u.TxHash,
                        outputIndex: u.OutputIndex,
                        oneTimePub:  u.OneTimePub,
                        commit:      u.AmountCommit,
                        staked:      false,
                })
        }
        for _, u := range snap.StakedUTXOs {
                byHeight[u.BlockHeight] = append(byHeight[u.BlockHeight], utxoRef{
                        txHash:      u.TxHash,
                        outputIndex: u.OutputIndex,
                        oneTimePub:  u.OneTimePub,
                        commit:      u.AmountCommit,
                        staked:      true,
                })
        }

        for height, refs := range byHeight {
                raw, err := db.GetRawBlockByHeight(height)
                if err != nil || raw == nil {
                        // Block may be pruned or absent — skip silently.
                        continue
                }
                var blk core.Block
                if err := json.Unmarshal(raw, &blk); err != nil {
                        log.Warn("amountcommit validate: block unmarshal failed — skipping height",
                                "height", height, "err", err)
                        continue
                }

                // Build (txHash, outputIndex) → full output map for this block.
                type outKey struct {
                        TxHash      crypto.Hash32
                        OutputIndex uint32
                }
                type outVal struct {
                        commit     crypto.Commitment
                        oneTimePub crypto.Point32
                        txPubKey   crypto.Point32
                        encAmount  [8]byte
                }
                onChain := make(map[outKey]outVal, len(blk.Txs)*2)
                for _, tx := range blk.Txs {
                        txHash := tx.Hash()
                        for i, out := range tx.Outputs {
                                onChain[outKey{TxHash: txHash, OutputIndex: uint32(i)}] = outVal{
                                        commit:     out.AmountCommit,
                                        oneTimePub: out.OneTimePub,
                                        txPubKey:   out.TxPubKey,
                                        encAmount:  out.EncAmount,
                                }
                        }
                }

                for _, ref := range refs {
                        ov, found := onChain[outKey{TxHash: ref.txHash, OutputIndex: ref.outputIndex}]
                        if !found {
                                // Output not found at this height — skip to avoid false positives.
                                continue
                        }
                        checked++
                        if ref.commit == ov.commit {
                                continue
                        }
                        log.Warn("amountcommit validate: snapshot AmountCommit differs from raw block — patching",
                                "tx_hash",      fmt.Sprintf("%x", ref.txHash[:]),
                                "out_idx",      ref.outputIndex,
                                "block_height", height,
                                "staked",       ref.staked,
                                "snap_commit",  fmt.Sprintf("%x", ref.commit[:]),
                                "block_commit", fmt.Sprintf("%x", ov.commit[:]))

                        // 1. Patch the in-memory UTXO set so transfers succeed
                        //    immediately on this startup regardless of store write outcome.
                        if ref.staked {
                                utxos.PatchStakedAmountCommit(ref.txHash, ref.outputIndex, ov.commit)
                        } else {
                                utxos.PatchAmountCommit(ref.oneTimePub, ov.commit)
                        }

                        // 2. Persist the corrected AmountCommit to the u/ store so that
                        //    ReconcileWithStore on the next graceful startup does not
                        //    overwrite the in-memory/snapshot value with the still-corrupt
                        //    disk record.  Read-modify-write preserves all other fields.
                        su, getErr := db.GetUTXO(ref.txHash, ref.outputIndex)
                        if getErr != nil || su == nil {
                                log.Warn("amountcommit validate: in-memory patched but u/ store read failed — repair not durable",
                                        "tx_hash", fmt.Sprintf("%x", ref.txHash[:]),
                                        "out_idx", ref.outputIndex,
                                        "err",     getErr)
                                continue // not counted as fully fixed
                        }
                        su.AmountCommit = ov.commit
                        if putErr := db.PutUTXO(ref.txHash, ref.outputIndex, su); putErr != nil {
                                log.Warn("amountcommit validate: in-memory patched but u/ store write failed — repair not durable",
                                        "tx_hash", fmt.Sprintf("%x", ref.txHash[:]),
                                        "out_idx", ref.outputIndex,
                                        "err",     putErr)
                                continue // not counted as fully fixed
                        }
                        fixed++
                }
        }
        return
}

// ─── Background UTXO-store integrity audit ───────────────────────────────────
//
// Production incident: u/ (UTXO store) LevelDB entries carried AmountCommit
// values that diverged from the raw on-chain blocks, which was only discovered
// when a user could not withdraw ~148M APRO (ring construction used the wrong
// commitment).  verifyUTXOStoreEntries() catches this class of corruption, but
// only runs under --repair-db.  The background audit below detects the same
// divergence continuously, without a full-chain scan, so the api-server can
// alert admins in Telegram BEFORE any withdrawal is blocked.
//
// Each cycle performs two checks:
//  1. Sampled check — up to utxoAuditSampleN random UNSPENT u/ entries are
//     verified against the raw block they claim to originate from (reservoir
//     sampling over the u/ keyspace keeps memory bounded).
//  2. Recent-blocks check — the most recent utxoAuditRecentK blocks are fully
//     verified (every output vs. its u/ entry), because fresh entries are the
//     most likely to be touched by a bad restore or external writer.
//
// The audit NEVER writes to the store — it only reports.  Repair remains an
// explicit operator action (--repair-db) so an audit bug can never corrupt
// healthy entries.

const (
        // utxoAuditInterval is the pause between audit cycles.
        utxoAuditInterval = 10 * time.Minute
        // utxoAuditInitialDelay defers the first cycle so startup I/O settles.
        utxoAuditInitialDelay = 2 * time.Minute
        // utxoAuditSampleN is how many random unspent u/ entries are verified per cycle.
        utxoAuditSampleN = 200
        // utxoAuditRecentK is how many recent blocks are fully verified per cycle.
        utxoAuditRecentK = uint64(50)
        // utxoAuditMaxDetails caps the mismatch detail entries included in the result.
        utxoAuditMaxDetails = 20
)

// utxoAuditKey identifies one u/ store entry (tx hash + output index).
type utxoAuditKey struct {
        txHash crypto.Hash32
        outIdx uint32
}

// sampleUTXOKeys reservoir-samples up to n keys uniformly from the whole u/
// keyspace in a single iterator pass with O(n) memory.  Spent-ness is NOT
// checked here (that would add a point lookup per key over millions of keys);
// callers filter spent entries afterwards on the small sample only.
func sampleUTXOKeys(blockStore *store.DB, n int, rng *rand.Rand) ([]utxoAuditKey, error) {
        sample := make([]utxoAuditKey, 0, n)
        seen := 0
        err := blockStore.IterAllUTXOKeys(func(txHash crypto.Hash32, outIdx uint32) {
                seen++
                if len(sample) < n {
                        sample = append(sample, utxoAuditKey{txHash, outIdx})
                        return
                }
                if j := rng.IntN(seen); j < n {
                        sample[j] = utxoAuditKey{txHash, outIdx}
                }
        })
        return sample, err
}

// auditCompareOutput returns true when the stored entry matches the on-chain
// output.  BlockHeight is deliberately not compared — identical deterministic
// mint txs share one hash across heights (see verifyUTXOStoreEntries).
func auditCompareOutput(existing *store.StoredUTXO, out *core.Output) bool {
        return existing.AmountCommit == out.AmountCommit &&
                existing.OneTimePub == out.OneTimePub &&
                existing.TxPubKey == out.TxPubKey &&
                existing.EncAmount == out.EncAmount
}

// auditUTXOStore runs one audit cycle: sampled random unspent u/ entries plus
// a full verification of the most recent recentK blocks.  Read-only.
func auditUTXOStore(
        blockStore *store.DB,
        tipHeight uint64,
        sampleN int,
        recentK uint64,
        rng *rand.Rand,
        log *slog.Logger,
) api.UTXOAuditResult {
        start := time.Now()
        res := api.UTXOAuditResult{TipHeight: tipHeight}
        var firstErr error
        recordMismatch := func(txHash crypto.Hash32, outIdx uint32, height uint64, storeCommit, blockCommit [32]byte) {
                res.Mismatches++
                if len(res.MismatchDetails) < utxoAuditMaxDetails {
                        res.MismatchDetails = append(res.MismatchDetails, api.UTXOAuditMismatch{
                                TxHash:      fmt.Sprintf("%x", txHash[:]),
                                OutputIndex: outIdx,
                                Height:      height,
                                StoreCommit: fmt.Sprintf("%x", storeCommit[:]),
                                BlockCommit: fmt.Sprintf("%x", blockCommit[:]),
                        })
                }
                log.Warn("utxo-audit: store entry diverges from raw block",
                        "tx_hash", fmt.Sprintf("%x", txHash[:]),
                        "out_idx", outIdx,
                        "height", height,
                        "store_commit", fmt.Sprintf("%x", storeCommit[:]),
                        "block_commit", fmt.Sprintf("%x", blockCommit[:]),
                        "hint", "run --repair-db at the next maintenance window BEFORE any withdrawal from affected addresses")
        }

        // ── 1. Sampled check of random unspent u/ entries ─────────────────────────
        // Cache raw blocks decoded during this cycle so several sampled entries
        // from the same block cost one read+unmarshal.
        blockCache := make(map[uint64]*core.Block)
        loadBlock := func(h uint64) *core.Block {
                if b, ok := blockCache[h]; ok {
                        return b // may be nil (negative cache for pruned/unreadable)
                }
                raw, err := blockStore.GetRawBlockByHeight(h)
                if err != nil || raw == nil {
                        if err != nil && firstErr == nil {
                                firstErr = fmt.Errorf("height %d: %w", h, err)
                        }
                        blockCache[h] = nil
                        return nil
                }
                var b core.Block
                if err := json.Unmarshal(raw, &b); err != nil {
                        if firstErr == nil {
                                firstErr = fmt.Errorf("unmarshal height %d: %w", h, err)
                        }
                        blockCache[h] = nil
                        return nil
                }
                blockCache[h] = &b
                return &b
        }

        // Oversample so the cycle still verifies ~sampleN entries after spent
        // entries are filtered out of the raw sample.
        keys, iterErr := sampleUTXOKeys(blockStore, sampleN*4, rng)
        if iterErr != nil && firstErr == nil {
                firstErr = iterErr
        }
        for _, k := range keys {
                if res.SampledChecked >= sampleN {
                        break
                }
                if blockStore.IsUTXOSpent(k.txHash, k.outIdx) {
                        continue // spent — commitment no longer used for ring construction
                }
                existing, getErr := blockStore.GetUTXO(k.txHash, k.outIdx)
                if getErr != nil || existing == nil {
                        if getErr != nil && firstErr == nil {
                                firstErr = getErr
                        }
                        continue
                }
                b := loadBlock(existing.BlockHeight)
                if b == nil {
                        res.Skipped++ // pruned or unreadable block — cannot verify
                        continue
                }
                found := false
                for _, tx := range b.Txs {
                        if tx.Hash() != k.txHash {
                                continue
                        }
                        found = true
                        if int(k.outIdx) >= len(tx.Outputs) {
                                // Entry references an output index the on-chain tx
                                // does not have — definite divergence.
                                recordMismatch(k.txHash, k.outIdx, existing.BlockHeight,
                                        existing.AmountCommit, [32]byte{})
                                break
                        }
                        out := tx.Outputs[k.outIdx]
                        if !auditCompareOutput(existing, &out) {
                                recordMismatch(k.txHash, k.outIdx, existing.BlockHeight,
                                        existing.AmountCommit, out.AmountCommit)
                        }
                        break
                }
                if !found {
                        // Tx hash absent at the stored height.  Deterministic mint
                        // txs can legitimately record a different inclusion height,
                        // so this is counted as unverifiable rather than corruption
                        // (the recent-blocks pass below catches real divergence for
                        // fresh entries, and --repair-db verifies the full chain).
                        res.Skipped++
                        continue
                }
                res.SampledChecked++
        }

        // ── 2. Full verification of the most recent recentK blocks ───────────────
        startH := uint64(0)
        if tipHeight+1 > recentK {
                startH = tipHeight + 1 - recentK
        }
        for h := startH; h <= tipHeight; h++ {
                b := loadBlock(h)
                if b == nil {
                        res.Skipped++
                        continue
                }
                res.RecentBlocksChecked++
                for _, tx := range b.Txs {
                        txHash := tx.Hash()
                        for outIdx := range tx.Outputs {
                                existing, _ := blockStore.GetUTXO(txHash, uint32(outIdx))
                                if existing == nil {
                                        continue // absent entries are the gap-monitor's domain
                                }
                                res.RecentOutputsChecked++
                                out := tx.Outputs[outIdx]
                                if !auditCompareOutput(existing, &out) {
                                        recordMismatch(txHash, uint32(outIdx), h,
                                                existing.AmountCommit, out.AmountCommit)
                                }
                        }
                }
        }

        if firstErr != nil {
                res.Error = firstErr.Error()
        }
        res.CompletedAt = time.Now()
        res.DurationMs = time.Since(start).Milliseconds()
        return res
}

// startUTXOAuditLoop launches the periodic background audit goroutine.
// Results are pushed to the API server for /api/v1/admin/utxo-audit; the
// Node.js api-server polls that endpoint and fires the Telegram admin alert
// when mismatches>0.  apiSrv must be non-nil.
func startUTXOAuditLoop(blockStore *store.DB, apiSrv *api.Server, log *slog.Logger) {
        go func() {
                rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(os.Getpid())))
                time.Sleep(utxoAuditInitialDelay)
                for {
                        _, tip, err := blockStore.GetTip()
                        if err != nil {
                                log.Warn("utxo-audit: get tip failed — skipping cycle", "err", err)
                        } else {
                                res := auditUTXOStore(blockStore, tip, utxoAuditSampleN, utxoAuditRecentK, rng, log)
                                apiSrv.SetUTXOAuditResult(&res)
                                if res.Mismatches > 0 {
                                        log.Warn("utxo-audit: cycle found store/blockchain divergence",
                                                "mismatches", res.Mismatches,
                                                "sampled_checked", res.SampledChecked,
                                                "recent_blocks_checked", res.RecentBlocksChecked,
                                                "skipped", res.Skipped,
                                                "duration_ms", res.DurationMs)
                                } else {
                                        log.Info("utxo-audit: cycle clean",
                                                "sampled_checked", res.SampledChecked,
                                                "recent_blocks_checked", res.RecentBlocksChecked,
                                                "recent_outputs_checked", res.RecentOutputsChecked,
                                                "skipped", res.Skipped,
                                                "duration_ms", res.DurationMs)
                                }
                        }
                        time.Sleep(utxoAuditInterval)
                }
        }()
        log.Info("utxo-audit: background integrity audit scheduled",
                "interval", utxoAuditInterval.String(),
                "initial_delay", utxoAuditInitialDelay.String(),
                "sample_n", utxoAuditSampleN,
                "recent_blocks", utxoAuditRecentK)
}
