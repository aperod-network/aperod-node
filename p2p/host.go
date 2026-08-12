package p2p

import (
        "context"
        "crypto/tls"
        "encoding/json"
        "fmt"
        "log/slog"
        "net"
        "os"
        "path/filepath"
        "runtime/debug"
        "sync"
        "sync/atomic"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// Config holds p2p networking parameters.
type Config struct {
        ListenAddr    string
        Bootnodes     []string
        MaxPeers      int
        MinPeers      int
        MaxPeersPerIP int    // max inbound connections from one source IP (0 = unlimited, recommended: 3)
        // MinOutbound is the number of peer slots reserved exclusively for
        // outbound dial-out connections.  Inbound connections may only occupy
        // up to (MaxPeers − MinOutbound) slots; once that cap is reached new
        // inbound connections are politely rejected while outbound dials to
        // bootnodes / discovery peers continue unimpeded.  A validator under
        // an inbound flood can therefore always broadcast produced blocks to
        // the rest of the network.  Recommended: 4.  0 = feature disabled.
        MinOutbound int
        NodeID      string // hex-encoded public key or random ID
        UserAgent   string
        // TLSConfig enables authenticated encrypted transport.
        // When non-nil, the listener is wrapped with tls.NewListener and
        // outbound dials use tls.DialWithDialer.  Both sides must present a
        // certificate; the peer fingerprint is logged on connect and available
        // via PeerFingerprint(conn).
        // nil = plain TCP (unit tests only — never use nil in production).
        TLSConfig *tls.Config
        // SelfFingerprint is the hex-encoded SHA-256 SPKI fingerprint of this
        // node's own TLS identity key.  When non-empty, any peer that presents
        // the same fingerprint is rejected immediately after the TLS handshake
        // with a clear ERROR log and an actionable hint.  This guards against
        // the "rsync copied p2p_identity.key" scenario where both nodes
        // present identical certificates and silently fail to peer.
        // Set by main.go from the fingerprint returned by LoadOrSaveP2PIdentity.
        SelfFingerprint string
        // AllowedPeers is an optional list of hex-encoded SHA-256 SPKI
        // fingerprints that are permitted to connect.  When non-empty, any
        // peer whose TLS fingerprint is not on the list is disconnected
        // immediately after the TLS handshake with a clear log entry.
        // An empty slice means open network (default behaviour).
        AllowedPeers []string
        // PeerWhitelist is an optional list of IP addresses and/or CIDR
        // ranges (e.g. "1.2.3.4", "10.0.0.0/8") that are allowed to
        // connect inbound.  When non-empty, any inbound connection whose
        // source IP is not covered by an entry is dropped immediately —
        // before any P2P handshake.  Outbound dial-outs are not affected.
        // An empty slice means all source IPs are accepted (default).
        PeerWhitelist []string
        // MaxPendingHandshakes limits the number of inbound TCP connections
        // that are concurrently in the TLS handshake phase.  A peer that
        // opens many connections but never completes the handshake would
        // otherwise hold one goroutine each for up to 10 s; this cap bounds
        // the blast radius to MaxPendingHandshakes goroutines.
        // 0 = no limit (not recommended for production).  Default: 20.
        MaxPendingHandshakes int
        // BadBlockHeightLead is how many blocks ahead of the node's tip a block
        // must be before it counts as a rogue-fork strike.  Default: 1000.
        BadBlockHeightLead uint64
        // BadBlockBanThreshold is the number of rogue-fork strikes that trigger a
        // temporary ban of the remote IP.  Default: 10.
        BadBlockBanThreshold int
        // BadBlockBanDuration is how long the ban lasts after the threshold is
        // exceeded.  Default: 24h.
        BadBlockBanDuration time.Duration
        // GetBlockStallTimeout is how long the relay waits for a MsgBlock
        // response after sending MsgGetBlock before it considers the request
        // stalled.  On stall detection the node logs a WARN and re-issues a
        // MsgGetHeaders to restart the sync pipeline from the current tip.
        // Default: 15s.  Lower values are useful in unit tests.
        GetBlockStallTimeout time.Duration
        // KeepaliveInterval is how often a MsgPing is sent to each connected
        // peer so that the peer's ReadTimeout never fires due to silence from
        // our side.  Must be in [1s, ReadTimeout/2] (i.e. [1s, 15s]).
        // Default: 10s (applied in NewHost when zero).
        KeepaliveInterval time.Duration
        // BanFile is the path to the JSON file used to persist active bans across
        // node restarts.  When empty, ban persistence is disabled.  Set to "-" to
        // explicitly disable persistence.  The file is written atomically (tmp +
        // rename) so a crash during a write never corrupts the previous snapshot.
        BanFile string
        // WhitelistFile is the path to a JSON file used to persist the peer IP
        // whitelist across node restarts.  When non-empty, SetPeerWhitelist writes
        // the full list atomically; on Start() the file is loaded and merged with
        // cfg.PeerWhitelist.  When empty, whitelist changes are lost on restart.
        // Set to "-" to explicitly disable persistence.
        WhitelistFile string
        // MaxBlockIngestPerSec caps the number of blocks per second that any
        // single peer may push to this node.  The dispatch goroutine sleeps as
        // needed to honour the limit, creating TCP-level backpressure without
        // dropping blocks.  Default: 50.  0 = disabled.
        MaxBlockIngestPerSec int
        // TxRateBurst is the token-bucket capacity for incoming P2P
        // transactions from one source IP: up to this many transactions may
        // arrive back-to-back before throttling kicks in.
        // 0 = tx rate limiting disabled (unit-test friendly); production
        // nodes get the default (50) via config.DefaultConfig().
        TxRateBurst int
        // TxRateSustained is the sustained refill rate in transactions per
        // second per source IP once the burst allowance is used up.
        // Only effective when TxRateBurst > 0.  Recommended: 10.
        TxRateSustained int
        // TxRateBanThreshold is the number of throttled (dropped)
        // transactions after which the source IP is temporarily banned.
        // The counter resets as soon as the peer drops back below the rate
        // limit, so only sustained flooding accumulates toward a ban.
        // 0 = throttle only, never ban.  Recommended: 100.
        TxRateBanThreshold int
        // TxRateBanDuration is how long the ban lasts after
        // TxRateBanThreshold is exceeded.  Default: 1h (applied in NewHost
        // when zero and TxRateBurst > 0).
        TxRateBanDuration time.Duration
        // MaxStaleBootnodeAge is the maximum time a bootnode may go without a
        // successful DNS resolution before a WARN is emitted on every
        // discovery tick.  The warning includes the bootnode address and the
        // age since last successful resolution so operators can identify and
        // fix decommissioned hostnames before the peer count silently drops to
        // zero.  Default: 24h (applied in NewHost when zero).
        MaxStaleBootnodeAge time.Duration
}

// connIP extracts the host part from an "IP:port" address string.
func connIP(addr string) string {
        host, _, err := net.SplitHostPort(addr)
        if err != nil {
                return addr
        }
        return host
}

// ipInWhitelist reports whether ip is covered by any entry in the whitelist.
// wlNets is the list of parsed CIDRs; wlIPs is the list of individual IPs.
// Returns true immediately when both slices are nil (open network).
func ipInWhitelist(ip net.IP, wlNets []*net.IPNet, wlIPs []net.IP) bool {
        for _, n := range wlNets {
                if n.Contains(ip) {
                        return true
                }
        }
        for _, a := range wlIPs {
                if a.Equal(ip) {
                        return true
                }
        }
        return false
}

// Handler is the callback interface for handling incoming p2p messages.
type Handler interface {
        OnBlock(*core.Block)
        OnTransaction(*core.Transaction)
        OnVote(VoteMsg)
        CurrentHeight() uint64
        CurrentTailHashes(n int) []crypto.Hash32
        GetBlock(hash crypto.Hash32) *core.Block
}

// pendingBlockEntry records one outstanding MsgGetBlock request so the
// stall-detection ticker can log actionable diagnostics if no MsgBlock arrives.
type pendingBlockEntry struct {
        sentAt      time.Time
        headerHeight uint64 // block height from the header, for WARN log context
}

// peerTokenBucket is a simple token-bucket rate limiter used to cap the
// per-peer block ingest rate.  It is not safe for concurrent use; callers
// must ensure only the dispatch goroutine calls Wait.
type peerTokenBucket struct {
        tokens   float64
        lastTime time.Time
        rate     float64 // tokens per second
        burst    float64 // max tokens (= rate, i.e. one second of tokens)
}

// wait blocks (sleeps) until one token is available, then consumes it.
// Returns 0 immediately when the rate is 0 or negative (limiter disabled).
// The duration slept is the minimum needed to refill exactly one token,
// creating backpressure without busy-waiting.
//
// Correctness invariant: after sleeping we advance lastTime past the sleep
// duration so the sleep interval cannot be double-counted as freshly-accrued
// tokens on the very next call.  Without this, each sleeping call would be
// immediately followed by a free call (the sleep itself refills the token),
// yielding pairs of blocks at twice the configured rate.
func (b *peerTokenBucket) wait() time.Duration {
        if b.rate <= 0 {
                return 0
        }
        now := time.Now()
        elapsed := now.Sub(b.lastTime).Seconds()
        // Advance lastTime FIRST so a subsequent call that races
        // in immediately after the sleep does not re-count elapsed.
        b.lastTime = now
        b.tokens += elapsed * b.rate
        if b.tokens > b.burst {
                b.tokens = b.burst
        }
        if b.tokens >= 1.0 {
                b.tokens -= 1.0
                return 0
        }
        // Insufficient tokens: compute how long until one token accrues.
        need := 1.0 - b.tokens
        waitDur := time.Duration(need / b.rate * float64(time.Second))
        b.tokens = 0
        time.Sleep(waitDur)
        // Advance lastTime through the sleep so the sleep interval is not
        // credited as freshly-accrued tokens on the next call.  Using Add
        // rather than time.Now() keeps the accounting deterministic
        // regardless of scheduler latency.
        b.lastTime = b.lastTime.Add(waitDur)
        return waitDur
}

// Peer represents a connected remote node.
type Peer struct {
        conn     net.Conn
        addr     string
        id       string
        height   uint64
        mu       sync.Mutex
        outbound bool

        // lastPongAt is the Unix nanosecond timestamp of the most recent MsgPong
        // received from this peer.  Initialised to the connection start time so
        // the first pong-deadline check does not fire before the peer has had a
        // chance to reply.  Updated by dispatch on every MsgPong; read by the
        // keepalive goroutine to detect peers that have stopped replying altogether.
        // Uses atomic ops so dispatch and the keepalive goroutine coordinate without
        // holding any other mutex.
        lastPongAt atomic.Int64

        // lastHeadersRequestedAt is the Unix nanosecond timestamp of the most
        // recent requestHeaders call that was triggered from a MsgPong handler.
        // Used together with lastHeadersRequestedAtHeight to implement the
        // Pong-triggered requestHeaders cooldown; see the MsgPong dispatch
        // handler for the full decision logic.
        // Updated atomically by the dispatch goroutine; no mutex needed.
        lastHeadersRequestedAt atomic.Int64

        // lastHeadersRequestedAtHeight is the peer-reported height that was
        // current when the most recent Pong-triggered requestHeaders was sent.
        // The MsgPong handler only fires a new requestHeaders when the peer
        // advances strictly beyond this value (new relevant state) OR when the
        // 2×KeepaliveInterval self-heal window has elapsed (covers the case
        // where the first request was silently dropped during UTXO rebuild).
        // Zero means no Pong-triggered request has been sent yet, so the first
        // Pong with a height gap always fires immediately.
        // Updated atomically by the dispatch goroutine; no mutex needed.
        lastHeadersRequestedAtHeight atomic.Uint64

        // pendingBlocksMu guards pendingBlocks.  All access must hold this mutex.
        pendingBlocksMu sync.Mutex
        // pendingBlocks maps block hash → the time MsgGetBlock was sent for that
        // hash and the block's height from the header.  Added in handleHeaders
        // when MsgGetBlock is sent; removed when the corresponding MsgBlock
        // arrives via dispatch.  The stall-detection ticker inspects this map to
        // detect silently-dropped GetBlock responses and re-issue MsgGetHeaders.
        // The map is owned exclusively by this Peer; it is cleaned up
        // automatically when the Peer is dereferenced on disconnect.
        pendingBlocks map[crypto.Hash32]pendingBlockEntry

        // ingestBucket throttles block ingestion from this peer.  Owned
        // exclusively by the dispatch goroutine; no mutex needed.
        ingestBucket peerTokenBucket
}

// Send transmits a message to this peer.
func (p *Peer) Send(msgType MessageType, payload interface{}) error {
        p.mu.Lock()
        defer p.mu.Unlock()
        p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
        return writeMsg(p.conn, msgType, payload)
}

// badBlockStrikeTTL is how long a strike record lives without activity before
// it is discarded.  An attacker that sends one bad block per hour from a unique
// IP can only grow the strike map by badBlockMaxTrackedIPs entries; older
// records are pruned in maintainLoop.
const badBlockStrikeTTL = time.Hour

// stableConnTime is the minimum duration a peer must stay connected before its
// dial back-off counter is reset.  Peers that connect and disconnect faster
// than this threshold are considered flapping and keep their failure count so
// that exponential back-off continues to throttle reconnect storms.
const stableConnTime = 60 * time.Second

// badBlockMaxTrackedIPs caps the number of distinct IPs in badBlockCounts so a
// distributed attacker cannot exhaust node memory by registering many unique IPs.
const badBlockMaxTrackedIPs = 1024

// badBlockStrike records the misbehaviour history for one remote IP.
type badBlockStrike struct {
        count    int
        lastSeen time.Time
}

// whitelistExemptMaxEvents caps the in-memory ring buffer for whitelist
// exemption events so memory cannot grow unboundedly on a busy node.
const whitelistExemptMaxEvents = 100

// banEventMaxEvents caps the in-memory ring buffer for peer-ban events.
const banEventMaxEvents = 200

// stallEventMaxEvents caps the in-memory ring buffer for block-fetch stall
// events so memory cannot grow unboundedly during prolonged sync storms.
const stallEventMaxEvents = 200

// StallEvent records a single block-fetch stall event emitted when the
// GetBlockStallTimeout fires without a MsgBlock response from a peer.
// Stored in a ring buffer and exposed via GetStallEvents so the API server
// can poll and surface them in the Admin Panel notification log.
type StallEvent struct {
        PeerAddr    string    `json:"peer_addr"`
        StalledCount int      `json:"stalled_count"` // number of blocks that stalled in one tick
        At          time.Time `json:"at"`
}

// BanEvent records a single peer-ban event emitted when the wrong-fork
// threshold is exceeded.  Stored in a ring buffer and exposed via
// GetBanEvents so the API server can poll and send Telegram alerts.
type BanEvent struct {
        IP              string    `json:"ip"`
        PeerAddr        string    `json:"peer_addr"`
        PeerID          string    `json:"peer_id"`
        Reason          string    `json:"reason"`
        Violations      int       `json:"violations"`
        BanDurationSecs int64     `json:"ban_duration_secs"`
        At              time.Time `json:"at"`
}

// WhitelistExemptionEvent records a single "strike skipped due to whitelist"
// event for the Admin Panel notification log.
type WhitelistExemptionEvent struct {
        IP          string    `json:"ip"`
        PeerAddr    string    `json:"peer_addr"`
        BlockHeight uint64    `json:"block_height"`
        OurTip      uint64    `json:"our_tip"`
        At          time.Time `json:"at"`
}

// Host is the p2p networking host.
type Host struct {
        cfg     Config
        handler Handler
        log     *slog.Logger

        mu       sync.RWMutex
        peers    map[string]*Peer // addr → Peer
        peerList []string         // known peer addrs

        listener net.Listener
        done     chan struct{}

        mgr     *PeerMgr       // ban list
        gossip      *GossipFilter  // dedup filter for relay
        headers     HeaderProvider // optional: serves headers for sync
        blockByHash   func(crypto.Hash32) *core.Block // optional: LevelDB fallback for GetBlock
        blockByHeight func(uint64) *core.Block        // optional: LevelDB fallback for HeadersFrom

        // pendingHandshakes counts inbound connections that are currently
        // executing the TLS handshake.  Guarded by MaxPendingHandshakes;
        // uses atomic ops so acceptLoop and handleConn coordinate without
        // holding h.mu.
        pendingHandshakes atomic.Int64

        // badBlockCounts tracks out-of-range-block strikes per remote IP.
        // Entries expire after badBlockStrikeTTL and the map is capped at
        // badBlockMaxTrackedIPs to prevent memory exhaustion by distributed
        // attackers.  Guarded by badBlockMu.
        badBlockMu     sync.Mutex
        badBlockCounts map[string]badBlockStrike // bare IP → strike record

        // wlMu guards wlNets, wlIPs, and cfg.PeerWhitelist so that the write
        // path can swap the whitelist atomically while acceptLoop reads it
        // without holding h.mu (the main peer-table lock).
        wlMu sync.RWMutex

        // wlNets and wlIPs hold the parsed form of cfg.PeerWhitelist so that
        // acceptLoop can check each inbound IP with a net.IPNet.Contains call
        // rather than re-parsing the CIDR strings on every connection.
        // Both are nil when cfg.PeerWhitelist is empty (open network).
        wlNets []*net.IPNet
        wlIPs  []net.IP

        // wlMutate serialises the entire read-snapshot → compute-new-list →
        // apply (wlMu.Lock swap) → persist sequence for all mutation paths
        // (SetPeerWhitelist, AddToWhitelist, RemoveFromWhitelist).  Without
        // this lock, two concurrent AddToWhitelist calls would each snapshot
        // the same list, append their own entry, and the last writer would
        // silently drop the other entry.
        wlMutate sync.Mutex

        // wlPersistMu serialises the snapshot+write+rename sequence for the
        // whitelist file so concurrent calls never interleave two partial writes.
        wlPersistMu sync.Mutex

        // wlExemptMu guards wlExemptEvents so the block-processing loop can
        // append events concurrently with the API server reading them.
        wlExemptMu sync.Mutex
        // wlExemptEvents is a ring buffer of whitelist-exemption events for
        // the Admin Panel notification log.  Capped at whitelistExemptMaxEvents.
        wlExemptEvents []WhitelistExemptionEvent

        // txRate throttles incoming P2P transactions per source IP so a
        // slow mempool flood cannot force constant eviction churn during
        // SelectTxs.  nil when Config.TxRateBurst <= 0 (disabled).
        txRate *txRateLimiter

        // banEventMu guards banEvents so the block-processing loop can append
        // events concurrently with the API server reading them.
        banEventMu sync.Mutex
        // banEvents is a ring buffer of peer-ban events for the Admin Panel
        // notification log.  Capped at banEventMaxEvents.
        banEvents []BanEvent

        // stallEventMu guards stallEvents so the keepalive goroutine can append
        // events concurrently with the API server reading them.
        stallEventMu sync.Mutex
        // stallEvents is a ring buffer of block-fetch stall events for the Admin
        // Panel notification log.  Capped at stallEventMaxEvents.
        stallEvents []StallEvent

        // keepaliveIntervalNs holds the current live keepalive Ping interval in
        // nanoseconds.  Updated atomically by SetKeepaliveInterval so the change
        // is visible to all active peer goroutines on their next ping tick without
        // any lock or restart.  Initialised from cfg.KeepaliveInterval in NewHost.
        keepaliveIntervalNs atomic.Int64

        // bootnodeLastResolved maps each raw bootnode string (as it appears in
        // cfg.Bootnodes) to the IP:port addresses it most recently resolved to.
        // Updated on every successful DNS resolution in Start() and maintainLoop.
        // DNS failures leave the previous entry intact (retention behaviour).
        // bootnodeSet is always rebuilt from this map so a bootnode that moves to
        // a new IP loses its old address's privileged status on the next tick.
        // Protected by h.mu (write-lock for mutations, read-lock for lookups).
        bootnodeLastResolved map[string][]string

        // bootnodeLastResolvedAt maps each raw bootnode string to the wall-clock
        // time of its most recent successful DNS resolution.  Initialised to
        // time.Now() when Start() is called so new nodes get a full grace period
        // before any stale-bootnode WARN fires.  Updated by applyBootnodeResolution
        // on every successful resolution.  Protected by h.mu.
        bootnodeLastResolvedAt map[string]time.Time

        // bootnodeSet is the flat set of currently-privileged IP:port addresses,
        // rebuilt from bootnodeLastResolved on every write.  Used by dialPeer to
        // skip exponential back-off — only the ban list can block a bootnode.
        // Protected by h.mu.
        bootnodeSet map[string]struct{}

        // maintainNow is an optional channel that tests can send on to trigger
        // an immediate maintainLoop tick without waiting for the 10-second ticker.
        // nil in production (Start always initialises it).
        maintainNow chan struct{}

        // ── Dial-gate: serialises ban writes with dial initiations ────────────
        //
        // dialGateMu is held (briefly) by both dialPeer and BanPeer so that
        // "check IsBanned, then register intent to dial" is atomic with
        // "write ban, then cancel in-flight dials."  Without this, a goroutine
        // can pass IsBanned, have BanPeer run and complete, and then still
        // initiate a TCP connection to the now-banned address.
        //
        // The lock is never held across network I/O; it guards only the tiny
        // map operations described above.
        dialGateMu sync.Mutex
        // dialingIPs maps the canonical bare IP of an in-progress outbound
        // dial to the set of active cancel functions keyed by a unique dial ID.
        // BanPeer drains and invokes all cancel functions for the banned IP so
        // that any in-flight DialContext call is aborted before the TCP
        // connection is established.
        dialingIPs map[string]map[uint64]context.CancelFunc
        // nextDialID is the monotonically increasing ID assigned to each new
        // in-flight dial so entries can be removed from dialingIPs on exit
        // without ambiguity.
        nextDialID atomic.Uint64

        // dialContextFunc is the function used for outbound TCP dials.
        // In production it is (&net.Dialer{}).DialContext (the DialTimeout
        // deadline is set by the context created in dialPeer).
        // Tests may replace it with a function that blocks on a channel to
        // exercise the race between an in-progress dial and a concurrent ban.
        // All accesses must be guarded by dialFnMu to prevent data races when
        // tests replace the function while a startup dial goroutine is active.
        dialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)
        dialFnMu        sync.RWMutex

        // pendingConns maps the canonical bare IP of a just-connected peer to
        // the live TCP/TLS conn that has not yet been handed to handleConn.
        // BanPeer closes every conn in pendingConns[ip] so that a connection
        // that completed dialContextFunc after BanPeer's cancel is still
        // prevented from reaching the peer message loop.
        //
        // The field is accessed only while dialGateMu is held.
        pendingConns map[string]map[uint64]net.Conn

        // postConnectHook, when non-nil, is called by dialPeer between the
        // post-dial ban check (which registers the conn in pendingConns) and
        // the launch of handleConn.  This gives tests a deterministic window
        // to call BanPeer while the conn is registered as pending, in order to
        // verify that BanPeer closes it before handleConn can start.
        // Always nil in production.
        postConnectHook func()

        // listenFunc is the function used to open the TCP listener in Start().
        // In production it is always net.Listen (set in NewHost).  Tests may
        // replace it with a custom factory whose body runs at the real bind
        // point — making it the only reliable way to assert the whitelist-before-
        // listener ordering invariant: if net.Listen (or this field) is ever
        // moved before loadWhitelistFromFile, the test factory executes before
        // the whitelist is populated and the test fails immediately.
        //
        // Ordering invariant (must never regress):
        //   loadWhitelistFromFile  ->  listenFunc / net.Listen  ->  tls.NewListener
        listenFunc func(network, addr string) (net.Listener, error)

        // pongGetHeadersTotal counts how many times the MsgPong handler has
        // passed the cooldown gate and actually called requestHeaders.  It does
        // NOT count requestHeaders calls from the sync ticker or stall
        // detector.  Incremented atomically so tests can read it without a
        // lock.  Only meaningful in test scenarios; always zero in production
        // code that does not inspect it.
        pongGetHeadersTotal atomic.Int64
}

