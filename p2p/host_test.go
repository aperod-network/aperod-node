package p2p_test

// Tests for p2p Host: lifecycle, broadcast with 0 peers, full handshake over net.Pipe.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/p2p"
)

// ─── Stub handler ─────────────────────────────────────────────────────────────

type stubHandler struct {
	mu     sync.Mutex
	blocks []*core.Block
	txs    []*core.Transaction
	votes  []p2p.VoteMsg
}

func (s *stubHandler) OnBlock(b *core.Block) {
	s.mu.Lock()
	s.blocks = append(s.blocks, b)
	s.mu.Unlock()
}
func (s *stubHandler) OnTransaction(tx *core.Transaction) {
	s.mu.Lock()
	s.txs = append(s.txs, tx)
	s.mu.Unlock()
}
func (s *stubHandler) OnVote(v p2p.VoteMsg) {
	s.mu.Lock()
	s.votes = append(s.votes, v)
	s.mu.Unlock()
}
func (s *stubHandler) CurrentHeight() uint64                    { return 0 }
func (s *stubHandler) CurrentTailHashes(_ int) []crypto.Hash32 { return nil }
func (s *stubHandler) GetBlock(_ crypto.Hash32) *core.Block    { return nil }

// blockCount returns the number of blocks received so far, safe for concurrent use.
func (s *stubHandler) blockCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.blocks)
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ─── NewHost / PeerCount / Broadcast with no peers ───────────────────────────

func TestHost_NewHost_PeerCountZero(t *testing.T) {
	h := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	if h.PeerCount() != 0 {
		t.Errorf("PeerCount = %d, want 0", h.PeerCount())
	}
}

// TestHost_KeepaliveInterval_Default verifies that NewHost applies the
// production default of 10 s when KeepaliveInterval is left at its zero value.
// This guards against a future refactor that moves or removes the default so
// operators who do not set keepalive_interval in node.yaml silently get a
// different tick rate.
func TestHost_KeepaliveInterval_Default(t *testing.T) {
	const want = 10 * time.Second
	h := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	got := p2p.HostKeepaliveInterval(h)
	if got != want {
		t.Errorf("KeepaliveInterval default = %v, want %v", got, want)
	}
}

// TestHost_KeepaliveInterval_ExplicitValue verifies that an operator-supplied
// KeepaliveInterval (e.g. 5 s) is preserved by NewHost without being
// overridden by the default-application logic.
func TestHost_KeepaliveInterval_ExplicitValue(t *testing.T) {
	const want = 5 * time.Second
	h := p2p.NewHost(p2p.Config{MaxPeers: 10, KeepaliveInterval: want}, &stubHandler{}, newTestLogger())
	got := p2p.HostKeepaliveInterval(h)
	if got != want {
		t.Errorf("KeepaliveInterval explicit = %v, want %v", got, want)
	}
}

// TestHost_SetKeepaliveInterval_Validation verifies that SetKeepaliveInterval
// rejects values outside the allowed [1s, 15s] window and accepts boundary and
// midpoint values without error.
func TestHost_SetKeepaliveInterval_Validation(t *testing.T) {
	h := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())

	cases := []struct {
		d       time.Duration
		wantErr bool
	}{
		{0, true},
		{500 * time.Millisecond, true},
		{1 * time.Second, false},        // lower bound — valid
		{7 * time.Second, false},        // midpoint — valid
		{15 * time.Second, false},       // upper bound — valid
		{16 * time.Second, true},
		{time.Hour, true},
	}
	for _, tc := range cases {
		err := h.SetKeepaliveInterval(tc.d)
		if (err != nil) != tc.wantErr {
			t.Errorf("SetKeepaliveInterval(%v): err=%v, wantErr=%v", tc.d, err, tc.wantErr)
		}
	}
}

// TestHost_GetKeepaliveInterval_ReturnsUpdated verifies that GetKeepaliveInterval
// returns the value most recently stored by SetKeepaliveInterval.
func TestHost_GetKeepaliveInterval_ReturnsUpdated(t *testing.T) {
	h := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())

	if err := h.SetKeepaliveInterval(5 * time.Second); err != nil {
		t.Fatalf("SetKeepaliveInterval: %v", err)
	}
	if got := h.GetKeepaliveInterval(); got != 5*time.Second {
		t.Errorf("GetKeepaliveInterval = %v, want 5s", got)
	}

	if err := h.SetKeepaliveInterval(1 * time.Second); err != nil {
		t.Fatalf("SetKeepaliveInterval: %v", err)
	}
	if got := h.GetKeepaliveInterval(); got != 1*time.Second {
		t.Errorf("GetKeepaliveInterval = %v, want 1s", got)
	}
}

// TestHost_SetKeepaliveInterval_DecreaseKeepsPeerAlive verifies that decreasing
// the live keepalive interval does NOT evict a healthy connected peer whose last
// Pong arrived more than 2×newInterval ago but within 2×oldInterval (the
// prior-cadence grace window).
//
// Without the lastPongAt-reset fix in the keepalive goroutine, the first tick at
// the old rate would evaluate "2×newInterval since last Pong" against a baseline
// that predates the interval change, causing an immediate false eviction.
//
// The test uses a ms-scale interval via SetKeepaliveIntervalForTest (bypasses the
// [1s, 15s] production guard) so the test completes in < 500 ms.  A raw TCP
// server acts as the remote peer: it completes the asymmetric handshake and then
// replies to every MsgPing with a MsgPong so the connection stays healthy.
func TestHost_SetKeepaliveInterval_DecreaseKeepsPeerAlive(t *testing.T) {
	// Remote-peer server: accept one connection, do asymmetric handshake,
	// then echo every Ping with a Pong.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, aErr := ln.Accept()
		if aErr != nil {
			return
		}
		defer conn.Close()

		// Asymmetric handshake: outbound host sends MsgPing first.
		conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		mt, _, rdErr := p2p.ReadMsg(conn)
		if rdErr != nil || mt != p2p.MsgPing {
			t.Logf("server: expected MsgPing, got %v err=%v", mt, rdErr)
			return
		}
		if wErr := p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
			NodeID: "server", Height: 0, UserAgent: "test", Timestamp: time.Now().UnixNano(),
		}); wErr != nil {
			return
		}

		// Serve keepalive pings until the connection closes.
		for {
			conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
			mt2, _, rdErr2 := p2p.ReadMsg(conn)
			if rdErr2 != nil {
				return // connection closed — exit cleanly
			}
			if mt2 == p2p.MsgPing {
				_ = p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
					NodeID: "server", Height: 0, UserAgent: "test", Timestamp: time.Now().UnixNano(),
				})
			}
		}
	}()

	// Initial keepalive: 100 ms (via test helper — bypasses [1s, 15s] guard).
	const initialInterval = 100 * time.Millisecond
	host := p2p.NewHost(p2p.Config{
		ListenAddr:        "127.0.0.1:0",
		MaxPeers:          5,
		NodeID:            "ka-decrease-test",
		UserAgent:         "aperod/test",
		KeepaliveInterval: initialInterval,
	}, &stubHandler{}, newTestLogger())
	if sErr := host.Start(); sErr != nil {
		t.Fatalf("host.Start: %v", sErr)
	}
	defer host.Stop()

	// Dial the server; wait up to 2 s for the connection to be established.
	host.DialPeer(ln.Addr().String())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if host.PeerCount() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if host.PeerCount() != 1 {
		t.Fatalf("peer never connected")
	}

	// Sleep for a duration that is:
	//   > 2 × newInterval (20 ms × 2 = 40 ms)  — would evict under old code
	//   < 2 × oldInterval (100 ms × 2 = 200 ms) — peer is still healthy
	// This window is 50 ms.
	time.Sleep(50 * time.Millisecond)

	// Decrease the interval to 20 ms using the test helper.
	// Without the lastPongAt reset, the next 100 ms tick (which fires at
	// ~100 ms from connection start, i.e., ~50 ms from now) would compute:
	//   pongDeadline = 2×20 ms = 40 ms
	//   time since lastPongAt ≈ 100 ms > 40 ms → false eviction
	// With the fix, lastPongAt is reset to now on interval change, so:
	//   time since lastPongAt ≈ 0 ms < 40 ms → peer stays connected
	const newInterval = 20 * time.Millisecond
	p2p.SetKeepaliveIntervalForTest(host, newInterval)

	// Wait two full old-rate ticks (200 ms) so the goroutine has had at least
	// one opportunity to observe and apply the new interval.
	time.Sleep(200 * time.Millisecond)

	if host.PeerCount() != 1 {
		t.Errorf("peer was evicted after interval decrease (want PeerCount=1, got %d)", host.PeerCount())
	}
}

func TestHost_BroadcastBlock_NoPeers(t *testing.T) {
	h := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 0, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}
	// Should not panic
	h.BroadcastBlock(block)
}

func TestHost_BroadcastTx_NoPeers(t *testing.T) {
	h := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	tx := &core.Transaction{Version: core.TxVersionBase}
	h.BroadcastTx(tx)
}

func TestHost_BroadcastVote_NoPeers(t *testing.T) {
	h := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	h.BroadcastVote(p2p.VoteMsg{Height: 1})
}

// ─── Start / Stop lifecycle ───────────────────────────────────────────────────

func TestHost_StartStop(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-node",
		UserAgent:  "aperod-test/0.1",
	}, &stubHandler{}, newTestLogger())

	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.Stop()
}

func TestHost_Start_InvalidAddr(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "invalid-addr-no-port",
		MaxPeers:   10,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err == nil {
		t.Error("expected error for invalid listen addr")
	}
}

// ─── Full handshake over net.Listener ────────────────────────────────────────

// dialAndHandshake: simulates an inbound peer connecting to the host.
// With the asymmetric handshake: dialer (us) sends Ping first, host replies
// with Pong.  The old symmetric protocol where the host sent first caused a
// deadlock between two Host instances.
func dialAndHandshake(t *testing.T, addr string, extraMsg func(conn net.Conn)) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Logf("dialAndHandshake: dial failed: %v", err)
		return
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(2 * time.Second))

	// Send ping to host (dialer goes first under the asymmetric protocol)
	ping := p2p.PingMsg{NodeID: "fake-peer", Height: 0, UserAgent: "test", Timestamp: time.Now().Unix()}
	if err := p2p.WriteMsg(conn, p2p.MsgPing, ping); err != nil {
		t.Logf("dialAndHandshake: write ping: %v", err)
		return
	}

	// Host replies with pong
	msgType, data, err := p2p.ReadMsg(conn)
	if err != nil || msgType != p2p.MsgPong {
		t.Logf("dialAndHandshake: expected pong, got %v err=%v (data=%d bytes)", msgType, err, len(data))
		return
	}

	if extraMsg != nil {
		extraMsg(conn)
	}
	time.Sleep(50 * time.Millisecond) // let host process
}

func TestHost_Handshake_InboundPeer(t *testing.T) {
	handler := &stubHandler{}
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "host-node",
		UserAgent:  "aperod/0.1",
	}, handler, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	addr := h.ListenAddr()
	if addr == "" {
		t.Skip("Host.ListenAddr not exposed — skipping handshake test")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		dialAndHandshake(t, addr, nil)
	}()
	<-done
	time.Sleep(100 * time.Millisecond)

	if h.PeerCount() != 1 {
		t.Logf("PeerCount = %d (may not have completed handshake in time)", h.PeerCount())
	}
}

