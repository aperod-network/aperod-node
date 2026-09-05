// export_test.go — exposes internal Host and PeerMgr methods for tests in package p2p_test.
// This file is compiled only during `go test` because it lives in package p2p
// (not p2p_test) alongside the implementation.

package p2p

import (
	"context"
	"net"
	"time"
)

// BadBlockMaxTrackedIPs is the exported alias for badBlockMaxTrackedIPs,
// available to package p2p_test.
const BadBlockMaxTrackedIPs = badBlockMaxTrackedIPs

// BadBlockStrikeCount returns the current number of IPs tracked in the
// bad-block strike map.  Exported for testing only.
func (h *Host) BadBlockStrikeCount() int {
	h.badBlockMu.Lock()
	defer h.badBlockMu.Unlock()
	return len(h.badBlockCounts)
}

// RecordBadBlockStrike directly records one strike for ip, applying the same
// cap and expiry logic as the real dispatch path but without going through the
// network layer.  Returns the resulting strike count (0 if the cap was full
// and ip was not already tracked).  Exported for testing only.
func (h *Host) RecordBadBlockStrike(ip string) int {
	h.badBlockMu.Lock()
	defer h.badBlockMu.Unlock()
	strike := h.badBlockCounts[ip]
	if !strike.lastSeen.IsZero() && time.Since(strike.lastSeen) > badBlockStrikeTTL {
		strike.count = 0
	}
	_, alreadyTracked := h.badBlockCounts[ip]
	if alreadyTracked || len(h.badBlockCounts) < badBlockMaxTrackedIPs {
		strike.count++
		strike.lastSeen = time.Now()
		h.badBlockCounts[ip] = strike
	}
	return strike.count
}

// HostCanDial reports whether the host's PeerMgr would allow dialling addr
// right now (not banned and back-off window has elapsed).  Exported for
// integration tests that need to inspect dial-backoff state without going
// through the network layer.
func HostCanDial(h *Host, addr string) bool {
	return h.mgr.CanDial(addr)
}

// HostRecordDialFail directly records a dial failure in the host's PeerMgr,
// advancing the back-off counter for addr.  Exported for tests that need to
// inject back-off state without going through the network layer.
func HostRecordDialFail(h *Host, addr string) {
	h.mgr.OnDialFail(addr)
}

// HostSetBootnodeResolved records resolved as the current IP:port addresses
// for the raw bootnode string raw, then rebuilds bootnodeSet.  An empty or
// nil resolved slice removes raw's contribution so its addresses return to
// normal back-off behaviour.  This mirrors what maintainLoop does on a
// successful DNS resolution tick — use it in tests instead of calling Start().
func HostSetBootnodeResolved(h *Host, raw string, resolved []string) {
	h.mu.Lock()
	if len(resolved) == 0 {
		delete(h.bootnodeLastResolved, raw)
		h.rebuildBootnodeSet()
	} else {
		h.applyBootnodeResolution(raw, resolved)
	}
	h.mu.Unlock()
}

// HostIsBootnode reports whether addr is currently in the host's privileged
// bootnode set.  Exported for diagnostic assertions in tests.
func HostIsBootnode(h *Host, addr string) bool {
	return h.isBootnode(addr)
}

// HostTriggerMaintain fires one immediate maintainLoop tick without waiting
// for the 10-second ticker.  Safe to call from any goroutine; non-blocking
// (the channel is buffered with capacity 1, so a pending unfired tick is
// never lost when the goroutine hasn't consumed the previous signal yet).
func HostTriggerMaintain(h *Host) {
	select {
	case h.maintainNow <- struct{}{}:
	default: // tick already pending — maintainLoop will pick it up
	}
}

// HostAddKnownPeer seeds addr into the host's peerList so that the
// MinPeers path inside maintainLoop can re-dial it after a drop.  In
// production peerList is populated by the MsgPeers peer-exchange
// protocol; tests that skip the exchange call this helper instead.
func HostAddKnownPeer(h *Host, addr string) {
	h.addKnownPeers([]string{addr})
}

// HostKeepaliveInterval returns the KeepaliveInterval stored in the host's
// config after NewHost has applied any defaults.  Exported for unit tests that
// verify the default-application and explicit-value-wiring paths without
// starting a listener or connecting any peers.
func HostKeepaliveInterval(h *Host) time.Duration {
	return h.cfg.KeepaliveInterval
}

// HostGetBlockStallTimeout returns the GetBlockStallTimeout stored in the
// host's config after NewHost has applied any defaults.  Exported for unit
// tests that verify the node.yaml → p2p.Config → Host wiring (e.g. a raised
// 60s value must survive NewHost instead of falling back to the 15s default).
func HostGetBlockStallTimeout(h *Host) time.Duration {
	return h.cfg.GetBlockStallTimeout
}