// NewHost creates a new p2p host.
func NewHost(cfg Config, handler Handler, log *slog.Logger) *Host {
        // Apply defaults for rogue-peer ban parameters so callers that do not
        // set these fields (e.g. unit tests) get safe, production-grade behaviour.
        if cfg.BadBlockHeightLead == 0 {
                cfg.BadBlockHeightLead = 1000
        }
        if cfg.BadBlockBanThreshold == 0 {
                cfg.BadBlockBanThreshold = 10
        }
        if cfg.BadBlockBanDuration == 0 {
                cfg.BadBlockBanDuration = 24 * time.Hour
        }
        if cfg.GetBlockStallTimeout == 0 {
                cfg.GetBlockStallTimeout = 15 * time.Second
        }
        if cfg.KeepaliveInterval == 0 {
                cfg.KeepaliveInterval = 10 * time.Second
        }
        if cfg.MaxStaleBootnodeAge == 0 {
                cfg.MaxStaleBootnodeAge = 24 * time.Hour
        }
        if cfg.TxRateBurst > 0 && cfg.TxRateBanDuration == 0 {
                cfg.TxRateBanDuration = time.Hour
        }
        // Note: TxRateBurst = 0 means "tx rate limiting disabled" — no default
        // is applied here so that unit tests constructing p2p.Config{} directly
        // are not throttled.  Production nodes get the defaults via
        // config.DefaultConfig() → burst 50, sustained 10, ban threshold 100.
        // Note: MaxBlockIngestPerSec = 0 means "disabled" — no default is applied
        // here so that unit tests that construct p2p.Config{} directly are not
        // throttled.  Production nodes set the default via DefaultConfig() → 50.

        // Parse PeerWhitelist entries once at construction time so acceptLoop
        // can do cheap net.IPNet.Contains checks without re-parsing strings.
        var wlNets []*net.IPNet
        var wlIPs []net.IP
        for _, entry := range cfg.PeerWhitelist {
                if _, ipNet, err := net.ParseCIDR(entry); err == nil {
                        wlNets = append(wlNets, ipNet)
                } else if ip := net.ParseIP(entry); ip != nil {
                        wlIPs = append(wlIPs, ip)
                } else {
                        log.Warn("peer_whitelist: ignoring unparseable entry", "entry", entry)
                }
        }

        // The TCP dialer carries no Timeout; the deadline is embedded in the
        // context created by dialPeer via context.WithTimeout(…, DialTimeout).
        // This keeps the total TCP+TLS deadline uniform and allows BanPeer to
        // cancel in-flight dials via a single cancel func.
        defaultDialer := &net.Dialer{}
        h := &Host{
                cfg:            cfg,
                handler:        handler,
                log:            log,
                peers:          make(map[string]*Peer),
                done:           make(chan struct{}),
                mgr:            newPeerMgrWithFile(cfg.BanFile),
                gossip:         NewGossipFilter(),
                badBlockCounts: make(map[string]badBlockStrike),
                wlNets:         wlNets,
                wlIPs:          wlIPs,
                listenFunc:     net.Listen,
                dialContextFunc: defaultDialer.DialContext,
                dialingIPs:     make(map[string]map[uint64]context.CancelFunc),
                pendingConns:   make(map[string]map[uint64]net.Conn),
                bootnodeLastResolved:   make(map[string][]string),
                bootnodeLastResolvedAt: make(map[string]time.Time),
                bootnodeSet:            make(map[string]struct{}),
                maintainNow:            make(chan struct{}, 1),
        }
        h.txRate = newTxRateLimiter(cfg.TxRateBurst, cfg.TxRateSustained, cfg.TxRateBanThreshold)
        h.keepaliveIntervalNs.Store(int64(cfg.KeepaliveInterval))
        return h
}

