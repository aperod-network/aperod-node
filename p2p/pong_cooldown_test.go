package p2p_test

// Tests for the MsgPong requestHeaders cooldown added in host.go.
//
// These tests verify that:
//   1. Rapid Pongs at the same peer height only trigger one Pong-triggered
//      requestHeaders call (the redundant chatter when a MsgBlock is still in
//      flight is eliminated).
//   2. A Pong at a new (higher) height immediately triggers a fresh request
//      (the catching-up path is not regressed).
//   3. The 2×KeepaliveInterval self-heal fallback fires at the right time
//      when the first requestHeaders was silently dropped.
//
// The tests use HostPongGetHeadersTotal — an exported atomic counter that
// counts only Pong-handler-triggered calls to requestHeaders, excluding the
// sync ticker and stall detector — so timer jitter from those background
// paths cannot corrupt the assertions.
//
// Test design: the catching host always DIALS OUT to a controlled server.
// The server drives the Pong messages at specific times.  The server runs for
// less than one keepalive interval (default 10 s) so the catching host never
// sends a Ping to the server, avoiding the need to handle keepalive replies
// on the server side.

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/p2p"
)

// TestPongCooldown_SuppressesRedundantRequestHeaders verifies that sending
// multiple Pong messages at the same peer height triggers requestHeaders only
// once (on the first Pong), and that a subsequent Pong at a higher height
// triggers exactly one more call.
//
// This covers the common case where a peer has just produced a new block and
// reports height N+1 while the MsgBlock is still in flight: every keepalive
// Pong until the block arrives should NOT fan out into a GetHeaders storm.
func TestPongCooldown_SuppressesRedundantRequestHeaders(t *testing.T) {
	t.Parallel()

	// Server: inbound side.  Drives Pong messages at controlled heights.
	// Total run time is well under one KeepaliveInterval (10 s), so the
	// catching host never sends a Ping back — no concurrent-write handling
	// needed on the server side.
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
		conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck

		// Inbound handshake: wait for MsgPing, reply MsgPong(0).
		// Height=0 → catching host's initial Pong dispatch does NOT fire
		// requestHeaders (0 is not > CurrentHeight()=0).
		mt, _, rdErr := p2p.ReadMsg(conn)
		if rdErr != nil || mt != p2p.MsgPing {
			t.Logf("server: expected MsgPing, got %v err=%v", mt, rdErr)
			return
		}
		if wErr := p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
			NodeID: "cooldown-server", Height: 0, UserAgent: "test",
			Timestamp: time.Now().UnixNano(),
		}); wErr != nil {
			t.Logf("server: write handshake Pong: %v", wErr)
			return
		}

		// Drain the one unconditional post-handshake MsgGetHeaders.
		conn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
		mt, _, rdErr = p2p.ReadMsg(conn)
		if rdErr != nil || mt != p2p.MsgGetHeaders {
			t.Logf("server: expected initial MsgGetHeaders, got %v err=%v", mt, rdErr)
			return
		}

		conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck

		// ── Phase 1: 3 rapid Pongs at height=5. ──────────────────────────────
		// The catching host's CurrentHeight() is 0 throughout the test
		// (stubHandler never advances), so all Pongs satisfy the outer
		// msg.Height > CurrentHeight() predicate.  Only the first Pong at
		// height=5 must fire requestHeaders; subsequent identical-height Pongs
		// must be suppressed by the cooldown gate.
		for i := 0; i < 3; i++ {
			if wErr := p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
				NodeID: "cooldown-server", Height: 5, UserAgent: "test",
				Timestamp: time.Now().UnixNano(),
			}); wErr != nil {
				t.Logf("server: write Pong(5)[%d]: %v", i, wErr)
				return
			}
		}

		// Allow the dispatch goroutine time to process all 3 messages.
		// 200 ms is well under the 3 s sync ticker so no ticker fires pollute
		// the counter in this window.
		time.Sleep(200 * time.Millisecond)

		// ── Phase 2: Pong at height=6 (peer advanced). ────────────────────────
		// The peer has moved to a strictly higher height; the cooldown gate
		// must let this through immediately.
		if wErr := p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
			NodeID: "cooldown-server", Height: 6, UserAgent: "test",
			Timestamp: time.Now().UnixNano(),
		}); wErr != nil {
			t.Logf("server: write Pong(6): %v", wErr)
			return
		}

		// Allow the dispatch goroutine to process the final Pong.
		time.Sleep(100 * time.Millisecond)

		// Hold the connection open briefly so the catching host does not see an
		// unexpected EOF before we read the counter.
		time.Sleep(50 * time.Millisecond)
	}()

	// Catching host: always at height 0 (stubHandler never advances).
	catchingHost := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             5,
		NodeID:               "catching-node",
		UserAgent:            "aperod/test",
		BadBlockHeightLead:   1_000,
		BadBlockBanThreshold: 10_000,
	}, &stubHandler{}, newTestLogger())
	if err := catchingHost.Start(); err != nil {
		t.Fatalf("catchingHost.Start: %v", err)
	}
	defer catchingHost.Stop()

	// Snapshot counter before dialling so any pre-connection noise is excluded.
	baseline := p2p.HostPongGetHeadersTotal(catchingHost)

	catchingHost.DialPeer(ln.Addr().String())

	// Wait for server goroutine to finish (it drives all Pong sends and sleeps).
	<-serverDone

	// Give the dispatch goroutine a final moment to commit.
	time.Sleep(20 * time.Millisecond)

	got := p2p.HostPongGetHeadersTotal(catchingHost) - baseline

	// Phase 1: first Pong(5) → 1 call.  Second and third Pong(5) → 0 each.
	// Phase 2: Pong(6) → 1 call.
	// Total expected: 2.
	if got != 2 {
		t.Errorf("Pong-triggered GetHeaders count = %d, want 2 "+
			"(1 for first Pong(5), 0 for repeated Pong(5)×2, 1 for Pong(6))", got)
	}
}

