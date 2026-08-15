package p2p_test

// bootnode_dial_backoff_test.go — unit tests confirming that maintainLoop
// applies per-bootnode exponential back-off capped at MaxDialBackoff.
//
// The tests exercise five invariants:
//  1. After a dial failure, the bootnode is in a back-off window and
//     maintainLoop does NOT re-dial on the very next tick.
//  2. Once the back-off window expires, maintainLoop retries the dial.
//  3. MaxDialBackoff defaults to 5 m when the caller passes 0.
//  4. The MinPeers known-peer redial path respects the same back-off window;
//     a bootnode that has failed is NOT dialled through peerList while in back-off.
//  5. A negative MaxDialBackoff is treated as invalid and replaced with the default.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/p2p"
)

// stubDialFail is a dial function that always returns a "connection refused"
// error without touching the network.
func stubDialFail(_ context.Context, network, _ string) (net.Conn, error) {
	return nil, &net.OpError{
		Op:  "dial",
		Net: network,
		Err: fmt.Errorf("connection refused (test stub)"),
	}
}

// TestBootnodeDialBackoff_DefaultMaxDialBackoff confirms that NewHost applies
// the 5 m default when MaxDialBackoff is left at zero.
func TestBootnodeDialBackoff_DefaultMaxDialBackoff(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   5,
		NodeID:     "test-default-backoff",
		UserAgent:  "aperod-test/0.1",
		// MaxDialBackoff intentionally left at zero → default must be applied.
	}, &stubHandler{}, log)

	got := p2p.HostMaxDialBackoff(h)
	if got != 5*time.Minute {
		t.Errorf("MaxDialBackoff default: got %v, want 5m", got)
	}
}

// TestBootnodeDialBackoff_InBackoffAfterFail confirms that after a bootnode
// dial failure is injected, HostBootnodeInBackoff returns true and
// maintainLoop does not issue another dial on the next tick.
func TestBootnodeDialBackoff_InBackoffAfterFail(t *testing.T) {
	const bootnodeAddr = "127.0.0.1:19941"
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	h := p2p.NewHost(p2p.Config{
		ListenAddr:          "127.0.0.1:0",
		Bootnodes:           []string{bootnodeAddr},
		MaxPeers:            5,
		NodeID:              "test-backoff-in-window",
		UserAgent:           "aperod-test/0.1",
		MaxDialBackoff:      2 * time.Second, // short for test
		MaxStaleBootnodeAge: time.Hour,        // long so the stale WARN doesn't interfere
	}, &stubHandler{}, log)

	// Pre-register the bootnode address so isBootnode() returns true.
	p2p.HostSetBootnodeResolved(h, bootnodeAddr, []string{bootnodeAddr})

	// Count how many times the dial function is invoked.
	var dialCount atomic.Int64
	p2p.SetDialFunc(h, func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialCount.Add(1)
		return stubDialFail(ctx, network, addr)
	})

	if err := h.Start(); err != nil {
		t.Fatalf("Host.Start: %v", err)
	}
	defer h.Stop()

	// Wait until at least one startup dial attempt has failed and the
	// back-off state has been recorded.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p2p.HostBootnodeInBackoff(h, bootnodeAddr) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !p2p.HostBootnodeInBackoff(h, bootnodeAddr) {
		t.Fatal("expected bootnode to be in back-off window after dial failure, but HostBootnodeInBackoff returned false")
	}

	// Trigger a maintainLoop tick while still in the back-off window.
	beforeCount := dialCount.Load()
	p2p.HostTriggerMaintain(h)
	time.Sleep(100 * time.Millisecond) // give the goroutine time to run

	afterCount := dialCount.Load()
	if afterCount > beforeCount {
		t.Errorf("maintainLoop fired a dial while bootnode was in back-off window: count went from %d to %d", beforeCount, afterCount)
	}
}

// TestBootnodeDialBackoff_NegativeMaxDialBackoff confirms that a negative
// MaxDialBackoff is treated as invalid and replaced with the 5 m production
// default rather than silently disabling the throttle.
func TestBootnodeDialBackoff_NegativeMaxDialBackoff(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	h := p2p.NewHost(p2p.Config{
		ListenAddr:     "127.0.0.1:0",
		MaxPeers:       5,
		NodeID:         "test-negative-backoff",
		UserAgent:      "aperod-test/0.1",
		MaxDialBackoff: -1 * time.Second, // invalid: negative → must be replaced with default
	}, &stubHandler{}, log)

	got := p2p.HostMaxDialBackoff(h)
	if got != 5*time.Minute {
		t.Errorf("negative MaxDialBackoff was not replaced with 5 m default: got %v", got)
	}
}

