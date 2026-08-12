package p2p_test

// ── Deterministic concurrency tests ────────────────────────────────────────
//
// TestBanDial_ConcurrentBan_KnownPeer and TestBanDial_ConcurrentBan_Bootnode
// force the exact race described in task #1650:
//
//  1. dialPeer passes the IsBanned check (ban not written yet) and registers
//     its cancel func in the dial gate.
//  2. BanPeer is called — it acquires the dial gate, writes the ban, and
//     invokes the registered cancel func.
//  3. The injected dialContextFunc observes context.Canceled and returns
//     without establishing any TCP connection.
//
// Without the dial-gate mechanism, step 3 would be replaced by a successful
// TCP connect to the target listener, and dialCount would reach 1.
//
// TestBanReconnect_MaintainLoopSkipsBannedPeer verifies that maintainLoop does
// not attempt to re-dial a peer that was just banned — even when the banned
// address is present in peerList and the peer count is below MinPeers.
//
// The scenario targets the window described in task #1650: when a rogue-fork
// ban fires (or BanPeer is called) just before maintainLoop reads the peer
// table, the maintain tick could theoretically launch a dial goroutine for the
// now-banned address before the goroutine scheduler gets to run the IsBanned
// check inside dialPeer.  The fix adds a redundant IsBanned guard at the
// top of the maintainLoop iteration so the goroutine is not even spawned for
// a known-banned addr.
//
// Test structure:
//  1. Start a stub TCP listener that counts incoming connection attempts; any
//     connection it receives means a dial was made to the banned address.
//  2. Start a host with MinPeers=1 (so maintainLoop will try to reconnect
//     when the peer table is empty).
//  3. Ban the listener's address, then seed it into peerList.
//  4. Fire one maintainLoop tick immediately via HostTriggerMaintain.
//  5. Assert that the listener received zero connections and PeerCount is 0.
//
// The test also covers BanPeer's bare-IP propagation: the host bans by the
// listen address ("IP:port") and maintainLoop re-dials that same address, so
// IsBanned must match via the bare-IP entry that Ban() now writes in addition
// to the full "IP:port" key.

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/p2p"
)

// TestBanDial_ConcurrentBan_KnownPeer exercises the dial-gate race for the
// known-peer (MinPeers/peerList) path:
//
//  1. dialPeer passes IsBanned (false), registers its cancel func.
//  2. BanPeer cancels that func before the dial reaches the network.
//  3. The listener must receive zero TCP connections.
func TestBanDial_ConcurrentBan_KnownPeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var dialCount atomic.Int32
	go func() {
		for {
			c, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			dialCount.Add(1)
			c.Close()
		}
	}()

	targetAddr := ln.Addr().String()

	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		MinPeers:   1,
		NodeID:     "test-concurrent-ban-known",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// dialStarted is closed by the injected dialContextFunc the moment it is
	// entered — i.e. after dialPeer has passed IsBanned and registered its
	// cancel func in the dial gate, but before any TCP socket is created.
	dialStarted := make(chan struct{})
	var dialStartedOnce atomic.Bool

	p2p.SetDialFunc(h, func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Signal that we are inside the dial — IsBanned was false, cancel is
		// registered, TCP connect has not started yet.
		if dialStartedOnce.CompareAndSwap(false, true) {
			close(dialStarted)
		}
		// Block until either BanPeer cancels us or the test times out.
		select {
		case <-ctx.Done():
			// BanPeer cancelled: return without connecting.
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			// Safety: avoid hanging the test forever if cancellation breaks.
			return nil, context.DeadlineExceeded
		}
	})

	// Seed the target into peerList so dialPeer is triggered by MinPeers path.
	p2p.HostAddKnownPeer(h, targetAddr)
	// Trigger one immediate maintain tick to launch the dialPeer goroutine.
	p2p.HostTriggerMaintain(h)

	// Wait until the dial goroutine has entered dialContextFunc (IsBanned was
	// false, cancel func is registered in the dial gate).
	select {
	case <-dialStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("dial goroutine did not start within 2 s")
	}

	// Now call BanPeer.  This must: (a) write the ban under dialGateMu,
	// (b) invoke the registered cancel func so dialContextFunc returns
	// context.Canceled, and (c) prevent the TCP connect from reaching ln.
	h.BanPeer(targetAddr, "concurrent ban test", 24*time.Hour)

	// Allow time for the cancelled dial goroutine to finish.
	time.Sleep(100 * time.Millisecond)

	if n := dialCount.Load(); n != 0 {
		t.Errorf("listener received %d TCP connection(s) after BanPeer cancelled the in-flight dial — dial-gate race not closed", n)
	}
	t.Logf("✓ concurrent ban cancelled in-flight dial to %s before TCP connect (dialCount=%d)", targetAddr, dialCount.Load())
}

