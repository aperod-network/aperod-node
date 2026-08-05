package p2p

import (
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

// Peer represents a connected remote node.
type Peer struct {
        conn     net.Conn
        addr     string
        id       string
        height   uint64
        mu       sync.Mutex
        outbound bool
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
        gossip  *GossipFilter  // dedup filter for relay
        headers HeaderProvider // optional: serves headers for sync

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

        return &Host{
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
        }
}

// SetHeaderProvider attaches a header provider used to serve GetHeaders requests.
// Call this before Start() when the host is embedded in a full node.
func (h *Host) SetHeaderProvider(hp HeaderProvider) {
        h.headers = hp
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

// BanPeer bans the peer at addr for duration d.  The connection (if any) is
// closed immediately and future dial/accept attempts from that address are
// rejected.
func (h *Host) BanPeer(addr, reason string, d time.Duration) {
        h.mgr.Ban(addr, reason, d)
        h.mu.Lock()
        if p, ok := h.peers[addr]; ok {
                p.conn.Close()
                delete(h.peers, addr)
        }
        h.mu.Unlock()
        h.log.Info("peer banned", "addr", addr, "reason", reason, "duration", d)
}

// Start binds the listener and begins accepting connections.
func (h *Host) Start() error {
        // Restore persisted bans from disk before accepting any connections so
        // previously-banned peers are blocked immediately on restart — without
        // waiting for them to accumulate 10 strikes again.
        h.mgr.LoadBansFromFile()

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

        ln, err := net.Listen("tcp", h.cfg.ListenAddr)
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

        go h.acceptLoop()
        go h.maintainLoop()

        // Dial bootnodes — resolve DNS hostnames before dialling so the
        // canonical peer key in h.peers is always an IP:port string.
        for _, addr := range h.cfg.Bootnodes {
                go func(a string) {
                        resolved, err := resolveBootnode(a)
                        if err != nil {
                                h.log.Warn("bootnode dns resolve failed", "addr", a, "err", err)
                                return
                        }
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
                wlLen := len(h.cfg.PeerWhitelist)
                h.wlMu.RUnlock()
                if wlLen > 0 {
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
                select {
                case <-h.done:
                        return
                case <-ticker.C:
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
                                for _, addr := range known {
                                        h.mu.RLock()
                                        _, connected := h.peers[addr]
                                        h.mu.RUnlock()
                                        if !connected {
                                                go h.dialPeer(addr)
                                        }
                                }
                                // Always retry configured bootnodes when isolated — these
                                // are the only anchors when peerList is empty (e.g. fresh
                                // start or after a network partition).
                                for _, raw := range h.cfg.Bootnodes {
                                        resolved, err := resolveBootnode(raw)
                                        if err != nil {
                                                continue
                                        }
                                        for _, addr := range resolved {
                                                h.mu.RLock()
                                                _, connected := h.peers[addr]
                                                h.mu.RUnlock()
                                                if !connected {
                                                        go h.dialPeer(addr)
                                                }
                                        }
                                }
                        }
                }
        }
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
        // Respect ban list and exponential back-off window.
        if !h.mgr.CanDial(addr) {
                h.log.Debug("dialPeer: addr is banned or in back-off window", "addr", addr)
                return
        }

        h.log.Debug("dialing peer", "addr", addr)
        var conn net.Conn
        if h.cfg.TLSConfig != nil {
                // Outbound TLS dial: the TLS handshake completes before
                // handleConn is invoked, so PeerFingerprint is available
                // immediately on the first call inside handleConn.
                tlsConn, err := tls.DialWithDialer(
                        &net.Dialer{Timeout: DialTimeout},
                        "tcp", addr, h.cfg.TLSConfig,
                )
                if err != nil {
                        h.log.Warn("tls dial failed", "addr", addr, "err", err)
                        h.mgr.OnDialFail(addr)
                        return
                }
                conn = tlsConn
        } else {
                var err error
                conn, err = net.DialTimeout("tcp", addr, DialTimeout)
                if err != nil {
                        h.log.Warn("dial failed", "addr", addr, "err", err)
                        h.mgr.OnDialFail(addr)
                        return
                }
        }
        go h.handleConn(conn, true)
}

func (h *Host) handleConn(conn net.Conn, outbound bool) {
        addr := conn.RemoteAddr().String()

        // connectedAt is set (to non-zero) only once the peer reaches the
        // message loop (i.e. both the TCP dial and the P2P handshake succeeded).
        // The back-off defer below uses it to decide the outcome on every exit
        // path — including early returns from a failed handshake.
        var connectedAt time.Time

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
                        delete(h.peers, addr)
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

        peer := &Peer{conn: conn, addr: addr, outbound: outbound}

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

        // Initiate header sync if peer is ahead
        if peerHeight > h.handler.CurrentHeight() {
                h.requestHeaders(peer)
        }

        // Mark the connection as having reached the message loop.  The back-off
        // defer registered at the top of handleConn reads this value to decide
        // whether the session was healthy or should be counted as a failure.
        connectedAt = time.Now()

        // Message loop
        defer func() {
                conn.Close()
                h.mu.Lock()
                delete(h.peers, addr)
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
                        if block.Header.Height > ourTip+h.cfg.BadBlockHeightLead {
                                // Whitelisted peers are trusted validators; skip the
                                // strike counter entirely so a temporarily-ahead validator
                                // is never auto-banned for being on a longer fork.
                                // The block is still validated normally below.
                                h.wlMu.RLock()
                                wlNets := h.wlNets
                                wlIPs := h.wlIPs
                                wlLen := len(h.cfg.PeerWhitelist)
                                h.wlMu.RUnlock()
                                if wlLen > 0 {
                                        if remoteIP := net.ParseIP(peerIP); remoteIP != nil && ipInWhitelist(remoteIP, wlNets, wlIPs) {
                                                h.log.Debug("out-of-range block from whitelisted peer — strike skipped",
                                                        "peer", peer.addr,
                                                        "ip", peerIP,
                                                        "block_height", block.Header.Height,
                                                        "our_tip", ourTip)
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
                                        h.mgr.Ban(peerIP, "repeated out-of-range blocks (wrong fork)", banDuration)
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
                                        return nil
                                }
                                return nil
                        }
                        // Valid-height block: reset the bad-block counter for this IP.
                        h.badBlockMu.Lock()
                        delete(h.badBlockCounts, peerIP)
                        h.badBlockMu.Unlock()

                processBlock:
                        // Gossip relay: forward to all other peers the first time we see this block.
                        blockHash := block.Hash()
                        isNew := h.gossip.MarkAndCheck(blockHash)
                        h.handler.OnBlock(block)
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
        if h.headers != nil {
                limit := msg.Limit
                if limit <= 0 || limit > 500 {
                        limit = 500
                }
                coreHeaders := h.headers.HeadersFrom(msg.KnownHashes, limit)
                headers = make([]SerializedHeader, 0, len(coreHeaders))
                for _, ch := range coreHeaders {
                        headers = append(headers, SerializedHeader{
                                Height:       ch.Height,
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

func (h *Host) handleHeaders(peer *Peer, msg HeadersMsg) {
        if len(msg.Headers) == 0 {
                return
        }
        // Request each unknown block
        for _, sh := range msg.Headers {
                hash := crypto.Hash32(sh.Hash)
                if h.handler.GetBlock(hash) == nil {
                        peer.Send(MsgGetBlock, GetBlockMsg{Hash: hash})
                }
        }
}

func (h *Host) handleGetBlock(peer *Peer, msg GetBlockMsg) error {
        block := h.handler.GetBlock(msg.Hash)
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