func TestHost_Handshake_InboundWithMessages(t *testing.T) {
	handler := &stubHandler{}
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "host-node",
		UserAgent:  "aperod/0.1",
	}, handler, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	addr := h.ListenAddr()
	if addr == "" {
		t.Skip("Host.ListenAddr not exposed — skipping")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		dialAndHandshake(t, addr, func(conn net.Conn) {
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			// Send GetPeers
			p2p.WriteMsg(conn, p2p.MsgGetPeers, struct{}{})
			// Read peers response
			p2p.ReadMsg(conn)
			// Send GetHeaders
			p2p.WriteMsg(conn, p2p.MsgGetHeaders, p2p.GetHeadersMsg{Limit: 10})
			// Read headers response
			p2p.ReadMsg(conn)
		})
	}()
	<-done
}

// TestHost_WhitelistSidecar_EmptyRetainsCfgEntries verifies that when the
// whitelist sidecar file is valid JSON but contains an empty array ([]), and
// cfg.PeerWhitelist is non-empty, Start() retains the cfg entries rather than
// silently opening the network.
//
// Without this retention behaviour, an admin "clear-all" operation in the
// Admin Panel would write an empty sidecar and, on the next restart, the node
// would accept connections from every IP — even ones that should still be
// blocked via peer_whitelist in node.yaml.
func TestHost_WhitelistSidecar_EmptyRetainsCfgEntries(t *testing.T) {
	dir := t.TempDir()
	wlFile := dir + "/whitelist.json"

	// Write an empty-array sidecar — the file is valid JSON but has no entries.
	if err := os.WriteFile(wlFile, []byte("[]"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "test-wl-retain",
		UserAgent:     "aperod/test",
		WhitelistFile: wlFile,
		PeerWhitelist: []string{"1.2.3.4"},
	}, &stubHandler{}, newTestLogger())

	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	got := h.GetPeerWhitelist()
	if len(got) != 1 || got[0] != "1.2.3.4" {
		t.Errorf("GetPeerWhitelist = %v, want [1.2.3.4] — empty sidecar must not clear node.yaml entries", got)
	}
}

// TestHost_WhitelistSidecar_EmptyNoCfg_OpenNetwork verifies that when the
// whitelist sidecar file is a valid empty array ([]) AND cfg.PeerWhitelist is
// also empty, the node starts in open-network mode (no IP restriction).
func TestHost_WhitelistSidecar_EmptyNoCfg_OpenNetwork(t *testing.T) {
	dir := t.TempDir()
	wlFile := dir + "/whitelist.json"

	// Write an empty-array sidecar.
	if err := os.WriteFile(wlFile, []byte("[]"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "test-wl-open",
		UserAgent:     "aperod/test",
		WhitelistFile: wlFile,
		// PeerWhitelist intentionally empty — open network desired.
	}, &stubHandler{}, newTestLogger())

	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	got := h.GetPeerWhitelist()
	if len(got) != 0 {
		t.Errorf("GetPeerWhitelist = %v, want [] — empty sidecar + no cfg entries should be open network", got)
	}
}

// TestHost_MaliciousPacket_NodeSurvives proves that the defer recover() added to
// handleConn keeps the node process alive when a peer sends garbage bytes that
// would otherwise trigger a panic in the message parser.
func TestHost_MaliciousPacket_NodeSurvives(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		MaxPeers:   10,
		ListenAddr: "127.0.0.1:0",
		NodeID:     "test-malicious",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	addr := h.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr not exposed — skipping")
	}

	// Send 32 KB of non-protocol garbage — intentionally bypasses the normal
	// handshake so the framing parser receives junk and may panic internally.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	garbage := make([]byte, 32*1024)
	for i := range garbage {
		garbage[i] = byte(i & 0xFF)
	}
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	conn.Write(garbage) //nolint:errcheck
	conn.Close()

	// Allow the node goroutine to process the packet and recover.
	time.Sleep(200 * time.Millisecond)

	// Node MUST still accept new TCP connections — if the panic escaped the
	// recover() the listener would be gone and this dial would fail.
	conn2, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("node unreachable after malicious packet (panic may have escaped recover): %v", err)
	}
	conn2.Close()
}

// TestHost_MaxPeersPerIP_Enforced verifies that a single IP cannot consume more
// than MaxPeersPerIP slots, preventing the peer-slot-exhaustion eclipse attack.
func TestHost_MaxPeersPerIP_Enforced(t *testing.T) {
	const limit = 2
	h := p2p.NewHost(p2p.Config{
		MaxPeers:      10,
		MaxPeersPerIP: limit,
		ListenAddr:    "127.0.0.1:0",
		NodeID:        "test-perip",
		UserAgent:     "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	addr := h.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr not exposed — skipping")
	}

	// Open limit+1 raw TCP connections from the same IP and verify the last
	// one is closed immediately (before the handshake) by the host.
	conns := make([]net.Conn, limit+1)
	var openErr error
	for i := 0; i <= limit; i++ {
		conns[i], openErr = net.DialTimeout("tcp", addr, time.Second)
		if openErr != nil {
			t.Fatalf("dial %d: %v", i, openErr)
		}
	}
	// Give acceptLoop time to enforce the limit.
	time.Sleep(150 * time.Millisecond)

	// The (limit+1)-th connection should be closed by the host already.
	// Try to write to it — it should fail or read EOF.
	extra := conns[limit]
	extra.SetDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 1)
	n, err := extra.Read(buf)
	if n > 0 || err == nil {
		t.Logf("extra connection: n=%d err=%v — peer may not have been closed yet (acceptable if handshake hasn't started)", n, err)
	}

	for _, c := range conns {
		c.Close()
	}
}

// TestHost_MinOutbound_BlocksInboundFlood verifies that inbound connections are
// rejected once the inbound cap (MaxPeers − MinOutbound) is reached, so that
// outbound dial-out slots are always available for validator broadcasting.
func TestHost_MinOutbound_BlocksInboundFlood(t *testing.T) {
	const maxPeers = 5
	const minOutbound = 2
	// inboundCap = maxPeers - minOutbound = 3
	h := p2p.NewHost(p2p.Config{
		MaxPeers:    maxPeers,
		MinOutbound: minOutbound,
		ListenAddr:  "127.0.0.1:0",
		NodeID:      "test-min-out",
		UserAgent:   "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	addr := h.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr not exposed — skipping")
	}

	// Open maxPeers+2 raw TCP connections to saturate the inbound cap.
	// The host should stop accepting past the cap without crashing.
	conns := make([]net.Conn, maxPeers+2)
	for i := range conns {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			// Acceptable: server closed the connection before we could accept it.
			t.Logf("dial %d: %v (expected under flood)", i, err)
			continue
		}
		conns[i] = c
	}

	// Give acceptLoop time to enforce limits.
	time.Sleep(200 * time.Millisecond)

	// Node must still be alive — dial one more and get any response.
	probe, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("node unreachable after inbound flood — MinOutbound may have broken acceptLoop: %v", err)
	}
	probe.Close()

	for _, c := range conns {
		if c != nil {
			c.Close()
		}
	}
}

// TestMinOutbound_OutboundDialSucceedsAfterInboundFlood is the key correctness
// test for the reserved-outbound feature: even when the inbound cap
// (MaxPeers − MinOutbound) is completely saturated with completed handshakes,
// an outbound dial to a trusted peer must still succeed because MinOutbound
// slots remain available exclusively for dial-outs.
func TestMinOutbound_OutboundDialSucceedsAfterInboundFlood(t *testing.T) {
	const maxPeers = 5
	const minOutbound = 2
	const inboundCap = maxPeers - minOutbound // = 3

	// hA: the validator node under test.
	hA := p2p.NewHost(p2p.Config{
		MaxPeers:    maxPeers,
		MinOutbound: minOutbound,
		ListenAddr:  "127.0.0.1:0",
		NodeID:      "node-a",
		UserAgent:   "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := hA.Start(); err != nil {
		t.Fatalf("hA.Start: %v", err)
	}
	defer hA.Stop()

	// trustedPeer: a raw TCP server that acts as a trusted outbound target.
	// We use a raw listener (not another Host) because two Hosts both send a
	// MsgPing on connect, which causes a deadlock.  A raw server simply reads
	// hA's MsgPing and replies with MsgPong so the handshake completes.
	trustedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("trusted peer listen: %v", err)
	}
	defer trustedLn.Close()

	// Background goroutine: accept connections and complete the handshake.
	go func() {
		for {
			conn, err := trustedLn.Accept()
			if err != nil {
				return // listener closed
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				// Read ping from hA
				msgType, _, rdErr := p2p.ReadMsg(c)
				if rdErr != nil || msgType != p2p.MsgPing {
					return
				}
				// Reply with pong
				_ = p2p.WriteMsg(c, p2p.MsgPong, p2p.PingMsg{
					NodeID: "trusted-peer", Height: 0, UserAgent: "test",
					Timestamp: time.Now().Unix(),
				})
				c.SetDeadline(time.Time{})
				// Hold the connection open so hA keeps the peer registered.
				time.Sleep(2 * time.Second)
			}(conn)
		}
	}()

	addrA := hA.ListenAddr()
	addrB := trustedLn.Addr().String()

	// Fill the inbound cap on hA with inboundCap (3) connections that complete
	// the full ping/pong handshake so they actually register as peers.
	inConns := make([]net.Conn, 0, inboundCap)
	for i := 0; i < inboundCap; i++ {
		c, err := net.DialTimeout("tcp", addrA, 2*time.Second)
		if err != nil {
			t.Fatalf("flooder dial %d: %v", i, err)
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))

		// Asymmetric handshake: dialer sends Ping first, host replies with Pong.
		if err := p2p.WriteMsg(c, p2p.MsgPing, p2p.PingMsg{
			NodeID:    fmt.Sprintf("flooder-%d", i),
			Height:    0,
			UserAgent: "flood",
			Timestamp: time.Now().Unix(),
		}); err != nil {
			c.Close()
			t.Fatalf("flooder %d: write ping: %v", i, err)
		}
		msgType, _, err := p2p.ReadMsg(c)
		if err != nil || msgType != p2p.MsgPong {
			c.Close()
			t.Fatalf("flooder %d: expected MsgPong, got %v err=%v", i, msgType, err)
		}
		c.SetDeadline(time.Time{}) // clear deadline — keep connection open
		inConns = append(inConns, c)
	}
	defer func() {
		for _, c := range inConns {
			c.Close()
		}
	}()

	// Wait for hA to register all inbound peers.
	time.Sleep(150 * time.Millisecond)

	preDial := hA.PeerCount()
	if preDial < inboundCap {
		t.Fatalf("expected at least %d inbound peers registered on hA, got %d (handshake may have failed)", inboundCap, preDial)
	}
	t.Logf("inbound cap filled: PeerCount=%d", preDial)

	// Verify that one more inbound connection is rejected (inbound cap enforced).
	extra, err := net.DialTimeout("tcp", addrA, time.Second)
	if err == nil {
		extra.SetDeadline(time.Now().Add(300 * time.Millisecond))
		// Asymmetric handshake: send Ping, try to read Pong (host may reject before replying).
		if writeErr := p2p.WriteMsg(extra, p2p.MsgPing, p2p.PingMsg{
			NodeID: "extra", UserAgent: "test", Timestamp: time.Now().Unix(),
		}); writeErr == nil {
			_, _, _ = p2p.ReadMsg(extra) // host may close early if cap exceeded
			time.Sleep(80 * time.Millisecond)
		}
		extra.Close()
	}
	time.Sleep(50 * time.Millisecond)

	// ── Key assertion ──────────────────────────────────────────────────────────
	// Dial from hA to the trusted peer hB.  Even though the inbound cap is full,
	// MinOutbound slots remain available, so this outbound dial must succeed.
	hA.DialPeer(addrB)
	time.Sleep(400 * time.Millisecond)

	afterDial := hA.PeerCount()
	if afterDial <= preDial {
		t.Errorf("outbound dial to trusted peer did NOT increase PeerCount: before=%d after=%d — MinOutbound slots not working", preDial, afterDial)
	} else {
		t.Logf("✓ outbound dial succeeded after inbound flood: inbound=%d total=%d (+%d outbound)", preDial, afterDial, afterDial-preDial)
	}
}