// GetKeepaliveInterval returns the current live keepalive Ping interval.
// Thread-safe; may be called concurrently with SetKeepaliveInterval.
func (h *Host) GetKeepaliveInterval() time.Duration {
        return time.Duration(h.keepaliveIntervalNs.Load())
}

// SetKeepaliveInterval updates the keepalive Ping interval for all active peer
// connections without restarting the node.  Existing connections pick up the
// new interval on their next keepalive tick.  d must be in [1s, 15s].
func (h *Host) SetKeepaliveInterval(d time.Duration) error {
        if d < time.Second || d > 15*time.Second {
                return fmt.Errorf("keepalive_interval must be in [1s, 15s], got %s", d)
        }
        h.keepaliveIntervalNs.Store(int64(d))
        return nil
}

// rebuildBootnodeSet repopulates h.bootnodeSet from h.bootnodeLastResolved.
// Must be called with h.mu held (write lock).
func (h *Host) rebuildBootnodeSet() {
        newSet := make(map[string]struct{})
        for _, addrs := range h.bootnodeLastResolved {
                for _, addr := range addrs {
                        newSet[addr] = struct{}{}
                }
        }
        h.bootnodeSet = newSet
}

// applyBootnodeResolution records the resolved addresses for a single raw
// bootnode string and rebuilds bootnodeSet so retired addresses are removed
// immediately.  Also stamps bootnodeLastResolvedAt with the current time so
// the stale-bootnode WARN in maintainLoop can calculate the age accurately.
// Must be called with h.mu held (write lock).
func (h *Host) applyBootnodeResolution(raw string, resolved []string) {
        h.bootnodeLastResolved[raw] = resolved
        h.bootnodeLastResolvedAt[raw] = time.Now()
        h.rebuildBootnodeSet()
}

// SetHeaderProvider attaches a header provider used to serve GetHeaders requests.
// Call this before Start() when the host is embedded in a full node.
func (h *Host) SetHeaderProvider(hp HeaderProvider) {
        h.headers = hp
}

// SetBlockFetcher registers LevelDB-backed fallback functions used when a
// requested block or header is not in the in-memory ring buffer.  Both
// functions may return nil when the block is genuinely absent.
//
// byHash is used in handleGetBlock; byHeight is used in handleGetHeaders to
// serve sync headers for blocks that have been evicted from the ring (i.e.
// when the syncing peer is more than ringSize blocks behind the local tip).
func (h *Host) SetBlockFetcher(
        byHash func(crypto.Hash32) *core.Block,
        byHeight func(uint64) *core.Block,
) {
        h.blockByHash = byHash
        h.blockByHeight = byHeight
}

// GetBanEvents returns all peer-ban events that occurred at or after since.
// Thread-safe; safe to call concurrently with block processing.
// Returns an empty (non-nil) slice when no events match.
func (h *Host) GetBanEvents(since time.Time) []BanEvent {
        h.banEventMu.Lock()
        defer h.banEventMu.Unlock()
        out := make([]BanEvent, 0)
        for _, e := range h.banEvents {
                if !e.At.Before(since) {
                        out = append(out, e)
                }
        }
        return out
}

// GetStallEvents returns all block-fetch stall events that occurred at or after
// since.  Thread-safe; safe to call concurrently with the keepalive goroutine.
// Returns an empty (non-nil) slice when no events match.
func (h *Host) GetStallEvents(since time.Time) []StallEvent {
        h.stallEventMu.Lock()
        defer h.stallEventMu.Unlock()
        out := make([]StallEvent, 0)
        for _, e := range h.stallEvents {
                if !e.At.Before(since) {
                        out = append(out, e)
                }
        }
        return out
}

// GetWhitelistExemptions returns all whitelist-exemption events that occurred at or
// after since.  Thread-safe; safe to call concurrently with block processing.
// Returns an empty (non-nil) slice when no events match.
func (h *Host) GetWhitelistExemptions(since time.Time) []WhitelistExemptionEvent {
        h.wlExemptMu.Lock()
        defer h.wlExemptMu.Unlock()
        out := make([]WhitelistExemptionEvent, 0)
        for _, e := range h.wlExemptEvents {
                if !e.At.Before(since) {
                        out = append(out, e)
                }
        }
        return out
}

// GetPeerWhitelist returns a snapshot of the current peer IP whitelist entries.
// Thread-safe; safe to call concurrently with any mutation method.
func (h *Host) GetPeerWhitelist() []string {
        h.wlMu.RLock()
        defer h.wlMu.RUnlock()
        cp := make([]string, len(h.cfg.PeerWhitelist))
        copy(cp, h.cfg.PeerWhitelist)
        return cp
}

// applyWhitelistLocked parses entries, persists to the sidecar file first,
// and only swaps the in-memory state if persistence succeeded.
//
// Write-first design: the sidecar is the source of truth on restart; if we
// swapped in-memory first and then failed to persist, the live whitelist and
// the durable sidecar would diverge silently — the admin would receive a
// success response for a change that is lost on restart.
//
// MUST be called while h.wlMutate is held; never call directly.
func (h *Host) applyWhitelistLocked(entries []string) error {
        var nets []*net.IPNet
        var ips []net.IP
        valid := make([]string, 0, len(entries))
        for _, entry := range entries {
                if _, ipNet, err := net.ParseCIDR(entry); err == nil {
                        nets = append(nets, ipNet)
                        valid = append(valid, entry)
                } else if ip := net.ParseIP(entry); ip != nil {
                        ips = append(ips, ip)
                        valid = append(valid, entry)
                } else {
                        h.log.Warn("applyWhitelist: ignoring unparseable entry", "entry", entry)
                }
        }

        // Persist first.  If this fails the in-memory state is not touched so
        // the live access-control list remains consistent with the on-disk copy.
        if err := h.saveWhitelistToFile(valid); err != nil {
                return err
        }

        h.wlMu.Lock()
        h.wlNets = nets
        h.wlIPs = ips
        cp := make([]string, len(valid))
        copy(cp, valid)
        h.cfg.PeerWhitelist = cp
        h.wlMu.Unlock()

        h.log.Info("p2p: peer whitelist updated", "entries", len(valid))
        return nil
}

// SetPeerWhitelist atomically replaces the peer IP whitelist with entries and
// persists the new list to cfg.WhitelistFile (if configured).  Each element
// must be either a bare IP address ("1.2.3.4") or a CIDR range ("10.0.0.0/8");
// invalid entries are logged and silently skipped.
// The change takes effect immediately for all new inbound connections.
// Returns an error if the sidecar file cannot be written; in that case the
// in-memory whitelist is not modified.
func (h *Host) SetPeerWhitelist(entries []string) error {
        h.wlMutate.Lock()
        defer h.wlMutate.Unlock()
        return h.applyWhitelistLocked(entries)
}

// AddToWhitelist adds a single IP or CIDR to the peer whitelist.
// Returns an error if the entry is not a valid IP or CIDR, or if the sidecar
// file cannot be written (in which case the in-memory list is not modified).
// No-op (returns nil) when the entry is already present.
// The entire read-snapshot → append → apply → persist sequence is serialised
// under wlMutate, preventing lost-update races with concurrent calls.
func (h *Host) AddToWhitelist(entry string) error {
        // Validate before acquiring the lock to fail fast without contention.
        if _, _, err := net.ParseCIDR(entry); err != nil {
                if net.ParseIP(entry) == nil {
                        return fmt.Errorf("invalid IP or CIDR: %q", entry)
                }
        }

        h.wlMutate.Lock()
        defer h.wlMutate.Unlock()

        h.wlMu.RLock()
        current := make([]string, len(h.cfg.PeerWhitelist))
        copy(current, h.cfg.PeerWhitelist)
        h.wlMu.RUnlock()

        for _, e := range current {
                if e == entry {
                        return nil // already present; idempotent
                }
        }
        return h.applyWhitelistLocked(append(current, entry))
}

// RemoveFromWhitelist removes a single IP or CIDR from the peer whitelist.
// Returns (true, nil) when the entry was found and removed.
// Returns (false, nil) when the entry was not found in the list.
// Returns (false, err) when the entry was found but the sidecar file could
// not be written; in that case the in-memory list is not modified.
// The entire read-snapshot → filter → apply → persist sequence is serialised
// under wlMutate, preventing lost-update races with concurrent calls.
func (h *Host) RemoveFromWhitelist(entry string) (bool, error) {
        h.wlMutate.Lock()
        defer h.wlMutate.Unlock()

        h.wlMu.RLock()
        current := make([]string, len(h.cfg.PeerWhitelist))
        copy(current, h.cfg.PeerWhitelist)
        h.wlMu.RUnlock()

        updated := current[:0:0]
        found := false
        for _, e := range current {
                if e == entry {
                        found = true
                        continue
                }
                updated = append(updated, e)
        }
        if !found {
                return false, nil
        }
        if updated == nil {
                updated = []string{}
        }
        if err := h.applyWhitelistLocked(updated); err != nil {
                return false, err
        }
        return true, nil
}

// loadWhitelistFromFile establishes the authoritative whitelist on startup.
// It returns an error when the sidecar file exists but cannot be read or
// decoded — the caller (Start) must abort rather than continue, because
// continuing with a degraded or empty whitelist would allow inbound peers
// that should be blocked (fail-open access control).
//
// Sidecar-as-authoritative semantics:
//   - Sidecar exists and is valid → use its entries exclusively; ignore
//     cfg.PeerWhitelist.  Admin-made removals of node.yaml entries therefore
//     survive restarts: once the sidecar exists it is the sole source of truth.
//   - Sidecar exists but cannot be read or parsed → return a fatal error so
//     Start() aborts.  The operator must repair or remove the file.
//   - Sidecar does not exist and cfg.PeerWhitelist non-empty → seed the sidecar
//     from cfg so future restarts are consistent; return nil.
//   - Neither → open network; no-op; return nil.
//
// Called once inside Start() before the listener opens; no lock required.
func (h *Host) loadWhitelistFromFile() error {
        if h.cfg.WhitelistFile == "" || h.cfg.WhitelistFile == "-" {
                return nil
        }

        data, err := os.ReadFile(h.cfg.WhitelistFile)
        switch {
        case err == nil:
                // Sidecar exists — it is authoritative; ignore cfg.PeerWhitelist.
                var fileEntries []string
                if jsonErr := json.Unmarshal(data, &fileEntries); jsonErr != nil {
                        return fmt.Errorf("p2p: whitelist sidecar %q is corrupt (JSON parse error): %w — "+
                                "repair or remove the file to restart the node", h.cfg.WhitelistFile, jsonErr)
                }
                // json.Unmarshal sets fileEntries to nil for a JSON null value.
                // A null sidecar is not a valid empty list: it indicates a truncated or
                // tampered file.  Fail-closed rather than opening inbound access.
                if fileEntries == nil {
                        return fmt.Errorf("p2p: whitelist sidecar %q contains JSON null (not a valid "+
                                "entry array) — repair or remove the file to restart the node",
                                h.cfg.WhitelistFile)
                }
                var nets []*net.IPNet
                var ips []net.IP
                valid := make([]string, 0, len(fileEntries))
                for _, entry := range fileEntries {
                        if _, ipNet, parseErr := net.ParseCIDR(entry); parseErr == nil {
                                nets = append(nets, ipNet)
                                valid = append(valid, entry)
                        } else if ip := net.ParseIP(entry); ip != nil {
                                ips = append(ips, ip)
                                valid = append(valid, entry)
                        } else {
                                // A corrupt/tampered sidecar with invalid entries must not
                                // be silently ignored: if all entries are invalid, the node
                                // would run as an open network.  Fail-closed instead.
                                return fmt.Errorf("p2p: whitelist sidecar %q contains an invalid "+
                                        "IP/CIDR entry %q — repair or remove the file to restart the node",
                                        h.cfg.WhitelistFile, entry)
                        }
                }
                // When the sidecar is empty but node.yaml still has peer_whitelist
                // entries, retain the static config entries rather than transitioning
                // to an open (unbounded) network.  This prevents an admin "clear-all"
                // in the Admin Panel from accidentally removing ban-exemptions for
                // trusted relay nodes defined in node.yaml.
                // To fully disable the whitelist the operator must also clear
                // peer_whitelist in node.yaml and restart.
                if len(valid) == 0 && len(h.cfg.PeerWhitelist) > 0 {
                        h.log.Info("p2p: whitelist sidecar is empty — retaining node.yaml peer_whitelist",
                                "cfg_entries", len(h.cfg.PeerWhitelist),
                                "file", h.cfg.WhitelistFile)
                        // h.wlNets / h.wlIPs / h.cfg.PeerWhitelist were set by the
                        // constructor from node.yaml; leave them intact.
                        // The sidecar will be re-seeded on the next AddToWhitelist call.
                        return nil
                }
                // Overwrite the in-memory state entirely (no merge with cfg).
                h.wlNets = nets
                h.wlIPs = ips
                h.cfg.PeerWhitelist = valid
                h.log.Info("p2p: peer whitelist loaded from sidecar (authoritative)",
                        "entries", len(valid), "file", h.cfg.WhitelistFile)

        case os.IsNotExist(err):
                // First boot — seed the sidecar from cfg so future restarts are
                // consistent and admin removals of cfg entries persist correctly.
                if len(h.cfg.PeerWhitelist) == 0 {
                        return nil // open network; nothing to seed
                }
                if seedErr := h.saveWhitelistToFile(h.cfg.PeerWhitelist); seedErr != nil {
                        return fmt.Errorf("p2p: whitelist sidecar %q could not be created on first boot: %w — "+
                                "check directory permissions or set whitelist_file to a writable path",
                                h.cfg.WhitelistFile, seedErr)
                }
                h.log.Info("p2p: seeded whitelist sidecar from node.yaml",
                        "entries", len(h.cfg.PeerWhitelist), "file", h.cfg.WhitelistFile)

        default:
                // The file exists (or the path is unreadable for another reason).
                // Fail-closed: return an error so Start() aborts rather than
                // allowing inbound connections that the sidecar was meant to block.
                return fmt.Errorf("p2p: whitelist sidecar %q cannot be read: %w — "+
                        "check permissions or remove the file to restart the node", h.cfg.WhitelistFile, err)
        }
        return nil
}

