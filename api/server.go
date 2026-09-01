// Package api provides the Aperod JSON-RPC 2.0 API server.
// Methods follow the apr_ namespace convention.
package api

import (
        "context"
        "encoding/hex"
        "encoding/json"
        "fmt"
        "log/slog"
        "net/http"
        "runtime"
        "strconv"
        "sync"
        "sync/atomic"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/store"
)

// utxoCacheEntry holds a serialised /address/{addr}/utxos response body and
// an expiry timestamp.  Entries are stored in Server.utxoAddrCache and evicted
// lazily on next access after they expire.
type utxoCacheEntry struct {
	body      []byte
	expiresAt time.Time
}

// utxoCacheTTL is how long a cached /address/{addr}/utxos response is kept.
// The mint-UTXO monitor polls every 5 minutes, so a 90-second TTL means at
// most one live scan per address per monitor cycle instead of one per call.
const utxoCacheTTL = 90 * time.Second

// Server is the JSON-RPC 2.0 HTTP server.
type Server struct {
        addr        string
        chain       *core.Chain
        mempool     *core.Mempool
        utxos       *core.UTXOSet
        registry    *core.ValidatorRegistry  // live PoS validator registry (optional)
        myKey       *crypto.LockedValidatorKey // node's own validator key for admin stake ops (optional)
        blockStore  *store.DB               // optional: LevelDB store for pruned-block fallback
        log         *slog.Logger
        mux         *http.ServeMux
        hub         *Hub
        apiKey      string   // optional; empty = dev mode (no auth)
        corsOrigins []string // empty = allow all ("*")
        rateLimiter *RateLimiter

        // utxoAudit holds the latest background UTXO-store audit result,
        // pushed by cmd/node via SetUTXOAuditResult and served on
        // /api/v1/admin/utxo-audit.  Guarded by utxoAuditMu.
        utxoAuditMu sync.Mutex
        utxoAudit   *UTXOAuditResult
        peerCounter            func() int   // optional; wired to p2p.Host.PeerCount by cmd/node
        pendingHandshakeCounter func() int64 // optional; wired to p2p.Host.PendingHandshakes by cmd/node
        reconnectBackoffFlag    func() bool  // optional; wired to p2p.Host.ReconnectBackoffActive by cmd/node
        // tsRejectedCounter returns the count of blocks rejected by the timejacking guard.
        // Wired from consensus.Engine.TimestampRejectedCount in cmd/node after engine start.
        tsRejectedCounter func() int64

        // mintScheduler schedules an admin mint for inclusion in the next produced
        // block and blocks until it is committed (returning the tx hash hex and
        // inclusion height) or the timeout expires.  Wired from
        // consensus.Engine.ScheduleAdminMint in cmd/node after engine start.
        // nil = this node is not an active validator; admin mints are refused.
        mintScheduler func(addr string, amountNAPR uint64, timeout time.Duration) (string, uint64, error)

        banListFn func() []BanEntry                         // optional; wired to p2p.Host.ListBans by cmd/node
        banLiftFn func(string) bool                         // optional; wired to p2p.Host.LiftBan by cmd/node
        banAddFn  func(addr, reason string, d time.Duration) // optional; wired to p2p.Host.Ban by cmd/node

        // banEventFn returns peer-ban events since a given time.
        // Wired to p2p.Host.GetBanEvents by cmd/node.
        banEventFn func(since time.Time) []BanEventEntry

        // whitelistExemptFn returns whitelist-exemption events since a given time.
        // Wired to p2p.Host.GetWhitelistExemptions by cmd/node.
        whitelistExemptFn func(since time.Time) []WhitelistExemptionEntry

        // peerListFn returns a snapshot of all currently connected peers with
        // their heights and direction.  Wired to p2p.Host.GetPeerList by cmd/node.
        // Optional — GET /api/v1/network/peers returns 503 when not wired.
        peerListFn func() []PeerListEntry

        // stallEventFn returns block-fetch stall events since a given time.
        // Wired to p2p.Host.GetStallEvents by cmd/node.
        stallEventFn func(since time.Time) []StallEventEntry

        // bootnodeWarnEventFn returns malformed/stale bootnode warning events since
        // a given time.  Wired to p2p.Host.GetBootnodeWarnEvents by cmd/node.
        bootnodeWarnEventFn func(since time.Time) []BootnodeWarnEntry

        // duplicateIdentityEventFn returns duplicate-identity fingerprint conflict
        // events since a given time.
        // Wired to p2p.Host.GetDuplicateIdentityEvents by cmd/node.
        duplicateIdentityEventFn func(since time.Time) []DuplicateIdentityEntry

        // peerWhitelist holds the parsed peer_whitelist entries from node.yaml.
        // Stored so /api/v1/status can report them to operators.
        peerWhitelist []string

        // whitelistGetFn / whitelistAddFn / whitelistRemoveFn are wired to
        // p2p.Host.GetPeerWhitelist / AddToWhitelist / RemoveFromWhitelist by
        // cmd/node after the P2P layer is started.  Optional — the whitelist
        // endpoints return 503 when not wired.
        whitelistGetFn    func() []string
        whitelistAddFn    func(string) error
        whitelistRemoveFn func(string) (bool, error)

        // p2pKeepaliveGetFn returns the current live keepalive Ping interval.
        // Wired to p2p.Host.GetKeepaliveInterval by cmd/node.
        // Optional — GET /api/v1/network/p2p-config returns 503 when not wired.
        p2pKeepaliveGetFn func() time.Duration
        // p2pKeepaliveSetFn updates the live keepalive Ping interval.
        // Returns an error when the value is outside the allowed [1s, 15s] range.
        // Wired to p2p.Host.SetKeepaliveInterval by cmd/node.
        // Optional — POST /api/v1/network/p2p-config returns 503 when not wired.
        p2pKeepaliveSetFn func(time.Duration) error
        // p2pKeepalivePersistFn writes the new keepalive interval back to
        // node.yaml (atomic tmp+rename) so it survives a node restart.
        // Wired to cmd/node persistKeepaliveInterval.  Optional — when not
        // wired, POST updates only the live value and reports persisted:false.
        p2pKeepalivePersistFn func(time.Duration) error
        // p2pKeepaliveYAMLFn re-reads node.yaml and returns the persisted
        // keepalive interval, so GET can report live vs persisted drift.
        // Optional — when not wired the yaml fields are omitted from GET.
        p2pKeepaliveYAMLFn func() (time.Duration, error)

        // p2pBadBlockBanThreshold is the configured rogue-fork ban threshold
        // (number of out-of-range blocks before a peer is banned).
        // Set via SetP2PBanConfig after the P2P host is started.
        p2pBadBlockBanThreshold int
        // p2pBadBlockBanDurationSecs is BadBlockBanDuration in whole seconds.
        // Set via SetP2PBanConfig after the P2P host is started.
        p2pBadBlockBanDurationSecs int64
        // p2pBadBlockHeightLead is how many blocks ahead of our tip a peer's
        // block height must be before it counts as an out-of-range strike.
        // Set via SetP2PBanConfig after the P2P host is started.
        p2pBadBlockHeightLead uint64
        // p2pBanGetFn returns the current LIVE wrong-fork ban parameters.
        // Wired to p2p.Host.GetBanConfig by cmd/node.  Optional — when not
        // wired, GET falls back to the static SetP2PBanConfig values.
        p2pBanGetFn func() (int, time.Duration, uint64)
        // p2pBanSetFn updates the live wrong-fork ban parameters without a
        // restart.  Wired to p2p.Host.SetBanConfig by cmd/node.  Optional —
        // POST returns 503 for ban fields when not wired.
        p2pBanSetFn func(int, time.Duration, uint64) error

        // P2P identity fields — set via SetNodeIdentity after TLS key is loaded.
        tlsFingerprint string // SHA-256 fingerprint of the node's TLS certificate
        p2pListenAddr  string // TCP listen address for P2P (e.g. "0.0.0.0:30303")
        nodeID         string // hex node ID derived from the validator public key

        // nodeViewKeyHex is the hex-encoded Ed25519 view private scalar from node.yaml (optional).
        // When non-empty, restAddressUTXOs uses it to decrypt enc_amount for owned stealth outputs
        // inline, without requiring the caller to supply a view_key_hex query parameter.
        nodeViewKeyHex string

        // pruningMode is "light" or "archive" (default "archive").
        // Set via SetPruningMode so the API can hint about pruned UTXOs.
        pruningMode string

        // keepBlocks is the number of recent blocks whose full data is retained
        // (cfg.Pruning.KeepBlocks).  Used by restUTXO to compute blocks_until_pruned.
        keepBlocks uint64

        // txTotal is an O(1) cached total non-coinbase tx count.
        // Updated atomically so no lock is needed in hot paths.
        txTotal int64

        // syncing is 1 while the node is loading blocks from disk on startup,
        // and 0 once it is fully ready.  Changed atomically — no lock needed.
        // Default is 1 (syncing) so callers see the correct state before SetReady.
        syncing int32

        // utxoRebuilding is 1 for ~90 s after startup to signal that the UTXO
        // index is still settling even though block loading has completed.
        // During this window wallets should not interpret a 0 balance or empty
        // UTXO list as proof that funds are gone.  Cleared by SetUTXOReady().
        // Default is 1 so every restart begins with the flag set.
        utxoRebuilding int32

        // startupRescue is 1 for the lifetime of the process when the node used
        // the rescue snapshot path at startup (UTXO-count mismatch in the primary
        // snapshot, falling back to a tail scan from the rescue snapshot height).
        // Never cleared — operators can query /api/v1/status at any time to check
        // whether the current process recovered via the rescue path.
        startupRescue int32

        // phantomKICount is the number of key images found in the startup snapshot
        // that are absent from the persistent LevelDB key-image index.  These
        // "phantom" entries arise when an OOM kill saves a snapshot that includes
        // in-flight mempool key images that were never confirmed on-chain.  Each
        // phantom entry marks a live UTXO as "spent" even though it is active,
        // blocking all withdrawal attempts for that address until the operator
        // runs --rebuild-key-images.  Set once by cmd/node after snapshot restore;
        // 0 means no phantom entries were detected.
        phantomKICount int64

        // syncingHeight and tipHeight track startup block-scan progress so that
        // /api/v1/status can report how far along the replay is.
        // syncingHeight is the last block processed; tipHeight is the total to load.
        // Both are updated atomically by SetSyncProgress in cmd/node/main.go.
        syncingHeight int64
        tipHeight     int64

        // storeMissingBlocks is the number of heights in the in-memory window
        // (startLoad..tipHeight) for which no block was found in the LevelDB
        // store at startup.  Set once by SetStoreMissingBlocks immediately after
        // loadRecentBlocksFromStore; 0 means no gaps detected.
        storeMissingBlocks int64

        // storeMissingFirstBlock and storeMissingLastBlock are the lowest and
        // highest heights at which missing blocks were detected during the startup
        // window scan. Both are 0 when storeMissingBlocks is 0.
        storeMissingFirstBlock int64
        storeMissingLastBlock  int64

        // utxoStoreMissing is the number of unspent transaction outputs in the
        // sampled tail of the chain whose u/ (UTXO store) LevelDB entry is absent.
        // Set once by SetUTXOStoreMissing after the startup gap sample in
        // cmd/node/main.go.  0 means the store looks healthy; > 0 means the
        // operator should run --repair-db before any withdrawal is attempted.
        utxoStoreMissing int64

        // snapshotMu guards the snapshot status fields below.
        snapshotMu sync.Mutex
        // lastSnapshotHeight is the block height of the most recently saved snapshot.
        // Zero until the first successful save this session.
        lastSnapshotHeight uint64
        // lastSnapshotSavedAt is the wall-clock time when the last successful
        // snapshot save completed.  IsZero() until first success.
        lastSnapshotSavedAt time.Time
        // lastSnapshotErrStr is the error from the most recent failed save attempt.
        // Empty when the last attempt succeeded or no attempt has been made.
        lastSnapshotErrStr string
        // lastSnapshotSaveDurMs is the wall-clock milliseconds taken by the most
        // recent successful snapshot save.  Zero until the first timed save.
        lastSnapshotSaveDurMs int64
        // lastSnapshotTimeoutSec is the effective systemd TimeoutStopSec read at
        // shutdown time.  Zero when systemd is not the supervisor or the value
        // cannot be determined.
        lastSnapshotTimeoutSec float64
        // snapshotTimingFromPrevious is true when lastSnapshotSaveDurMs and
        // lastSnapshotTimeoutSec were restored from the LevelDB metadata written
        // by the previous process's shutdown handler — i.e. the current process
        // has not yet completed its own shutdown snapshot and the timing data
        // shown to operators is from the prior run.
        // Cleared to false as soon as SetSnapshotTimings is called during the
        // current session (i.e. after the first in-process snapshot save).
        snapshotTimingFromPrevious bool

        // utxoAddrCache is a short-TTL in-memory cache for /address/{addr}/utxos
        // responses. The mint-UTXO monitor calls this endpoint every 5 minutes
        // for each admin-mint address; without caching each call does an O(n)
        // UTXO scan with Ed25519 point ops that pins all CPU cores for 20-30 s.
        utxoAddrCache sync.Map // key: addr+"|"+viewKeyHex  value: *utxoCacheEntry

        // dataDir is the node's data directory (cfg.DataDir).  Set via SetDataDir
        // after the API server is created; used by snapshot and chaindb export
        // endpoints to locate files for the one-command node-join workflow.
        dataDir string

        // stakingPoolFn returns (remaining nAPRO, init nAPRO, reward mode string).
        // Wired from consensus.Engine after startup.  nil = not wired.
        stakingPoolFn func() (uint64, uint64, string)
        // blockRewardFn returns the consensus base reward for the next block.
        // Priority tips are transaction-dependent and are not included.
        blockRewardFn func() uint64

        // rssStatsFn returns the process Resident Set Size in bytes.
        // Wired from cmd/node after startup via SetRSSStatsFn.
        // Returns 0 when not wired (non-Linux environments, unit tests).
        rssStatsFn func() int64

        // staleBootnodeFn returns the list of configured bootnodes whose DNS has
        // not resolved successfully for longer than MaxStaleBootnodeAge.
        // Wired to p2p.Host.GetStaleBootnodes by cmd/node.
        // nil = P2P layer not running (field omitted from /health response).
        staleBootnodeFn func() []StaleBootnodeEntry
}