// SetKeepaliveIntervalForTest sets the live keepalive interval atomically
// without enforcing the [1s, 15s] production range constraint.  Use only in
// unit tests that need ms-scale intervals to avoid slow wall-clock waits.
func SetKeepaliveIntervalForTest(h *Host, d time.Duration) {
	h.keepaliveIntervalNs.Store(int64(d))
}

// SetListenFunc replaces the TCP listen factory used by Start().  The factory
// receives the same (network, addr) arguments that net.Listen would receive and
// must return a valid net.Listener (or an error).  Because the factory runs at
// the exact point where net.Listen is called — after loadWhitelistFromFile and
// before tls.NewListener — it is the only reliable place to assert the
// whitelist-before-listener ordering invariant: if a future refactor moves
// h.listenFunc above loadWhitelistFromFile the factory executes before the
// whitelist is populated and any assertion inside it fails immediately.
//
// Pass nil to restore the default (net.Listen).
func SetListenFunc(h *Host, fn func(network, addr string) (net.Listener, error)) {
	if fn == nil {
		h.listenFunc = net.Listen
	} else {
		h.listenFunc = fn
	}
}

// SetPostConnectHook sets a callback that dialPeer invokes after successfully
// dialling and registering the connection in pendingConns, but before calling
// go handleConn.  The hook gives a deterministic test window to call BanPeer
// while the conn is pending: BanPeer will close it via cancelInFlightDials so
// handleConn never receives a live connection to a banned peer.
//
// Pass nil to clear the hook (no-op in production; the field is always nil).
func SetPostConnectHook(h *Host, fn func()) {
	h.postConnectHook = fn
}

// HostPongGetHeadersTotal returns the number of times the MsgPong handler has
// passed the cooldown gate and actually called requestHeaders.  It counts only
// Pong-triggered calls; sync-ticker and stall-detector calls are excluded.
// Exposed for the pong-cooldown unit test; always zero in production runs that
// do not call this function.
func HostPongGetHeadersTotal(h *Host) int64 {
	return h.pongGetHeadersTotal.Load()
}

// SetDialFunc replaces the outbound TCP dial function used by dialPeer.
// The replacement receives a context that BanPeer may cancel while the dial is
// in progress; the function must honour context cancellation and return
// context.Canceled (or context.DeadlineExceeded) without establishing a
// connection.  This lets deterministic concurrency tests inject a blocking
// dial that the test can release or abort on demand.
//
// The write is guarded by h.dialFnMu so it is safe to call concurrently with
// in-flight dials (which hold h.dialFnMu for read while invoking the function).
//
// Pass nil to restore the production default ((&net.Dialer{}).DialContext).
func SetDialFunc(h *Host, fn func(ctx context.Context, network, addr string) (net.Conn, error)) {
	h.dialFnMu.Lock()
	if fn == nil {
		defaultDialer := &net.Dialer{}
		h.dialContextFunc = defaultDialer.DialContext
	} else {
		h.dialContextFunc = fn
	}
	h.dialFnMu.Unlock()
}

// HostSeedConnectedPeer inserts a synthetic connected peer keyed by addr into
// the host's peer table without going through the network handshake.  Used by
// the banned-peer-exchange test to place a peer whose IP can then be banned
// independently of any real connection.  The peer has a nil conn; callers must
// not trigger any code path that writes to the peer.
func HostSeedConnectedPeer(h *Host, addr string) {
	h.mu.Lock()
	h.peers[addr] = &Peer{addr: addr}
	h.mu.Unlock()
}

// HostRemoveConnectedPeer removes a synthetic peer inserted by
// HostSeedConnectedPeer. Exported for test cleanup before Host.Stop attempts to
// close all peer connections.
func HostRemoveConnectedPeer(h *Host, addr string) {
	h.mu.Lock()
	delete(h.peers, addr)
	h.mu.Unlock()
}

// HostPeersToAdvertise returns the addresses the host would include in an
// outbound MsgPeers reply (banned IPs filtered out).  Exercises the exact
// production filter used by the MsgGetPeers handler.
func HostPeersToAdvertise(h *Host) []string {
	return h.peersToAdvertise()
}

// HostBanPeer bans addr via the host's PeerMgr for duration d.  Exported so the
// banned-peer-exchange test can register a ban without a full BanPeer call
// (which also tears down connections and cancels dials).
func HostBanPeer(h *Host, addr, reason string, d time.Duration) {
	h.mgr.Ban(addr, reason, d)
}

// TimestampStrikeCount returns the number of IPs currently tracked in the
// future-timestamp strike map.  Exported for testing only.
func (h *Host) TimestampStrikeCount() int {
	h.tsMu.Lock()
	defer h.tsMu.Unlock()
	return len(h.tsStrikeCounts)
}

