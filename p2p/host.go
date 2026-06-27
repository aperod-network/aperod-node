package p2p

import (
        "fmt"
        "log/slog"
        "net"
        "sync"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// Config holds p2p networking parameters.
type Config struct {
        ListenAddr string
        Bootnodes  []string
        MaxPeers   int
        MinPeers   int
        NodeID     string // hex-encoded public key or random ID
        UserAgent  string
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
}

// NewHost creates a new p2p host.
func NewHost(cfg Config, handler Handler, log *slog.Logger) *Host {
        return &Host{
                cfg:     cfg,
                handler: handler,
                log:     log,
                peers:   make(map[string]*Peer),
                done:    make(chan struct{}),
                mgr:     newPeerMgr(),
                gossip:  NewGossipFilter(),
        }
}

// SetHeaderProvider attaches a header provider used to serve GetHeaders requests.
// Call this before Start() when the host is embedded in a full node.
func (h *Host) SetHeaderProvider(hp HeaderProvider) {
        h.headers = hp
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
        ln, err := net.Listen("tcp", h.cfg.ListenAddr)
        if err != nil {
                return fmt.Errorf("listen %s: %w", h.cfg.ListenAddr, err)
        }
        h.listener = ln
        h.log.Info("p2p listening", "addr", h.cfg.ListenAddr)

        go h.acceptLoop()
        go h.maintainLoop()

        // Dial bootnodes
        for _, addr := range h.cfg.Bootnodes {
                go h.dialPeer(addr)
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

                // Eclipse-attack mitigation (3.5.1): reject inbound connections
                // when the peer table is already full.
                if h.cfg.MaxPeers > 0 {
                        h.mu.RLock()
                        count := len(h.peers)
                        h.mu.RUnlock()
                        if count >= h.cfg.MaxPeers {
                                h.log.Debug("inbound connection rejected: MaxPeers reached",
                                        "addr", conn.RemoteAddr().String(),
                                        "max", h.cfg.MaxPeers)
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

                        h.mu.RLock()
                        count := len(h.peers)
                        known := make([]string, len(h.peerList))
                        copy(known, h.peerList)
                        h.mu.RUnlock()

                        if count < h.cfg.MinPeers {
                                for _, addr := range known {
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

func (h *Host) dialPeer(addr string) {
        h.mu.RLock()
        _, already := h.peers[addr]
        count := len(h.peers)
        h.mu.RUnlock()

        if already || count >= h.cfg.MaxPeers {
                return
        }
        if h.mgr.IsBanned(addr) {
                h.log.Debug("dialPeer: addr is banned", "addr", addr)
                return
        }

        h.log.Debug("dialing peer", "addr", addr)
        conn, err := net.DialTimeout("tcp", addr, DialTimeout)
        if err != nil {
                h.log.Debug("dial failed", "addr", addr, "err", err)
                return
        }
        go h.handleConn(conn, true)
}

func (h *Host) handleConn(conn net.Conn, outbound bool) {
        addr := conn.RemoteAddr().String()

        // Reject banned peers immediately
        if h.mgr.IsBanned(addr) {
                h.log.Debug("handleConn: banned peer rejected", "addr", addr)
                conn.Close()
                return
        }

        peer := &Peer{conn: conn, addr: addr, outbound: outbound}

        // Handshake: send ping
        ping := PingMsg{
                NodeID:    h.cfg.NodeID,
                Height:    h.handler.CurrentHeight(),
                UserAgent: h.cfg.UserAgent,
                Timestamp: time.Now().UnixNano(),
        }
        if err := writeMsg(conn, MsgPing, ping); err != nil {
                conn.Close()
                return
        }

        // Expect pong
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
        peer.id = pong.NodeID
        peer.height = pong.Height

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
                "peer_height", pong.Height,
                "direction", map[bool]string{true: "out", false: "in"}[outbound],
        )

        // Initiate header sync if peer is ahead
        if pong.Height > h.handler.CurrentHeight() {
                h.requestHeaders(peer)
        }

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

func (h *Host) addKnownPeers(addrs []string) {
        h.mu.Lock()
        defer h.mu.Unlock()
        known := make(map[string]bool, len(h.peerList))
        for _, a := range h.peerList {
                known[a] = true
        }
        for _, a := range addrs {
                if !known[a] {
                        h.peerList = append(h.peerList, a)
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
                },
                Txs: sb.Txs,
        }
}