// NewServer creates a new API server.
func NewServer(addr string, chain *core.Chain, mempool *core.Mempool, utxos *core.UTXOSet, log *slog.Logger) *Server {
        s := &Server{
                addr:        addr,
                chain:       chain,
                mempool:     mempool,
                utxos:       utxos,
                log:         log,
                mux:         http.NewServeMux(),
                hub:         NewHub(log),
                rateLimiter: NewRateLimiter(),
                syncing:        1, // syncing until SetReady() is called
                utxoRebuilding: 1, // set until SetUTXOReady() is called ~90 s after startup
        }
        s.registerRoutes()
        return s
}

// SetReady marks the node as fully loaded and no longer syncing.
// Call this after all blocks have been replayed from disk so that
// /api/v1/status reports syncing=false and UTXO supply queries proceed normally.
func (s *Server) SetReady() { atomic.StoreInt32(&s.syncing, 0) }

// SetUTXOReady signals that the UTXO index has fully settled after startup.
// Call ~90 s after SetReady() to clear the utxo_rebuilding flag so wallets
// resume showing live balance figures without the "rebuilding" banner.
func (s *Server) SetUTXOReady() { atomic.StoreInt32(&s.utxoRebuilding, 0) }

// SetStartupRescue marks that this process started via the snapshot rescue path
// (UTXO-count mismatch in the primary snapshot, tail scan from rescue height).
// Call once when the rescue path is activated in cmd/node/main.go.
// The flag is never cleared — it is visible on /api/v1/status for the lifetime
// of the process so operators can confirm the recovery mode at any time.
func (s *Server) SetStartupRescue() { atomic.StoreInt32(&s.startupRescue, 1) }

