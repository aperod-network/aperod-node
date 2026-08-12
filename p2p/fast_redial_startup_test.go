package p2p_test

// Tests for TASK #2008: reconnect to known-good peers fast after a node restart.
//
// Problem: after a restart the node used to wait for the first discovery /
// maintain tick (10 s) before dialling its configured known-good peers.  A
// deploy health check that inspects peer_count at 30 s would then flap a
// warning even though the network is healthy.  Start() must fire an immediate
// outbound dial of every configured bootnode AND every dialable "host:port"
// whitelist entry the instant it is called — not on the first ticker tick.
//
// These tests exercise the NEW whitelist fast-redial path in Start() directly
// (they would fail if the whitelist-dial block were removed), plus the shared
// outbound admission gate in dialPeer that enforces MaxPeers / MaxPeersPerIP
// atomically across concurrent redials.

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/p2p"
)

// waitForPeerCount polls h.PeerCount() until it reaches want or the deadline
// elapses; returns the last observed count.
func waitForPeerCount(h *p2p.Host, want int, budget time.Duration) int {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if h.PeerCount() >= want {
			return h.PeerCount()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return h.PeerCount()
}

// TestStart_FastRedialBootnodeOnStartup proves the immediate startup dial:
// the second host connects to the first via its Bootnodes entry in far less
// time than the 10 s maintain-tick interval that gated the old code path.
func TestStart_FastRedialBootnodeOnStartup(t *testing.T) {
	listener := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "listener",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := listener.Start(); err != nil {
		t.Fatalf("listener.Start: %v", err)
	}
	t.Cleanup(listener.Stop)

	listenAddr := listener.ListenAddr()
	if listenAddr == "" {
		t.Skip("ListenAddr not available")
	}

	dialer := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "dialer",
		UserAgent:  "aperod/test",
		Bootnodes:  []string{listenAddr},
	}, &stubHandler{}, newTestLogger())

	start := time.Now()
	if err := dialer.Start(); err != nil {
		t.Fatalf("dialer.Start: %v", err)
	}
	t.Cleanup(dialer.Stop)

	const maintainTick = 10 * time.Second
	const budget = 3 * time.Second
	got := waitForPeerCount(dialer, 1, budget)
	elapsed := time.Since(start)
	if got < 1 {
		t.Fatalf("dialer did not connect to bootnode within %v — immediate "+
			"startup dial regressed (would only fire after the %v maintain tick)",
			budget, maintainTick)
	}
	if elapsed >= maintainTick {
		t.Fatalf("connection took %v (>= %v maintain tick) — dial not immediate", elapsed, maintainTick)
	}
	t.Logf("✓ dialer connected to bootnode in %v (well under %v maintain tick)", elapsed, maintainTick)
}

// TestStart_FastRedialWhitelistPeer proves the NEW path: a dialable "host:port"
// whitelist entry (with NO bootnodes configured) is dialed immediately at
// startup.  Removing the whitelist-dial block in Start() makes this fail.
func TestStart_FastRedialWhitelistPeer(t *testing.T) {
	listener := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "listener",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := listener.Start(); err != nil {
		t.Fatalf("listener.Start: %v", err)
	}
	t.Cleanup(listener.Stop)

	listenAddr := listener.ListenAddr()
	if listenAddr == "" {
		t.Skip("ListenAddr not available")
	}

	// NO Bootnodes — the ONLY way this host can connect is via the dialable
	// host:port whitelist entry being dialed at startup.
	dialer := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "dialer",
		UserAgent:     "aperod/test",
		PeerWhitelist: []string{listenAddr},
	}, &stubHandler{}, newTestLogger())

	start := time.Now()
	if err := dialer.Start(); err != nil {
		t.Fatalf("dialer.Start: %v", err)
	}
	t.Cleanup(dialer.Stop)

	const budget = 3 * time.Second
	if got := waitForPeerCount(dialer, 1, budget); got < 1 {
		t.Fatalf("dialer did not connect to whitelist host:port entry within %v — "+
			"the Start() whitelist fast-redial path did not fire", budget)
	}
	t.Logf("✓ dialer connected to whitelist host:port peer in %v", time.Since(start))
}

// TestStart_WhitelistBareIPAndCIDRNotDialed asserts that bare-IP and CIDR
// whitelist entries (the common inbound-ACL form) are NOT dialed at startup:
// dialableWhitelistAddr must reject them, so the outbound dial function is
// never invoked and Start() returns nil.
func TestStart_WhitelistBareIPAndCIDRNotDialed(t *testing.T) {
	dialer := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "dialer",
		UserAgent:     "aperod/test",
		PeerWhitelist: []string{"203.0.113.7", "10.0.0.0/8", "::1", "2001:db8::/32"},
	}, &stubHandler{}, newTestLogger())

	var dialCount int32
	p2p.SetDialFunc(dialer, func(_ context.Context, _, addr string) (net.Conn, error) {
		atomic.AddInt32(&dialCount, 1)
		t.Errorf("no outbound dial expected for bare-IP/CIDR whitelist entries, got dial to %q", addr)
		return nil, context.Canceled
	})

	if err := dialer.Start(); err != nil {
		t.Fatalf("dialer.Start returned error for bare-IP/CIDR whitelist: %v", err)
	}
	t.Cleanup(dialer.Stop)

	// Give any (erroneous) startup dial goroutines time to fire.
	time.Sleep(300 * time.Millisecond)
	if n := atomic.LoadInt32(&dialCount); n != 0 {
		t.Fatalf("outbound dial invoked %d time(s) for non-dialable whitelist entries; want 0", n)
	}
	t.Logf("✓ bare-IP and CIDR whitelist entries were not dialed")
}