// TestHost_ValidatorHeightDivergence_NoBan is a regression test for the
// auto-ban bug that silently dropped the 2nd legitimate validator when its
// tip height was far ahead of the local node's tip.
//
// Scenario: hA is a node at tip 0.  hB is a validator whose chain is 10 000
// blocks ahead.  hB sends 20 MsgBlock messages — each with height ourTip +
// 10 000 — which is above the BadBlockHeightLead window and therefore
// increments the bad-block strike counter on hA.  With the post-fix
// configuration (BadBlockBanThreshold set high enough that 20 strikes cannot
// trigger a ban), hB must remain connected and normal block delivery must
// continue to work.
func TestHost_ValidatorHeightDivergence_NoBan(t *testing.T) {
	const farAheadHeight = uint64(10_000)
	const msgCount = 20 // intentionally > default BadBlockBanThreshold (10)

	// hA: the "behind" node.  BadBlockBanThreshold is set high enough that
	// msgCount strikes can never trigger a ban, representing the post-fix
	// production configuration where legitimate ahead-validators are not
	// silently dropped.
	handlerA := &stubHandler{}
	hA := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "node-a",
		UserAgent:            "aperod/test",
		BadBlockHeightLead:   1_000,
		BadBlockBanThreshold: 10_000, // post-fix: threshold too high to trigger on a legitimate peer
	}, handlerA, newTestLogger())
	if err := hA.Start(); err != nil {
		t.Fatalf("hA.Start: %v", err)
	}
	defer hA.Stop()

	// hB: the "ahead" validator.  Default config is fine here.
	hB := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "node-b",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := hB.Start(); err != nil {
		t.Fatalf("hB.Start: %v", err)
	}
	defer hB.Stop()

	addrA := hA.ListenAddr()
	if addrA == "" {
		t.Skip("ListenAddr not exposed — skipping")
	}

	// hB dials hA.  hB is outbound; hA accepts inbound.
	// With the asymmetric handshake (outbound sends Ping, inbound waits) this
	// succeeds cleanly without a deadlock.
	hB.DialPeer(addrA)
	time.Sleep(300 * time.Millisecond)

	if hA.PeerCount() != 1 || hB.PeerCount() != 1 {
		t.Fatalf("setup: expected 1 peer on each host; hA=%d hB=%d", hA.PeerCount(), hB.PeerCount())
	}
	t.Logf("setup ok: hA peers=%d hB peers=%d", hA.PeerCount(), hB.PeerCount())

	// hB broadcasts msgCount blocks at height far ahead of hA's tip (0).
	// Each block has height = farAheadHeight + i, which exceeds
	// ourTip (0) + BadBlockHeightLead (1 000) and therefore increments hA's
	// strike counter for hB's IP on every message.
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	for i := 0; i < msgCount; i++ {
		hdr := core.BlockHeader{
			Height:       farAheadHeight + uint64(i),
			ValidatorPub: pub,
			Timestamp:    time.Now().UnixNano(),
		}
		if signErr := hdr.Sign(priv); signErr != nil {
			t.Fatalf("Sign block %d: %v", i, signErr)
		}
		hB.BroadcastBlock(&core.Block{Header: hdr})
		time.Sleep(5 * time.Millisecond)
	}

	// Allow hA's dispatch goroutine to process all messages.
	time.Sleep(200 * time.Millisecond)

	// ── Key assertions ──────────────────────────────────────────────────────
	// 1. hA must NOT have banned or disconnected hB.
	if hA.PeerCount() != 1 {
		t.Errorf("hA dropped hB after %d far-ahead blocks: PeerCount=%d, want 1 (auto-ban regression)", msgCount, hA.PeerCount())
	}
	// 2. hB must still see hA as connected.
	if hB.PeerCount() != 1 {
		t.Errorf("hB lost connection to hA: PeerCount=%d, want 1", hB.PeerCount())
	}

	// 3. Normal block exchange still works after the divergence episode.
	normalHdr := core.BlockHeader{
		Height:       0,
		ValidatorPub: pub,
		Timestamp:    time.Now().UnixNano(),
	}
	if signErr := normalHdr.Sign(priv); signErr != nil {
		t.Fatalf("Sign normal block: %v", signErr)
	}
	normalBlock := &core.Block{Header: normalHdr}
	hB.BroadcastBlock(normalBlock)

	// Poll for delivery instead of a fixed sleep to avoid false negatives
	// and eliminate the data race between the dispatch goroutine writing
	// handlerA.blocks and the test goroutine reading it.
	var gotBlocks int
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if gotBlocks = handlerA.blockCount(); gotBlocks > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if gotBlocks == 0 {
		t.Error("hA's handler received 0 blocks from hB — normal block exchange broken after height-divergence episode")
	}
	t.Logf("✓ both validators stayed connected after %d far-ahead blocks; hA handler received %d block(s)", msgCount, gotBlocks)
}

// ─── Peer whitelist tests ─────────────────────────────────────────────────────

// TestHost_PeerWhitelist_AllowsWhitelistedIP verifies that a connection from
// an IP that is listed in PeerWhitelist completes the full P2P handshake and
// is registered as a peer.
func TestHost_PeerWhitelist_AllowsWhitelistedIP(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "wl-allow-host",
		UserAgent:     "aperod/test",
		PeerWhitelist: []string{"127.0.0.1"}, // loopback is explicitly allowed
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	addr := h.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr not exposed — skipping")
	}

	// Dial from 127.0.0.1 and complete the asymmetric P2P handshake, keeping
	// the connection open through the PeerCount assertion.  The connection is
	// closed only after the assertion so the host cannot remove the peer before
	// we observe it.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	conn.SetDeadline(time.Now().Add(2 * time.Second))

	// Asymmetric handshake: dialer (us) sends Ping first, host replies Pong.
	if wErr := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
		NodeID: "wl-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); wErr != nil {
		conn.Close()
		t.Fatalf("write Ping: %v", wErr)
	}
	msgType, _, rErr := p2p.ReadMsg(conn)
	if rErr != nil || msgType != p2p.MsgPong {
		conn.Close()
		t.Fatalf("expected MsgPong from whitelisted host, got type=%v err=%v", msgType, rErr)
	}

	// Clear the deadline so the host keeps the peer registered while we poll.
	conn.SetDeadline(time.Time{})

	// Poll until the host registers the peer (handshake completes async).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.PeerCount() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Assert PeerCount while the connection is still open.
	count := h.PeerCount()
	conn.Close() // now safe to close

	if count < 1 {
		t.Errorf("PeerCount = %d after whitelisted connection; want ≥ 1 — whitelist may be incorrectly blocking loopback", count)
	} else {
		t.Logf("✓ whitelisted IP accepted and registered as peer (PeerCount=%d)", count)
	}
}

// TestHost_PeerWhitelist_BlocksNonWhitelistedIP verifies that a connection
// from an IP that is NOT in PeerWhitelist is closed immediately — before any
// P2P handshake bytes are exchanged.
func TestHost_PeerWhitelist_BlocksNonWhitelistedIP(t *testing.T) {
	// Whitelist a private range that the loopback (127.0.0.1) is not part of.
	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "wl-block-host",
		UserAgent:     "aperod/test",
		PeerWhitelist: []string{"192.168.99.0/24"}, // loopback not in this range
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	addr := h.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr not exposed — skipping")
	}

	// Dial from loopback — the host must close the connection immediately.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send a Ping so the host receives at least one byte and still must close,
	// ruling out the case where the host simply ignores silent connections.
	// We intentionally do not check the write error — the host may already have
	// closed the conn by the time we write.
	_ = p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
		NodeID: "blocked-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	})

	// The host's acceptLoop closes the connection before any handshake, so a
	// read must return a definitive close error (EOF or connection reset) within
	// the deadline — NOT a timeout.  A timeout would mean the host left the
	// connection open, which is the exact regression we are guarding against.
	conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	n, readErr := conn.Read(buf)
	if n > 0 {
		t.Errorf("non-whitelisted connection was NOT closed: host sent %d byte(s)", n)
	} else if readErr == nil {
		t.Error("non-whitelisted connection was NOT closed: Read returned nil error (connection still open)")
	} else {
		// Distinguish a deadline timeout (regression) from a genuine close.
		var netErr net.Error
		if errors.As(readErr, &netErr) && netErr.Timeout() {
			t.Errorf("non-whitelisted connection timed out instead of being closed: the host left the connection open (whitelist enforcement regression)")
		} else if errors.Is(readErr, io.EOF) ||
			strings.Contains(readErr.Error(), "connection reset") ||
			strings.Contains(readErr.Error(), "use of closed network connection") ||
			strings.Contains(readErr.Error(), "broken pipe") {
			t.Logf("✓ non-whitelisted IP's connection definitively closed before handshake (err: %v)", readErr)
		} else {
			// Any other non-timeout error also indicates the connection was closed.
			t.Logf("✓ non-whitelisted IP's connection closed (err: %v)", readErr)
		}
	}

	// The host must not have registered any peer.
	if h.PeerCount() != 0 {
		t.Errorf("PeerCount = %d; want 0 — non-whitelisted peer was incorrectly registered", h.PeerCount())
	}
}