// SetPhantomKICount records the number of key images in the startup snapshot
// that are absent from the persistent LevelDB key-image index (phantom entries).
// Call once from cmd/node/main.go after the post-snapshot phantom-KI check.
// The value is exposed on /api/v1/status so the API server monitor can fire a
// Telegram alert before any withdrawal attempt triggers the "key image spent"
// error path.  0 means no phantom entries were detected.
func (s *Server) SetPhantomKICount(n int) {
        atomic.StoreInt64(&s.phantomKICount, int64(n))
}

// SetSnapshotSaved records a successful snapshot save at the given chain height.
// Called by the periodic-save goroutine and the shutdown handler in cmd/node/main.go.
// Exposed via /api/v1/status so the API server's system monitor can track freshness.
func (s *Server) SetSnapshotSaved(height uint64) {
        s.snapshotMu.Lock()
        s.lastSnapshotHeight = height
        s.lastSnapshotSavedAt = time.Now()
        s.lastSnapshotErrStr = ""
        s.snapshotMu.Unlock()
}

// SetSnapshotTimings records the wall-clock duration of the last successful
// snapshot save and the effective systemd TimeoutStopSec at that moment.
// Both values are exposed via /api/v1/status so the Admin Panel can display
// the timeout-ratio risk indicator without requiring log access.
// timeoutSec == 0 means the value could not be determined (non-systemd host).
func (s *Server) SetSnapshotTimings(dur time.Duration, timeoutSec float64) {
        s.snapshotMu.Lock()
        s.lastSnapshotSaveDurMs = dur.Milliseconds()
        s.lastSnapshotTimeoutSec = timeoutSec
        s.snapshotTimingFromPrevious = false
        s.snapshotMu.Unlock()
}

// SetSnapshotTimingsFromPreviousShutdown pre-populates the snapshot timing
// fields from values persisted to LevelDB by the previous process's shutdown
// handler.  Unlike SetSnapshotTimings, this sets snapshotTimingFromPrevious
// to true so the API (and Admin Panel) can label the data accordingly.
// Call this once at startup, before the first in-process snapshot save.
func (s *Server) SetSnapshotTimingsFromPreviousShutdown(dur time.Duration, timeoutSec float64) {
        s.snapshotMu.Lock()
        s.lastSnapshotSaveDurMs = dur.Milliseconds()
        s.lastSnapshotTimeoutSec = timeoutSec
        s.snapshotTimingFromPrevious = true
        s.snapshotMu.Unlock()
}

// SetSnapshotFailed records a failed snapshot save attempt.
// The error message is exposed via /api/v1/status so the API server's monitoring
// loop can send a Telegram alert without polling the filesystem.
func (s *Server) SetSnapshotFailed(err error) {
        if err == nil {
                return
        }
        s.snapshotMu.Lock()
        s.lastSnapshotErrStr = err.Error()
        s.snapshotMu.Unlock()
}

// SetSyncProgress records how far the startup block scan has progressed.
// current is the height of the last block processed; tip is the chain tip height.
// Call this periodically inside the startup scan loop so /api/v1/status can
// report meaningful progress to operators while the node is still loading.
func (s *Server) SetSyncProgress(current, tip uint64) {
        atomic.StoreInt64(&s.syncingHeight, int64(current))
        atomic.StoreInt64(&s.tipHeight, int64(tip))
}

// SyncingHeight returns the last block height processed by the startup scan.
func (s *Server) SyncingHeight() int64 { return atomic.LoadInt64(&s.syncingHeight) }

// TipHeight returns the chain tip height that the startup scan is working toward.
func (s *Server) TipHeight() int64 { return atomic.LoadInt64(&s.tipHeight) }

// SetRegistry wires the live PoS validator registry so the API can serve
// /api/v1/validators and include validator_count in network stats.
func (s *Server) SetRegistry(r *core.ValidatorRegistry) { s.registry = r }

// SetValidatorKey provides the node's own validator private key to the API
// server so the /api/v1/admin/partial-unstake endpoint can create properly
// signed StakeAdminWithdraw transactions.  Optional — endpoint returns 503
// when no key is configured.
func (s *Server) SetValidatorKey(key *crypto.LockedValidatorKey) { s.myKey = key }

// APIKeyConfig optionally sets the required API key for write operations.
// Call before Start(). Empty string disables key enforcement (dev mode).
func (s *Server) SetAPIKey(key string) { s.apiKey = key }

// SetAllowedOrigins configures the CORS origin whitelist.
// Empty slice allows all origins ("*").
func (s *Server) SetAllowedOrigins(origins []string) { s.corsOrigins = origins }

// SetStore wires the LevelDB block store so the API can fall back to disk
// when looking up old or pruned blocks that have been evicted from memory.
// Optional — endpoints return 404 for old blocks when no store is wired.
func (s *Server) SetStore(db *store.DB) { s.blockStore = db }

// SetDataDir records the node's data directory so the snapshot and chaindb
// export endpoints can locate files for the one-command node-join workflow.
// Call immediately after NewServer, before Start().
func (s *Server) SetDataDir(dir string) { s.dataDir = dir }

// SetStakingPoolFn wires the staking pool status accessor from the consensus engine.
// fn must return (remaining nAPRO, init nAPRO, reward mode string).
func (s *Server) SetStakingPoolFn(fn func() (uint64, uint64, string)) {
        s.stakingPoolFn = fn
}

// SetBlockRewardFn wires the consensus-derived next-block base reward.
func (s *Server) SetBlockRewardFn(fn func() uint64) { s.blockRewardFn = fn }