// TestBanDial_ConcurrentBan_Bootnode mirrors TestBanDial_ConcurrentBan_KnownPeer
// but exercises the bootnode re-dial path inside maintainLoop, which uses a
// separate IsBanned check from the MinPeers/peerList path.
func TestBanDial_ConcurrentBan_Bootnode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var dialCount atomic.Int32
	go func() {
		for {
			c, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			dialCount.Add(1)
			c.Close()
		}
	}()

	targetAddr := ln.Addr().String()

	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-concurrent-ban-bootnode",
		UserAgent:  "aperod/test",
		Bootnodes:  []string{targetAddr},
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	dialStarted := make(chan struct{})
	var dialStartedOnce atomic.Bool

	p2p.SetDialFunc(h, func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dialStartedOnce.CompareAndSwap(false, true) {
			close(dialStarted)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, context.DeadlineExceeded
		}
	})

	// Seed bootnodeSet so the bootnode path fires without DNS.
	p2p.HostSetBootnodeResolved(h, targetAddr, []string{targetAddr})

	// Let Start()'s initial dial attempt land (if any) before we reset.
	time.Sleep(50 * time.Millisecond)
	dialCount.Store(0)
	dialStartedOnce.Store(false)
	// Re-open dialStarted: use a new channel for the maintain-tick dial.
	dialStarted2 := make(chan struct{})
	p2p.SetDialFunc(h, func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dialStartedOnce.CompareAndSwap(false, true) {
			close(dialStarted2)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, context.DeadlineExceeded
		}
	})

	p2p.HostTriggerMaintain(h)

	select {
	case <-dialStarted2:
	case <-time.After(2 * time.Second):
		t.Fatal("bootnode dial goroutine did not start within 2 s")
	}

	h.BanPeer(targetAddr, "concurrent bootnode ban test", 24*time.Hour)
	time.Sleep(100 * time.Millisecond)

	if n := dialCount.Load(); n != 0 {
		t.Errorf("listener received %d TCP connection(s) after BanPeer cancelled the in-flight bootnode dial — dial-gate race not closed", n)
	}
	t.Logf("✓ concurrent ban cancelled in-flight bootnode dial to %s before TCP connect (dialCount=%d)", targetAddr, dialCount.Load())
}

func TestBanReconnect_MaintainLoopSkipsBannedPeer(t *testing.T) {
	// ── Step 1: stub listener that counts incoming connections ────────────────
	// Any incoming TCP connection means a dial was attempted to the banned addr.
	// The listener accepts and immediately closes so the dial does not hang.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var dialCount atomic.Int32
	go func() {
		for {
			c, aErr := ln.Accept()
			if aErr != nil {
				return // listener closed; goroutine exits cleanly
			}
			dialCount.Add(1)
			c.Close()
		}
	}()

	bannedAddr := ln.Addr().String()

	// ── Step 2: start the host ────────────────────────────────────────────────
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		MinPeers:   1, // triggers the peerList reconnect path in maintainLoop
		NodeID:     "test-ban-reconnect",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// ── Step 3: ban the address, then seed it into peerList ───────────────────
	// BanPeer must store a bare-IP entry (via mgr.Ban) so that future
	// IsBanned("IP:port") calls on this address return true even when the
	// banned entry was written using the same "IP:port" key.
	h.BanPeer(bannedAddr, "rogue peer — no-reconnect test", 24*time.Hour)

	// Confirm the ban is visible before seeding peerList.
	bans := h.ListBans()
	if len(bans) == 0 {
		t.Fatal("BanPeer did not register any ban entries")
	}

	// Seed the banned address into peerList; this is what maintainLoop iterates
	// when count < MinPeers.  In production, peerList is populated by the
	// MsgPeers peer-exchange protocol.
	p2p.HostAddKnownPeer(h, bannedAddr)

	// ── Step 4: fire one maintainLoop tick immediately ────────────────────────
	p2p.HostTriggerMaintain(h)

	// Allow enough time for the tick to complete and for any would-be dial
	// goroutines to run and reach the listener.  dialPeer does a synchronous
	// TCP dial with a timeout; if the IsBanned guard is bypassed, the dial to
	// our local listener completes in well under 50 ms on any CI runner.
	time.Sleep(150 * time.Millisecond)

	// ── Step 5: assertions ────────────────────────────────────────────────────
	if n := dialCount.Load(); n != 0 {
		t.Errorf("maintainLoop dialed the banned peer %d time(s), want 0 — "+
			"IsBanned guard in maintainLoop or dialPeer may be missing", n)
	}
	if pc := h.PeerCount(); pc != 0 {
		t.Errorf("PeerCount = %d after maintainLoop tick, want 0 — "+
			"banned peer was connected despite the ban", pc)
	}
	t.Logf("✓ maintainLoop did not reconnect to banned peer %s (dialCount=%d)",
		bannedAddr, dialCount.Load())
}

