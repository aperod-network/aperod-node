package p2p_test

// Tests that a custom get_block_stall_timeout loaded from a node.yaml-style
// file is applied by the p2p host: NewHost must retain the raised value
// (instead of falling back to the 15s default) and the stall-detection
// ticker must fire at the configured interval.  The production
// config.Config → p2p.Config mapping itself is covered by
// TestBuildP2PConfig_GetBlockStallTimeoutFromYAML in cmd/node, which invokes
// the real buildP2PConfig used at startup.

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/p2p"
)

// TestStallTimeout_HostRetainsRaisedValue verifies that NewHost keeps a
// raised 60s stall timeout (slow-link configuration) rather than resetting
// it to the built-in 15s default, and that the zero value still yields 15s.
func TestStallTimeout_HostRetainsRaisedValue(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "stall-cfg-test",
		UserAgent:            "aperod/test",
		GetBlockStallTimeout: 60 * time.Second,
	}, &stubHandler{}, newTestLogger())
	if got := p2p.HostGetBlockStallTimeout(h); got != 60*time.Second {
		t.Fatalf("host: GetBlockStallTimeout = %v, want 60s (raised value was lost)", got)
	}

	hDefault := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "stall-default-test",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if got := p2p.HostGetBlockStallTimeout(hDefault); got != 15*time.Second {
		t.Fatalf("host default: GetBlockStallTimeout = %v, want 15s", got)
	}
}