func (s *Server) currentBlockRewardNAPRO() string {
        if s.blockRewardFn == nil {
                return "0"
        }
        return strconv.FormatUint(s.blockRewardFn(), 10)
}

func (s *Server) currentBlockRewardAPRO() float64 {
        if s.blockRewardFn == nil {
                return 0
        }
        return float64(s.blockRewardFn()) / 1e8
}

// SetRSSStatsFn wires a function returning the process Resident Set Size in
// bytes so /api/health can expose live memory stats without SSH access.
// Optional — reports rss_bytes=0 when not wired (e.g. unit tests).
func (s *Server) SetRSSStatsFn(f func() int64) { s.rssStatsFn = f }

// SetPruningMode records the node's pruning mode ("archive" or "light") so
// stake endpoints can detect when a missing UTXO may have been pruned rather
// than simply spent, and return a descriptive error.  Call after NewServer.
func (s *Server) SetPruningMode(mode string) { s.pruningMode = mode }

// SetKeepBlocks records cfg.Pruning.KeepBlocks so restUTXO can compute the
// blocks_until_pruned warning field for UTXOs approaching the prune window.
func (s *Server) SetKeepBlocks(n uint64) { s.keepBlocks = n }

// SetNodeViewKey stores the node's configured view private key (hex-encoded).
// When set, restAddressUTXOs automatically decrypts enc_amount for all owned
// UTXOs — both transparent outputs and stealth outputs — without requiring the
// caller to supply a view_key_hex query parameter.
func (s *Server) SetNodeViewKey(hexKey string) { s.nodeViewKeyHex = hexKey }

// FlushUTXOCache removes all cached /address/{addr}/utxos entries immediately.
// Intended for use in tests that mutate the UTXO set and then require a fresh
// response from the REST handler on the very next call, bypassing the normal TTL.
func (s *Server) FlushUTXOCache() {
        s.utxoAddrCache.Range(func(k, _ interface{}) bool {
                s.utxoAddrCache.Delete(k)
                return true
        })
}

// SetPeerCounter wires a function returning the live P2P peer count so
// /metrics can report it. Optional — /metrics reports 0 peers if unset.
func (s *Server) SetPeerCounter(f func() int) { s.peerCounter = f }

// SetPendingHandshakeCounter wires a function returning the number of inbound
// connections currently in the TLS handshake phase so /api/v1/network/stats
// can report it (Task #504).
func (s *Server) SetPendingHandshakeCounter(f func() int64) { s.pendingHandshakeCounter = f }

// SetReconnectBackoffFlag wires a function reporting whether at least one
// bootnode is currently inside its dial back-off window so
// /api/v1/network/stats can expose reconnect_backoff_active (Task: relay
// silently stuck with 0 peers after back-off cap). Optional — reports false
// when unset.
func (s *Server) SetReconnectBackoffFlag(f func() bool) { s.reconnectBackoffFlag = f }

// StallEventEntry is one block-fetch stall event returned by the REST API.
// Mirrors p2p.StallEvent; defined here to avoid an import cycle.
type StallEventEntry struct {
        PeerAddr    string    `json:"peer_addr"`
        StalledCount int      `json:"stalled_count"`
        At          time.Time `json:"at"`
}

// BanEventEntry is one peer-ban event returned by the REST API.
// Mirrors p2p.BanEvent; defined here to avoid an import cycle.
type BanEventEntry struct {
        IP              string    `json:"ip"`
        PeerAddr        string    `json:"peer_addr"`
        PeerID          string    `json:"peer_id"`
        Reason          string    `json:"reason"`
        Violations      int       `json:"violations"`
        BanDurationSecs int64     `json:"ban_duration_secs"`
        At              time.Time `json:"at"`
}

// BootnodeWarnEntry is one malformed/stale bootnode warning event returned by the REST API.
// Mirrors p2p.BootnodeWarnEvent; defined here to avoid an import cycle.
type BootnodeWarnEntry struct {
        Bootnode string    `json:"bootnode"`
        Err      string    `json:"err"`
        AgeSecs  int64     `json:"age_secs"`
        At       time.Time `json:"at"`
}

// DuplicateIdentityEntry is one duplicate-identity fingerprint conflict event
// returned by the REST API.
// Mirrors p2p.DuplicateIdentityEvent; defined here to avoid an import cycle.
type DuplicateIdentityEntry struct {
        Addr        string    `json:"addr"`
        Fingerprint string    `json:"fingerprint"`
        At          time.Time `json:"at"`
}

// WhitelistExemptionEntry is one whitelist-exemption event returned by the REST API.
// Mirrors p2p.WhitelistExemptionEvent; defined here to avoid an import cycle.
type WhitelistExemptionEntry struct {
        IP          string    `json:"ip"`
        PeerAddr    string    `json:"peer_addr"`
        BlockHeight uint64    `json:"block_height"`
        OurTip      uint64    `json:"our_tip"`
        At          time.Time `json:"at"`
}

// PeerListEntry is one connected peer snapshot returned by the REST API.
// Mirrors p2p.PeerListEntry; defined here to avoid an import cycle.
type PeerListEntry struct {
        Addr      string `json:"addr"`
        Height    uint64 `json:"height"`
        Direction string `json:"direction"` // "in" or "out"
}

// BanEntry is a snapshot of one active P2P ban entry, returned by the REST API.
type BanEntry struct {
        Addr      string    `json:"addr"`
        Reason    string    `json:"reason"`
        ExpiresAt time.Time `json:"expires_at"`
}

// SetBanListFunc wires a function that returns active P2P bans.
// Optional — GET /api/v1/network/bans returns 503 when not wired.
func (s *Server) SetBanListFunc(f func() []BanEntry) { s.banListFn = f }

// SetBanLiftFunc wires a function that removes a P2P ban by addr.
// Returns true when a ban was found and lifted.
// Optional — DELETE /api/v1/network/bans/:addr returns 503 when not wired.
func (s *Server) SetBanLiftFunc(f func(string) bool) { s.banLiftFn = f }

// SetBanAddFunc wires a function that adds a new P2P ban.
// Optional — POST /api/v1/network/bans returns 503 when not wired.
func (s *Server) SetBanAddFunc(f func(addr, reason string, d time.Duration)) { s.banAddFn = f }

// SetPeerWhitelist stores the active peer IP whitelist entries from node.yaml
// so /api/v1/status can return them to operators for verification.
// Call after the p2p.Host is constructed in cmd/node.
func (s *Server) SetPeerWhitelist(entries []string) {
        cp := make([]string, len(entries))
        copy(cp, entries)
        s.peerWhitelist = cp
}

// SetTimestampRejectedCounter wires a function returning the live count of blocks
// rejected by the timejacking guard.  Optional — reports 0 when unset.
func (s *Server) SetTimestampRejectedCounter(f func() int64) { s.tsRejectedCounter = f }

// SetMintScheduler wires the consensus engine's admin-mint scheduler.
// Call immediately after engine construction, before Start().  When unwired,
// POST /api/v1/admin/mint returns 503 — mints must never fall back to the
// legacy height=0 mempool path (shared key image per address).
func (s *Server) SetMintScheduler(f func(addr string, amountNAPR uint64, timeout time.Duration) (string, uint64, error)) {
        s.mintScheduler = f
}