// saveWhitelistToFile atomically writes entries to cfg.WhitelistFile so the
// list survives a node restart.  Returns nil on success.
// Returns an error when the file cannot be created or written; the caller
// (applyWhitelistLocked) must treat any non-nil error as fatal and must NOT
// update the in-memory whitelist, keeping live state consistent with the disk.
// A no-op (returns nil) when WhitelistFile is empty or "-".
func (h *Host) saveWhitelistToFile(entries []string) error {
        if h.cfg.WhitelistFile == "" || h.cfg.WhitelistFile == "-" {
                return nil
        }
        h.wlPersistMu.Lock()
        defer h.wlPersistMu.Unlock()

        data, err := json.MarshalIndent(entries, "", "  ")
        if err != nil {
                return fmt.Errorf("p2p: whitelist persist: marshal failed: %w", err)
        }
        dir := filepath.Dir(h.cfg.WhitelistFile)
        tmp, err := os.CreateTemp(dir, ".p2p_whitelist_*.tmp")
        if err != nil {
                return fmt.Errorf("p2p: whitelist persist: create temp in %q failed: %w", dir, err)
        }
        tmpName := tmp.Name()
        if _, err := tmp.Write(data); err != nil {
                tmp.Close()
                os.Remove(tmpName)
                return fmt.Errorf("p2p: whitelist persist: write to %q failed: %w", tmpName, err)
        }
        if err := tmp.Close(); err != nil {
                os.Remove(tmpName)
                return fmt.Errorf("p2p: whitelist persist: close %q failed: %w", tmpName, err)
        }
        if err := os.Rename(tmpName, h.cfg.WhitelistFile); err != nil {
                os.Remove(tmpName)
                return fmt.Errorf("p2p: whitelist persist: rename %q→%q failed: %w", tmpName, h.cfg.WhitelistFile, err)
        }
        h.log.Debug("p2p: whitelist persisted", "entries", len(entries), "file", h.cfg.WhitelistFile)
        return nil
}

// DropPeer closes the active connection to addr without banning it.  The peer
// may reconnect immediately; this is intended for tests that need to simulate
// a transient network drop without permanently blacklisting the address.
// Returns true when a connection was found and closed; false when addr is not
// currently connected.
func (h *Host) DropPeer(addr string) bool {
	h.mu.Lock()
	p, ok := h.peers[addr]
	if ok {
		p.conn.Close()
		delete(h.peers, addr)
	}
	h.mu.Unlock()
	if ok {
		h.log.Info("p2p: peer connection dropped (test hook)", "addr", addr)
	}
	return ok
}

// cancelInFlightDials cancels every outbound dial currently in progress to ip
// (canonical bare IP) and closes every connection that completed dialContextFunc
// but has not yet been handed to handleConn.  Must be called while dialGateMu
// is held.
//
// Closing dialingIPs cancels the context so dialContextFunc returns immediately
// (preventing new TCP connections for in-flight dials).  Closing pendingConns
// covers the narrower window where dialContextFunc has already returned a live
// conn but the conn has not yet been passed to handleConn: closing it here
// ensures handleConn is never called for a banned peer even if the TCP
// handshake completed before the ban was committed.
func (h *Host) cancelInFlightDials(ip string) {
        for _, cancel := range h.dialingIPs[ip] {
                cancel()
        }
        delete(h.dialingIPs, ip)
        for _, conn := range h.pendingConns[ip] {
                conn.Close()
        }
        delete(h.pendingConns, ip)
}

// BanPeer bans the peer at addr for duration d.  The connection (if any) is
// closed immediately and future dial/accept attempts from that address are
// rejected.
//
// Ban commitment and dial initiation are serialized under dialGateMu so that
// no new TCP connection to the banned IP can start after this function
// returns:
//   - If a dialPeer goroutine has not yet entered the gate → it will see the
//     ban inside the gate and abort before calling DialContext.
//   - If a dialPeer goroutine is already inside the gate (IsBanned passed,
//     cancel registered, DialContext not yet called) → BanPeer cancels its
//     context so DialContext returns immediately with context.Canceled.
//
// All currently established connections from the same IP are also closed.
func (h *Host) BanPeer(addr, reason string, d time.Duration) {
        bannedIP := connIP(addr)
        h.dialGateMu.Lock()
        h.mgr.Ban(addr, reason, d)
        h.cancelInFlightDials(bannedIP)
        h.dialGateMu.Unlock()

        h.mu.Lock()
        for a, p := range h.peers {
                if connIP(a) == bannedIP {
                        p.conn.Close()
                        delete(h.peers, a)
                }
        }
        h.mu.Unlock()
        h.log.Info("peer banned", "addr", addr, "ip", bannedIP, "reason", reason, "duration", d)
}

// banTxFlooder temporarily bans an IP that kept flooding transactions past
// TxRateBanThreshold.  Mirrors the wrong-fork ban path: the ban is keyed by
// bare IP (reconnects on a new source port stay blocked), all established
// connections from the IP are closed, and a BanEvent is recorded for the
// Admin Panel notification log.  Whitelisted IPs (trusted validators/relays)
// are never banned — their excess transactions are still dropped by the
// throttle, but a legitimate relay bursting during sync must not lose
// connectivity.
func (h *Host) banTxFlooder(peerIP string, peer *Peer, violations int) {
        h.wlMu.RLock()
        wlNets := h.wlNets
        wlIPs := h.wlIPs
        h.wlMu.RUnlock()
        if len(wlNets) > 0 || len(wlIPs) > 0 {
                if remoteIP := net.ParseIP(peerIP); remoteIP != nil && ipInWhitelist(remoteIP, wlNets, wlIPs) {
                        h.log.Warn("tx flood from whitelisted peer — throttled but not banned",
                                "peer", peer.addr, "ip", peerIP, "violations", violations)
                        return
                }
        }

        const reason = "sustained transaction flood (tx rate limit)"
        banDuration := h.cfg.TxRateBanDuration
        // Commit the ban and cancel any in-flight dials to this IP under
        // dialGateMu so no new TCP connection can start after the ban is written.
        h.dialGateMu.Lock()
        h.mgr.Ban(peerIP, reason, banDuration)
        h.cancelInFlightDials(peerIP)
        h.dialGateMu.Unlock()
        // Close ALL currently established connections from the same IP.
        h.mu.Lock()
        for addr, p := range h.peers {
                if connIP(addr) == peerIP {
                        p.conn.Close()
                        delete(h.peers, addr)
                }
        }
        h.mu.Unlock()
        // Forget the rate-limit state so a post-ban reconnect starts fresh.
        h.txRate.forget(peerIP)
        h.log.Info("peer IP banned for transaction flood",
                "ip", peerIP, "addr", peer.addr, "violations", violations, "duration", banDuration)
        // Record the ban event so the API server can poll and notify admins.
        h.banEventMu.Lock()
        h.banEvents = append(h.banEvents, BanEvent{
                IP:              peerIP,
                PeerAddr:        peer.addr,
                PeerID:          peer.id,
                Reason:          reason,
                Violations:      violations,
                BanDurationSecs: int64(banDuration.Seconds()),
                At:              time.Now(),
        })
        if len(h.banEvents) > banEventMaxEvents {
                h.banEvents = h.banEvents[len(h.banEvents)-banEventMaxEvents:]
        }
        h.banEventMu.Unlock()
}

// Start binds the listener and begins accepting connections.
func (h *Host) Start() error {
        // Restore persisted bans from disk before accepting any connections so
        // previously-banned peers are blocked immediately on restart — without
        // waiting for them to accumulate 10 strikes again.
        // A corrupt or unreadable ban file is a fatal error: continuing with an
        // empty ban list would allow previously-banned IPs to reconnect without
        // serving any additional strikes.  The operator must repair or remove
        // the file to restart the node.
        if err := h.mgr.LoadBansFromFile(); err != nil {
                return err
        }

        // Load the sidecar whitelist file (if configured).  loadWhitelistFromFile
        // returns a fatal error when the file exists but is unreadable or corrupt;
        // abort startup in that case rather than running fail-open (all IPs would
        // be accepted, defeating the whitelist's access-control purpose).
        if err := h.loadWhitelistFromFile(); err != nil {
                return err
        }
        if n := h.mgr.BannedCount(); n > 0 {
                h.log.Info("p2p: restored bans from file", "count", n, "file", h.cfg.BanFile)
        }

        // Ordering invariant: h.listenFunc (net.Listen in production) is called
        // HERE — after loadWhitelistFromFile above.  Tests override listenFunc
        // with a factory that captures GetPeerWhitelist() at bind time; if a
        // future refactor moves this call above loadWhitelistFromFile, the test
        // factory executes before the whitelist is populated and fails immediately.
        ln, err := h.listenFunc("tcp", h.cfg.ListenAddr)
        if err != nil {
                return fmt.Errorf("listen %s: %w", h.cfg.ListenAddr, err)
        }
        // Wrap the TCP listener with TLS when a TLS config is provided so that
        // all accepted connections are automatically upgraded to encrypted,
        // mutually authenticated transport.
        if h.cfg.TLSConfig != nil {
                h.listener = tls.NewListener(ln, h.cfg.TLSConfig)
        } else {
                h.listener = ln
        }
        h.log.Info("p2p listening", "addr", h.cfg.ListenAddr, "tls", h.cfg.TLSConfig != nil)
        // Log the node's own fingerprint at INFO so operators can immediately
        // verify identity consistency across nodes without grepping main.go logs.
        // The fingerprint is set by main.go from LoadOrSaveP2PIdentity; it is
        // empty when TLS is disabled (unit-test mode) — skip the log in that case.
        if h.cfg.SelfFingerprint != "" {
                h.log.Info("p2p node identity fingerprint", "fingerprint", h.cfg.SelfFingerprint)
        }

        go h.acceptLoop()
        go h.maintainLoop()

        // Seed bootnodeLastResolvedAt to now so every configured bootnode
        // starts with a full MaxStaleBootnodeAge grace period.  Without this
        // seed, a bootnode whose first resolution attempt fails would have a
        // zero timestamp and immediately trigger stale warnings on the first
        // maintainLoop tick.
        h.mu.Lock()
        seedTime := time.Now()
        for _, addr := range h.cfg.Bootnodes {
                if _, exists := h.bootnodeLastResolvedAt[addr]; !exists {
                        h.bootnodeLastResolvedAt[addr] = seedTime
                }
        }
        h.mu.Unlock()

        // Dial bootnodes — resolve DNS hostnames before dialling so the
        // canonical peer key in h.peers is always an IP:port string.
        // applyBootnodeResolution records each successful resolution in
        // bootnodeLastResolved and rebuilds bootnodeSet so dialPeer can
        // skip back-off before maintainLoop fires its first tick.
        for _, addr := range h.cfg.Bootnodes {
                go func(a string) {
                        resolved, err := resolveBootnode(a)
                        if err != nil {
                                h.log.Warn("bootnode dns resolve failed", "bootnode", a, "err", err)
                                return
                        }
                        h.mu.Lock()
                        h.applyBootnodeResolution(a, resolved)
                        h.mu.Unlock()
                        for _, r := range resolved {
                                h.dialPeer(r)
                        }
                }(addr)
        }
        return nil
}

// Stop shuts down the host gracefully.
func (h *Host) Stop() {
        close(h.done)
        if h.listener != nil {
                h.listener.Close()
        }
        h.mu.Lock()
        defer h.mu.Unlock()
        for _, p := range h.peers {
                p.conn.Close()
        }
}

// BroadcastBlock sends a block to all connected peers.
func (h *Host) BroadcastBlock(block *core.Block) {
        h.mu.RLock()
        peers := make([]*Peer, 0, len(h.peers))
        for _, p := range h.peers {
                peers = append(peers, p)
        }
        h.mu.RUnlock()

        // Serialize block (simplified — in production use binary encoding)
        sb := blockToMsg(block)
        for _, p := range peers {
                if err := p.Send(MsgBlock, sb); err != nil {
                        h.log.Warn("broadcast block failed", "peer", p.addr, "err", err)
                }
        }
}

// BroadcastTx sends a transaction to all connected peers.
func (h *Host) BroadcastTx(tx *core.Transaction) {
        h.mu.RLock()
        peers := make([]*Peer, 0, len(h.peers))
        for _, p := range h.peers {
                peers = append(peers, p)
        }
        h.mu.RUnlock()

        for _, p := range peers {
                if err := p.Send(MsgTx, tx); err != nil {
                        h.log.Warn("broadcast tx failed", "peer", p.addr, "err", err)
                }
        }
}

// BroadcastVote sends a finalization vote to all peers.
func (h *Host) BroadcastVote(vote VoteMsg) {
        h.mu.RLock()
        peers := make([]*Peer, 0, len(h.peers))
        for _, p := range h.peers {
                peers = append(peers, p)
        }
        h.mu.RUnlock()

        for _, p := range peers {
                if err := p.Send(MsgVote, vote); err != nil {
                        h.log.Warn("broadcast vote failed", "peer", p.addr, "err", err)
                }
        }
}

// PeerCount returns the number of connected peers.
func (h *Host) PeerCount() int {
        h.mu.RLock()
        defer h.mu.RUnlock()
        return len(h.peers)
}

// PendingHandshakes returns the number of inbound TCP connections that are
// currently in the TLS handshake phase.  Operators can watch this counter
// to detect a TLS-handshake flood (see Task #504).
func (h *Host) PendingHandshakes() int64 {
        return h.pendingHandshakes.Load()
}