// TestStallTimeout_ConfiguredValueDrivesStallTicker verifies end-to-end that
// the stall-detection ticker fires at the *configured* timeout, not at the
// built-in default.  The timeout is loaded from a node.yaml-style file
// (1200ms — scaled down from the production 60s so the test stays fast; the
// value flows through the identical config.Load → p2p.Config → NewHost path).
//
// A raw TCP peer completes the handshake at height=3, serves fake headers,
// and silently drops every MsgGetBlock — the same stall simulation as
// TestSync_RelayStall_ReissuesGetHeaders.  Unlike that test, the assertion
// here is on a stall-specific observable — GetStallEvents — so a re-issued
// MsgGetHeaders from the ordinary 3s sync ticker can never satisfy the test:
//
//   1. No StallEvent may exist before the configured timeout has elapsed
//      (measured from a baseline taken BEFORE the host is started, so no
//      earlier ticker could have fired unobserved).
//   2. A StallEvent MUST be recorded within the [timeout, timeout+grace]
//      window.  If the config-to-host wiring broke and the host silently used
//      the 15s default, no StallEvent appears in the window and the test fails.
func TestStallTimeout_ConfiguredValueDrivesStallTicker(t *testing.T) {
	const peerHeight = uint64(3)
	const stallTimeout = 1200 * time.Millisecond

	// ── Load the stall timeout from a node.yaml-style config file ───────────
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(yamlPath, []byte("p2p:\n  get_block_stall_timeout: 1200ms\n  max_peers: 10\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cfg, err := config.Load(yamlPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.P2P.GetBlockStallTimeout != stallTimeout {
		t.Fatalf("config: got %v, want %v", cfg.P2P.GetBlockStallTimeout, stallTimeout)
	}

	// ── Build fake headers with distinct non-zero hashes ────────────────────
	fakeHeaders := make([]p2p.SerializedHeader, peerHeight)
	for i := uint64(0); i < peerHeight; i++ {
		var sh p2p.SerializedHeader
		sh.Height = i + 1
		sh.Hash[0] = byte(i + 1)
		sh.Hash[1] = 0xBE
		sh.Hash[2] = 0xEF
		fakeHeaders[i] = sh
	}

	// ── Raw TCP "peer" that stalls every GetBlock ───────────────────────────
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var tsMu sync.Mutex
	var firstGetHeadersAt time.Time

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		// Asymmetric handshake: the relay dials us outbound → sends Ping first.
		conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		msgType, _, rdErr := p2p.ReadMsg(conn)
		if rdErr != nil || msgType != p2p.MsgPing {
			t.Logf("peer server: expected MsgPing, got %v err=%v", msgType, rdErr)
			return
		}
		if wErr := p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
			NodeID:    "stall-cfg-peer",
			Height:    peerHeight,
			UserAgent: "test",
			Timestamp: time.Now().Unix(),
		}); wErr != nil {
			t.Logf("peer server: write Pong: %v", wErr)
			return
		}

		for {
			conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
			mt, _, rdErr2 := p2p.ReadMsg(conn)
			if rdErr2 != nil {
				return
			}
			switch mt {
			case p2p.MsgGetHeaders:
				tsMu.Lock()
				if firstGetHeadersAt.IsZero() {
					firstGetHeadersAt = time.Now()
				}
				tsMu.Unlock()
				if wErr := p2p.WriteMsg(conn, p2p.MsgHeaders, p2p.HeadersMsg{
					Headers: fakeHeaders,
				}); wErr != nil {
					return
				}
			case p2p.MsgGetBlock:
				// Silently drop — this is the simulated stall.
			case p2p.MsgPing:
				_ = p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
					NodeID: "stall-cfg-peer", Height: peerHeight,
					UserAgent: "test", Timestamp: time.Now().Unix(),
				})
			}
		}
	}()

	// ── Relay host configured from the yaml-loaded value ────────────────────
	relayHost := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             cfg.P2P.MaxPeers,
		NodeID:               "relay-stall-cfg-test",
		UserAgent:            "aperod/test",
		GetBlockStallTimeout: cfg.P2P.GetBlockStallTimeout,
	}, &stubHandler{}, newTestLogger())
	if got := p2p.HostGetBlockStallTimeout(relayHost); got != stallTimeout {
		t.Fatalf("host: GetBlockStallTimeout = %v, want %v", got, stallTimeout)
	}

	// Baseline BEFORE the host starts: any StallEvent from any ticker created
	// at any later point is at or after this instant, so the "no early stall"
	// assertion below cannot miss an event.
	baseline := time.Now()

	if err := relayHost.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer relayHost.Stop()

	relayHost.DialPeer(ln.Addr().String())

	// ── Wait until the sync pipeline is primed (first GetHeaders sent) ──────
	deadline := time.Now().Add(3 * time.Second)
	var t0 time.Time
	for time.Now().Before(deadline) {
		tsMu.Lock()
		t0 = firstGetHeadersAt
		tsMu.Unlock()
		if !t0.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if t0.IsZero() {
		t.Fatal("peer server never received a MsgGetHeaders from the relay")
	}

	// ── Assertion 1: no stall event before the configured timeout ───────────
	// The earliest possible pendingBlocks entry is created after t0 (the
	// GetBlock requests follow the headers response), so no stall may be
	// recorded until at least t0 + stallTimeout.  Check just before that
	// point (with scheduling grace).
	earlyCheck := t0.Add(stallTimeout - 300*time.Millisecond)
	if wait := time.Until(earlyCheck); wait > 0 {
		time.Sleep(wait)
	}
	if events := relayHost.GetStallEvents(baseline); len(events) != 0 {
		t.Fatalf("StallEvent recorded only %v after the first GetHeaders — "+
			"earlier than the configured %v stall timeout; the host is using a "+
			"smaller timeout than configured (event: %+v)",
			time.Since(t0), stallTimeout, events[0])
	}

	// ── Assertion 2: a stall event MUST appear near the configured timeout ──
	// Allow up to stallTimeout + 1.3s for ticker phase + scheduling.  This
	// window (≤ 2.5s total) is far below the 15s default, so if the config
	// wiring broke and the default was in effect, no event appears and the
	// test fails.  A GetHeaders re-issue from the 3s sync ticker cannot
	// satisfy this assertion because the sync ticker never records StallEvents.
	windowEnd := t0.Add(stallTimeout + 1300*time.Millisecond)
	var stallAt time.Time
	for time.Now().Before(windowEnd) {
		if events := relayHost.GetStallEvents(baseline); len(events) > 0 {
			stallAt = events[0].At
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stallAt.IsZero() {
		t.Fatalf("no StallEvent recorded within %v of the first GetHeaders — "+
			"the configured %v stall timeout was not applied (host likely fell "+
			"back to the 15s default)", stallTimeout+1300*time.Millisecond, stallTimeout)
	}
	t.Logf("StallEvent recorded %.0fms after first GetHeaders (configured stallTimeout=%v)",
		float64(stallAt.Sub(t0).Milliseconds()), stallTimeout)
}
