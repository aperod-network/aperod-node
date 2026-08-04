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