// ListBans returns a snapshot of all currently active P2P bans.
func (h *Host) ListBans() []BanInfo {
        return h.mgr.ListBans()
}

// LiftBan removes the P2P ban for addr. Returns true if the ban existed.
func (h *Host) LiftBan(addr string) bool {
        return h.mgr.LiftBan(addr)
}

// PeerHeight returns the last-reported chain height for the peer at addr and
// whether the peer is currently connected.  The height is updated by the
// MsgPong dispatch handler on every keepalive reply, so callers can use this
// to verify that the keepalive Ping/Pong cycle propagates validator tip
// advances to the relay's peer table.
func (h *Host) PeerHeight(addr string) (uint64, bool) {
        h.mu.RLock()
        defer h.mu.RUnlock()
        p, ok := h.peers[addr]
        if !ok {
                return 0, false
        }
        return p.height, true
}

// ListenAddr returns the actual bound address (useful when ListenAddr was ":0").
func (h *Host) ListenAddr() string {
        if h.listener == nil {
                return ""
        }
        return h.listener.Addr().String()
}

// ─── Internal loops ───────────────────────────────────────────────────────────

func (h *Host) acceptLoop() {
        for {
                conn, err := h.listener.Accept()
                if err != nil {
                        select {
                        case <-h.done:
                                return
                        default:
                                h.log.Warn("accept error", "err", err)
                                continue
                        }
                }

                // IP whitelist: when peer_whitelist is non-empty, reject any
                // inbound connection whose source IP is not covered by the list.
                // This is the earliest possible rejection point — before TLS
                // handshake, MaxPeers check, or per-IP limit — so unknown nodes
                // cause zero resource usage beyond one Accept() call.
                // Outbound dials are never subject to this check.
                //
                // wlMu is taken for a snapshot read so that SetPeerWhitelist
                // can swap the list atomically without blocking Accept().
                h.wlMu.RLock()
                wlNets := h.wlNets
                wlIPs := h.wlIPs
                h.wlMu.RUnlock()
                if len(wlNets) > 0 || len(wlIPs) > 0 {
                        remoteIP := net.ParseIP(connIP(conn.RemoteAddr().String()))
                        if remoteIP == nil || !ipInWhitelist(remoteIP, wlNets, wlIPs) {
                                h.log.Info("inbound connection rejected: IP not in peer_whitelist",
                                        "addr", conn.RemoteAddr().String())
                                conn.Close()
                                continue
                        }
                }

                // Eclipse-attack mitigation (3.5.1): reject inbound connections
                // when the peer table is already full.
                if h.cfg.MaxPeers > 0 {
                        h.mu.RLock()
                        total := len(h.peers)
                        outCount := 0
                        if h.cfg.MinOutbound > 0 {
                                for _, p := range h.peers {
                                        if p.outbound {
                                                outCount++
                                        }
                                }
                        }
                        h.mu.RUnlock()

                        // Hard cap on total peers.
                        if total >= h.cfg.MaxPeers {
                                h.log.Debug("inbound connection rejected: MaxPeers reached",
                                        "addr", conn.RemoteAddr().String(),
                                        "max", h.cfg.MaxPeers)
                                conn.Close()
                                continue
                        }
                        // MinOutbound: reserve slots exclusively for outbound dial-outs
                        // so a validator under inbound flood can still broadcast blocks.
                        // Inbound connections are capped at (MaxPeers − MinOutbound);
                        // outbound dials are not subject to this cap.
                        if h.cfg.MinOutbound > 0 {
                                inboundCap := h.cfg.MaxPeers - h.cfg.MinOutbound
                                inboundCount := total - outCount
                                if inboundCount >= inboundCap {
                                        h.log.Debug("inbound connection rejected: MinOutbound slots reserved",
                                                "addr", conn.RemoteAddr().String(),
                                                "inbound", inboundCount,
                                                "cap", inboundCap,
                                                "min_outbound", h.cfg.MinOutbound)
                                        conn.Close()
                                        continue
                                }
                        }
                }

                // Per-IP limit: prevents one IP from consuming all peer slots
                // (eclipse / peer-slot-exhaustion attack, task #415).
                if h.cfg.MaxPeersPerIP > 0 {
                        remoteIP := connIP(conn.RemoteAddr().String())
                        h.mu.RLock()
                        ipCount := 0
                        for peerAddr := range h.peers {
                                if connIP(peerAddr) == remoteIP {
                                        ipCount++
                                }
                        }
                        h.mu.RUnlock()
                        if ipCount >= h.cfg.MaxPeersPerIP {
                                h.log.Debug("inbound connection rejected: MaxPeersPerIP reached",
                                        "addr", conn.RemoteAddr().String(),
                                        "ip", remoteIP,
                                        "max", h.cfg.MaxPeersPerIP)
                                conn.Close()
                                continue
                        }
                }

                // Handshake-goroutine semaphore: an attacker that opens many
                // TCP connections but never completes the TLS handshake would
                // hold one goroutine per connection for up to 10 s.
                // MaxPendingHandshakes caps the total in-flight handshakes so
                // the node cannot be goroutine-starved by a connect-flood.
                if h.cfg.MaxPendingHandshakes > 0 && h.cfg.TLSConfig != nil {
                        cur := h.pendingHandshakes.Add(1)
                        if cur > int64(h.cfg.MaxPendingHandshakes) {
                                h.pendingHandshakes.Add(-1)
                                h.log.Info("MaxPendingHandshakes reached — inbound connection rejected",
                                        "addr", conn.RemoteAddr().String(),
                                        "limit", h.cfg.MaxPendingHandshakes)
                                conn.Close()
                                continue
                        }
                }

                go h.handleConn(conn, false)
        }
}

// maintainLoop periodically dials new peers if below MinPeers and prunes ban entries.
func (h *Host) maintainLoop() {
        ticker := time.NewTicker(10 * time.Second)
        defer ticker.Stop()
        for {
                // Wait for the next scheduled tick, an explicit test-hook trigger,
                // or a shutdown signal.  Both tick sources (ticker.C and maintainNow)
                // fall through to the same maintain logic below; only done causes an
                // early return.  Go select cases do not fall through, so the body
                // must live outside the select statement.
                select {
                case <-h.done:
                        return
                case <-h.maintainNow: // test hook: fire one tick immediately
                case <-ticker.C:
                }

                // Prune expired bans
                h.mgr.Prune()

                // Prune stale bad-block strike records so the map stays bounded
                // even when a distributed attacker registers many unique IPs.
                h.badBlockMu.Lock()
                now := time.Now()
                for ip, s := range h.badBlockCounts {
                        if now.Sub(s.lastSeen) > badBlockStrikeTTL {
                                delete(h.badBlockCounts, ip)
                        }
                }
                h.badBlockMu.Unlock()

                h.mu.RLock()
                count := len(h.peers)
                known := make([]string, len(h.peerList))
                copy(known, h.peerList)
                h.mu.RUnlock()

                if count < h.cfg.MinPeers {
                        // Re-dial known peers discovered via peer exchange.
                        // Check IsBanned here — before launching the goroutine —
                        // to close the brief window where a peer was banned by the
                        // rogue-fork path (or BanPeer) and the ban write raced with
                        // this loop's snapshot of h.peers.  dialPeer has its own
                        // IsBanned guard as a second layer, but skipping the goroutine
                        // entirely avoids unnecessary TCP dial attempts and the small
                        // scheduler-induced window where the goroutine may run before
                        // a concurrent ban write completes.
                        for _, addr := range known {
                                if h.mgr.IsBanned(addr) {
                                        continue
                                }
                                h.mu.RLock()
                                _, connected := h.peers[addr]
                                h.mu.RUnlock()
                                if !connected {
                                        go h.dialPeer(addr)
                                }
                        }
                }

                // Always retry configured bootnodes on every tick regardless
                // of the current peer count.  Bootnodes are the only anchors
                // when peerList is empty (fresh start or network partition),
                // and — crucially — dialPeer skips the exponential back-off
                // window for them so two validators that restart at the same
                // time reconnect within seconds, not minutes.
                //
                // Re-resolve DNS outside h.mu (network I/O) then update under
                // the write lock.  Each successful resolution calls
                // applyBootnodeResolution which rebuilds bootnodeSet from
                // bootnodeLastResolved, so a bootnode that moves to a new IP
                // loses its old address's privileged status immediately.
                // A DNS failure for a given bootnode leaves its last-known
                // addresses intact (temporary outage retention).
                type rawResult struct {
                        raw      string
                        resolved []string
                }
                var freshResults []rawResult
                for _, raw := range h.cfg.Bootnodes {
                        resolved, err := resolveBootnode(raw)
                        if err != nil {
                                h.log.Warn("bootnode dns resolve failed", "bootnode", raw, "err", err)
                                continue // DNS failure: preserve last-known addrs
                        }
                        freshResults = append(freshResults, rawResult{raw, resolved})
                }
                // Collect the full set of bootnode addresses to dial (including
                // retained last-known addrs for bootnodes whose DNS just failed).
                // Also snapshot the last-resolved timestamps for stale-age checks.
                var allBootnodeAddrs []string
                type staleSample struct {
                        raw     string
                        lastAt  time.Time
                }
                var staleSamples []staleSample
                h.mu.Lock()
                for _, rr := range freshResults {
                        h.applyBootnodeResolution(rr.raw, rr.resolved)
                }
                for _, addrs := range h.bootnodeLastResolved {
                        allBootnodeAddrs = append(allBootnodeAddrs, addrs...)
                }
                for _, raw := range h.cfg.Bootnodes {
                        if lastAt, ok := h.bootnodeLastResolvedAt[raw]; ok {
                                staleSamples = append(staleSamples, staleSample{raw, lastAt})
                        }
                }
                h.mu.Unlock()

                // Warn when a bootnode has not resolved successfully for longer
                // than MaxStaleBootnodeAge.  The warning fires on every discovery
                // tick while the condition persists so operators see it clearly
                // in logs without having to search for the original failure.
                tickNow := time.Now()
                for _, ss := range staleSamples {
                        age := tickNow.Sub(ss.lastAt)
                        if age > h.cfg.MaxStaleBootnodeAge {
                                h.log.Warn("bootnode stale: DNS has not resolved successfully since last attempt",
                                        "bootnode", ss.raw,
                                        "age", age.Round(time.Second),
                                        "max_stale_bootnode_age", h.cfg.MaxStaleBootnodeAge,
                                )
                        }
                }

                for _, addr := range allBootnodeAddrs {
                        // Mirror the IsBanned guard applied to the MinPeers path
                        // above: skip banned bootnodes before touching h.mu so the
                        // ban check and the connection-table check are both done
                        // outside the lock, minimising lock contention and closing
                        // the goroutine-scheduling window described in task #1650.
                        if h.mgr.IsBanned(addr) {
                                continue
                        }
                        h.mu.RLock()
                        _, connected := h.peers[addr]
                        h.mu.RUnlock()
                        if !connected {
                                go h.dialPeer(addr)
                        }
                }
        }
}

// isBootnode reports whether addr is a resolved address of a configured
// bootnode.  Callers must not hold h.mu.
func (h *Host) isBootnode(addr string) bool {
        h.mu.RLock()
        _, ok := h.bootnodeSet[addr]
        h.mu.RUnlock()
        return ok
}

// DialPeer initiates an outbound connection to addr.  The dial happens in a
// background goroutine; use PeerCount() after a short wait to confirm it
// succeeded.  Outbound connections are not subject to the inbound cap
// enforced by MinOutbound, so this succeeds even when the node is under an
// inbound flood.
func (h *Host) DialPeer(addr string) {
        go h.dialPeer(addr)
}