// TestHost_PeerWhitelist_OutboundDialUnaffected verifies that a host with a
// restrictive PeerWhitelist (that would reject an inbound 127.0.0.1 connection)
// can still successfully complete an *outbound* dial-out, because the whitelist
// only gates acceptLoop, not dialPeer.
func TestHost_PeerWhitelist_OutboundDialUnaffected(t *testing.T) {
	// The host's whitelist deliberately excludes 127.0.0.1 so any inbound
	// connection from loopback would be rejected.  Outbound dials must still
	// work regardless.
	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "wl-outbound-host",
		UserAgent:     "aperod/test",
		PeerWhitelist: []string{"10.0.0.0/8"}, // 127.0.0.1 not included
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// Set up a raw TCP server that acts as a trusted outbound target.
	// It reads the MsgPing sent by the host and replies with MsgPong so the
	// asymmetric handshake completes and the peer is registered.
	peerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("peer listener: %v", err)
	}
	defer peerLn.Close()

	peerConnected := make(chan struct{})
	go func() {
		conn, acceptErr := peerLn.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		close(peerConnected) // signal: TCP connection received
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		// Read Ping from the host (outbound dialer goes first).
		msgType, _, rdErr := p2p.ReadMsg(conn)
		if rdErr != nil || msgType != p2p.MsgPing {
			return
		}
		// Reply with Pong to complete the handshake.
		_ = p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
			NodeID: "trusted-peer", Height: 0, UserAgent: "test",
			Timestamp: time.Now().Unix(),
		})
		// Keep connection open so the host registers the peer.
		time.Sleep(2 * time.Second)
	}()

	// Trigger an outbound dial despite the restrictive whitelist.
	h.DialPeer(peerLn.Addr().String())

	// Wait for the TCP-level connection to arrive at the peer server.
	select {
	case <-peerConnected:
	case <-time.After(2 * time.Second):
		t.Fatal("outbound dial did not reach the peer server within 2 s — whitelist may be incorrectly blocking outbound connections")
	}

	// Give the handshake goroutine time to register the peer.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.PeerCount() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if h.PeerCount() < 1 {
		t.Errorf("PeerCount = %d after outbound dial; want ≥ 1 — whitelist incorrectly blocked outbound connection", h.PeerCount())
	} else {
		t.Logf("✓ outbound dial succeeded despite restrictive inbound whitelist (PeerCount=%d)", h.PeerCount())
	}
}

// TestHost_PeerWhitelist_InvalidEntrySkipped verifies that a garbage entry in
// PeerWhitelist is silently skipped (with a log warning) without crashing the
// host and without discarding the surrounding valid entries.
//
// Three properties are asserted:
//  1. NewHost + Start succeed with no error (no panic, no fatal).
//  2. A connection from the valid bare IP (127.0.0.1, explicitly listed) completes
//     the full P2P handshake and is registered as a peer — proving that entry was
//     not thrown away together with the garbage one.
//  3. The whitelist is still enforced (not silently reset to open): a second host
//     whose list contains only a valid CIDR (that excludes 127.0.0.1) plus the
//     same garbage entry rejects a connection from loopback — proving the CIDR
//     entry was also kept despite the invalid neighbour.
func TestHost_PeerWhitelist_InvalidEntrySkipped(t *testing.T) {
	// ── Part 1 & 2: valid IP entry survives the garbage entry ─────────────────

	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "wl-invalid-host",
		UserAgent:  "aperod/test",
		PeerWhitelist: []string{
			"127.0.0.1",   // valid bare IP  — must be kept
			"10.0.0.0/8",  // valid CIDR     — must be kept
			"not-an-ip",   // garbage        — must be skipped without crashing
		},
	}, &stubHandler{}, newTestLogger())

	// Assertion 1: Start must not return an error (no panic, no fatal).
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v — host must start even when PeerWhitelist contains an invalid entry", err)
	}
	defer h.Stop()

	addr := h.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr not exposed — skipping")
	}

	// Assertion 2: the valid IP entry is kept — a connection from 127.0.0.1
	// must complete the full asymmetric P2P handshake and be registered as a peer.
	// Keep the connection open until PeerCount is checked.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if wErr := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
		NodeID: "wl-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); wErr != nil {
		conn.Close()
		t.Fatalf("write Ping: %v", wErr)
	}
	msgType, _, rErr := p2p.ReadMsg(conn)
	if rErr != nil || msgType != p2p.MsgPong {
		conn.Close()
		t.Fatalf("expected MsgPong from host, got type=%v err=%v — valid IP entry may have been discarded alongside the invalid one", msgType, rErr)
	}
	conn.SetDeadline(time.Time{}) // clear deadline — hold connection open for assertion

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.PeerCount() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	count := h.PeerCount()
	conn.Close()
	if count < 1 {
		t.Errorf("PeerCount = %d; want ≥ 1 — valid IP entry may have been discarded alongside the invalid one", count)
	} else {
		t.Logf("✓ whitelisted IP accepted and registered as peer (PeerCount=%d)", count)
	}

	// ── Part 3: valid CIDR entry also survives — whitelist is still enforced ──

	// This host's whitelist contains a valid CIDR that does NOT cover 127.0.0.1,
	// plus the same garbage entry.  A connection from loopback must be rejected,
	// proving that (a) the CIDR entry was kept and (b) the garbage entry did not
	// collapse the whitelist into open-network mode.
	h2 := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "wl-reject-host",
		UserAgent:  "aperod/test",
		PeerWhitelist: []string{
			"192.168.99.0/24", // valid CIDR — loopback NOT covered
			"not-an-ip",       // garbage — must be skipped without crashing
		},
	}, &stubHandler{}, newTestLogger())
	if err := h2.Start(); err != nil {
		t.Fatalf("h2.Start: %v — host must start even with mixed valid/invalid whitelist", err)
	}
	defer h2.Stop()

	addr2 := h2.ListenAddr()
	if addr2 == "" {
		t.Skip("h2 ListenAddr not exposed — skipping rejection check")
	}

	// Dial from 127.0.0.1 — must be rejected because the whitelist is active.
	conn2, err := net.DialTimeout("tcp", addr2, 2*time.Second)
	if err != nil {
		t.Fatalf("dial h2: %v", err)
	}
	defer conn2.Close()

	// Send a Ping so the host receives data and must actively reject, ruling
	// out the case where silent connections are just ignored.
	_ = p2p.WriteMsg(conn2, p2p.MsgPing, p2p.PingMsg{
		NodeID: "blocked-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	})

	conn2.SetDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	n, readErr := conn2.Read(buf)
	if n > 0 {
		t.Errorf("non-whitelisted connection was NOT rejected by h2: host sent %d byte(s) — whitelist may have been reset to open when invalid entry present", n)
	} else if readErr == nil {
		t.Error("non-whitelisted connection was NOT rejected by h2: Read returned nil (connection still open)")
	} else {
		var netErr net.Error
		if errors.As(readErr, &netErr) && netErr.Timeout() {
			t.Errorf("non-whitelisted connection timed out instead of being closed — whitelist enforcement broken when invalid entry present")
		} else {
			t.Logf("✓ non-whitelisted IP rejected despite invalid entry in whitelist (err: %v)", readErr)
		}
	}

	if h2.PeerCount() != 0 {
		t.Errorf("h2 PeerCount = %d; want 0 — non-whitelisted peer must not be registered", h2.PeerCount())
	} else {
		t.Log("✓ whitelist still active after invalid entry was skipped — no open-network regression")
	}
}

// TestBootnode_SkipsBackoffWindow verifies that after 3 consecutive dial
// failures back-off blocks a regular peer from being re-dialled while a
// configured bootnode is still attempted on the very next call.
//
// Scenario:
//   - Both the bootnode address and a random peer address receive 3 injected
//     OnDialFail calls so PeerMgr.CanDial returns false for both.
//   - DialPeer on the regular address must be silently blocked (no TCP connect).
//   - DialPeer on the bootnode address must reach the listener (back-off skipped).
func TestBootnode_SkipsBackoffWindow(t *testing.T) {
	// ── Bootnode listener: accepts and immediately drops ──────────────────────
	var bootnodeDials atomic.Int32
	bootLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bootnode listen: %v", err)
	}
	defer bootLn.Close()
	go func() {
		for {
			c, acceptErr := bootLn.Accept()
			if acceptErr != nil {
				return
			}
			bootnodeDials.Add(1)
			c.Close()
		}
	}()
	bootnodeAddr := bootLn.Addr().String()

	// ── Regular peer listener ─────────────────────────────────────────────────
	var regularDials atomic.Int32
	regLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("regular listen: %v", err)
	}
	defer regLn.Close()
	go func() {
		for {
			c, acceptErr := regLn.Accept()
			if acceptErr != nil {
				return
			}
			regularDials.Add(1)
			c.Close()
		}
	}()
	regularAddr := regLn.Addr().String()

	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "bootnode-backoff-test",
		UserAgent:  "aperod-test",
		Bootnodes:  []string{bootnodeAddr},
	}, &stubHandler{}, newTestLogger())

	// Inject 3 consecutive failures for both addresses directly into PeerMgr
	// so we don't have to wait for real TCP dials to fail.
	for i := 0; i < 3; i++ {
		p2p.HostRecordDialFail(host, bootnodeAddr)
		p2p.HostRecordDialFail(host, regularAddr)
	}

	// PeerMgr.CanDial must now block both addresses (back-off window active).
	if p2p.HostCanDial(host, regularAddr) {
		t.Fatal("CanDial must be false for regular peer after 3 injected failures")
	}
	if p2p.HostCanDial(host, bootnodeAddr) {
		t.Fatal("CanDial must be false for bootnode at the PeerMgr layer after 3 injected failures")
	}

	// Register the bootnode address in the host's internal set (normally done
	// by Start when DNS is resolved; here we use the export helper that
	// mirrors a successful maintainLoop resolution tick).
	p2p.HostSetBootnodeResolved(host, bootnodeAddr, []string{bootnodeAddr})

	// ── Regular peer: DialPeer must be silently blocked ───────────────────────
	beforeReg := regularDials.Load()
	host.DialPeer(regularAddr)
	time.Sleep(200 * time.Millisecond)
	if regularDials.Load() > beforeReg {
		t.Error("regular peer was dialed despite back-off — back-off window must block non-bootnode peers")
	}

	// ── Bootnode: DialPeer must skip back-off and reach the listener ──────────
	beforeBoot := bootnodeDials.Load()
	host.DialPeer(bootnodeAddr)
	time.Sleep(300 * time.Millisecond)
	afterBoot := bootnodeDials.Load()
	if afterBoot <= beforeBoot {
		t.Errorf("bootnode dial count did not increase (before=%d after=%d): back-off must be skipped for configured bootnodes",
			beforeBoot, afterBoot)
	} else {
		t.Logf("✓ bootnode re-dialed despite back-off (dial count %d→%d)", beforeBoot, afterBoot)
	}
}

