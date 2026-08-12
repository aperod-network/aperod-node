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