func (h *Host) dialPeer(addr string) {
        h.mu.RLock()
        _, already := h.peers[addr]
        count := len(h.peers)
        h.mu.RUnlock()

        if already || count >= h.cfg.MaxPeers {
                return
        }

        // ── Dial-gate: atomic ban-check + in-flight registration ─────────────
        //
        // dialGateMu is also held (write) by BanPeer when it writes a ban and
        // cancels in-flight dials.  Holding it here (read phase) ensures that
        // the sequence "IsBanned → false, register intent, dial" is atomic with
        // "write ban, cancel intents" — so no TCP connection can be initiated
        // to an IP that has just been banned.
        //
        // The mutex is released BEFORE net.DialContext so it is never held
        // across network I/O.
        // The context carries the total dial+handshake deadline (DialTimeout)
        // so that both the TCP connect and the TLS handshake are bounded even
        // when the remote peer accepts TCP but stalls the TLS negotiation.
        // BanPeer cancels the same context (via the cancel func registered in
        // dialingIPs) so in-flight dials abort immediately when the peer is banned.
        ctx, cancel := context.WithTimeout(context.Background(), DialTimeout)
        dialID := h.nextDialID.Add(1)
        canonIP := connIP(addr)

        h.dialGateMu.Lock()
        // Configured bootnodes skip the exponential back-off window so a
        // simultaneous restart of both validators does not leave the network
        // isolated for up to 5 minutes.  The ban list still applies — a
        // bootnode that is actively banned cannot be re-dialled.
        if h.isBootnode(addr) {
                if h.mgr.IsBanned(addr) {
                        h.dialGateMu.Unlock()
                        cancel()
                        h.log.Debug("dialPeer: bootnode is banned", "addr", addr)
                        return
                }
        } else if !h.mgr.CanDial(addr) {
                h.dialGateMu.Unlock()
                cancel()
                h.log.Debug("dialPeer: addr is banned or in back-off window", "addr", addr)
                return
        }
        // Register the cancel func so BanPeer can abort this dial.
        if h.dialingIPs[canonIP] == nil {
                h.dialingIPs[canonIP] = make(map[uint64]context.CancelFunc)
        }
        h.dialingIPs[canonIP][dialID] = cancel
        h.dialGateMu.Unlock()

        // Remove the registration when the dial concludes (success or failure).
        defer func() {
                h.dialGateMu.Lock()
                delete(h.dialingIPs[canonIP], dialID)
                if len(h.dialingIPs[canonIP]) == 0 {
                        delete(h.dialingIPs, canonIP)
                }
                h.dialGateMu.Unlock()
                cancel()
        }()

        h.log.Debug("dialing peer", "addr", addr)
        // Load the dial function under the read lock so that a concurrent
        // SetDialFunc (used by tests) cannot race with this read.
        h.dialFnMu.RLock()
        dialFn := h.dialContextFunc
        h.dialFnMu.RUnlock()

        // Establish the TCP connection.  If BanPeer is called while DialContext
        // is in progress, it invokes the registered cancel func, causing
        // DialContext to return immediately with context.Canceled — so no TCP
        // socket reaches the remote listener.
        tcpConn, err := dialFn(ctx, "tcp", addr)
        if err != nil {
                if ctx.Err() != nil {
                        h.log.Debug("dial aborted (peer banned during dial)", "addr", addr)
                } else {
                        h.log.Warn("dial failed", "addr", addr, "err", err)
                        h.mgr.OnDialFail(addr)
                }
                return
        }

        var conn net.Conn
        if h.cfg.TLSConfig != nil {
                // Outbound TLS handshake: wrap the TCP conn and negotiate TLS.
                // HandshakeContext honours the cancel so a ban during the TLS
                // phase also aborts cleanly.
                tlsConn := tls.Client(tcpConn, h.cfg.TLSConfig)
                if err := tlsConn.HandshakeContext(ctx); err != nil {
                        tcpConn.Close()
                        if ctx.Err() != nil {
                                h.log.Debug("tls handshake aborted (peer banned during dial)", "addr", addr)
                        } else {
                                h.log.Warn("tls handshake failed", "addr", addr, "err", err)
                                h.mgr.OnDialFail(addr)
                        }
                        return
                }
                conn = tlsConn
        } else {
                conn = tcpConn
        }

        // ── Post-connect gate: prevent handoff to handleConn if banned ────────
        //
        // dialContextFunc may return a live conn even when the context was
        // cancelled (e.g. if the TCP connect completed in the same scheduler
        // tick as BanPeer's cancel).  Register the conn as pending under
        // dialGateMu: this is atomic with BanPeer's ban-write, so exactly one
        // of the following is guaranteed:
        //
        //   A) IsBanned is true inside our gate ⟹ we close conn here.
        //   B) BanPeer runs after we register ⟹ it closes conn via pendingConns.
        //   C) BanPeer has already run and returned ⟹ IsBanned is true (case A).
        //
        // In all cases handleConn is either called with an already-closed conn
        // (case B, fails at first read/write and is rejected by its own IsBanned
        // guard) or is never called at all (cases A and C).
        pendingID := h.nextDialID.Add(1)
        h.dialGateMu.Lock()
        if h.mgr.IsBanned(addr) {
                h.dialGateMu.Unlock()
                conn.Close()
                h.log.Debug("dropping connection to just-banned peer", "addr", addr)
                return
        }
        if h.pendingConns[canonIP] == nil {
                h.pendingConns[canonIP] = make(map[uint64]net.Conn)
        }
        h.pendingConns[canonIP][pendingID] = conn
        h.dialGateMu.Unlock()

        // postConnectHook, when non-nil (tests only), fires here so the test
        // can call BanPeer while conn is registered as pending and verify it
        // is closed by cancelInFlightDials rather than reaching handleConn.
        if h.postConnectHook != nil {
                h.postConnectHook()
        }

        go h.handleConn(conn, true)

        // Deregister the pending conn — handleConn is now the owner.
        // If BanPeer already ran and closed the conn, the entry was already
        // removed from pendingConns by cancelInFlightDials; the delete is a no-op.
        h.dialGateMu.Lock()
        if m := h.pendingConns[canonIP]; m != nil {
                delete(m, pendingID)
                if len(m) == 0 {
                        delete(h.pendingConns, canonIP)
                }
        }
        h.dialGateMu.Unlock()
}

func (h *Host) handleConn(conn net.Conn, outbound bool) {
        addr := conn.RemoteAddr().String()

        // connectedAt is set (to non-zero) only once the peer reaches the
        // message loop (i.e. both the TCP dial and the P2P handshake succeeded).
        // The back-off defer below uses it to decide the outcome on every exit
        // path — including early returns from a failed handshake.
        var connectedAt time.Time

        // peer is declared early so that the panic-recovery defer (registered
        // below, before handleConn creates the Peer object) can reference it.
        // It is nil until the Peer is created after the TLS handshake succeeds.
        // The identity-safe cleanup in both defers checks for nil before use.
        var peer *Peer

        // Safety net: catch panics from malformed peer messages so a single
        // misbehaving peer cannot crash the node process.
        defer func() {
                if r := recover(); r != nil {
                        h.log.Error("panic in P2P handleConn — peer dropped, node is safe",
                                "peer", addr,
                                "panic", fmt.Sprintf("%v", r),
                                "stack", string(debug.Stack()))
                        conn.Close()
                        h.mu.Lock()
                        // Identity-safe: do not evict a replacement peer registered
                        // at the same address after a DropPeer / reconnect race.
                        // peer is nil when the panic occurred before Peer creation
                        // (e.g. during TLS handshake); in that case no entry was
                        // ever registered so there is nothing to delete.
                        if peer != nil {
                                if current, ok := h.peers[addr]; ok && current == peer {
                                        delete(h.peers, addr)
                                }
                        }
                        h.mu.Unlock()
                }
        }()

        // Back-off update for outbound connections — fires on EVERY exit path,
        // covering TCP dial errors, TLS failures, P2P handshake failures, and
        // session drops.  connectedAt is zero for pre-message-loop exits (treat
        // as failure) and non-zero once the message loop starts.
        //   • connectedAt is zero           → handshake failed   → OnDialFail
        //   • lasted < stableConnTime       → peer flapped       → OnDialFail
        //   • lasted ≥ stableConnTime       → healthy session    → OnDialSuccess
        if outbound {
                defer func() {
                        if connectedAt.IsZero() || time.Since(connectedAt) < stableConnTime {
                                h.mgr.OnDialFail(addr)
                        } else {
                                h.mgr.OnDialSuccess(addr)
                        }
                }()
        }

        // Pending-handshake semaphore: the slot is acquired by acceptLoop for
        // every inbound TLS connection.  We must release it on ALL exit paths —
        // including early returns before the TLS block (e.g. the ban check
        // below).  releaseHS is idempotent; calling it more than once is safe.
        // We also call it explicitly right after a successful handshake so that
        // the slot is freed as early as possible rather than at connection close.
        hsSlotHeld := !outbound && h.cfg.MaxPendingHandshakes > 0 && h.cfg.TLSConfig != nil
        releaseHS := func() {
                if hsSlotHeld {
                        h.pendingHandshakes.Add(-1)
                        hsSlotHeld = false
                }
        }
        defer releaseHS() // safety net: covers ban-check return and any other early exit

        // Reject banned peers immediately
        if h.mgr.IsBanned(addr) {
                h.log.Debug("handleConn: banned peer rejected", "addr", addr)
                conn.Close()
                return
        }

        // When TLS is enabled the accepted conn is a *tls.Conn whose handshake
        // is lazy (fires on first Read/Write).  Complete it eagerly here so:
        //   a) unauthenticated / plain-TCP connections are dropped immediately
        //      with a clear log line rather than partway through the Aperod
        //      application handshake, and
        //   b) PeerFingerprint is available before any application data flows.
        if tlsConn, ok := conn.(*tls.Conn); ok {
                tlsConn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
                if err := tlsConn.Handshake(); err != nil {
                        // releaseHS() via defer; no explicit call needed here.
                        h.log.Debug("tls handshake failed — plaintext or unauthorized peer rejected",
                                "addr", addr, "err", err)
                        conn.Close()
                        return
                }
                releaseHS() // handshake complete — free the slot early, before message loop
                tlsConn.SetDeadline(time.Time{}) //nolint:errcheck
                fp := PeerFingerprint(conn)
                h.log.Debug("tls handshake ok", "addr", addr, "fingerprint", fp)

                // Identity-conflict guard: reject any peer that presents the same
                // TLS fingerprint as our own identity key.  This happens when a new
                // node was bootstrapped by rsyncing chain data from an existing node
                // and the p2p_identity.key file was copied along with the chain data.
                // Both nodes then present identical TLS certificates and TLS rejects
                // the handshake silently, leaving peer_count at 0 with no clear cause.
                //
                // Logging ERROR (not Warn) so operators notice immediately; the hint
                // provides the exact remediation command.
                if h.cfg.SelfFingerprint != "" && fp == h.cfg.SelfFingerprint {
                        h.log.Error("p2p identity conflict detected — peer shares our TLS fingerprint",
                                "addr", addr,
                                "fingerprint", fp,
                                "hint", "delete the p2p_identity.key file in your data directory and restart, or restart the node with --reset-p2p-identity",
                        )
                        conn.Close()
                        return
                }

                // Validator allow-list: when AllowedPeers is non-empty, only
                // fingerprints on the list may proceed.  An empty list means
                // open network (no restriction).
                if len(h.cfg.AllowedPeers) > 0 {
                        allowed := false
                        for _, a := range h.cfg.AllowedPeers {
                                if a == fp {
                                        allowed = true
                                        break
                                }
                        }
                        if !allowed {
                                h.log.Info("peer rejected: fingerprint not in allowed_peers list",
                                        "addr", addr, "fingerprint", fp)
                                conn.Close()
                                return
                        }
                        h.log.Debug("peer fingerprint allowed", "addr", addr, "fingerprint", fp)
                }
        }

        ingestRate := float64(h.cfg.MaxBlockIngestPerSec)
        peer = &Peer{
                conn:          conn,
                addr:          addr,
                outbound:      outbound,
                pendingBlocks: make(map[crypto.Hash32]pendingBlockEntry),
                ingestBucket: peerTokenBucket{
                        tokens:   ingestRate, // start full so first burst is free
                        lastTime: time.Now(),
                        rate:     ingestRate,
                        burst:    ingestRate,
                },
        }

        // Handshake — asymmetric:
        //   Outbound (dialer)  : send Ping → receive Pong
        //   Inbound (acceptor) : receive Ping → send Pong
        // Both sides are trying to send first results in a deadlock where
        // each side reads the other's Ping expecting a Pong and closes.
        selfMsg := PingMsg{
                NodeID:    h.cfg.NodeID,
                Height:    h.handler.CurrentHeight(),
                UserAgent: h.cfg.UserAgent,
                Timestamp: time.Now().UnixNano(),
        }

        var peerID string
        var peerHeight uint64

        if outbound {
                // Dialer: send Ping, wait for Pong
                if err := writeMsg(conn, MsgPing, selfMsg); err != nil {
                        conn.Close()
                        return
                }
                msgType, data, err := readMsg(conn)
                if err != nil || msgType != MsgPong {
                        conn.Close()
                        return
                }
                var pong PingMsg
                if err := unmarshal(data, &pong); err != nil {
                        conn.Close()
                        return
                }
                peerID = pong.NodeID
                peerHeight = pong.Height
        } else {
                // Acceptor: wait for Ping, send Pong
                msgType, data, err := readMsg(conn)
                if err != nil || msgType != MsgPing {
                        conn.Close()
                        return
                }
                var theirPing PingMsg
                if err := unmarshal(data, &theirPing); err != nil {
                        conn.Close()
                        return
                }
                peerID = theirPing.NodeID
                peerHeight = theirPing.Height
                if err := writeMsg(conn, MsgPong, selfMsg); err != nil {
                        conn.Close()
                        return
                }
        }

        peer.id = peerID
        peer.height = peerHeight

        h.mu.Lock()
        if _, exists := h.peers[addr]; exists {
                h.mu.Unlock()
                conn.Close()
                return
        }
        h.peers[addr] = peer
        h.mu.Unlock()

        h.log.Info("peer connected",
                "addr", addr,
                "peer_height", peerHeight,
                "direction", map[bool]string{true: "out", false: "in"}[outbound],
        )

        // Initiate header sync unconditionally now that the peer is registered.
        //
        // Why unconditional (not "if peerHeight > CurrentHeight()"): the height
        // carried by the handshake Ping/Pong is captured when the remote's
        // handleConn starts — the inbound side builds its Pong payload BEFORE
        // it even reads our Ping.  A block produced during the handshake window
        // is broadcast before this peer entry exists in the remote's peer table
        // (silently missed) AND leaves peerHeight stale-equal to our local
        // height, so a height-gap guard here would skip the request and the
        // node would stay stalled until the keepalive Pong cycle or the
        // GetBlock stall timer fires (10–15 s by default).  One unconditional
        // GetHeaders round-trip closes that window: when we are already at the
        // remote tip the response is an empty header list and handleHeaders
        // no-ops, so the extra cost is a single small message per connection.
        h.requestHeaders(peer)

        // Mark the connection as having reached the message loop.  The back-off
        // defer registered at the top of handleConn reads this value to decide
        // whether the session was healthy or should be counted as a failure.
        connectedAt = time.Now()

        // Seed lastPongAt to the connection start time so the first keepalive
        // pong-deadline check does not immediately evict a freshly connected peer
        // that has not yet had a chance to reply to the first MsgPing.
        peer.lastPongAt.Store(connectedAt.UnixNano())

        // Keepalive: send a Ping to the remote peer every 10 s so that the
        // peer's ReadTimeout (30 s) never fires due to silence from our side.
        // This is critical for relay/sync-only nodes that have no messages of
        // their own to send once the initial GetHeaders exchange is complete.
        // The goroutine is stopped via keepaliveDone before the connection is
        // closed (defers run LIFO, so close(keepaliveDone) fires before the
        // conn.Close defer below).
        keepaliveDone := make(chan struct{})
        go func() {
                curKeepalive := h.GetKeepaliveInterval()
                ping  := time.NewTicker(curKeepalive)
                sync  := time.NewTicker(3 * time.Second)
                stall := time.NewTicker(h.cfg.GetBlockStallTimeout)
                defer ping.Stop()
                defer sync.Stop()
                defer stall.Stop()
                for {
                        select {
                        case <-ping.C:
                                // Live-configurable keepalive interval: check whether an
                                // operator has updated the interval via SetKeepaliveInterval.
                                // When the interval changes we must reset lastPongAt to now
                                // before applying the new deadline — otherwise a decrease
                                // (e.g. 10 s → 1 s) would immediately evaluate the shorter
                                // 2×1 s window against a lastPongAt that was last updated
                                // under the old 10 s cadence, falsely evicting a healthy peer
                                // before any ping has even been sent at the new rate.
                                // Resetting the baseline to now gives the peer a full
                                // 2×newInterval grace window starting from this tick.
                                if newInterval := h.GetKeepaliveInterval(); newInterval != curKeepalive {
                                        peer.lastPongAt.Store(time.Now().UnixNano())
                                        curKeepalive = newInterval
                                        ping.Reset(curKeepalive)
                                }
                                // Pong-deadline check: if the peer has not replied to our
                                // keepalive Pings for longer than 2× KeepaliveInterval, the
                                // connection is considered dead (e.g. a half-open TCP session
                                // after a peer crash).  Close the connection so the slot is
                                // freed without waiting for the OS TCP keepalive (~2 h).
                                pongDeadline := 2 * curKeepalive
                                lastPong := time.Unix(0, peer.lastPongAt.Load())
                                if time.Since(lastPong) > pongDeadline {
                                        h.log.Warn("keepalive: peer silent — no pong received within deadline, evicting",
                                                "peer", peer.addr,
                                                "deadline", pongDeadline,
                                                "since_last_pong", time.Since(lastPong).Round(time.Millisecond),
                                        )
                                        conn.Close()
                                        return
                                }
                                if err := peer.Send(MsgPing, PingMsg{
                                        NodeID:    h.cfg.NodeID,
                                        Height:    h.handler.CurrentHeight(),
                                        UserAgent: h.cfg.UserAgent,
                                        Timestamp: time.Now().UnixNano(),
                                }); err != nil {
                                        return // connection is gone; goroutine exits cleanly
                                }
                        case <-sync.C:
                                // Re-request headers whenever we are still behind this peer.
                                // The processBlock re-trigger relies on CurrentHeight() having
                                // already advanced (async engine), so it is unreliable for
                                // large sync gaps.  This periodic check fills that gap: every
                                // 3 s we ask for the next header batch until we catch up.
                                if h.handler.CurrentHeight() < peer.height {
                                        h.requestHeaders(peer)
                                }
                        case <-stall.C:
                                // Stall detection: scan this peer's outstanding MsgGetBlock
                                // requests.  Any entry older than GetBlockStallTimeout means
                                // the peer did not serve the block (pruned, reorg, or race).
                                // Log one WARN per stalled hash so operators see the exact
                                // block hash and height, then re-issue MsgGetHeaders so the
                                // sync pipeline restarts from the current tip rather than
                                // hanging forever.
                                //
                                // Using a dedicated ticker (not the sync.C ticker) ensures
                                // stall re-issues are driven solely by the stall timeout and
                                // are independently observable in tests.
                                now := time.Now()
                                peer.pendingBlocksMu.Lock()
                                type stalledEntry struct {
                                        hash   crypto.Hash32
                                        height uint64
                                }
                                var stalled []stalledEntry
                                for hash, entry := range peer.pendingBlocks {
                                        if now.Sub(entry.sentAt) > h.cfg.GetBlockStallTimeout {
                                                // Skip if the block arrived from another peer in
                                                // the meantime — no actual stall in that case.
                                                if h.handler.GetBlock(hash) != nil {
                                                        delete(peer.pendingBlocks, hash)
                                                        continue
                                                }
                                                stalled = append(stalled, stalledEntry{
                                                        hash:   hash,
                                                        height: entry.headerHeight,
                                                })
                                                delete(peer.pendingBlocks, hash)
                                        }
                                }
                                peer.pendingBlocksMu.Unlock()
                                if len(stalled) > 0 {
                                        for _, s := range stalled {
                                                h.log.Warn("relay sync stall: peer did not serve block; re-issuing GetHeaders",
                                                        "peer", peer.addr,
                                                        "block_hash", fmt.Sprintf("%x", s.hash[:8]),
                                                        "block_height", s.height,
                                                        "stall_after", h.cfg.GetBlockStallTimeout,
                                                        "our_height", h.handler.CurrentHeight(),
                                                        "peer_height", peer.height,
                                                )
                                        }
                                        // Record a single stall event for the entire tick so the
                                        // Admin Panel notification log shows stalled peer + count.
                                        ev := StallEvent{
                                                PeerAddr:    peer.addr,
                                                StalledCount: len(stalled),
                                                At:          time.Now(),
                                        }
                                        h.stallEventMu.Lock()
                                        h.stallEvents = append(h.stallEvents, ev)
                                        if len(h.stallEvents) > stallEventMaxEvents {
                                                h.stallEvents = h.stallEvents[len(h.stallEvents)-stallEventMaxEvents:]
                                        }
                                        h.stallEventMu.Unlock()
                                        h.requestHeaders(peer)
                                }
                        case <-keepaliveDone:
                                return
                        case <-h.done:
                                return
                        }
                }
        }()
        defer close(keepaliveDone)

        // Message loop
        defer func() {
                conn.Close()
                h.mu.Lock()
                // Identity-safe cleanup: only remove the peer if it is still
                // the same *Peer object registered at startup of this
                // handleConn invocation.  DropPeer (and a racing reconnect)
                // may have already removed this entry and replaced it with a
                // fresh peer at the same address; a bare delete would evict
                // the replacement, leaving the node with zero peers and no
                // automatic recovery path.
                if current, ok := h.peers[addr]; ok && current == peer {
                        delete(h.peers, addr)
                }
                h.mu.Unlock()
                h.log.Info("peer disconnected", "addr", addr)
        }()

        for {
                select {
                case <-h.done:
                        return
                default:
                }

                msgType, data, err := readMsg(conn)
                if err != nil {
                        return
                }

                if err := h.dispatch(peer, msgType, data); err != nil {
                        h.log.Warn("dispatch error", "peer", addr, "type", msgType, "err", err)
                }
        }
}