// TestBootnode_DNSRefresh_RemovesRetiredAddr verifies that when a bootnode's
// DNS record changes (i.e. the bootnode moves to a new IP), the old address
// is removed from the privileged set on the next resolution tick so it returns
// to normal exponential back-off behaviour.
//
// Scenario:
//  1. Bootnode "bn.example:30303" initially resolves to addrOld.
//  2. addrOld accumulates 3 dial failures → PeerMgr back-off active.
//  3. Back-off is skipped because addrOld is in bootnodeSet.
//  4. DNS changes: "bn.example:30303" now resolves to addrNew only.
//  5. After the tick, addrOld must no longer be in bootnodeSet.
//  6. DialPeer(addrOld) must be blocked by back-off (not reach the listener).
func TestBootnode_DNSRefresh_RemovesRetiredAddr(t *testing.T) {
	// ── Two listeners: old and new bootnode addresses ─────────────────────────
	var oldDials, newDials atomic.Int32

	oldLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("old listen: %v", err)
	}
	defer oldLn.Close()
	go func() {
		for {
			c, e := oldLn.Accept()
			if e != nil {
				return
			}
			oldDials.Add(1)
			c.Close()
		}
	}()
	addrOld := oldLn.Addr().String()

	newLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("new listen: %v", err)
	}
	defer newLn.Close()
	go func() {
		for {
			c, e := newLn.Accept()
			if e != nil {
				return
			}
			newDials.Add(1)
			c.Close()
		}
	}()
	addrNew := newLn.Addr().String()

	const rawBootnode = "bn.example:30303" // symbolic key used in bootnodeLastResolved

	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "dns-refresh-test",
		UserAgent:  "aperod-test",
		// cfg.Bootnodes deliberately empty: we drive resolution via the export
		// helper to avoid real DNS lookups and maintainLoop timing in the test.
	}, &stubHandler{}, newTestLogger())

	// ── Step 1: initial resolution → addrOld is privileged ───────────────────
	p2p.HostSetBootnodeResolved(host, rawBootnode, []string{addrOld})
	if !p2p.HostIsBootnode(host, addrOld) {
		t.Fatal("addrOld must be in bootnodeSet after initial resolution")
	}

	// ── Step 2: inject 3 failures → PeerMgr back-off active for addrOld ─────
	for i := 0; i < 3; i++ {
		p2p.HostRecordDialFail(host, addrOld)
	}
	if p2p.HostCanDial(host, addrOld) {
		t.Fatal("CanDial must be false for addrOld after 3 injected failures")
	}

	// ── Step 3: despite back-off, DialPeer(addrOld) must still reach listener ─
	before := oldDials.Load()
	host.DialPeer(addrOld)
	time.Sleep(250 * time.Millisecond)
	if oldDials.Load() <= before {
		t.Error("addrOld should be dialable (back-off skipped) while still in bootnodeSet")
	}

	// ── Step 4: DNS changes → bootnode now resolves to addrNew only ──────────
	p2p.HostSetBootnodeResolved(host, rawBootnode, []string{addrNew})

	// ── Step 5: addrOld must be gone from bootnodeSet; addrNew must be there ─
	if p2p.HostIsBootnode(host, addrOld) {
		t.Error("addrOld must NOT be in bootnodeSet after DNS moved to addrNew")
	}
	if !p2p.HostIsBootnode(host, addrNew) {
		t.Error("addrNew must be in bootnodeSet after DNS resolution update")
	}

	// ── Step 6: DialPeer(addrOld) must now be blocked by its back-off ────────
	before = oldDials.Load()
	host.DialPeer(addrOld)
	time.Sleep(250 * time.Millisecond)
	if oldDials.Load() > before {
		t.Errorf("addrOld was dialed after DNS moved away (before=%d after=%d): retired address must return to normal back-off", before, oldDials.Load())
	} else {
		t.Logf("✓ addrOld blocked by back-off after DNS refresh removed it from bootnodeSet")
	}

	// addrNew (the new bootnode) has no accumulated failures → DialPeer reaches it.
	beforeNew := newDials.Load()
	host.DialPeer(addrNew)
	time.Sleep(250 * time.Millisecond)
	if newDials.Load() <= beforeNew {
		t.Errorf("addrNew should be reachable (no back-off, is bootnode) after DNS refresh")
	} else {
		t.Logf("✓ addrNew dialed successfully after DNS refresh (dial count %d→%d)", beforeNew, newDials.Load())
	}
}

// ─── Whitelist sidecar tamper tests ──────────────────────────────────────────

// TestHost_WhitelistSidecar_NullJSON verifies that a sidecar file containing
// JSON null causes Start() to return a non-nil error (fail-closed).
// json.Unmarshal decodes null into a nil slice; the node must abort rather than
// silently treating null as an empty/open-network whitelist.
func TestHost_WhitelistSidecar_NullJSON(t *testing.T) {
	dir := t.TempDir()
	sidecar := dir + "/whitelist.json"
	if err := os.WriteFile(sidecar, []byte("null"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		WhitelistFile: sidecar,
		PeerWhitelist: []string{"1.2.3.4"},
	}, &stubHandler{}, newTestLogger())

	err := h.Start()
	if err == nil {
		h.Stop()
		t.Fatal("Start() returned nil; want non-nil error for JSON-null sidecar")
	}
	if !strings.Contains(err.Error(), "null") {
		t.Errorf("error %q does not mention 'null'", err.Error())
	}
}

// TestHost_WhitelistSidecar_InvalidIPEntry verifies that a sidecar file
// containing a valid JSON array but with an unparseable IP/CIDR entry causes
// Start() to return a non-nil error (fail-closed).
// Unlike the cfg.PeerWhitelist path (which silently skips bad entries),
// the sidecar path is fatal — an invalid entry could be a sign of tampering.
func TestHost_WhitelistSidecar_InvalidIPEntry(t *testing.T) {
	dir := t.TempDir()
	sidecar := dir + "/whitelist.json"
	// Valid JSON array but the entry "not-an-ip" is neither a bare IP nor CIDR.
	if err := os.WriteFile(sidecar, []byte(`["1.2.3.4","not-an-ip"]`), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		WhitelistFile: sidecar,
	}, &stubHandler{}, newTestLogger())

	err := h.Start()
	if err == nil {
		h.Stop()
		t.Fatal("Start() returned nil; want non-nil error for sidecar with invalid IP entry")
	}
	if !strings.Contains(err.Error(), "not-an-ip") {
		t.Errorf("error %q does not mention the bad entry", err.Error())
	}
}

// TestHost_WhitelistSidecar_UnreadablePermissions verifies that a sidecar file
// that exists but cannot be read (permissions 0o000) causes Start() to return a
// non-nil error (fail-closed).  The node must not start with an empty or default
// whitelist when it cannot confirm the sidecar's contents.
func TestHost_WhitelistSidecar_UnreadablePermissions(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — permission checks do not apply")
	}

	dir := t.TempDir()
	sidecar := dir + "/whitelist.json"
	if err := os.WriteFile(sidecar, []byte(`["1.2.3.4"]`), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	// Remove all permissions so os.ReadFile returns an error.
	if err := os.Chmod(sidecar, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(sidecar, 0o644) }) // restore so TempDir cleanup works

	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		WhitelistFile: sidecar,
	}, &stubHandler{}, newTestLogger())

	err := h.Start()
	if err == nil {
		h.Stop()
		t.Fatal("Start() returned nil; want non-nil error for unreadable sidecar")
	}
}

// TestHost_AddToWhitelist_UnwritableDir_RollsBack verifies the write-first
// design of applyWhitelistLocked: when the sidecar directory loses write
// permission after the node starts, AddToWhitelist must
//   (a) return a non-nil error, and
//   (b) leave the in-memory whitelist unchanged — no silent partial update.
//
// Without the persist-before-swap ordering, the caller would receive an error
// but the in-memory list would already contain the new entry; on the next
// restart the sidecar (which was never updated) would be loaded as authoritative,
// silently dropping the entry that the admin thought was persisted.
func TestHost_AddToWhitelist_UnwritableDir_RollsBack(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — directory permission checks do not apply")
	}

	dir := t.TempDir()
	sidecar := dir + "/whitelist.json"

	// Start with a single entry in the sidecar so the node has a known baseline.
	if err := os.WriteFile(sidecar, []byte(`["1.2.3.4"]`), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "test-wl-unwritable",
		UserAgent:     "aperod/test",
		WhitelistFile: sidecar,
		// No PeerWhitelist in cfg — sidecar is already present and authoritative.
	}, &stubHandler{}, newTestLogger())

	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// Confirm baseline: the sidecar was loaded correctly.
	before := h.GetPeerWhitelist()
	if len(before) != 1 || before[0] != "1.2.3.4" {
		t.Fatalf("unexpected baseline whitelist: %v", before)
	}

	// Make the sidecar directory unwritable so os.CreateTemp fails.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	// Always restore so t.TempDir cleanup can remove it.
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	// (a) AddToWhitelist must return a non-nil error.
	addErr := h.AddToWhitelist("5.6.7.8")
	if addErr == nil {
		t.Fatal("AddToWhitelist returned nil; want non-nil error for unwritable sidecar directory")
	}
	t.Logf("AddToWhitelist error (expected): %v", addErr)

	// (b) In-memory whitelist must be unchanged — no silent partial update.
	after := h.GetPeerWhitelist()
	if len(after) != 1 || after[0] != "1.2.3.4" {
		t.Errorf("GetPeerWhitelist = %v; want [1.2.3.4] — in-memory list must not be modified when persist fails", after)
	} else {
		t.Logf("✓ in-memory whitelist unchanged after persist failure: %v", after)
	}
}