// SetWhitelistGetFunc wires a function returning the current peer whitelist entries.
// Optional — GET /api/v1/network/whitelist returns 503 when not wired.
func (s *Server) SetWhitelistGetFunc(f func() []string) { s.whitelistGetFn = f }

// SetWhitelistAddFunc wires a function that adds one IP or CIDR to the live whitelist.
// Optional — POST /api/v1/network/whitelist returns 503 when not wired.
func (s *Server) SetWhitelistAddFunc(f func(string) error) { s.whitelistAddFn = f }

// SetWhitelistRemoveFunc wires a function that removes one IP or CIDR from the
// live whitelist.  Returns (true, nil) when found and removed, (false, nil) when
// not found, or (false, err) when persistence fails.
// Optional — DELETE /api/v1/network/whitelist/:entry returns 503 when not wired.
func (s *Server) SetWhitelistRemoveFunc(f func(string) (bool, error)) { s.whitelistRemoveFn = f }

// SetP2PKeepaliveGetFunc wires a function returning the current live keepalive
// Ping interval.  Wired to p2p.Host.GetKeepaliveInterval by cmd/node.
// Optional — GET /api/v1/network/p2p-config returns 503 when not wired.
func (s *Server) SetP2PKeepaliveGetFunc(f func() time.Duration) { s.p2pKeepaliveGetFn = f }

// SetP2PKeepaliveSetFunc wires a function that updates the live keepalive
// Ping interval.  Wired to p2p.Host.SetKeepaliveInterval by cmd/node.
// Optional — POST /api/v1/network/p2p-config returns 503 when not wired.
func (s *Server) SetP2PKeepaliveSetFunc(f func(time.Duration) error) { s.p2pKeepaliveSetFn = f }

// SetP2PKeepalivePersistFunc wires a function that persists the keepalive
// interval back to node.yaml (atomic tmp+rename) so it survives a restart.
// Optional — when not wired, POST only updates the live value.
func (s *Server) SetP2PKeepalivePersistFunc(f func(time.Duration) error) {
        s.p2pKeepalivePersistFn = f
}

// SetP2PKeepaliveYAMLFunc wires a function that re-reads node.yaml and returns
// the persisted keepalive interval so GET can report live vs persisted drift.
// Optional — when not wired the yaml fields are omitted from the GET response.
func (s *Server) SetP2PKeepaliveYAMLFunc(f func() (time.Duration, error)) {
        s.p2pKeepaliveYAMLFn = f
}

// SetP2PBanConfig stores the static rogue-fork ban parameters read from
// node.yaml so that GET /api/v1/network/p2p-config can include them in its
// response.  Call this once after the P2P host is started and the config has
// been validated.
func (s *Server) SetP2PBanConfig(threshold int, durationSecs int64, heightLead uint64) {
        s.p2pBadBlockBanThreshold = threshold
        s.p2pBadBlockBanDurationSecs = durationSecs
        s.p2pBadBlockHeightLead = heightLead
}

// SetP2PBanConfigGetFunc wires a function returning the current LIVE
// wrong-fork ban parameters (threshold, duration, height lead).  Wired to
// p2p.Host.GetBanConfig by cmd/node.  When wired, GET /api/v1/network/
// p2p-config reports these live values instead of the static node.yaml ones.
func (s *Server) SetP2PBanConfigGetFunc(f func() (int, time.Duration, uint64)) {
        s.p2pBanGetFn = f
}

// SetP2PBanConfigSetFunc wires a function that updates the live wrong-fork
// ban parameters.  Wired to p2p.Host.SetBanConfig by cmd/node.
// Optional — POST /api/v1/network/p2p-config returns 503 for ban fields
// when not wired.
func (s *Server) SetP2PBanConfigSetFunc(f func(int, time.Duration, uint64) error) {
        s.p2pBanSetFn = f
}

// SetStallEventFunc wires a function that returns block-fetch stall events
// recorded since a given time.  Wired to p2p.Host.GetStallEvents by cmd/node.
// Optional — GET /api/v1/network/stall-events returns 503 when not wired.
func (s *Server) SetStallEventFunc(f func(since time.Time) []StallEventEntry) {
        s.stallEventFn = f
}

// SetBootnodeWarnEventFunc wires a function that returns malformed/stale bootnode
// warning events recorded since a given time.
// Wired to p2p.Host.GetBootnodeWarnEvents by cmd/node.
// Optional — GET /api/v1/network/bootnode-warn-events returns 503 when not wired.
func (s *Server) SetBootnodeWarnEventFunc(f func(since time.Time) []BootnodeWarnEntry) {
        s.bootnodeWarnEventFn = f
}

// StaleBootnodeEntry is one bootnode reported as stale in the /health response.
// Mirrors p2p.StaleBootnode; defined here to avoid an import cycle.
type StaleBootnodeEntry struct {
        Bootnode   string `json:"bootnode"`
        AgeSeconds int64  `json:"age_seconds"`
}

// SetStaleBootnodeFn wires a function that returns the list of currently-stale
// bootnodes (DNS not resolved for longer than MaxStaleBootnodeAge).
// Wired to p2p.Host.GetStaleBootnodes by cmd/node.
// Optional — field is omitted from /health when not wired.
func (s *Server) SetStaleBootnodeFn(f func() []StaleBootnodeEntry) {
        s.staleBootnodeFn = f
}

// SetDuplicateIdentityEventFunc wires a function that returns duplicate-identity
// fingerprint conflict events recorded since a given time.
// Wired to p2p.Host.GetDuplicateIdentityEvents by cmd/node.
// Optional — GET /api/v1/network/duplicate-identity-events returns 503 when not wired.
func (s *Server) SetDuplicateIdentityEventFunc(f func(since time.Time) []DuplicateIdentityEntry) {
        s.duplicateIdentityEventFn = f
}

// SetBanEventFunc wires a function that returns peer-ban events recorded since a
// given time.  Wired to p2p.Host.GetBanEvents by cmd/node.
// Optional — GET /api/v1/network/ban-events returns 503 when not wired.
func (s *Server) SetBanEventFunc(f func(since time.Time) []BanEventEntry) {
        s.banEventFn = f
}

// SetWhitelistExemptFunc wires a function that returns whitelist-exemption events
// recorded since a given time.  Wired to p2p.Host.GetWhitelistExemptions by cmd/node.
// Optional — GET /api/v1/network/whitelist-exemptions returns 503 when not wired.
func (s *Server) SetWhitelistExemptFunc(f func(since time.Time) []WhitelistExemptionEntry) {
        s.whitelistExemptFn = f
}

// SetPeerListFunc wires a function that returns all currently connected peers
// with their last-reported heights and direction.
// Wired to p2p.Host.GetPeerList by cmd/node.
// Optional — GET /api/v1/network/peers returns 503 when not wired.
func (s *Server) SetPeerListFunc(f func() []PeerListEntry) {
        s.peerListFn = f
}

// SetNodeIdentity stores the P2P TLS fingerprint, listen address, and node ID
// so they can be returned by GET /api/v1/network/identity.
// Call after p2p.LoadOrSaveP2PIdentity succeeds in cmd/node.
func (s *Server) SetNodeIdentity(fingerprint, listenAddr, nodeID string) {
        s.tlsFingerprint = fingerprint
        s.p2pListenAddr = listenAddr
        s.nodeID = nodeID
}