// dispatch routes incoming messages to the appropriate handler.
func (h *Host) dispatch(peer *Peer, msgType MessageType, data []byte) error {
        switch msgType {
        case MsgPing:
                var msg PingMsg
                if err := unmarshal(data, &msg); err != nil {
                        return err
                }
                // Respond with pong
                return peer.Send(MsgPong, PingMsg{
                        NodeID:    h.cfg.NodeID,
                        Height:    h.handler.CurrentHeight(),
                        UserAgent: h.cfg.UserAgent,
                        Timestamp: time.Now().UnixNano(),
                })

        case MsgPong:
                // Keepalive pong: record receipt time for the pong-deadline check in
                // the keepalive goroutine, update the peer's reported height, and return.
                // No response needed.
                var msg PingMsg // Pong reuses the same wire struct as Ping.
                if err := unmarshal(data, &msg); err != nil {
                        return err
                }
                // Record the time of receipt so the keepalive goroutine can detect
                // peers that have gone silent (half-open TCP connections after a crash).
                peer.lastPongAt.Store(time.Now().UnixNano())
                if msg.Height > 0 {
                        h.mu.Lock()
                        if p, ok := h.peers[peer.addr]; ok {
                                p.height = msg.Height
                        }
                        h.mu.Unlock()
                }
                // If the peer is ahead of us, re-trigger header sync.  This self-heals
                // the case where the initial requestHeaders fired during the UTXO-rebuild
                // window (when AddBlock silently rejects incoming blocks because UTXO
                // queries are not yet enabled).  On the next keepalive Pong the node
                // detects the height gap and restarts the sync pipeline automatically,
                // without requiring a manual restart.
                //
                // Cooldown: only fire when the peer has advanced to a height we have
                // not yet requested headers for (new relevant state).  This suppresses
                // back-to-back redundant requests when the peer stays 1 block ahead
                // while its MsgBlock is still in flight — a scenario where every
                // regular keepalive Pong would otherwise fire a request.  A
                // 2×KeepaliveInterval fallback preserves the self-heal benefit: if
                // the first request was silently dropped (e.g. during UTXO rebuild)
                // the node retries after at most two keepalive cycles.
                if msg.Height > h.handler.CurrentHeight() {
                        nowNs := time.Now().UnixNano()
                        lastReqHeight := peer.lastHeadersRequestedAtHeight.Load()
                        lastReqNs := peer.lastHeadersRequestedAt.Load()
                        selfHealNs := int64(2 * h.GetKeepaliveInterval())
                        if msg.Height > lastReqHeight || nowNs-lastReqNs >= selfHealNs {
                                peer.lastHeadersRequestedAt.Store(nowNs)
                                peer.lastHeadersRequestedAtHeight.Store(msg.Height)
                                h.pongGetHeadersTotal.Add(1)
                                h.requestHeaders(peer)
                        }
                }
                return nil

        case MsgGetHeaders:
                var msg GetHeadersMsg
                if err := unmarshal(data, &msg); err != nil {
                        return err
                }
                return h.handleGetHeaders(peer, msg)

        case MsgHeaders:
                var msg HeadersMsg
                if err := unmarshal(data, &msg); err != nil {
                        return err
                }
                h.handleHeaders(peer, msg)
                return nil

        case MsgGetBlock:
                var msg GetBlockMsg
                if err := unmarshal(data, &msg); err != nil {
                        return err
                }
                return h.handleGetBlock(peer, msg)

        case MsgBlock:
                var msg SerializedBlock
                if err := unmarshal(data, &msg); err != nil {
                        return err
                }
                block := msgToBlock(msg)
                if block != nil {
                        // Rogue-fork spam protection: ban peers that repeatedly send
                        // blocks far ahead of our tip (wrong-fork / CPU-waste attack).
                        // Counter and ban are keyed by bare IP so a reconnect on a new
                        // source port does not bypass the enforcement.
                        ourTip := h.handler.CurrentHeight()
                        peerIP := connIP(peer.addr)
                        // A strike is only warranted when the received block is far
                        // ahead of our tip AND the sending peer itself is NOT far ahead
                        // of us.  A peer that is genuinely further along (peer.height >
                        // ourTip+BadBlockHeightLead) is a node we are actively syncing
                        // from; the blocks it sends at its own tip arrive via gossip
                        // before our sync pipeline has applied the intermediate blocks.
                        // Counting those as strikes would permanently ban the validator
                        // we are catching up to — the exact bug that caused relay nodes
                        // started from a snapshot to get banned during the catch-up gap.
                        //
                        // Rogue peers that fabricate future-height blocks announce a
                        // peer.height that is at or below our tip (they pretend to be
                        // at the same height), so the second condition catches them.
                        if block.Header.Height > ourTip+h.cfg.BadBlockHeightLead &&
                                peer.height <= ourTip+h.cfg.BadBlockHeightLead {
                                // Whitelisted peers are trusted validators; skip the
                                // strike counter entirely so a temporarily-ahead validator
                                // is never auto-banned for being on a longer fork.
                                // The block is still validated normally below.
                                h.wlMu.RLock()
                                wlNets := h.wlNets
                                wlIPs := h.wlIPs
                                h.wlMu.RUnlock()
                                if len(wlNets) > 0 || len(wlIPs) > 0 {
                                        if remoteIP := net.ParseIP(peerIP); remoteIP != nil && ipInWhitelist(remoteIP, wlNets, wlIPs) {
                                                h.log.Debug("out-of-range block from whitelisted peer — strike skipped",
                                                        "peer", peer.addr,
                                                        "ip", peerIP,
                                                        "block_height", block.Header.Height,
                                                        "our_tip", ourTip)
                                                // Record for the Admin Panel notification log so
                                                // operators can confirm the whitelist exemption is
                                                // working without SSH access.
                                                h.wlExemptMu.Lock()
                                                h.wlExemptEvents = append(h.wlExemptEvents, WhitelistExemptionEvent{
                                                        IP:          peerIP,
                                                        PeerAddr:    peer.addr,
                                                        BlockHeight: block.Header.Height,
                                                        OurTip:      ourTip,
                                                        At:          time.Now(),
                                                })
                                                if len(h.wlExemptEvents) > whitelistExemptMaxEvents {
                                                        h.wlExemptEvents = h.wlExemptEvents[len(h.wlExemptEvents)-whitelistExemptMaxEvents:]
                                                }
                                                h.wlExemptMu.Unlock()
                                                // Fall through to normal block processing below.
                                                goto processBlock
                                        }
                                }
                                h.badBlockMu.Lock()
                                strike := h.badBlockCounts[peerIP]
                                // Reset stale strikes so a long-dormant IP starts fresh.
                                if !strike.lastSeen.IsZero() && time.Since(strike.lastSeen) > badBlockStrikeTTL {
                                        strike.count = 0
                                }
                                // Only track if below the per-map cap or already present.
                                _, alreadyTracked := h.badBlockCounts[peerIP]
                                if alreadyTracked || len(h.badBlockCounts) < badBlockMaxTrackedIPs {
                                        strike.count++
                                        strike.lastSeen = time.Now()
                                        h.badBlockCounts[peerIP] = strike
                                }
                                count := strike.count
                                h.badBlockMu.Unlock()

                                h.log.Debug("out-of-range block from peer",
                                        "peer", peer.addr,
                                        "ip", peerIP,
                                        "block_height", block.Header.Height,
                                        "our_tip", ourTip,
                                        "count", count)
                                if count >= h.cfg.BadBlockBanThreshold {
                                        // Ban by bare IP so reconnects on new source ports are
                                        // also rejected.  IsBanned checks both IP:port and bare
                                        // IP, so this blocks all future connections from the host.
                                        banDuration := h.cfg.BadBlockBanDuration
                                        // Commit the ban and cancel any in-flight dials to this
                                        // IP under dialGateMu so that no new TCP connection can
                                        // be initiated after the ban is written.
                                        h.dialGateMu.Lock()
                                        h.mgr.Ban(peerIP, "repeated out-of-range blocks (wrong fork)", banDuration)
                                        h.cancelInFlightDials(peerIP)
                                        h.dialGateMu.Unlock()
                                        // Close ALL currently established connections from the
                                        // same IP, not just the one that triggered the threshold.
                                        h.mu.Lock()
                                        for addr, p := range h.peers {
                                                if connIP(addr) == peerIP {
                                                        p.conn.Close()
                                                        delete(h.peers, addr)
                                                }
                                        }
                                        h.mu.Unlock()
                                        // Remove the now-banned IP from the strike map.
                                        h.badBlockMu.Lock()
                                        delete(h.badBlockCounts, peerIP)
                                        h.badBlockMu.Unlock()
                                        h.log.Info("peer IP banned for wrong-fork blocks",
                                                "ip", peerIP, "addr", peer.addr, "duration", banDuration)
                                        // Record the ban event so the API server can poll
                                        // and send an admin Telegram notification.
                                        h.banEventMu.Lock()
                                        h.banEvents = append(h.banEvents, BanEvent{
                                                IP:              peerIP,
                                                PeerAddr:        peer.addr,
                                                PeerID:          peer.id,
                                                Reason:          "repeated out-of-range blocks (wrong fork)",
                                                Violations:      count,
                                                BanDurationSecs: int64(banDuration.Seconds()),
                                                At:              time.Now(),
                                        })
                                        if len(h.banEvents) > banEventMaxEvents {
                                                h.banEvents = h.banEvents[len(h.banEvents)-banEventMaxEvents:]
                                        }
                                        h.banEventMu.Unlock()
                                        return nil
                                }
                                return nil
                        }
                        // Valid-height block: reset the bad-block counter for this IP.
                        h.badBlockMu.Lock()
                        delete(h.badBlockCounts, peerIP)
                        h.badBlockMu.Unlock()

                processBlock:
                        // Rate-limit block ingestion per peer.  The token bucket
                        // sleeps the dispatch goroutine (which is the only reader
                        // for this peer's conn) when tokens are exhausted, creating
                        // TCP-level backpressure without dropping any blocks.
                        if waited := peer.ingestBucket.wait(); waited > 0 {
                                h.log.Warn("p2p: block ingest rate limit fired — applying backpressure",
                                        "peer", peer.addr,
                                        "waited_ms", waited.Milliseconds(),
                                        "limit_per_sec", h.cfg.MaxBlockIngestPerSec,
                                        "block_height", block.Header.Height)
                        }

                        // Clear the pending-block entry for this peer so the
                        // stall-detection ticker does not log a false warning for a
                        // block that arrived normally.  Each Peer tracks only the
                        // requests it sent, so deleting here is always safe.
                        blockHash := block.Hash()
                        peer.pendingBlocksMu.Lock()
                        delete(peer.pendingBlocks, blockHash)
                        peer.pendingBlocksMu.Unlock()

                        // Gossip relay: forward to all other peers the first time we see this block.
                        isNew := h.gossip.MarkAndCheck(blockHash)
                        h.handler.OnBlock(block)
                        // If this block extended our tip and we're still behind
                        // this peer, immediately request the next header batch so
                        // sync progresses without waiting for the next Pong cycle.
                        // Without this, only the first block in a 500-header batch
                        // gets applied (AddBlock requires strict ordering) and the
                        // node stalls indefinitely when resyncing a large gap.
                        if newTip := h.handler.CurrentHeight(); newTip > ourTip && newTip < peer.height {
                                h.requestHeaders(peer)
                        }
                        if isNew {
                                sb := blockToMsg(block)
                                fromAddr := peer.addr
                                h.mu.RLock()
                                relayPeers := make([]*Peer, 0, len(h.peers))
                                for addr, rp := range h.peers {
                                        if addr != fromAddr {
                                                relayPeers = append(relayPeers, rp)
                                        }
                                }
                                h.mu.RUnlock()
                                for _, rp := range relayPeers {
                                        if err := rp.Send(MsgBlock, sb); err != nil {
                                                h.log.Debug("gossip relay block failed", "peer", rp.addr, "err", err)
                                        }
                                }
                        }
                }
                return nil

        case MsgTx:
                // Per-source-IP tx rate limit: a peer flooding transactions
                // just below the mempool eviction rate can force constant
                // eviction churn and degrade SelectTxs / block-production
                // latency.  Check BEFORE unmarshal so throttled floods do not
                // even pay the deserialization cost.  Keyed by bare IP so a
                // reconnect on a new source port does not reset the budget.
                if h.txRate != nil {
                        txPeerIP := connIP(peer.addr)
                        allowed, banNow, violations := h.txRate.allow(txPeerIP)
                        if !allowed {
                                h.log.Warn("tx rate limit exceeded — dropping transaction",
                                        "peer", peer.addr,
                                        "ip", txPeerIP,
                                        "violations", violations,
                                        "burst", h.cfg.TxRateBurst,
                                        "sustained_per_sec", h.cfg.TxRateSustained)
                                if banNow {
                                        h.banTxFlooder(txPeerIP, peer, violations)
                                }
                                return nil
                        }
                }
                var tx core.Transaction
                if err := unmarshal(data, &tx); err != nil {
                        return err
                }
                // Gossip relay: forward to all other peers the first time we see this tx.
                txHash := tx.Hash()
                isNew := h.gossip.MarkAndCheck(txHash)
                h.handler.OnTransaction(&tx)
                if isNew {
                        fromAddr := peer.addr
                        h.mu.RLock()
                        relayPeers := make([]*Peer, 0, len(h.peers))
                        for addr, rp := range h.peers {
                                if addr != fromAddr {
                                        relayPeers = append(relayPeers, rp)
                                }
                        }
                        h.mu.RUnlock()
                        for _, rp := range relayPeers {
                                if err := rp.Send(MsgTx, &tx); err != nil {
                                        h.log.Debug("gossip relay tx failed", "peer", rp.addr, "err", err)
                                }
                        }
                }
                return nil

        case MsgVote:
                var vote VoteMsg
                if err := unmarshal(data, &vote); err != nil {
                        return err
                }
                h.handler.OnVote(vote)
                return nil

        case MsgGetPeers:
                return h.handleGetPeers(peer)

        case MsgPeers:
                var msg PeersMsg
                if err := unmarshal(data, &msg); err != nil {
                        return err
                }
                h.addKnownPeers(msg.Addrs)
                return nil

        default:
                return fmt.Errorf("unknown message type: 0x%02x", msgType)
        }
}