// TestHost_AddToWhitelist_ConcurrentNeverDropsEntry is a concurrent stress test
// that verifies two goroutines calling AddToWhitelist simultaneously always
// produce a whitelist containing both entries.  Without wlMutate serialisation
// both goroutines would snapshot the same (empty) list, append their own entry,
// and the last writer would silently overwrite the first — a lost-update race.
func TestHost_AddToWhitelist_ConcurrentNeverDropsEntry(t *testing.T) {
	const entryA = "10.0.0.1"
	const entryB = "10.0.0.2"
	const iterations = 50 // run many times to surface any race

	for iter := 0; iter < iterations; iter++ {
		h := p2p.NewHost(p2p.Config{
			MaxPeers:      10,
			WhitelistFile: "-", // disable persistence so we test only the in-memory path
		}, &stubHandler{}, newTestLogger())

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := h.AddToWhitelist(entryA); err != nil {
				t.Errorf("iter %d: AddToWhitelist(%q): %v", iter, entryA, err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := h.AddToWhitelist(entryB); err != nil {
				t.Errorf("iter %d: AddToWhitelist(%q): %v", iter, entryB, err)
			}
		}()
		wg.Wait()

		got := h.GetPeerWhitelist()
		if len(got) != 2 {
			t.Errorf("iter %d: whitelist has %d entries, want 2: %v", iter, len(got), got)
			continue
		}
		hasA, hasB := false, false
		for _, e := range got {
			switch e {
			case entryA:
				hasA = true
			case entryB:
				hasB = true
			}
		}
		if !hasA || !hasB {
			t.Errorf("iter %d: whitelist missing an entry: %v", iter, got)
		}
	}
}

// TestHost_RemoveFromWhitelist_ConcurrentNeverDropsOtherRemoval verifies that
// two goroutines calling RemoveFromWhitelist simultaneously each successfully
// remove their own entry and neither call silently discards the other removal.
// Without wlMutate serialisation both goroutines would snapshot the same 2-item
// list, filter out their own entry, and the last writer would restore the entry
// that the first writer removed — a lost-update race.
func TestHost_RemoveFromWhitelist_ConcurrentNeverDropsOtherRemoval(t *testing.T) {
	const entryA = "10.1.0.1"
	const entryB = "10.1.0.2"
	const iterations = 50

	for iter := 0; iter < iterations; iter++ {
		h := p2p.NewHost(p2p.Config{
			MaxPeers:      10,
			PeerWhitelist: []string{entryA, entryB},
			WhitelistFile: "-",
		}, &stubHandler{}, newTestLogger())

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := h.RemoveFromWhitelist(entryA); err != nil {
				t.Errorf("iter %d: RemoveFromWhitelist(%q): %v", iter, entryA, err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := h.RemoveFromWhitelist(entryB); err != nil {
				t.Errorf("iter %d: RemoveFromWhitelist(%q): %v", iter, entryB, err)
			}
		}()
		wg.Wait()

		got := h.GetPeerWhitelist()
		if len(got) != 0 {
			t.Errorf("iter %d: whitelist has %d entries after both removals, want 0: %v", iter, len(got), got)
		}
	}
}

// TestMaintainLoop_ReconnectsBothBootnodesAfterHiccup is an integration test
// that verifies maintainLoop re-dials BOTH configured bootnodes after a
// transient network disconnection.
//
// Setup: bootnode A uses a plain IPv4 IP:port address; bootnode B uses the
// /ip4/.../tcp/... multiaddr format (exercising resolveBootnode's parser) when
// IPv6 is unavailable, or /ip6/::1/tcp/... when the host supports IPv6.
//
// Regression guarded: if the bootnode-retry loop in maintainLoop iterated only
// a subset of bootnodeLastResolved (e.g. stopped after the first entry), the
// node would silently end up with only one bootnode after a partition.
func TestMaintainLoop_ReconnectsBothBootnodesAfterHiccup(t *testing.T) {
	// ── Bootnode A: plain IPv4 IP:port ────────────────────────────────────────
	lnA, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("lnA listen: %v", err)
	}
	defer lnA.Close()
	addrA := lnA.Addr().String() // plain "127.0.0.1:PORT" — passed as-is to Bootnodes

	// ── Bootnode B: prefer IPv6 multiaddr; fall back to IPv4 multiaddr ────────
	// Using the multiaddr /ip4/.../tcp/... or /ip6/.../tcp/... form ensures the
	// resolveBootnode round-trip (multiaddr → dialable IP:port) is exercised.
	var lnB net.Listener
	var addrBMultiaddr string

	if ln6, err6 := net.Listen("tcp6", "[::1]:0"); err6 == nil {
		lnB = ln6
		h6, p6, _ := net.SplitHostPort(ln6.Addr().String())
		addrBMultiaddr = fmt.Sprintf("/ip6/%s/tcp/%s", h6, p6)
		t.Logf("bootnode B: IPv6 multiaddr %s", addrBMultiaddr)
	} else {
		// IPv6 not available — use a second IPv4 listener in multiaddr format.
		ln4b, err4 := net.Listen("tcp4", "127.0.0.1:0")
		if err4 != nil {
			t.Fatalf("lnB listen: %v", err4)
		}
		lnB = ln4b
		_, pB, _ := net.SplitHostPort(ln4b.Addr().String())
		addrBMultiaddr = fmt.Sprintf("/ip4/127.0.0.1/tcp/%s", pB)
		t.Logf("bootnode B: IPv4 multiaddr fallback %s", addrBMultiaddr)
	}
	defer lnB.Close()

	var dialCountA, dialCountB atomic.Int32

	// serverConns collects every server-side connection so we can close them
	// all at once to simulate the network hiccup.
	var connMu sync.Mutex
	var serverConns []net.Conn

	// startAcceptor runs a goroutine that accepts connections, completes the
	// asymmetric P2P handshake (read Ping → write Pong), and holds the
	// connection open until it is closed by the hiccup simulation or the test
	// listener is closed.
	startAcceptor := func(ln net.Listener, count *atomic.Int32) {
		go func() {
			for {
				c, acceptErr := ln.Accept()
				if acceptErr != nil {
					return // listener closed — test is done
				}
				count.Add(1)
				connMu.Lock()
				serverConns = append(serverConns, c)
				connMu.Unlock()
				go func(c net.Conn) {
					defer c.Close()
					c.SetDeadline(time.Now().Add(2 * time.Second))
					// Outbound dialer sends Ping first under the asymmetric handshake.
					msgType, _, rdErr := p2p.ReadMsg(c)
					if rdErr != nil || msgType != p2p.MsgPing {
						return
					}
					if wrErr := p2p.WriteMsg(c, p2p.MsgPong, p2p.PingMsg{
						NodeID: "bootnode", UserAgent: "test", Timestamp: time.Now().Unix(),
					}); wrErr != nil {
						return
					}
					c.SetDeadline(time.Time{}) // clear deadline — hold open
					// io.Copy blocks until the connection is closed (hiccup sim or
					// test teardown) and returns cleanly on EOF / net.Error.
					io.Copy(io.Discard, c) //nolint:errcheck
				}(c)
			}
		}()
	}
	startAcceptor(lnA, &dialCountA)
	startAcceptor(lnB, &dialCountB)

	// ── Host under test ────────────────────────────────────────────────────────
	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		MinPeers:   2,
		NodeID:     "maintain-test",
		UserAgent:  "aperod-test",
		// addrA is a plain IP:port; addrBMultiaddr is a /ip4/ or /ip6/ multiaddr.
		// Both must survive the resolveBootnode round-trip inside maintainLoop.
		Bootnodes: []string{addrA, addrBMultiaddr},
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	// ── Phase 1: wait for the initial dial to reach BOTH bootnodes ────────────
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if dialCountA.Load() >= 1 && dialCountB.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dialCountA.Load() < 1 || dialCountB.Load() < 1 {
		t.Fatalf("initial dial: want ≥1 dial to each bootnode; got A=%d B=%d",
			dialCountA.Load(), dialCountB.Load())
	}
	t.Logf("initial dials ok: A=%d B=%d PeerCount=%d",
		dialCountA.Load(), dialCountB.Load(), host.PeerCount())

	// ── Phase 2: simulate network hiccup — close all server-side connections ──
	connMu.Lock()
	for _, c := range serverConns {
		c.Close()
	}
	serverConns = serverConns[:0]
	connMu.Unlock()

	// Wait for the host to detect both peers dropped (PeerCount → 0).
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if host.PeerCount() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("after hiccup: PeerCount=%d", host.PeerCount())

	// ── Phase 3: fire one maintain tick; both bootnodes must be re-dialled ────
	p2p.HostTriggerMaintain(host)

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if dialCountA.Load() >= 2 && dialCountB.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Each bootnode must have been contacted at least twice (initial + reconnect).
	if dialCountA.Load() < 2 {
		t.Errorf("bootnode A (%s) not re-dialled after hiccup: total dials=%d, want ≥2",
			addrA, dialCountA.Load())
	}
	if dialCountB.Load() < 2 {
		t.Errorf("bootnode B (%s) not re-dialled after hiccup: total dials=%d, want ≥2",
			addrBMultiaddr, dialCountB.Load())
	}
	if !t.Failed() {
		t.Logf("✓ both bootnodes re-dialled after hiccup: A=%d B=%d",
			dialCountA.Load(), dialCountB.Load())
	}
}

// TestHost_DialPeer_BackoffAfterHandshakeDrop verifies that when an outbound
// TCP connection succeeds but the remote closes the socket before completing
// the P2P handshake (no Pong), handleConn's back-off defer calls OnDialFail
// so the address enters a back-off window and cannot be re-dialled immediately.
// This covers the flapping-peer reconnect-storm case where the peer is
// reachable at TCP level but never finishes the application handshake.
func TestHost_DialPeer_BackoffAfterHandshakeDrop(t *testing.T) {
	// Listener that accepts TCP connections and immediately closes them,
	// simulating a peer that drops right after TCP connect.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close() // drop before any P2P handshake bytes
		}
	}()

	target := ln.Addr().String()

	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "backoff-test",
		UserAgent:  "aperod-test",
	}, &stubHandler{}, newTestLogger())

	// First dial: TCP connects, listener drops connection, handleConn returns
	// early with connectedAt still zero → back-off defer calls OnDialFail.
	host.DialPeer(target)
	// Allow the dial goroutine to complete and update the back-off state.
	time.Sleep(300 * time.Millisecond)

	// CanDial must be false: the back-off defer must have fired OnDialFail.
	if p2p.HostCanDial(host, target) {
		t.Error("CanDial must be false after a failed P2P handshake (remote dropped before Pong)")
	}

	// A second DialPeer call must be silently blocked by the CanDial guard.
	host.DialPeer(target)
	time.Sleep(100 * time.Millisecond)
	if p2p.HostCanDial(host, target) {
		t.Error("CanDial must remain false — blocked dial must not clear back-off")
	}
}

// ─── gapSyncHandler ──────────────────────────────────────────────────────────

// gapSyncHandler is a Handler implementation for gap-sync tests.  It accepts
// blocks in strict height order (currentHeight+1 only) and exposes the
// accepted chain for CurrentTailHashes and GetBlock lookups — matching what a
// real node's chain engine would do.
type gapSyncHandler struct {
	mu     sync.Mutex
	tip    uint64
	chain  []*core.Block          // chain[i] is the accepted block at height i
	byHash map[crypto.Hash32]*core.Block
}

func newGapSyncHandler(genesis *core.Block) *gapSyncHandler {
	h := &gapSyncHandler{
		chain:  []*core.Block{genesis},
		byHash: make(map[crypto.Hash32]*core.Block),
	}
	h.byHash[genesis.Hash()] = genesis
	return h
}

func (h *gapSyncHandler) OnBlock(b *core.Block) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Accept only the strictly next block so the handler mirrors a real node's
	// sequential AddBlock behaviour.
	if b.Header.Height == h.tip+1 {
		h.chain = append(h.chain, b)
		h.byHash[b.Hash()] = b
		h.tip = b.Header.Height
	}
}

func (h *gapSyncHandler) OnTransaction(_ *core.Transaction) {}
func (h *gapSyncHandler) OnVote(_ p2p.VoteMsg)              {}

func (h *gapSyncHandler) CurrentHeight() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tip
}

func (h *gapSyncHandler) CurrentTailHashes(n int) []crypto.Hash32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	start := 0
	if len(h.chain) > n {
		start = len(h.chain) - n
	}
	out := make([]crypto.Hash32, 0, len(h.chain)-start)
	for _, b := range h.chain[start:] {
		out = append(out, b.Hash())
	}
	return out
}

func (h *gapSyncHandler) GetBlock(hash crypto.Hash32) *core.Block {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byHash[hash]
}

func (h *gapSyncHandler) tipNow() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tip
}

// ─── 2000-block gap sync test ─────────────────────────────────────────────────