// TimestampRejectedCount returns the current live count (0 when not wired).
func (s *Server) TimestampRejectedCount() int64 {
        if s.tsRejectedCounter == nil {
                return 0
        }
        return s.tsRejectedCounter()
}

// SetStoreMissingBlocks records the number of heights in the startup
// in-memory window that had no block in the LevelDB store.
// Call once after loadRecentBlocksFromStore completes in cmd/node.
// 0 means no gaps were detected.
func (s *Server) SetStoreMissingBlocks(n int64)      { atomic.StoreInt64(&s.storeMissingBlocks, n) }
func (s *Server) SetStoreMissingFirstBlock(n int64)  { atomic.StoreInt64(&s.storeMissingFirstBlock, n) }
func (s *Server) SetStoreMissingLastBlock(n int64)   { atomic.StoreInt64(&s.storeMissingLastBlock, n) }

// SetUTXOStoreMissing records the number of unspent outputs in the sampled
// chain tail whose u/ LevelDB entry was absent at startup.  Call once from
// cmd/node/main.go after sampleUTXOStoreGaps completes.  0 means no gaps.
// The value is exposed on /api/v1/status so the api-server monitor can fire
// a Telegram alert before any withdrawal attempt triggers the error path.
func (s *Server) SetUTXOStoreMissing(n int64) { atomic.StoreInt64(&s.utxoStoreMissing, n) }

// SetTxTotal sets the initial total non-coinbase tx count (call once after
// loading the chain from disk to avoid an O(n) scan on every stats request).
func (s *Server) SetTxTotal(n int64) { atomic.StoreInt64(&s.txTotal, n) }

// AddTxCount increments the cached tx counter by delta (call from OnBlockProduced).
func (s *Server) AddTxCount(delta int64) { atomic.AddInt64(&s.txTotal, delta) }

// TxTotal returns the current cached total non-coinbase tx count.
func (s *Server) TxTotal() int64 { return atomic.LoadInt64(&s.txTotal) }

// Hub returns the WebSocket hub (for node to push events).
func (s *Server) Hub() *Hub { return s.hub }

func (s *Server) registerRoutes() {
        s.mux.HandleFunc("/", s.handleRPC)
        s.mux.HandleFunc("/health", s.handleHealth)
        s.mux.HandleFunc("/metrics", s.handleMetrics)
        s.mux.Handle("/ws", s.hub.Handler())
        s.registerRESTRoutes()
}

// ServeHTTP implements http.Handler so Server can be used with httptest.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
        s.mux.ServeHTTP(w, r)
}

// Start binds and serves. Blocks until server returns.
// The full middleware chain is: CORS → RateLimit → routes.
func (s *Server) Start() error {
        cors := CORSConfig{AllowedOrigins: s.corsOrigins}
        handler := cors.Middleware(s.rateLimiter.Middleware(s.mux))
        srv := &http.Server{
                Addr:         s.addr,
                Handler:      handler,
                ReadTimeout:  15 * time.Second,
                WriteTimeout: 120 * time.Second,
                IdleTimeout:  120 * time.Second,
        }
        s.log.Info("API server listening", "addr", s.addr)
        return srv.ListenAndServe()
}

// ─── JSON-RPC 2.0 types ───────────────────────────────────────────────────────

type rpcRequest struct {
        JSONRPC string          `json:"jsonrpc"`
        ID      interface{}     `json:"id"`
        Method  string          `json:"method"`
        Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
        JSONRPC string      `json:"jsonrpc"`
        ID      interface{} `json:"id"`
        Result  interface{} `json:"result,omitempty"`
        Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
        Code    int    `json:"code"`
        Message string `json:"message"`
}

const (
        errCodeParse   = -32700
        errCodeInvalid = -32600
        errCodeMethod  = -32601
        errCodeParams  = -32602
        errCodeInternal = -32603
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
        var ms runtime.MemStats
        runtime.ReadMemStats(&ms)

        rssBytes := int64(0)
        if s.rssStatsFn != nil {
                rssBytes = s.rssStatsFn()
        }

        resp := map[string]interface{}{
                "status": "ok",
                "height": s.chain.Height(),
                "time":   time.Now().UTC().Format(time.RFC3339),
                "memory": map[string]interface{}{
                        "rss_bytes":        rssBytes,
                        "heap_alloc_bytes": ms.HeapAlloc,
                        "in_memory_blocks": s.chain.InMemoryBlockCount(),
                        "mempool_count":    s.mempool.Count(),
                        "mempool_bytes":    s.mempool.TotalBytes(),
                },
        }
        // Include stale_bootnodes when the P2P layer is wired so the Admin
        // Panel health widget can highlight degraded bootnode DNS without SSH.
        if s.staleBootnodeFn != nil {
                stale := s.staleBootnodeFn()
                if stale == nil {
                        stale = []StaleBootnodeEntry{}
                }
                resp["stale_bootnodes"] = stale
        }
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(resp)
}