func (h *Host) requestHeaders(peer *Peer) {
        tail := h.handler.CurrentTailHashes(32)
        peer.Send(MsgGetHeaders, GetHeadersMsg{
                KnownHashes: tail,
                Limit:       500,
        })
}

func (h *Host) handleGetHeaders(peer *Peer, msg GetHeadersMsg) error {
        var headers []SerializedHeader
        if h.headers != nil || (h.blockByHash != nil && h.blockByHeight != nil) {
                limit := msg.Limit
                if limit <= 0 || limit > 500 {
                        limit = 500
                }

                var coreHeaders []core.BlockHeader

                // When a LevelDB fetcher is registered, prefer the store-backed
                // lookup: it can locate common ancestors far outside the in-memory
                // ring.  HeadersFrom falls back to height 1 when it finds no match,
                // then returns headers from wherever the ring starts — skipping the
                // entire gap a restarting relay node needs to fill.
                if h.blockByHash != nil && h.blockByHeight != nil {
                        coreHeaders = h.headersFromStore(msg.KnownHashes, limit)
                }

                // Fall back to in-memory ring when the store lookup found nothing
                // (no common ancestor in LevelDB, or fetcher not registered).
                if len(coreHeaders) == 0 && h.headers != nil {
                        coreHeaders = h.headers.HeadersFrom(msg.KnownHashes, limit)
                }

                headers = make([]SerializedHeader, 0, len(coreHeaders))
                for _, ch := range coreHeaders {
                        headers = append(headers, SerializedHeader{
                                Height:       ch.Height,
                                Hash:         ch.Hash(),
                                PrevHash:     ch.PrevHash,
                                MerkleRoot:   ch.MerkleRoot,
                                Timestamp:    ch.Timestamp,
                                Round:        ch.Round,
                                ValidatorPub: ch.ValidatorPub,
                                Signature:    ch.Signature,
                                OraclePrice:  ch.OraclePrice,
                                BaseFee:      ch.BaseFee,
                        })
                }
        }
        return peer.Send(MsgHeaders, HeadersMsg{Headers: headers})
}

// headersFromStore finds the highest common ancestor in the persistent store
// (using blockByHash) and returns up to limit headers starting after it
// (using blockByHeight).  This is the LevelDB fallback for handleGetHeaders
// when knownHashes are outside the in-memory ring.
func (h *Host) headersFromStore(knownHashes []crypto.Hash32, limit int) []core.BlockHeader {
        // Find the highest block height we share with the syncing peer.
        bestHeight := uint64(0)
        for _, hash := range knownHashes {
                if b := h.blockByHash(hash); b != nil {
                        if b.Header.Height > bestHeight {
                                bestHeight = b.Header.Height
                        }
                }
        }
        if bestHeight == 0 {
                return nil // no common ancestor found even in persistent store
        }
        startH := bestHeight + 1
        headers := make([]core.BlockHeader, 0, limit)
        for i := 0; i < limit; i++ {
                b := h.blockByHeight(startH + uint64(i))
                if b == nil {
                        break
                }
                headers = append(headers, b.Header)
        }
        return headers
}

func (h *Host) handleHeaders(peer *Peer, msg HeadersMsg) {
        if len(msg.Headers) == 0 {
                return
        }
        // Request each unknown block and record it as pending so the per-peer
        // stall-detection ticker can detect if MsgBlock never arrives.
        now := time.Now()
        for _, sh := range msg.Headers {
                hash := crypto.Hash32(sh.Hash)
                if h.handler.GetBlock(hash) == nil {
                        peer.Send(MsgGetBlock, GetBlockMsg{Hash: hash})
                        peer.pendingBlocksMu.Lock()
                        if _, already := peer.pendingBlocks[hash]; !already {
                                peer.pendingBlocks[hash] = pendingBlockEntry{
                                        sentAt:      now,
                                        headerHeight: sh.Height,
                                }
                        }
                        peer.pendingBlocksMu.Unlock()
                }
        }
}

func (h *Host) handleGetBlock(peer *Peer, msg GetBlockMsg) error {
        block := h.handler.GetBlock(msg.Hash)
        if block == nil && h.blockByHash != nil {
                // Fall back to persistent store for blocks outside the in-memory ring.
                block = h.blockByHash(msg.Hash)
        }
        if block == nil {
                return nil // we don't have it
        }
        return peer.Send(MsgBlock, blockToMsg(block))
}

func (h *Host) handleGetPeers(peer *Peer) error {
        h.mu.RLock()
        addrs := make([]string, 0, len(h.peers))
        for addr := range h.peers {
                addrs = append(addrs, addr)
        }
        h.mu.RUnlock()
        return peer.Send(MsgPeers, PeersMsg{Addrs: addrs})
}

// maxKnownPeers caps the peerList to prevent memory exhaustion from
// unbounded peer-addr accumulation (and outbound dial-flood amplification).
const maxKnownPeers = 512

func (h *Host) addKnownPeers(addrs []string) {
        h.mu.Lock()
        defer h.mu.Unlock()
        known := make(map[string]bool, len(h.peerList))
        for _, a := range h.peerList {
                known[a] = true
        }
        for _, a := range addrs {
                if len(h.peerList) >= maxKnownPeers {
                        break // cap reached; ignore excess peer addrs
                }
                if !known[a] {
                        h.peerList = append(h.peerList, a)
                        known[a] = true
                }
        }
}

// ─── Block serialization (simplified JSON) ────────────────────────────────────

// SerializedBlock is a JSON-friendly block for network transmission.
type SerializedBlock struct {
        Header SerializedHeader      `json:"header"`
        Txs    []core.Transaction    `json:"txs"`
}

// BlockToMsg is an exported alias for blockToMsg used in tests.
func BlockToMsg(b *core.Block) SerializedBlock { return blockToMsg(b) }

// MsgToBlock is an exported alias for msgToBlock used in tests.
func MsgToBlock(sb SerializedBlock) *core.Block { return msgToBlock(sb) }

func blockToMsg(b *core.Block) SerializedBlock {
        h := b.Header
        hash := b.Hash()
        return SerializedBlock{
                Header: SerializedHeader{
                        Height:       h.Height,
                        Hash:         hash,
                        PrevHash:     h.PrevHash,
                        MerkleRoot:   h.MerkleRoot,
                        Timestamp:    h.Timestamp,
                        Round:        h.Round,
                        ValidatorPub: h.ValidatorPub,
                        Signature:    h.Signature,
                        OraclePrice:  h.OraclePrice,
                        BaseFee:      h.BaseFee,
                },
                Txs: b.Txs,
        }
}

func msgToBlock(sb SerializedBlock) *core.Block {
        pub := crypto.ValidatorPubKey(sb.Header.ValidatorPub)
        return &core.Block{
                Header: core.BlockHeader{
                        Height:       sb.Header.Height,
                        PrevHash:     sb.Header.PrevHash,
                        MerkleRoot:   sb.Header.MerkleRoot,
                        Timestamp:    sb.Header.Timestamp,
                        Round:        sb.Header.Round,
                        ValidatorPub: pub,
                        Signature:    sb.Header.Signature,
                        OraclePrice:  sb.Header.OraclePrice,
                        BaseFee:      sb.Header.BaseFee,
                },
                Txs: sb.Txs,
        }
}