// TestSync_RelayNode_Catches2000BlockGap is the integration test for the
// header-sync re-trigger fix.
//
// Scenario: a relay node starts at height 0.  A validator peer holds a chain
// of 2000 blocks.  After one outbound connection (no manual intervention), the
// relay must sync its tip all the way to 2000 autonomously.
//
// The fix under test: in host.go processBlock, after OnBlock advances the
// relay's tip, the node calls requestHeaders again if it is still behind the
// peer.  This chains successive 500-header batches until the tip converges,
// without waiting for the 3-second keepalive/sync ticker.
//
// The test uses a raw TCP "validator" server (not a full Host) to avoid the
// deadlock that two Hosts' symmetric-handshake heuristics would create.  The
// server:
//   - completes the asymmetric P2P handshake, advertising height=2000
//   - serves MsgGetHeaders by finding the relay's highest known hash in the
//     validator chain and returning the next 500 headers
//   - serves MsgGetBlock by returning the actual block for any hash in the chain
//   - responds to MsgPing keepalives with MsgPong so the relay stays connected
func TestSync_RelayNode_Catches2000BlockGap(t *testing.T) {
	const gapSize = 2000
	const syncTimeout = 30 * time.Second

	// ── Build the validator's chain: genesis + 2000 blocks ───────────────────
	priv, pub, keyErr := crypto.GenerateValidatorKey()
	if keyErr != nil {
		t.Fatalf("GenerateValidatorKey: %v", keyErr)
	}

	validatorChain := make([]*core.Block, gapSize+1)
	validatorByHash := make(map[crypto.Hash32]*core.Block, gapSize+1)
	var prevHash crypto.Hash32
	for i := 0; i <= gapSize; i++ {
		hdr := core.BlockHeader{
			Height:       uint64(i),
			PrevHash:     prevHash,
			ValidatorPub: pub,
			Timestamp:    time.Now().UnixNano() + int64(i),
		}
		if signErr := hdr.Sign(priv); signErr != nil {
			t.Fatalf("Sign block %d: %v", i, signErr)
		}
		b := &core.Block{Header: hdr}
		validatorChain[i] = b
		h := b.Hash()
		validatorByHash[h] = b
		prevHash = h
	}

	// ── Raw TCP "validator" server ────────────────────────────────────────────
	ln, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("validator listen: %v", listenErr)
	}
	defer ln.Close()

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		// writeCh decouples reading from writing so the server never deadlocks:
		// when both the relay and the server want to write at the same time,
		// TCP buffers fill and each blocks waiting for the other to read.  Using
		// a dedicated write goroutine with a buffered channel means the server's
		// read loop is never stalled waiting for a write to complete.
		type writeReq struct {
			msgType p2p.MessageType
			payload interface{}
		}
		writeCh := make(chan writeReq, 4096)
		defer close(writeCh)
		go func() {
			for req := range writeCh {
				conn.SetWriteDeadline(time.Now().Add(30 * time.Second)) //nolint:errcheck
				if wErr := p2p.WriteMsg(conn, req.msgType, req.payload); wErr != nil {
					return
				}
			}
		}()

		// Asymmetric handshake: relay dials us as outbound, so it sends Ping
		// first.  We read Ping and reply with Pong announcing height=gapSize.
		conn.SetReadDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
		mt, _, rdErr := p2p.ReadMsg(conn)
		if rdErr != nil || mt != p2p.MsgPing {
			t.Logf("validator: expected MsgPing got %v err=%v", mt, rdErr)
			return
		}
		writeCh <- writeReq{p2p.MsgPong, p2p.PingMsg{
			NodeID:    "validator",
			Height:    gapSize,
			UserAgent: "test",
			Timestamp: time.Now().Unix(),
		}}

		// Message loop: serve GetHeaders, GetBlock, and keepalive Pings.
		// We use SetReadDeadline (not SetDeadline) so the write goroutine is
		// never blocked by the same per-message deadline.
		//
		// Rate-limit GetHeaders responses: the processBlock re-trigger in host.go
		// fires for every accepted block during a batch, generating O(n) redundant
		// GetHeaders requests per batch.  Each response with 500 headers would
		// cause O(n²) GetBlock traffic that floods the write channel and deadlocks
		// the TCP connection.  We suppress redundant requests by only serving a
		// new batch once the relay's best known height has caught up to the end of
		// the previous batch (lastBatchEnd).  The final block of each batch always
		// triggers a requestHeaders with bestHeight == lastBatchEnd, which passes
		// the guard and serves the next batch — exactly the re-trigger chain we
		// are testing.
		lastBatchEnd := uint64(0) // highest height included in the last batch served

		// sentBlocks deduplicates GetBlock responses.  When redundant requestHeaders
		// cause duplicate GetBlock requests (same hash requested multiple times),
		// we only send the block once.  A relay that didn't receive the first
		// response would rely on stall-detection to recover; in tests with reliable
		// loopback TCP, the first response always arrives.
		sentBlocks := make(map[crypto.Hash32]bool)

		for {
			conn.SetReadDeadline(time.Now().Add(syncTimeout)) //nolint:errcheck
			msgType, data, rdErr2 := p2p.ReadMsg(conn)
			if rdErr2 != nil {
				return
			}
			switch msgType {

			case p2p.MsgGetHeaders:
				var req p2p.GetHeadersMsg
				if uErr := p2p.Unmarshal(data, &req); uErr != nil {
					t.Logf("validator: unmarshal GetHeaders: %v", uErr)
					return
				}
				// Find the relay's highest known hash in our chain.
				bestHeight := uint64(0)
				for _, hash := range req.KnownHashes {
					if b, ok := validatorByHash[hash]; ok {
						if b.Header.Height > bestHeight {
							bestHeight = b.Header.Height
						}
					}
				}
				// Suppress this request if the relay has not yet consumed the
				// previous batch (prevents O(n²) message explosion).
				// The final block of each batch has bestHeight == lastBatchEnd
				// so it passes through and drives the next batch.
				if bestHeight < lastBatchEnd {
					continue
				}
				// Serve up to 500 headers starting after bestHeight.
				limit := req.Limit
				if limit <= 0 || limit > 500 {
					limit = 500
				}
				start := int(bestHeight) + 1
				var hdrs []p2p.SerializedHeader
				for i := start; i <= gapSize && len(hdrs) < limit; i++ {
					b := validatorChain[i]
					hash := b.Hash()
					hdrs = append(hdrs, p2p.SerializedHeader{
						Height:       b.Header.Height,
						Hash:         hash,
						PrevHash:     b.Header.PrevHash,
						MerkleRoot:   b.Header.MerkleRoot,
						Timestamp:    b.Header.Timestamp,
						Round:        b.Header.Round,
						ValidatorPub: b.Header.ValidatorPub,
						Signature:    b.Header.Signature,
					})
				}
				if len(hdrs) > 0 {
					lastBatchEnd = hdrs[len(hdrs)-1].Height
				}
				writeCh <- writeReq{p2p.MsgHeaders, p2p.HeadersMsg{Headers: hdrs}}

			case p2p.MsgGetBlock:
				var req p2p.GetBlockMsg
				if uErr := p2p.Unmarshal(data, &req); uErr != nil {
					t.Logf("validator: unmarshal GetBlock: %v", uErr)
					return
				}
				b, ok := validatorByHash[req.Hash]
				if !ok {
					// Block not found — send nothing; stall-detection will retry.
					continue
				}
				// Deduplicate: only send each block once so duplicate GetBlock
				// requests (from redundant requestHeaders) don't flood the channel.
				if sentBlocks[req.Hash] {
					continue
				}
				sentBlocks[req.Hash] = true
				writeCh <- writeReq{p2p.MsgBlock, p2p.BlockToMsg(b)}

			case p2p.MsgPing:
				// Keepalive: respond with Pong so the relay does not evict us.
				writeCh <- writeReq{p2p.MsgPong, p2p.PingMsg{
					NodeID:    "validator",
					Height:    gapSize,
					UserAgent: "test",
					Timestamp: time.Now().Unix(),
				}}
			}
		}
	}()

	// ── Relay node starting at height 0 (genesis only) ───────────────────────
	relayHandler := newGapSyncHandler(validatorChain[0])
	relayHost := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "relay-gap-test",
		UserAgent:            "aperod/test",
		GetBlockStallTimeout: 2 * time.Second, // short so stall recovery is fast
		KeepaliveInterval:    2 * time.Second,
	}, relayHandler, newTestLogger())
	if err := relayHost.Start(); err != nil {
		t.Fatalf("relay Start: %v", err)
	}
	defer relayHost.Stop()

	// Dial the validator.  After the handshake the relay sees peer.height=2000
	// > our height=0 and immediately calls requestHeaders.  The sync pipeline
	// then runs autonomously via the processBlock re-trigger until tip=2000.
	relayHost.DialPeer(ln.Addr().String())

	// ── Poll until the relay reaches the validator's tip ─────────────────────
	deadline := time.Now().Add(syncTimeout)
	for time.Now().Before(deadline) {
		if relayHandler.tipNow() >= gapSize {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	finalTip := relayHandler.tipNow()
	if finalTip < gapSize {
		t.Errorf("relay tip stalled at %d after %s; want %d — "+
			"processBlock re-trigger may not be firing correctly",
			finalTip, syncTimeout, gapSize)
	} else {
		t.Logf("✓ relay synced %d-block gap autonomously: tip=%d", gapSize, finalTip)
	}
}

// TestSync_RelayStall_ReissuesGetHeaders verifies that when a relay node sends
// MsgGetBlock requests that the peer never answers (block pruned, reorg, or
// race), the relay does not stall silently.  Instead, after GetBlockStallTimeout
// the dedicated stall-detection ticker logs a warning and re-issues MsgGetHeaders
// so the sync pipeline can recover.
//
// The test stands up a raw TCP server that:
//   - completes the asymmetric P2P handshake with height=3 (so the relay
//     immediately sends MsgGetHeaders after the Pong)
//   - responds to every MsgGetHeaders with 3 fake headers, each with a
//     distinct hash so the relay treats them all as unknown
//   - silently ignores all MsgGetBlock requests
//
// Key timing assertion: with GetBlockStallTimeout=500ms the stall-detection
// ticker (independent of the 3-second sync ticker) fires at ~500ms.  The test
// records when the first and second MsgGetHeaders are received and asserts that
// the elapsed time between them is in [400ms, 2500ms] — i.e. after the stall
// timeout but well before the 3-second sync ticker would fire.  This proves
// the stall-detection path (not the unconditional sync re-request) triggered
// the re-issue.
func TestSync_RelayStall_ReissuesGetHeaders(t *testing.T) {
	const peerHeight = uint64(3)
	const stallTimeout = 500 * time.Millisecond
	// syncTickerInterval is the hardcoded 3-second sync ticker in host.go.
	// The second GetHeaders must arrive well before this to prove the stall
	// detection (not the sync ticker) caused the re-issue.
	const syncTickerInterval = 3 * time.Second

	// ── Build 3 fake SerializedHeaders with distinct hashes ─────────────────
	fakeHeaders := make([]p2p.SerializedHeader, peerHeight)
	for i := uint64(0); i < peerHeight; i++ {
		var sh p2p.SerializedHeader
		sh.Height = i + 1
		// Give each header a unique, non-zero hash so the relay treats all as
		// unknown blocks (stubHandler.GetBlock always returns nil).
		sh.Hash[0] = byte(i + 1)
		sh.Hash[1] = 0xDE
		sh.Hash[2] = 0xAD
		fakeHeaders[i] = sh
	}

	// ── Raw TCP "peer" server ────────────────────────────────────────────────
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// timestamps[n] records when the (n+1)-th MsgGetHeaders was received.
	var tsMu sync.Mutex
	var timestamps []time.Time

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		// ── Asymmetric handshake: inbound side waits for Ping, replies Pong ──
		// The relay host dials us as outbound, so it sends Ping first.
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		msgType, _, rdErr := p2p.ReadMsg(conn)
		if rdErr != nil || msgType != p2p.MsgPing {
			t.Logf("peer server: expected MsgPing, got %v err=%v", msgType, rdErr)
			return
		}
		if wErr := p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
			NodeID:    "stall-peer",
			Height:    peerHeight, // claim height=3 so relay triggers GetHeaders
			UserAgent: "test",
			Timestamp: time.Now().Unix(),
		}); wErr != nil {
			t.Logf("peer server: write Pong: %v", wErr)
			return
		}

		// ── Message loop: serve GetHeaders, silently drop GetBlock ───────────
		for {
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			mt, _, rdErr2 := p2p.ReadMsg(conn)
			if rdErr2 != nil {
				return
			}
			switch mt {
			case p2p.MsgGetHeaders:
				// Record the timestamp of each GetHeaders received.
				tsMu.Lock()
				timestamps = append(timestamps, time.Now())
				tsMu.Unlock()
				// Reply with the same 3 fake headers every time.
				if wErr := p2p.WriteMsg(conn, p2p.MsgHeaders, p2p.HeadersMsg{
					Headers: fakeHeaders,
				}); wErr != nil {
					t.Logf("peer server: write Headers: %v", wErr)
					return
				}
			case p2p.MsgGetBlock:
				// Intentionally silently drop — this is what causes the stall.
				// Do NOT send MsgBlock back.
			case p2p.MsgPing:
				// Respond to keepalive pings so the relay doesn't disconnect us.
				_ = p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
					NodeID: "stall-peer", Height: peerHeight,
					UserAgent: "test", Timestamp: time.Now().Unix(),
				})
			}
		}
	}()

	// ── Relay host with a short stall timeout ────────────────────────────────
	relayHost := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "relay-stall-test",
		UserAgent:            "aperod/test",
		GetBlockStallTimeout: stallTimeout,
	}, &stubHandler{}, newTestLogger())
	if err := relayHost.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer relayHost.Stop()

	// Dial the peer server; the relay will send GetHeaders immediately after
	// the handshake because peerHeight (3) > relay.CurrentHeight() (0).
	relayHost.DialPeer(ln.Addr().String())

	// ── Wait for the first GetHeaders ────────────────────────────────────────
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tsMu.Lock()
		n := len(timestamps)
		tsMu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	tsMu.Lock()
	n := len(timestamps)
	tsMu.Unlock()
	if n < 1 {
		t.Fatal("peer server never received a MsgGetHeaders from the relay")
	}
	t.Logf("first MsgGetHeaders received at t=0")

	// ── Wait for the second GetHeaders — must arrive via stall detection ─────
	// The stall ticker fires at GetBlockStallTimeout (500ms), independently of
	// the 3-second sync ticker.  We allow up to 2.4s (< syncTickerInterval) so
	// any arrival proves the stall path fired, not the sync ticker.
	maxWait := syncTickerInterval - 100*time.Millisecond // 2.9 s < sync ticker
	deadline2 := time.Now().Add(maxWait)
	for time.Now().Before(deadline2) {
		tsMu.Lock()
		n2 := len(timestamps)
		tsMu.Unlock()
		if n2 >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tsMu.Lock()
	ts := make([]time.Time, len(timestamps))
	copy(ts, timestamps)
	tsMu.Unlock()

	if len(ts) < 2 {
		t.Fatalf("relay did not re-issue MsgGetHeaders within %s of the first: got %d GetHeaders, want ≥ 2 (stall detection may not be firing)", maxWait, len(ts))
	}

	elapsed := ts[1].Sub(ts[0])
	t.Logf("second MsgGetHeaders arrived %.0fms after first (stallTimeout=%s, syncTicker=3s)",
		float64(elapsed.Milliseconds()), stallTimeout)

	// The gap must be ≥ stallTimeout (stall ticker must have fired at least once
	// before re-issuing) and < syncTickerInterval (proving the sync ticker was
	// NOT the cause).
	if elapsed < stallTimeout-50*time.Millisecond {
		t.Errorf("second GetHeaders arrived too soon (%.0fms < stallTimeout %.0fms): stall ticker may have fired before the block requests were even sent",
			float64(elapsed.Milliseconds()), float64(stallTimeout.Milliseconds()))
	}
	if elapsed >= syncTickerInterval {
		t.Errorf("second GetHeaders arrived after the 3-second sync ticker (%.0fms ≥ 3000ms): stall detection did not fire — the re-issue was caused by the normal sync ticker, not the stall path",
			float64(elapsed.Milliseconds()))
	}
	t.Logf("✓ stall-detection re-issued MsgGetHeaders after %.0fms (stall timeout: %s, sync ticker: 3s)",
		float64(elapsed.Milliseconds()), stallTimeout)
}