// handleMetrics exposes a minimal Prometheus text-format snapshot of node
// health for scraping. No external client library is used — the format is
// simple enough to hand-write and keeps the Go module dependency-free.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
        peers := 0
        if s.peerCounter != nil {
                peers = s.peerCounter()
        }
        activeValidators, totalValidators := 0, 0
        if s.registry != nil {
                activeValidators, totalValidators = s.registry.Count()
        }

        w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

        fmt.Fprintf(w, "# HELP aperod_chain_height Current chain tip height.\n")
        fmt.Fprintf(w, "# TYPE aperod_chain_height gauge\n")
        fmt.Fprintf(w, "aperod_chain_height %d\n", s.chain.Height())

        fmt.Fprintf(w, "# HELP aperod_mempool_size Number of transactions currently in the mempool.\n")
        fmt.Fprintf(w, "# TYPE aperod_mempool_size gauge\n")
        fmt.Fprintf(w, "aperod_mempool_size %d\n", s.mempool.Count())

        fmt.Fprintf(w, "# HELP aperod_mempool_bytes Total byte size of all transactions currently in the mempool.\n")
        fmt.Fprintf(w, "# TYPE aperod_mempool_bytes gauge\n")
        fmt.Fprintf(w, "aperod_mempool_bytes %d\n", s.mempool.TotalBytes())

        fmt.Fprintf(w, "# HELP aperod_mempool_evictions_total Transactions evicted from the mempool since process start (TTL + capacity pressure).\n")
        fmt.Fprintf(w, "# TYPE aperod_mempool_evictions_total counter\n")
        fmt.Fprintf(w, "aperod_mempool_evictions_total %d\n", s.mempool.EvictionsTotal())

        fmt.Fprintf(w, "# HELP aperod_utxo_count Number of unspent outputs tracked in memory.\n")
        fmt.Fprintf(w, "# TYPE aperod_utxo_count gauge\n")
        fmt.Fprintf(w, "aperod_utxo_count %d\n", s.utxos.Count())

        fmt.Fprintf(w, "# HELP aperod_peer_count Number of connected P2P peers.\n")
        fmt.Fprintf(w, "# TYPE aperod_peer_count gauge\n")
        fmt.Fprintf(w, "aperod_peer_count %d\n", peers)

        fmt.Fprintf(w, "# HELP aperod_validator_count_active Number of currently active PoA/PoS validators.\n")
        fmt.Fprintf(w, "# TYPE aperod_validator_count_active gauge\n")
        fmt.Fprintf(w, "aperod_validator_count_active %d\n", activeValidators)

        fmt.Fprintf(w, "# HELP aperod_validator_count_total Total number of registered PoA/PoS validators.\n")
        fmt.Fprintf(w, "# TYPE aperod_validator_count_total gauge\n")
        fmt.Fprintf(w, "aperod_validator_count_total %d\n", totalValidators)

        fmt.Fprintf(w, "# HELP aperod_up Whether the API server is reachable (always 1 when scraped successfully).\n")
        fmt.Fprintf(w, "# TYPE aperod_up gauge\n")
        fmt.Fprintf(w, "aperod_up 1\n")
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        // CORS
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
        if r.Method == http.MethodOptions {
                w.WriteHeader(http.StatusOK)
                return
        }
        if r.Method != http.MethodPost {
                s.writeError(w, nil, errCodeInvalid, "only POST is supported")
                return
        }

        var req rpcRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                s.writeError(w, nil, errCodeParse, "parse error: "+err.Error())
                return
        }
        if req.JSONRPC != "2.0" {
                s.writeError(w, req.ID, errCodeInvalid, "jsonrpc must be '2.0'")
                return
        }

        result, err := s.dispatch(r.Context(), req.Method, req.Params)
        if err != nil {
                s.writeError(w, req.ID, errCodeInternal, err.Error())
                return
        }
        json.NewEncoder(w).Encode(rpcResponse{
                JSONRPC: "2.0",
                ID:      req.ID,
                Result:  result,
        })
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (interface{}, error) {
        switch method {
        case "apr_getNodeInfo":
                return s.aprGetNodeInfo()
        case "apr_getBlockByHeight":
                return s.aprGetBlockByHeight(params)
        case "apr_getBlockByHash":
                return s.aprGetBlockByHash(params)
        case "apr_getTransaction":
                return s.aprGetTransaction(params)
        case "apr_getMempoolInfo":
                return s.aprGetMempoolInfo()
        case "apr_getMempoolTxs":
                return s.aprGetMempoolTxs()
        case "apr_sendRawTransaction":
                return s.aprSendRawTransaction(params)
        case "apr_getBalance":
                return s.aprGetBalance(params)
        case "apr_validateAddress":
                return s.aprValidateAddress(params)
        case "apr_estimateFee":
                return s.aprEstimateFee(params)
        case "apr_walletSend":
                return s.aprWalletSend(ctx, params)
        case "apr_walletMaxSpendable":
                return s.aprWalletMaxSpendable(params)
        case "apr_walletEstimateFee":
                return s.aprWalletEstimateFee(params)
        case "apr_walletBatchSend":
                return s.aprWalletBatchSend(params)
        case "apr_scanUTXOs":
                return s.aprScanUTXOs(params)
        default:
                return nil, fmt.Errorf("method not found: %s", method)
        }
}

// ─── RPC Methods ──────────────────────────────────────────────────────────────

// NodeInfo is the response for apr_getNodeInfo.
type NodeInfo struct {
        ChainID   string `json:"chain_id"`
        Height    uint64 `json:"height"`
        TipHash   string `json:"tip_hash"`
        Timestamp string `json:"timestamp"`
        Mempool   int    `json:"mempool_count"`
        Version   string `json:"version"`
        BurnAddress string `json:"burn_address"`
}

func (s *Server) aprGetNodeInfo() (interface{}, error) {
        tip := s.chain.Tip()
        if tip == nil {
                return nil, fmt.Errorf("chain not initialized")
        }
        tipHash := tip.Hash()
        return NodeInfo{
                ChainID:   "aperod",
                Height:    tip.Header.Height,
                TipHash:   fmt.Sprintf("%x", tipHash[:]),
                Timestamp: time.Unix(0, tip.Header.Timestamp).UTC().Format(time.RFC3339),
                Mempool:   s.mempool.Count(),
                Version:   "0.1.0",
                BurnAddress: crypto.MainnetBurnAddress().String(),
        }, nil
}

// BlockResponse is returned by block-fetching methods.
type BlockResponse struct {
        Hash         string `json:"hash"`
        Height       uint64 `json:"height"`
        PrevHash     string `json:"prev_hash"`
        MerkleRoot   string `json:"merkle_root"`
        Timestamp    string `json:"timestamp"`
        Round        uint32 `json:"round"`
        ValidatorPub string `json:"validator_pub"`
        TxCount      int    `json:"tx_count"`
        Size         int    `json:"size"`
        // OraclePrice is the APRO/USD price embedded by the validator,
        // expressed as USD-per-APRO × 10^9 (9-decimal fixed-point uint64).
        // Zero means no price was embedded (pre-oracle or non-oracle block).
        OraclePrice uint64 `json:"oracle_price"`
        // FeesBurnedNAPRO includes both protocol base fees and explicit
        // intentional burns, expressed in nAPRO.
        FeesBurnedNAPRO string `json:"fees_burned_napro"`
}

func blockToResponse(b *core.Block) BlockResponse {
        h := b.Hash()
        baseFee := b.Header.BaseFee
        if baseFee == 0 {
                baseFee = core.InitialBaseFeePerByte
        }
        var burned uint64
        for i := range b.Txs {
                tx := &b.Txs[i]
                if tx.IsCoinbase() || tx.IsStake() {
                        continue
                }
                minFee := tx.MinFeeAt(baseFee)
                if tx.Fee < minFee {
                        burned += tx.Fee
                } else {
                        burned += minFee
                        if intentionalBurn, isBurn := tx.BurnAmount(); isBurn &&
                                burned <= ^uint64(0)-intentionalBurn {
                                burned += intentionalBurn
                        }
                }
        }
        return BlockResponse{
                Hash:            fmt.Sprintf("%x", h[:]),
                Height:          b.Header.Height,
                PrevHash:        fmt.Sprintf("%x", b.Header.PrevHash[:]),
                MerkleRoot:      fmt.Sprintf("%x", b.Header.MerkleRoot[:]),
                Timestamp:       time.Unix(0, b.Header.Timestamp).UTC().Format(time.RFC3339),
                Round:           b.Header.Round,
                ValidatorPub:    b.Header.ValidatorPub.Hex(),
                TxCount:         len(b.Txs),
                Size:            b.Size(),
                OraclePrice:     b.Header.OraclePrice,
                FeesBurnedNAPRO: strconv.FormatUint(burned, 10),
        }
}