// HostMaxClockSkewNs is the exported alias for hostMaxClockSkewNs so tests can
// build future-timestamp values relative to the exact production threshold.
const HostMaxClockSkewNs = hostMaxClockSkewNs

// BadBlockStrikeTTL is the exported alias for badBlockStrikeTTL so tests can
// advance time relative to the exact production TTL without duplicating the
// constant.
const BadBlockStrikeTTL = badBlockStrikeTTL

// HostSetBootnodeLastResolvedAt overwrites the last-successful-resolution
// timestamp for raw in bootnodeLastResolvedAt without changing any other
// bootnode state.  Use in tests to simulate a stale bootnode without waiting
// for a real DNS failure — set t to time.Now().Add(-age) before triggering a
// maintainLoop tick to make the stale-age check fire.  raw must already be
// present in cfg.Bootnodes for the stale check to sample it.
func HostSetBootnodeLastResolvedAt(h *Host, raw string, t time.Time) {
	h.mu.Lock()
	h.bootnodeLastResolvedAt[raw] = t
	h.mu.Unlock()
}

// SetBadBlockLastSeen overwrites the lastSeen timestamp for ip in the
// bad-block strike map without changing its strike count.  This lets unit
// tests simulate elapsed time (e.g. time.Now().Add(-2*BadBlockStrikeTTL))
// so the TTL expiry path can be exercised without waiting a real hour.
// No-op when ip is not currently in the map.  Exported for testing only.
func (h *Host) SetBadBlockLastSeen(ip string, t time.Time) {
	h.badBlockMu.Lock()
	defer h.badBlockMu.Unlock()
	if s, ok := h.badBlockCounts[ip]; ok {
		s.lastSeen = t
		h.badBlockCounts[ip] = s
	}
}

// HostBootnodeInBackoff reports whether addr is currently inside its
// per-bootnode exponential back-off window — i.e. whether maintainLoop would
// skip this address on the next tick.  Returns false when addr has no recorded
// failures (nextDial is zero).  Exported for testing only.
func HostBootnodeInBackoff(h *Host, addr string) bool {
	h.bootnodeMu.Lock()
	defer h.bootnodeMu.Unlock()
	e := h.bootnodeFailState[addr]
	return !e.nextDial.IsZero() && time.Now().Before(e.nextDial)
}

// HostRecordBootnodeFail directly injects one dial failure for addr into the
// per-bootnode back-off state without going through the network layer.
// Exported so tests can pre-load back-off state to exercise the throttle path.
func HostRecordBootnodeFail(h *Host, addr string) {
	h.recordBootnodeFail(addr)
}

// HostSetSendHook installs fn as the send function used by BroadcastBlock
// (and its retry path) instead of Peer.Send.  Pass nil to restore the
// default.  Exported for tests that inject transient send failures.
func HostSetSendHook(h *Host, fn func(*Peer, MessageType, interface{}) (int, error)) {
	if fn == nil {
		h.sendHook.Store(nil)
		return
	}
	h.sendHook.Store(&fn)
}

// NewTestPeer constructs a bare Peer around conn for transport-level tests
// that must exercise the production sendN path (byte accounting, poisoning)
// without a full host handshake.  Exported for testing only.
func NewTestPeer(conn net.Conn, addr string) *Peer {
	return &Peer{conn: conn, addr: addr}
}

// PeerPoisoned reports whether the peer's stream has been marked poisoned
// by a partial write.  Exported for testing only.
func (p *Peer) PeerPoisoned() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.poisoned
}

// PeerSendN exposes Peer.sendN for tests (byte-accounted send).
func (p *Peer) PeerSendN(msgType MessageType, payload interface{}) (int, error) {
	return p.sendN(msgType, payload)
}

// WriteMsgN exposes writeMsgN for transport-level partial-write tests.
func WriteMsgN(conn net.Conn, msgType MessageType, payload interface{}) (int, error) {
	return writeMsgN(conn, msgType, payload)
}

// PeerAddr returns the peer's address key as used in the host peer table.
// Exported for testing only.
func (p *Peer) PeerAddr() string {
	return p.addr
}

// HostPeers returns a snapshot of currently registered peers.  Exported for
// testing only.
func HostPeers(h *Host) []*Peer {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Peer, 0, len(h.peers))
	for _, p := range h.peers {
		out = append(out, p)
	}
	return out
}

// HostBroadcastRetryDelay returns the BroadcastRetryDelay stored in the
// host's config after NewHost has applied any defaults.  Exported for tests.
func HostBroadcastRetryDelay(h *Host) time.Duration {
	return h.cfg.BroadcastRetryDelay
}

// HostMaxDialBackoff returns the MaxDialBackoff value stored in the host's
// config after NewHost has applied any defaults.  Exported for tests that
// verify the default-application path without starting a real listener.
func HostMaxDialBackoff(h *Host) time.Duration {
	return h.cfg.MaxDialBackoff
}