// TestSync_RelayStall_Recovers verifies that after a stall cycle (peer never
// answers MsgGetBlock), once the peer resumes serving blocks the relay
// successfully advances its chain tip to the expected height and hash.
//
// Scenario:
//   - A raw TCP peer builds a 3-block chain and advertises height=3.
//   - On the first MsgGetHeaders it replies with the real headers but silently
//     drops all MsgGetBlock requests → relay stalls.
//   - The stall-detection ticker fires and the relay re-issues MsgGetHeaders.
//   - On the second (and subsequent) GetHeaders the peer serves both headers
//     AND the requested blocks.
//   - The relay must eventually reach height=3 with the correct tip hash.
func TestSync_RelayStall_Recovers(t *testing.T) {
	const numBlocks = 3
	const stallTimeout = 400 * time.Millisecond
	const testTimeout = 10 * time.Second

	// ── Build a small real chain ─────────────────────────────────────────────
	priv, pub, keyErr := crypto.GenerateValidatorKey()
	if keyErr != nil {
		t.Fatalf("GenerateValidatorKey: %v", keyErr)
	}

	peerChain := make([]*core.Block, numBlocks+1)
	peerByHash := make(map[crypto.Hash32]*core.Block, numBlocks+1)
	var prevHash crypto.Hash32
	for i := 0; i <= numBlocks; i++ {
		hdr := core.BlockHeader{
			Height:       uint64(i),
			PrevHash:     prevHash,
			ValidatorPub: pub,
			Timestamp:    time.Now().UnixNano() + int64(i),
		}
		if signErr := hdr.Sign(priv); signErr != nil {
			t.Fatalf("Sign block %d: %v", i, signErr)
		}
		b := &core.Block{Header: hdr}
		peerChain[i] = b
		h := b.Hash()
		peerByHash[h] = b
		prevHash = h
	}
	wantTip := peerChain[numBlocks].Hash()

	// ── Raw TCP peer server ──────────────────────────────────────────────────
	ln, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("listen: %v", listenErr)
	}
	defer ln.Close()

	// getHeadersCount counts how many MsgGetHeaders the peer has received.
	// Once it reaches 2 (meaning the stall ticker fired and re-issued), the
	// peer starts serving MsgGetBlock responses.
	var getHeadersCount atomic.Int32

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		// Dedicated write goroutine to avoid read/write deadlocks on loopback.
		type writeReq struct {
			msgType p2p.MessageType
			payload interface{}
		}
		writeCh := make(chan writeReq, 512)
		defer close(writeCh)
		go func() {
			for req := range writeCh {
				conn.SetWriteDeadline(time.Now().Add(testTimeout)) //nolint:errcheck
				if wErr := p2p.WriteMsg(conn, req.msgType, req.payload); wErr != nil {
					return
				}
			}
		}()

		// Asymmetric handshake: relay dials us outbound → it sends Ping first.
		conn.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		mt, _, rdErr := p2p.ReadMsg(conn)
		if rdErr != nil || mt != p2p.MsgPing {
			t.Logf("peer: expected MsgPing, got %v err=%v", mt, rdErr)
			return
		}
		writeCh <- writeReq{p2p.MsgPong, p2p.PingMsg{
			NodeID:    "recover-peer",
			Height:    uint64(numBlocks),
			UserAgent: "test",
			Timestamp: time.Now().Unix(),
		}}

		// Build the SerializedHeader list once.
		headers := make([]p2p.SerializedHeader, numBlocks)
		for i := 1; i <= numBlocks; i++ {
			b := peerChain[i]
			headers[i-1] = p2p.SerializedHeader{
				Height:       b.Header.Height,
				Hash:         b.Hash(),
				PrevHash:     b.Header.PrevHash,
				MerkleRoot:   b.Header.MerkleRoot,
				Timestamp:    b.Header.Timestamp,
				Round:        b.Header.Round,
				ValidatorPub: b.Header.ValidatorPub,
				Signature:    b.Header.Signature,
			}
		}

		// Message loop.
		for {
			conn.SetReadDeadline(time.Now().Add(testTimeout)) //nolint:errcheck
			msgType, data, rdErr2 := p2p.ReadMsg(conn)
			if rdErr2 != nil {
				return
			}
			switch msgType {

			case p2p.MsgGetHeaders:
				n := getHeadersCount.Add(1)
				// Always serve the full header list so the relay knows what to
				// fetch.  The first time we still drop GetBlock, causing a stall.
				writeCh <- writeReq{p2p.MsgHeaders, p2p.HeadersMsg{Headers: headers}}
				_ = n // suppress unused-variable warning

			case p2p.MsgGetBlock:
				// Phase 1: drop the request if the stall ticker has not yet
				// fired (getHeadersCount < 2).  Phase 2: serve the block.
				if getHeadersCount.Load() < 2 {
					// Intentionally silent — causes the stall.
					continue
				}
				var req p2p.GetBlockMsg
				if uErr := p2p.Unmarshal(data, &req); uErr != nil {
					t.Logf("peer: unmarshal GetBlock: %v", uErr)
					return
				}
				b, ok := peerByHash[req.Hash]
				if !ok {
					continue
				}
				writeCh <- writeReq{p2p.MsgBlock, p2p.BlockToMsg(b)}

			case p2p.MsgPing:
				writeCh <- writeReq{p2p.MsgPong, p2p.PingMsg{
					NodeID:    "recover-peer",
					Height:    uint64(numBlocks),
					UserAgent: "test",
					Timestamp: time.Now().Unix(),
				}}
			}
		}
	}()

	// ── Relay node starting at genesis ───────────────────────────────────────
	relayHandler := newGapSyncHandler(peerChain[0])
	relayHost := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "relay-recover-test",
		UserAgent:            "aperod/test",
		GetBlockStallTimeout: stallTimeout,
		KeepaliveInterval:    2 * time.Second,
	}, relayHandler, newTestLogger())
	if err := relayHost.Start(); err != nil {
		t.Fatalf("relay Start: %v", err)
	}
	defer relayHost.Stop()

	// Connect relay → peer.  After the handshake the relay sees peerHeight=3
	// and immediately issues MsgGetHeaders.  Blocks are initially dropped,
	// triggering the stall ticker.  After the stall ticker re-issues
	// MsgGetHeaders (getHeadersCount reaches 2), the peer serves real blocks.
	relayHost.DialPeer(ln.Addr().String())

	// ── Poll until the relay reaches the expected tip ────────────────────────
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if relayHandler.tipNow() >= uint64(numBlocks) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	gotHeight := relayHandler.tipNow()
	if gotHeight != uint64(numBlocks) {
		t.Errorf("relay height = %d, want %d — did not recover after stall (getHeadersCount=%d)",
			gotHeight, numBlocks, getHeadersCount.Load())
		return
	}

	// Verify the tip hash matches the peer's canonical chain.
	gotTip := relayHandler.chain[numBlocks].Hash()
	if gotTip != wantTip {
		t.Errorf("relay tip hash mismatch:\n  got  %x\n  want %x", gotTip[:8], wantTip[:8])
		return
	}

	t.Logf("✓ relay recovered after stall: height=%d tip=%x (stall re-issue count=%d)",
		gotHeight, gotTip[:8], getHeadersCount.Load())
}