// TestPongCooldown_SelfHealFallback verifies that when a peer stays at the
// same height and the 2×KeepaliveInterval self-heal window elapses, the Pong
// handler fires requestHeaders again — preserving the original self-heal
// semantics for the UTXO-rebuild case where the first request was silently
// dropped.
//
// This test uses a very short KeepaliveInterval so the 2×KeepaliveInterval
// self-heal window elapses in milliseconds rather than 20 seconds.
// Because the catching host sends Pings every 50 ms in this scenario, the
// server goroutine runs a concurrent write path (protected by a mutex) so it
// can respond to incoming Pings while also sending Pong messages on cue.
func TestPongCooldown_SelfHealFallback(t *testing.T) {
	t.Parallel()

	// Use a very short keepalive interval so the 2×KeepaliveInterval self-heal
	// window elapses in milliseconds rather than 20 seconds.
	const keepalive = 50 * time.Millisecond
	const selfHeal = 2 * keepalive // must match host.go logic

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// pongCh carries (height, timestamp) pairs for the write goroutine to send
	// as MsgPong messages.  The server goroutine feeds heights via this channel.
	type pongReq struct{ height uint64 }
	pongCh := make(chan pongReq, 8)

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, aErr := ln.Accept()
		if aErr != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck

		// Inbound handshake.
		mt, _, rdErr := p2p.ReadMsg(conn)
		if rdErr != nil || mt != p2p.MsgPing {
			t.Logf("server: expected MsgPing, got %v err=%v", mt, rdErr)
			return
		}
		if wErr := p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
			NodeID: "cooldown-server", Height: 0, UserAgent: "test",
			Timestamp: time.Now().UnixNano(),
		}); wErr != nil {
			t.Logf("server: write handshake Pong: %v", wErr)
			return
		}

		// Drain the initial MsgGetHeaders.
		conn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
		mt, _, rdErr = p2p.ReadMsg(conn)
		if rdErr != nil || mt != p2p.MsgGetHeaders {
			t.Logf("server: expected initial MsgGetHeaders, got %v err=%v", mt, rdErr)
			return
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck

		// ── Write goroutine ───────────────────────────────────────────────────
		// Serialises all writes to conn via a mutex shared with the Ping
		// responder below, so the read loop and test-directed Pong sends do not
		// interleave bytes on the wire.
		var wMu sync.Mutex
		writePong := func(h uint64) {
			wMu.Lock()
			defer wMu.Unlock()
			_ = p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
				NodeID: "cooldown-server", Height: h, UserAgent: "test",
				Timestamp: time.Now().UnixNano(),
			})
		}
		writeDone := make(chan struct{})
		go func() {
			defer close(writeDone)
			for req := range pongCh {
				writePong(req.height)
			}
		}()

		// ── Read loop: respond to keepalive Pings from the catching host. ─────
		// Because the catching host uses a 50 ms keepalive interval and this
		// server runs for ~300 ms, several Pings will arrive and must receive a
		// Pong reply to prevent the catching host from evicting the connection.
		for {
			conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
			mt, _, rdErr = p2p.ReadMsg(conn)
			if rdErr != nil {
				break // channel closed by test, connection done
			}
			if mt == p2p.MsgPing {
				wMu.Lock()
				_ = p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
					NodeID: "cooldown-server", Height: 0, UserAgent: "test",
					Timestamp: time.Now().UnixNano(),
				})
				wMu.Unlock()
			}
			// Discard MsgGetHeaders — we do not serve blocks in this test.
		}
		close(pongCh)
		<-writeDone
	}()

	catchingHost := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             5,
		NodeID:               "catching-node",
		UserAgent:            "aperod/test",
		BadBlockHeightLead:   1_000,
		BadBlockBanThreshold: 10_000,
		// KeepaliveInterval must be ≥ 1 s for NewHost; override via test helper.
	}, &stubHandler{}, newTestLogger())
	// Bypass the [1s, 15s] guard so we can test at ms scale.
	p2p.SetKeepaliveIntervalForTest(catchingHost, keepalive)

	if err := catchingHost.Start(); err != nil {
		t.Fatalf("catchingHost.Start: %v", err)
	}
	// No defer here — we call Stop explicitly before <-serverDone so the
	// server's read loop receives an EOF and exits cleanly.  A second Stop
	// call (from defer) would panic on the already-closed done channel.

	baseline := p2p.HostPongGetHeadersTotal(catchingHost)

	catchingHost.DialPeer(ln.Addr().String())

	// Wait for the peer to register.
	if !waitFor(2*time.Second, func() bool { return catchingHost.PeerCount() == 1 }) {
		t.Fatalf("peer never connected")
	}
	// Allow the unconditional post-handshake requestHeaders to complete.
	time.Sleep(50 * time.Millisecond)

	// ── Step 1: first Pong(5) — must fire immediately. ───────────────────────
	pongCh <- pongReq{5}
	time.Sleep(50 * time.Millisecond)

	afterStep1 := p2p.HostPongGetHeadersTotal(catchingHost) - baseline
	if afterStep1 != 1 {
		t.Fatalf("Step 1: first Pong(5) → %d Pong-triggered calls, want 1", afterStep1)
	}

	// ── Step 2: another Pong(5) immediately — must be suppressed. ────────────
	pongCh <- pongReq{5}
	time.Sleep(30 * time.Millisecond)

	afterStep2 := p2p.HostPongGetHeadersTotal(catchingHost) - baseline
	if afterStep2 != 1 {
		t.Errorf("Step 2: immediate Pong(5) → %d Pong-triggered calls, want still 1 (suppressed)",
			afterStep2)
	}

	// ── Step 3: wait for self-heal window, then send Pong(5) again. ──────────
	// After 2×KeepaliveInterval the cooldown must allow a retry at the same
	// height so the node can recover from a silently-dropped first request.
	time.Sleep(selfHeal + 20*time.Millisecond) // slight over-sleep for scheduler jitter
	pongCh <- pongReq{5}
	time.Sleep(50 * time.Millisecond)

	afterStep3 := p2p.HostPongGetHeadersTotal(catchingHost) - baseline
	if afterStep3 != 2 {
		t.Errorf("Step 3: Pong(5) after self-heal window → %d Pong-triggered calls, want 2 "+
			"(self-heal fallback must re-fire after 2×KeepaliveInterval)",
			afterStep3)
	}

	// Stop the catching host first; this closes the connection to the server,
	// which causes the server goroutine's ReadMsg to return an error and exit.
	catchingHost.Stop()
	<-serverDone
}