func (s *Server) aprGetBlockByHeight(params json.RawMessage) (interface{}, error) {
        var args struct {
                Height uint64 `json:"height"`
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        block := s.chain.GetByHeight(args.Height)
        if block == nil {
                return nil, fmt.Errorf("block not found at height %d", args.Height)
        }
        return blockToResponse(block), nil
}

func (s *Server) aprGetBlockByHash(params json.RawMessage) (interface{}, error) {
        var args struct {
                Hash string `json:"hash"`
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        b, err := hex.DecodeString(args.Hash)
        if err != nil || len(b) != 32 {
                return nil, fmt.Errorf("invalid hash: must be 64 hex chars")
        }
        var hash crypto.Hash32
        copy(hash[:], b)
        block := s.chain.GetByHash(hash)
        if block == nil {
                return nil, fmt.Errorf("block not found: %s", args.Hash[:16])
        }
        return blockToResponse(block), nil
}

func (s *Server) aprGetMempoolInfo() (interface{}, error) {
        hashes := s.mempool.Hashes()
        return map[string]interface{}{
                "count": len(hashes),
        }, nil
}

func (s *Server) aprGetMempoolTxs() (interface{}, error) {
        hashes := s.mempool.Hashes()
        out := make([]string, len(hashes))
        for i, h := range hashes {
                out[i] = fmt.Sprintf("%x", h[:])
        }
        return out, nil
}

func (s *Server) aprSendRawTransaction(params json.RawMessage) (interface{}, error) {
        var args struct {
                Tx     json.RawMessage `json:"tx"`
                APIKey string          `json:"api_key"` // alternative to X-API-Key header
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        // Check API key when one is configured
        if s.apiKey != "" && args.APIKey != s.apiKey {
                return nil, fmt.Errorf("unauthorized: missing or invalid api_key")
        }
        var tx core.Transaction
        if err := json.Unmarshal(args.Tx, &tx); err != nil {
                return nil, fmt.Errorf("decode tx: %w", err)
        }
        if err := s.mempool.Add(tx); err != nil {
                return nil, fmt.Errorf("rejected: %w", err)
        }
        hash := tx.Hash()
        return map[string]string{"hash": fmt.Sprintf("%x", hash[:])}, nil
}

func (s *Server) aprGetBalance(params json.RawMessage) (interface{}, error) {
        var args struct {
                Address  string `json:"address"`
                ViewKey  string `json:"view_key"` // hex-encoded view private scalar
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        addr := crypto.Address(args.Address)
        if err := crypto.Validate(addr); err != nil {
                return nil, fmt.Errorf("invalid address: %w", err)
        }

        // If view key provided, scan for balance; otherwise report 0 (privacy model)
        balance := uint64(0)
        if args.ViewKey != "" {
                // TODO Phase 2: instantiate WalletScanner and scan chain
        }

        return map[string]interface{}{
                "address": args.Address,
                "balance": balance,
                "unit":    "nAPRO",
        }, nil
}

func (s *Server) aprValidateAddress(params json.RawMessage) (interface{}, error) {
        var args struct {
                Address string `json:"address"`
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        addr := crypto.Address(args.Address)
        err := crypto.Validate(addr)
        valid := err == nil
        result := map[string]interface{}{
                "address": args.Address,
                "valid":   valid,
        }
        if !valid {
                result["error"] = err.Error()
        }
        return result, nil
}

// ─── apr_getTransaction (2.1.4) ───────────────────────────────────────────────

// TxResponse is returned by apr_getTransaction.
type TxResponse struct {
        Hash        string `json:"hash"`
        BlockHash   string `json:"block_hash"`
        BlockHeight uint64 `json:"block_height"`
        TxIndex     int    `json:"tx_index"`
        IsCoinbase  bool   `json:"is_coinbase"`
        Inputs      int    `json:"inputs"`
        Outputs     int    `json:"outputs"`
        Fee         uint64 `json:"fee"`
        Size        int    `json:"size"`
        Version     uint8  `json:"version"`
        // Pending is true when the tx is in the mempool but not yet confirmed.
        Pending bool `json:"pending,omitempty"`
        // IsBurn and BurnedNAPRO describe the explicit intentional burn marker.
        // BurnedNAPRO is decimal because browser JSON numbers lose uint64 precision.
        IsBurn bool `json:"is_burn"`
        BurnedNAPRO string `json:"burned_napro"`
        BurnAddress string `json:"burn_address"`
}

func txBurnResponseFields(tx *core.Transaction) (bool, string, string) {
        amount, ok := tx.BurnAmount()
        if !ok {
                return false, "0", ""
        }
        return true, fmt.Sprintf("%d", amount), crypto.MainnetBurnAddress().String()
}

func (s *Server) aprGetTransaction(params json.RawMessage) (interface{}, error) {
        var args struct {
                Hash string `json:"hash"`
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        b, err := hex.DecodeString(args.Hash)
        if err != nil || len(b) != 32 {
                return nil, fmt.Errorf("invalid hash: must be 64 hex chars")
        }
        var hash crypto.Hash32
        copy(hash[:], b)

        // Search confirmed chain first
        tx, loc, ok := s.chain.GetTransaction(hash)
        if ok {
                bHash := loc.Block.Hash()
                isBurn, burned, burnAddress := txBurnResponseFields(&tx)
                return TxResponse{
                        Hash:        args.Hash,
                        BlockHash:   fmt.Sprintf("%x", bHash[:]),
                        BlockHeight: loc.Block.Header.Height,
                        TxIndex:     loc.TxIndex,
                        IsCoinbase:  tx.IsCoinbase(),
                        Inputs:      len(tx.Inputs),
                        Outputs:     len(tx.Outputs),
                        Fee:         tx.Fee,
                        Size:        tx.Size(),
                        Version:     uint8(tx.Version),
                        IsBurn:      isBurn,
                        BurnedNAPRO: burned,
                        BurnAddress: burnAddress,
                }, nil
        }

        // Check mempool for unconfirmed tx
        if mp, found := s.mempool.Get(hash); found {
                isBurn, burned, burnAddress := txBurnResponseFields(&mp)
                return TxResponse{
                        Hash:       args.Hash,
                        IsCoinbase: mp.IsCoinbase(),
                        Inputs:     len(mp.Inputs),
                        Outputs:    len(mp.Outputs),
                        Fee:        mp.Fee,
                        Size:       mp.Size(),
                        Version:    uint8(mp.Version),
                        Pending:    true,
                        IsBurn:     isBurn,
                        BurnedNAPRO: burned,
                        BurnAddress: burnAddress,
                }, nil
        }

        return nil, fmt.Errorf("transaction not found: %s", args.Hash[:16])
}

// ─── apr_estimateFee (2.1.9) ─────────────────────────────────────────────────

func (s *Server) aprEstimateFee(params json.RawMessage) (interface{}, error) {
        var args struct {
                // SizeBytes is the estimated serialized transaction size.
                // If omitted, returns the minimum fee for a typical RingCT tx.
                SizeBytes int `json:"size_bytes"`
        }
        // params may be null — tolerate unmarshal failure
        _ = json.Unmarshal(params, &args)

        // Dynamic EIP-1559 fee: baseFeePerByte × tx_size_bytes.
        // Return the current InitialBaseFeePerByte as a reference rate.
        // Callers should multiply by their actual tx size; use /estimate_fee RPC for precision.
        sizeBytes := args.SizeBytes
        if sizeBytes == 0 {
                sizeBytes = 2000 // default: typical P2P transfer ~2 KB
        }
        estimatedFee := uint64(sizeBytes) * core.InitialBaseFeePerByte
        return map[string]interface{}{
                "fee":                estimatedFee,
                "base_fee_per_byte":  core.InitialBaseFeePerByte,
                "size_bytes":         sizeBytes,
                "unit":               "nAPRO",
                "flat":               false,
                "model":              "size_based_eip1559",
        }, nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func (s *Server) writeError(w http.ResponseWriter, id interface{}, code int, msg string) {
        json.NewEncoder(w).Encode(rpcResponse{
                JSONRPC: "2.0",
                ID:      id,
                Error:   &rpcError{Code: code, Message: msg},
        })
}