// TestStart_WhitelistBannedTargetNotDialed asserts a dialable whitelist target
// whose bare IP is banned is NOT dialed at startup: dialPeer's ban gate must
// short-circuit before the outbound dial function is invoked.
func TestStart_WhitelistBannedTargetNotDialed(t *testing.T) {
	const target = "203.0.113.9:30303"

	dialer := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "dialer",
		UserAgent:     "aperod/test",
		PeerWhitelist: []string{target},
	}, &stubHandler{}, newTestLogger())

	// Ban the bare IP BEFORE Start() so the startup redial must skip it.
	p2p.HostBanPeer(dialer, "203.0.113.9", "pre-banned for test", time.Hour)

	var dialCount int32
	p2p.SetDialFunc(dialer, func(_ context.Context, _, addr string) (net.Conn, error) {
		atomic.AddInt32(&dialCount, 1)
		t.Errorf("banned whitelist target must not be dialed, got dial to %q", addr)
		return nil, context.Canceled
	})

	if err := dialer.Start(); err != nil {
		t.Fatalf("dialer.Start: %v", err)
	}
	t.Cleanup(dialer.Stop)

	time.Sleep(300 * time.Millisecond)
	if n := atomic.LoadInt32(&dialCount); n != 0 {
		t.Fatalf("outbound dial invoked %d time(s) for a banned whitelist target; want 0", n)
	}
	t.Logf("✓ banned whitelist target was not dialed")
}

// TestStart_WhitelistRedial_RespectsMaxPeers validates the shared outbound
// admission gate: several dialable whitelist entries (distinct IPs) with
// MaxPeers=1 must result in AT MOST ONE outbound dial actually being
// initiated, even though all redial goroutines start concurrently.
//
// The injected dial func BLOCKS after being entered and records how many
// times it is invoked.  The first dial that wins the gate registers its
// in-flight reservation (dialingIPs) and then blocks inside the dial func;
// every other concurrent dial sees totalPeers(0)+totalInflight(1) >= 1 and is
// rejected by the reservation counter BEFORE reaching the dial func.  Without
// the shared admission gate, all four dials would pass the (old, racy)
// per-goroutine capacity snapshot and enter the dial func — so an invocation
// count > 1 proves the gate is missing/broken.
func TestStart_WhitelistRedial_RespectsMaxPeers(t *testing.T) {
	targets := []string{
		"203.0.113.11:30303",
		"203.0.113.12:30303",
		"203.0.113.13:30303",
		"203.0.113.14:30303",
	}

	dialer := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      1, // total cap of one connection
		NodeID:        "dialer",
		UserAgent:     "aperod/test",
		PeerWhitelist: targets,
	}, &stubHandler{}, newTestLogger())

	var dialInvocations int32
	release := make(chan struct{})
	p2p.SetDialFunc(dialer, func(ctx context.Context, _, addr string) (net.Conn, error) {
		atomic.AddInt32(&dialInvocations, 1)
		// Hold the single reservation open so concurrent dials must observe it
		// and be gated.  Unblock on context cancel (Stop) or test teardown.
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, context.Canceled
	})

	if err := dialer.Start(); err != nil {
		t.Fatalf("dialer.Start: %v", err)
	}
	t.Cleanup(func() {
		close(release)
		dialer.Stop()
	})

	// Give all four redial goroutines time to reach (or be gated before) the
	// blocking dial func.  The first holds its reservation for the whole window.
	time.Sleep(500 * time.Millisecond)

	if n := atomic.LoadInt32(&dialInvocations); n > 1 {
		t.Fatalf("MaxPeers=1 violated: outbound dial func invoked %d times via "+
			"concurrent whitelist redials — the admission gate did not count "+
			"in-flight reservations against the total cap", n)
	}
	t.Logf("✓ concurrent whitelist redials respected MaxPeers=1 (dial invocations=%d)",
		atomic.LoadInt32(&dialInvocations))
}

// TestStart_WhitelistDNSFailureNonFatal asserts that a dialable whitelist entry
// whose hostname fails DNS resolution does not abort Start(): resolution runs
// in a background goroutine and a failure is logged, not returned.
func TestStart_WhitelistDNSFailureNonFatal(t *testing.T) {
	dialer := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "dialer",
		UserAgent:  "aperod/test",
		// .invalid is reserved (RFC 6761) and never resolves.
		PeerWhitelist: []string{"nonexistent-host.invalid:30303"},
	}, &stubHandler{}, newTestLogger())

	if err := dialer.Start(); err != nil {
		t.Fatalf("dialer.Start must succeed despite an unresolvable whitelist "+
			"host:port entry (resolution is async/non-fatal): %v", err)
	}
	t.Cleanup(dialer.Stop)
	// Allow the async resolution goroutine to run and fail quietly.
	time.Sleep(200 * time.Millisecond)
	t.Logf("✓ Start() succeeded despite unresolvable whitelist host")
}
