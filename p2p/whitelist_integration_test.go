package p2p

// whitelist_integration_test.go — live network test that proves the CIDR
// whitelist check in acceptLoop actually drops outside-subnet connections.
//
// Unit tests in whitelist_test.go exercise ipInWhitelist in isolation.
// This file closes the gap by starting a real Host listener and verifying:
//   - a connection from 127.0.0.1 (inside 127.0.0.0/8) completes the full
//     ping/pong handshake and is registered as a peer; and
//   - a connection whose RemoteAddr is spoofed to 10.9.9.9:54321 (outside
//     127.0.0.0/8) via fakeAddrListener is closed by the host before a Pong
//     is sent.

import (
	"net"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── minimal handler stub ─────────────────────────────────────────────────────

// wlIntegHandler is a no-op Handler used by the whitelist integration tests.
// The inside_allowed scenario completes a full handshake, which means
// handleConn will call the Handler (e.g. CurrentHeight); a nil handler panics.
type wlIntegHandler struct{}

func (wlIntegHandler) OnBlock(_ *core.Block)                      {}
func (wlIntegHandler) OnTransaction(_ *core.Transaction)          {}
func (wlIntegHandler) OnVote(_ VoteMsg)                           {}
func (wlIntegHandler) CurrentHeight() uint64                      { return 0 }
func (wlIntegHandler) CurrentTailHashes(_ int) []crypto.Hash32   { return nil }
func (wlIntegHandler) GetBlock(_ crypto.Hash32) *core.Block      { return nil }

// ─── spoofing listener helpers ────────────────────────────────────────────────

// fakeAddr is a net.Addr with configurable Network/String values.
type fakeAddr struct {
	network string
	address string
}

func (a fakeAddr) Network() string { return a.network }
func (a fakeAddr) String() string  { return a.address }

// fakeAddrConn wraps a net.Conn and overrides RemoteAddr with a fixed value.
// This lets us make a real TCP connection appear to come from any source IP.
type fakeAddrConn struct {
	net.Conn
	remoteAddr net.Addr
}

func (f *fakeAddrConn) RemoteAddr() net.Addr { return f.remoteAddr }

// fakeAddrListener wraps a real net.Listener and replaces the RemoteAddr of
// every accepted connection with fakeRemote.
type fakeAddrListener struct {
	net.Listener
	fakeRemote net.Addr
}

func (f fakeAddrListener) Accept() (net.Conn, error) {
	conn, err := f.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &fakeAddrConn{Conn: conn, remoteAddr: f.fakeRemote}, nil
}

// ─── integration test ────────────────────────────────────────────────────────

// TestHost_CIDRWhitelist_Integration is the live-network integration test
// described in task 1436.  It verifies two scenarios against a Host configured
// with PeerWhitelist: ["127.0.0.0/8"]:
//
//  1. inside_allowed — a genuine TCP connection from 127.0.0.1 (which is
//     inside 127.0.0.0/8) completes the asymmetric Ping→Pong handshake and
//     the peer is registered in the host's peer table.
//
//  2. outside_rejected — a TCP connection whose RemoteAddr is spoofed to
//     10.9.9.9:54321 (outside 127.0.0.0/8) via fakeAddrListener is closed
//     by the host before it sends a Pong, and the host's peer count stays
//     at zero.
func TestHost_CIDRWhitelist_Integration(t *testing.T) {
	t.Run("inside_allowed", func(t *testing.T) {
		h := NewHost(Config{
			ListenAddr:    "127.0.0.1:0",
			MaxPeers:      10,
			NodeID:        "wl-host-in",
			UserAgent:     "aperod/test",
			PeerWhitelist: []string{"127.0.0.0/8"},
		}, wlIntegHandler{}, wlTestLogger())
		if err := h.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer h.Stop()

		addr := h.ListenAddr()
		if addr == "" {
			t.Skip("ListenAddr not exposed — skipping")
		}

		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		conn.SetDeadline(time.Now().Add(2 * time.Second))

		// Asymmetric handshake: dialer sends Ping first, host replies with Pong.
		ping := PingMsg{
			NodeID:    "inside-peer",
			Height:    0,
			UserAgent: "test",
			Timestamp: time.Now().Unix(),
		}
		if err := writeMsg(conn, MsgPing, ping); err != nil {
			t.Fatalf("write ping: %v", err)
		}

		msgType, _, err := readMsg(conn)
		if err != nil {
			t.Fatalf("read pong: %v — whitelist may have incorrectly rejected 127.0.0.1", err)
		}
		if msgType != MsgPong {
			t.Fatalf("expected MsgPong from host, got %v — handshake did not complete", msgType)
		}
		t.Log("inside 127.0.0.1 → Pong received: whitelist correctly admitted the connection ✓")

		// Poll until the host's handleConn goroutine registers the peer (or time out).
		// A Pong proves the handshake completed; a PeerCount of 1 proves the peer
		// was added to the peer table — both must be true per the task requirement.
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if h.PeerCount() == 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if got := h.PeerCount(); got != 1 {
			t.Errorf("PeerCount = %d after inside-subnet handshake, want 1 — "+
				"peer was not registered despite successful Pong exchange", got)
		} else {
			t.Logf("PeerCount = %d: peer registered after inside-subnet handshake ✓", got)
		}
	})

	t.Run("outside_rejected", func(t *testing.T) {
		// Build a real TCP listener that fakeAddrListener will wrap.
		rawLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("raw listen: %v", err)
		}

		// 10.9.9.9 is outside 127.0.0.0/8 — the host must reject it.
		outsideAddr := fakeAddr{network: "tcp", address: "10.9.9.9:54321"}
		fakeLn := fakeAddrListener{Listener: rawLn, fakeRemote: outsideAddr}

		h := NewHost(Config{
			MaxPeers:      10,
			NodeID:        "wl-host-out",
			UserAgent:     "aperod/test",
			PeerWhitelist: []string{"127.0.0.0/8"},
		}, wlIntegHandler{}, wlTestLogger())

		// Inject the spoofing listener directly so acceptLoop sees 10.9.9.9 as
		// the source of every inbound connection.  We bypass Start() because we
		// only want acceptLoop, not the bootnode dialler or maintainLoop, and
		// because Start() would create its own real listener over the fake one.
		h.listener = fakeLn
		go h.acceptLoop()
		defer h.Stop() // closes h.done and h.listener (fakeLn → rawLn)

		lnAddr := rawLn.Addr().String()

		// Dial the listener.  From the TCP stack's perspective the dial comes
		// from 127.0.0.1, but fakeAddrListener makes the host see 10.9.9.9.
		conn, err := net.DialTimeout("tcp", lnAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		conn.SetDeadline(time.Now().Add(500 * time.Millisecond))

		// Send a Ping so the host has something to respond to if the whitelist
		// were broken (it must NOT respond with Pong for an outside IP).
		ping := PingMsg{
			NodeID:    "outside-peer",
			Height:    0,
			UserAgent: "test",
			Timestamp: time.Now().Unix(),
		}
		_ = writeMsg(conn, MsgPing, ping) // host may close before this arrives

		// The host must close the connection before sending a Pong.
		// Reading should return io.EOF or a network error.
		_, _, readErr := readMsg(conn)
		if readErr == nil {
			t.Fatal("expected connection to be closed by host (whitelist outside-IP rejection), " +
				"but readMsg succeeded — acceptLoop whitelist check is NOT wired correctly")
		}
		t.Logf("outside 10.9.9.9 → connection closed before Pong: %v ✓", readErr)

		// Peer table must be empty — the rejected connection must not have been registered.
		if got := h.PeerCount(); got != 0 {
			t.Errorf("PeerCount = %d after outside-IP rejection, want 0 — "+
				"peer was registered despite whitelist block", got)
		}

		// The host itself must still be alive and accepting new connections.
		probe, err := net.DialTimeout("tcp", lnAddr, time.Second)
		if err != nil {
			t.Fatalf("host unreachable after whitelist rejection: %v", err)
		}
		probe.Close()
		t.Log("host remains alive after rejecting outside-IP connection ✓")
	})
}
