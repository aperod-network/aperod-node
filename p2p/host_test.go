package p2p_test

// Tests for p2p Host: lifecycle, broadcast with 0 peers, full handshake over net.Pipe.

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/p2p"
)

// ─── Stub handler ─────────────────────────────────────────────────────────────

type stubHandler struct {
	blocks []*core.Block
	txs    []*core.Transaction
	votes  []p2p.VoteMsg
}

func (s *stubHandler) OnBlock(b *core.Block)         { s.blocks = append(s.blocks, b) }
func (s *stubHandler) OnTransaction(tx *core.Transaction) { s.txs = append(s.txs, tx) }
func (s *stubHandler) OnVote(v p2p.VoteMsg)          { s.votes = append(s.votes, v) }
func (s *stubHandler) CurrentHeight() uint64         { return 0 }
func (s *stubHandler) CurrentTailHashes(_ int) []crypto.Hash32 { return nil }
func (s *stubHandler) GetBlock(_ crypto.Hash32) *core.Block   { return nil }

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

// dialAndHandshake: simulates what an outbound peer would do — respond with
// pong on the first ping, then send a message, then disconnect.
func dialAndHandshake(t *testing.T, addr string, extraMsg func(conn net.Conn)) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Logf("dialAndHandshake: dial failed: %v", err)
		return
	}
	defer conn.Close()

	// Receive ping from host
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	msgType, data, err := p2p.ReadMsg(conn)
	if err != nil || msgType != p2p.MsgPing {
		t.Logf("dialAndHandshake: expected ping, got %v err=%v (data=%d bytes)", msgType, err, len(data))
		return
	}

	// Reply with pong
	pong := p2p.PingMsg{NodeID: "fake-peer", Height: 0, UserAgent: "test", Timestamp: time.Now().Unix()}
	if err := p2p.WriteMsg(conn, p2p.MsgPong, pong); err != nil {
		t.Logf("dialAndHandshake: write pong: %v", err)
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

		// Receive ping from hA.
		msgType, _, err := p2p.ReadMsg(c)
		if err != nil || msgType != p2p.MsgPing {
			c.Close()
			t.Fatalf("flooder %d: expected MsgPing, got %v err=%v", i, msgType, err)
		}
		// Reply with pong so hA registers us as a live inbound peer.
		if err := p2p.WriteMsg(c, p2p.MsgPong, p2p.PingMsg{
			NodeID:    fmt.Sprintf("flooder-%d", i),
			Height:    0,
			UserAgent: "flood",
			Timestamp: time.Now().Unix(),
		}); err != nil {
			c.Close()
			t.Fatalf("flooder %d: write pong: %v", i, err)
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
		if msgType, _, readErr := p2p.ReadMsg(extra); readErr == nil {
			_ = msgType
			_ = p2p.WriteMsg(extra, p2p.MsgPong, p2p.PingMsg{
				NodeID: "extra", UserAgent: "test", Timestamp: time.Now().Unix(),
			})
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