// TestBootnodeDialBackoff_MinPeersBypassBlocked confirms that the MinPeers
// known-peer redial path does not bypass the per-bootnode back-off window.
//
// Scenario: a bootnode's resolved address is also present in peerList (which
// can happen via normal peer-exchange after the first successful connection).
// When the bootnode is in back-off and the host is below MinPeers, the
// MinPeers loop must skip the address rather than dialling it directly.
func TestBootnodeDialBackoff_MinPeersBypassBlocked(t *testing.T) {
	const bootnodeAddr = "127.0.0.1:19943"
	const backoff = 400 * time.Millisecond

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	h := p2p.NewHost(p2p.Config{
		ListenAddr:          "127.0.0.1:0",
		Bootnodes:           []string{bootnodeAddr},
		MaxPeers:            10,
		MinPeers:            5, // intentionally high so count < MinPeers is always true
		NodeID:              "test-minpeers-bypass",
		UserAgent:           "aperod-test/0.1",
		MaxDialBackoff:      backoff,
		MaxStaleBootnodeAge: time.Hour,
	}, &stubHandler{}, log)

	// Register the bootnode address in both the bootnode set and peerList so
	// maintainLoop's MinPeers path also sees it.
	p2p.HostSetBootnodeResolved(h, bootnodeAddr, []string{bootnodeAddr})
	p2p.HostAddKnownPeer(h, bootnodeAddr)

	// Count dial attempts to the bootnode address.
	var dialCount atomic.Int64
	p2p.SetDialFunc(h, func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == bootnodeAddr {
			dialCount.Add(1)
		}
		return stubDialFail(ctx, network, addr)
	})

	if err := h.Start(); err != nil {
		t.Fatalf("Host.Start: %v", err)
	}
	defer h.Stop()

	// Wait until the startup dial has failed and the back-off is recorded.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p2p.HostBootnodeInBackoff(h, bootnodeAddr) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !p2p.HostBootnodeInBackoff(h, bootnodeAddr) {
		t.Fatal("back-off state not recorded within 2 s of startup")
	}

	// Fire a maintain tick while the back-off is still active.
	// Neither the bootnode loop NOR the MinPeers peerList loop should dial.
	beforeCount := dialCount.Load()
	p2p.HostTriggerMaintain(h)
	time.Sleep(150 * time.Millisecond) // allow goroutines to run

	if dialCount.Load() > beforeCount {
		t.Errorf("MinPeers peerList path bypassed bootnode back-off: "+
			"dial count went from %d to %d while in back-off window",
			beforeCount, dialCount.Load())
	}

	// Now wait for the back-off to expire and confirm the next tick retries.
	time.Sleep(backoff + 50*time.Millisecond)

	if p2p.HostBootnodeInBackoff(h, bootnodeAddr) {
		t.Fatal("bootnode still in back-off after window should have expired")
	}

	beforeCount = dialCount.Load()
	p2p.HostTriggerMaintain(h)

	retryDeadline := time.Now().Add(2 * time.Second)
	retried := false
	for time.Now().Before(retryDeadline) {
		if dialCount.Load() > beforeCount {
			retried = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !retried {
		t.Errorf("bootnode was not retried after back-off expired via peerList path "+
			"(count before=%d, after=%d)", beforeCount, dialCount.Load())
	}
}

// TestBootnodeDialBackoff_RetryAfterExpiry confirms that once the back-off
// window expires, the next maintainLoop tick retries the dial.
func TestBootnodeDialBackoff_RetryAfterExpiry(t *testing.T) {
	const bootnodeAddr = "127.0.0.1:19942"
	const backoff = 300 * time.Millisecond // short so the test finishes fast

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	h := p2p.NewHost(p2p.Config{
		ListenAddr:          "127.0.0.1:0",
		Bootnodes:           []string{bootnodeAddr},
		MaxPeers:            5,
		NodeID:              "test-backoff-retry",
		UserAgent:           "aperod-test/0.1",
		MaxDialBackoff:      backoff,
		MaxStaleBootnodeAge: time.Hour,
	}, &stubHandler{}, log)

	p2p.HostSetBootnodeResolved(h, bootnodeAddr, []string{bootnodeAddr})

	var dialCount atomic.Int64
	p2p.SetDialFunc(h, func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialCount.Add(1)
		return stubDialFail(ctx, network, addr)
	})

	if err := h.Start(); err != nil {
		t.Fatalf("Host.Start: %v", err)
	}
	defer h.Stop()

	// Wait for the first failure to land and the back-off to be recorded.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p2p.HostBootnodeInBackoff(h, bootnodeAddr) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !p2p.HostBootnodeInBackoff(h, bootnodeAddr) {
		t.Fatal("back-off state not recorded within 2 s of startup")
	}

	// Wait for the back-off window to expire.
	time.Sleep(backoff + 100*time.Millisecond)

	if p2p.HostBootnodeInBackoff(h, bootnodeAddr) {
		t.Fatal("bootnode still reported as in-backoff after window should have expired")
	}

	// Now fire a maintainLoop tick and confirm a new dial attempt is made.
	beforeCount := dialCount.Load()
	p2p.HostTriggerMaintain(h)

	retried := false
	retryDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(retryDeadline) {
		if dialCount.Load() > beforeCount {
			retried = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !retried {
		t.Errorf("maintainLoop did not retry the bootnode dial after the back-off expired (count before=%d, after=%d)",
			beforeCount, dialCount.Load())
	}
}