// TestBanReconnect_BootnodePathSkipsBannedAddr verifies that the bootnode
// re-dial path inside maintainLoop also skips a banned address.  In
// production, a node configured with a bootnode that later gets banned (e.g.
// by BanPeer via the Admin Panel) must not reconnect to it on the next tick.
func TestBanReconnect_BootnodePathSkipsBannedAddr(t *testing.T) {
	// Stub listener — counts incoming connection attempts.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var dialCount atomic.Int32
	go func() {
		for {
			c, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			dialCount.Add(1)
			c.Close()
		}
	}()

	bannedAddr := ln.Addr().String()

	// Register bannedAddr as a bootnode so maintainLoop uses the bootnode
	// re-dial path (not the MinPeers/peerList path).
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-ban-bootnode",
		UserAgent:  "aperod/test",
		Bootnodes:  []string{bannedAddr},
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// Seed bootnodeSet to reflect the already-known address (skips DNS lookup).
	p2p.HostSetBootnodeResolved(h, bannedAddr, []string{bannedAddr})

	// Ban the bootnode.
	h.BanPeer(bannedAddr, "banned bootnode — no-reconnect test", 24*time.Hour)

	bans := h.ListBans()
	if len(bans) == 0 {
		t.Fatal("BanPeer did not register any ban entries")
	}

	// Reset the dial counter: Start() may have already dialled bannedAddr
	// during the initial bootnode dial goroutine.  We only care about dials
	// triggered by the maintainLoop tick below.
	time.Sleep(50 * time.Millisecond) // let any in-flight Start() dials land
	dialCount.Store(0)

	// Fire one maintainLoop tick.
	p2p.HostTriggerMaintain(h)
	time.Sleep(150 * time.Millisecond)

	if n := dialCount.Load(); n != 0 {
		t.Errorf("maintainLoop dialed the banned bootnode %d time(s), want 0 — "+
			"IsBanned guard in the bootnode re-dial path may be missing", n)
	}
	if pc := h.PeerCount(); pc != 0 {
		t.Errorf("PeerCount = %d after maintainLoop tick, want 0", pc)
	}
	t.Logf("✓ maintainLoop bootnode path did not reconnect to banned addr %s (dialCount=%d)",
		bannedAddr, dialCount.Load())
}

// TestBanReconnect_BareIPBanBlocksListenPort confirms that banning a peer via
// its ephemeral connection address ("IP:port") also blocks a future dial to
// the peer's listen port ("IP:listenPort") — the two are different strings but
// share the same bare IP.
//
// This is the direct unit test for the Ban() bare-IP propagation fix: without
// it, IsBanned("IP:listenPort") returned false when the ban was stored under
// "IP:connPort" and the bare IP had no separate entry.
func TestBanReconnect_BareIPBanBlocksListenPort(t *testing.T) {
	// Two listeners: one represents the "connection port" (ephemeral) we ban,
	// the other the "listen port" we try to dial after the ban.
	lnEphemeral, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral: %v", err)
	}
	defer lnEphemeral.Close()

	lnListen, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen listen: %v", err)
	}
	defer lnListen.Close()

	var dialCount atomic.Int32
	go func() {
		for {
			c, aErr := lnListen.Accept()
			if aErr != nil {
				return
			}
			dialCount.Add(1)
			c.Close()
		}
	}()

	ephemeralAddr := lnEphemeral.Addr().String() // "127.0.0.1:XXXXX" — banned addr
	listenAddr := lnListen.Addr().String()        // "127.0.0.1:YYYYY" — dial target

	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		MinPeers:   1,
		NodeID:     "test-bare-ip",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// Ban via the ephemeral address.  Ban() must also write a bare-IP entry so
	// that IsBanned(listenAddr) returns true.
	h.BanPeer(ephemeralAddr, "ephemeral-port ban", 24*time.Hour)

	// Seed the LISTEN address (different port!) into peerList.
	p2p.HostAddKnownPeer(h, listenAddr)

	// Fire one maintainLoop tick.
	p2p.HostTriggerMaintain(h)
	time.Sleep(150 * time.Millisecond)

	if n := dialCount.Load(); n != 0 {
		t.Errorf("maintainLoop dialed the listen port %s (%d time(s)) even though "+
			"the bare IP was banned via ephemeral port %s — bare-IP ban propagation broken",
			listenAddr, n, ephemeralAddr)
	}
	t.Logf("✓ bare-IP ban via ephemeral port %s blocked dial to listen port %s (dialCount=%d)",
		ephemeralAddr, listenAddr, dialCount.Load())
}
