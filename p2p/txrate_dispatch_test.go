package p2p_test

// Dispatch-level tests for the per-source-IP transaction rate limiter:
// a peer flooding MsgTx must be throttled (txs dropped before the handler)
// and, on sustained flooding, temporarily banned by bare IP.

import (
	"net"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/p2p"
)

// startTxRateHost starts a host with tx rate limiting configured for tests:
// burst 3, sustained 1 tx/sec, ban after 5 violations.
func startTxRateHost(t *testing.T, banThreshold int) (*p2p.Host, *stubHandler) {
	t.Helper()
	handler := &stubHandler{}
	h := p2p.NewHost(p2p.Config{
		ListenAddr:         "127.0.0.1:0",
		MaxPeers:           10,
		NodeID:             "test-txrate",
		UserAgent:          "aperod/test",
		TxRateBurst:        3,
		TxRateSustained:    1,
		TxRateBanThreshold: banThreshold,
		TxRateBanDuration:  time.Minute,
	}, handler, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(h.Stop)
	return h, handler
}

func TestTxRateDispatch_FloodIsThrottled(t *testing.T) {
	h, handler := startTxRateHost(t, 0) // throttle only, never ban

	conn, _ := connectPeer(t, h.ListenAddr())
	defer conn.Close()
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Flood 10 transactions back-to-back; burst allowance is 3.
	for i := 0; i < 10; i++ {
		if err := p2p.WriteMsg(conn, p2p.MsgTx, core.Transaction{}); err != nil {
			t.Fatalf("write tx %d: %v", i, err)
		}
	}
	time.Sleep(200 * time.Millisecond)

	// The whole flood arrives within well under a second, so at most the
	// burst (3) plus one refilled token can reach the handler.
	if got := handler.txCount(); got > 4 {
		t.Errorf("handler received %d txs from a 10-tx flood, want <= 4 (burst=3)", got)
	}
	if got := handler.txCount(); got < 3 {
		t.Errorf("handler received %d txs, want at least the burst of 3", got)
	}
	// Throttle-only mode: peer stays connected and unbanned.
	if h.PeerCount() != 1 {
		t.Errorf("PeerCount = %d after throttled flood, want 1 (no ban)", h.PeerCount())
	}
	if len(h.ListBans()) != 0 {
		t.Errorf("unexpected ban in throttle-only mode: %v", h.ListBans())
	}
}

func TestTxRateDispatch_PersistentFlooderIsBanned(t *testing.T) {
	h, handler := startTxRateHost(t, 5) // ban after 5 dropped txs

	conn, peerIP := connectPeer(t, h.ListenAddr())
	defer conn.Close()
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// 3 allowed (burst) + >=5 violations → ban fires during the flood.
	for i := 0; i < 12; i++ {
		if err := p2p.WriteMsg(conn, p2p.MsgTx, core.Transaction{}); err != nil {
			break // host may close the connection once the ban lands
		}
	}

	if !waitFor(time.Second, func() bool { return h.PeerCount() == 0 }) {
		t.Fatal("flooding peer was NOT disconnected after exceeding the ban threshold")
	}
	bans := h.ListBans()
	if len(bans) == 0 {
		t.Fatal("no ban entry recorded for the tx flooder")
	}
	found := false
	for _, b := range bans {
		if b.Addr == peerIP {
			found = true
			if remaining := time.Until(b.ExpiresAt); remaining > time.Minute || remaining < 30*time.Second {
				t.Errorf("ban duration ≈ %v, want ≈ 1m", remaining)
			}
		}
	}
	if !found {
		t.Errorf("ban entry for bare IP %q not found; bans: %v", peerIP, bans)
	}
	// Only the burst got through to the mempool path.
	if got := handler.txCount(); got > 4 {
		t.Errorf("handler received %d txs, want <= 4", got)
	}

	// Reconnect on a new source port must be rejected while the ban lasts.
	conn2, err := net.DialTimeout("tcp", h.ListenAddr(), 2*time.Second)
	if err != nil {
		t.Fatalf("host unreachable on reconnect: %v", err)
	}
	defer conn2.Close()
	conn2.SetDeadline(time.Now().Add(time.Second))
	_ = p2p.WriteMsg(conn2, p2p.MsgPing, p2p.PingMsg{
		NodeID: "tx-flooder-reconnect", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	})
	if _, _, readErr := p2p.ReadMsg(conn2); readErr == nil {
		t.Errorf("reconnect from banned IP %s was accepted (expected immediate close)", peerIP)
	}
	time.Sleep(50 * time.Millisecond)
	if h.PeerCount() != 0 {
		t.Errorf("banned IP re-registered as a peer (PeerCount=%d)", h.PeerCount())
	}
}

func TestTxRateDispatch_DisabledByDefaultInTests(t *testing.T) {
	// A Config without TxRateBurst must not throttle at all (backwards
	// compatibility for existing tests and explicit operator opt-out).
	handler := &stubHandler{}
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-txrate-off",
		UserAgent:  "aperod/test",
	}, handler, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	conn, _ := connectPeer(t, h.ListenAddr())
	defer conn.Close()
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}
	for i := 0; i < 20; i++ {
		if err := p2p.WriteMsg(conn, p2p.MsgTx, core.Transaction{}); err != nil {
			t.Fatalf("write tx %d: %v", i, err)
		}
	}
	if !waitFor(time.Second, func() bool { return handler.txCount() == 20 }) {
		t.Errorf("handler received %d/20 txs with rate limiting disabled", handler.txCount())
	}
}
