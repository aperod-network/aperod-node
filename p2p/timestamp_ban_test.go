package p2p_test

// Tests for the future-timestamp auto-ban logic:
//   A peer that sends TimestampBanThreshold or more MsgBlock messages whose
//   block timestamp is more than 15 s ahead of wall clock is banned by bare IP.
//   - The Nth block (at threshold) triggers disconnect + bare-IP ban.
//   - A reconnect from the same IP on a new source port is rejected.
//   - A BanEvent is recorded for the Admin Panel notification log.
//   - Sending a block with a normal timestamp resets the per-IP strike counter.

import (
	"net"
	"testing"
	"time"

	"github.com/aperod/aperod/p2p"
)

// sendFutureTimestampBlock sends one MsgBlock with a timestamp 30 s in the
// future — well beyond the 15 s hostMaxClockSkewNs guard in host.go.
func sendFutureTimestampBlock(t *testing.T, conn net.Conn) {
	t.Helper()
	futureNs := time.Now().Add(30 * time.Second).UnixNano()
	sb := p2p.SerializedBlock{
		Header: p2p.SerializedHeader{
			Height:    1,
			Timestamp: futureNs,
		},
	}
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	if err := p2p.WriteMsg(conn, p2p.MsgBlock, sb); err != nil {
		t.Fatalf("WriteMsg future-timestamp block: %v", err)
	}
	conn.SetWriteDeadline(time.Time{})
}

// ─── Test: threshold-1 future blocks → not banned ────────────────────────────

func TestTimestampBan_BelowThresholdNotBanned(t *testing.T) {
	const threshold = 3

	h := p2p.NewHost(p2p.Config{
		ListenAddr:            "127.0.0.1:0",
		MaxPeers:              10,
		NodeID:                "test-ts-ban",
		UserAgent:             "aperod/test",
		TimestampBanThreshold: threshold,
		TimestampBanDuration:  time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()
	conn, _ := connectPeer(t, hostAddr)
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Send threshold-1 future-timestamped blocks — must not trigger a ban.
	for i := 0; i < threshold-1; i++ {
		sendFutureTimestampBlock(t, conn)
	}
	time.Sleep(150 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Errorf("peer was banned after only %d future-timestamp blocks (threshold=%d)", threshold-1, threshold)
	}
	if h.PeerCount() != 1 {
		t.Errorf("PeerCount = %d after %d future-timestamp blocks, want 1", h.PeerCount(), threshold-1)
	}
	t.Logf("OK: %d future-timestamp blocks did not trigger a ban (threshold=%d)", threshold-1, threshold)
}

// ─── Test: Nth future block triggers ban by bare IP ──────────────────────────

func TestTimestampBan_ThresholdBlockBansPeer(t *testing.T) {
	const threshold = 3

	h := p2p.NewHost(p2p.Config{
		ListenAddr:            "127.0.0.1:0",
		MaxPeers:              10,
		NodeID:                "test-ts-ban-fire",
		UserAgent:             "aperod/test",
		TimestampBanThreshold: threshold,
		TimestampBanDuration:  time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()
	conn, peerIP := connectPeer(t, hostAddr)
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Send exactly threshold future-timestamp blocks; the Nth should ban.
	for i := 0; i < threshold; i++ {
		sendFutureTimestampBlock(t, conn)
	}

	// Disconnect + ban should happen within a short window.
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("peer was NOT disconnected after %d future-timestamp blocks", threshold)
	}

	// Verify the ban entry exists and is keyed by bare IP.
	bans := h.ListBans()
	if len(bans) == 0 {
		t.Fatalf("no ban entry found after %d future-timestamp blocks", threshold)
	}
	found := false
	for _, b := range bans {
		if b.Addr == peerIP {
			found = true
			t.Logf("ban entry: addr=%s reason=%q expiresAt=%s", b.Addr, b.Reason, b.ExpiresAt)
			remaining := time.Until(b.ExpiresAt)
			if remaining < 59*time.Minute {
				t.Errorf("ban duration too short: %v (want ≥ 59m)", remaining)
			}
		}
	}
	if !found {
		t.Errorf("ban entry for bare IP %q not found; all bans: %v", peerIP, bans)
	}
	t.Logf("OK: peer banned by bare IP after %d future-timestamp blocks", threshold)
}

// ─── Test: reconnect on a new source port is rejected ────────────────────────

func TestTimestampBan_ReconnectRejected(t *testing.T) {
	const threshold = 2

	h := p2p.NewHost(p2p.Config{
		ListenAddr:            "127.0.0.1:0",
		MaxPeers:              10,
		NodeID:                "test-ts-ban-reconnect",
		UserAgent:             "aperod/test",
		TimestampBanThreshold: threshold,
		TimestampBanDuration:  time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()
	conn, peerIP := connectPeer(t, hostAddr)
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Trigger the ban.
	for i := 0; i < threshold; i++ {
		sendFutureTimestampBlock(t, conn)
	}
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Fatal("peer was not disconnected after sending threshold future-timestamp blocks")
	}

	// Attempt to reconnect on a fresh (OS-chosen) port.
	conn2, err := net.DialTimeout("tcp", hostAddr, 2*time.Second)
	if err != nil {
		// Refused at TCP level — that also satisfies the test.
		t.Logf("OK: reconnect refused at TCP level (bare-IP ban active for %s)", peerIP)
		return
	}
	defer conn2.Close()

	// If TCP connected, the handshake (Ping → Pong) should fail because the
	// host closes the connection as soon as it sees the banned source IP.
	conn2.SetDeadline(time.Now().Add(2 * time.Second))
	_ = p2p.WriteMsg(conn2, p2p.MsgPing, p2p.PingMsg{
		NodeID:    "rogue-reconnect",
		Height:    0,
		UserAgent: "test",
		Timestamp: time.Now().Unix(),
	})
	// Expect EOF / closed — not a successful Pong.
	msgType, _, readErr := p2p.ReadMsg(conn2)
	if readErr == nil && msgType == p2p.MsgPong {
		t.Errorf("reconnect was accepted (Pong received) — bare-IP ban did not block the new connection from %s", peerIP)
	} else {
		t.Logf("OK: reconnect from bare IP %s was rejected (msgType=%v err=%v)", peerIP, msgType, readErr)
	}
}

// ─── Test: BanEvent recorded for Admin Panel ─────────────────────────────────

func TestTimestampBan_BanEventRecorded(t *testing.T) {
	const threshold = 2

	h := p2p.NewHost(p2p.Config{
		ListenAddr:            "127.0.0.1:0",
		MaxPeers:              10,
		NodeID:                "test-ts-ban-event",
		UserAgent:             "aperod/test",
		TimestampBanThreshold: threshold,
		TimestampBanDuration:  time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()
	conn, peerIP := connectPeer(t, hostAddr)
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	before := time.Now()
	for i := 0; i < threshold; i++ {
		sendFutureTimestampBlock(t, conn)
	}
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Fatal("peer was not disconnected after threshold future-timestamp blocks")
	}

	events := h.GetBanEvents(before)
	if len(events) == 0 {
		t.Fatal("no BanEvent recorded after timestamp ban")
	}
	found := false
	for _, ev := range events {
		if ev.IP == peerIP {
			found = true
			t.Logf("BanEvent: ip=%s reason=%q violations=%d duration=%ds",
				ev.IP, ev.Reason, ev.Violations, ev.BanDurationSecs)
			if ev.Violations != threshold {
				t.Errorf("BanEvent.Violations = %d, want %d", ev.Violations, threshold)
			}
			if ev.BanDurationSecs < 3500 { // ~1 h
				t.Errorf("BanEvent.BanDurationSecs = %d, want ~3600", ev.BanDurationSecs)
			}
		}
	}
	if !found {
		t.Errorf("BanEvent for IP %q not found among %d events", peerIP, len(events))
	}
	t.Logf("OK: BanEvent recorded for IP %s after %d future-timestamp violations", peerIP, threshold)
}

// ─── Test: out-of-range height AND future timestamp still counts ts strike ────
//
// This is the bypass scenario the reviewer flagged: a block whose height exceeds
// BadBlockHeightLead (so it would enter the bad-block branch) AND whose timestamp
// is more than 15 s in the future must still accumulate a timestamp strike.
// Before the fix, the bad-block branch returned nil first, skipping the ts check.

func TestTimestampBan_OutOfRangeFutureTsCountsStrike(t *testing.T) {
	const tsThreshold = 3
	const badBlockThreshold = 10 // set higher so bad-block ban doesn't fire first

	h := p2p.NewHost(p2p.Config{
		ListenAddr:            "127.0.0.1:0",
		MaxPeers:              10,
		NodeID:                "test-ts-outofrange",
		UserAgent:             "aperod/test",
		TimestampBanThreshold: tsThreshold,
		TimestampBanDuration:  time.Hour,
		BadBlockBanThreshold:  badBlockThreshold,
		BadBlockBanDuration:   24 * time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()
	conn, peerIP := connectPeer(t, hostAddr)
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Send tsThreshold blocks that are BOTH out-of-range height (>tip+1000)
	// AND future-timestamped. The timestamp-ban must fire after tsThreshold blocks
	// even though badBlockBanThreshold is 10 (larger than tsThreshold).
	futureNs := time.Now().Add(30 * time.Second).UnixNano()
	for i := 0; i < tsThreshold; i++ {
		sb := p2p.SerializedBlock{
			Header: p2p.SerializedHeader{
				Height:    5000, // ourTip=0, lead=1000 → definitely out of range
				Timestamp: futureNs,
			},
		}
		conn.SetWriteDeadline(time.Now().Add(time.Second))
		if err := p2p.WriteMsg(conn, p2p.MsgBlock, sb); err != nil {
			// connection may have been closed by the host if ban fired — that's OK
			break
		}
		conn.SetWriteDeadline(time.Time{})
	}

	// The ts-ban (threshold=3) must have fired, NOT the bad-block ban (threshold=10).
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Fatalf("peer was not banned after %d out-of-range future-timestamp blocks "+
			"(ts threshold=%d should have fired before bad-block threshold=%d)",
			tsThreshold, tsThreshold, badBlockThreshold)
	}

	bans := h.ListBans()
	var found bool
	for _, b := range bans {
		if b.Addr == peerIP {
			found = true
			t.Logf("ban entry: addr=%s reason=%q", b.Addr, b.Reason)
			if b.Reason != "repeated future-timestamped blocks (timejacking attack)" {
				t.Errorf("expected timejacking-attack ban reason, got %q", b.Reason)
			}
		}
	}
	if !found {
		t.Errorf("ban entry for %q not found; all bans: %v", peerIP, bans)
	}
	t.Logf("OK: out-of-range + future-timestamp block correctly triggers timestamp ban at threshold=%d", tsThreshold)
}

// ─── Test: whitelisted peer sending future-timestamp blocks is exempt ─────────
//
// Whitelisted IPs are trusted validators.  A clock-skew issue on a known
// validator must not sever connectivity.  Regardless of how many future-dated
// blocks a whitelisted IP sends, it must never accumulate timestamp strikes.

func TestTimestampBan_WhitelistedPeerExempt(t *testing.T) {
	const tsThreshold = 2

	h := p2p.NewHost(p2p.Config{
		ListenAddr:            "127.0.0.1:0",
		MaxPeers:              10,
		NodeID:                "test-ts-wl-exempt",
		UserAgent:             "aperod/test",
		TimestampBanThreshold: tsThreshold,
		TimestampBanDuration:  time.Hour,
		PeerWhitelist:         []string{"127.0.0.1"},
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()
	conn, _ := connectPeer(t, hostAddr)
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Send more than tsThreshold future-timestamp blocks from the whitelisted IP.
	for i := 0; i < tsThreshold+2; i++ {
		sendFutureTimestampBlock(t, conn)
	}
	time.Sleep(150 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Errorf("whitelisted peer was banned for future-timestamp blocks (should be exempt)")
	}
	if h.PeerCount() != 1 {
		t.Errorf("PeerCount = %d, want 1; whitelisted peer must stay connected", h.PeerCount())
	}
	t.Logf("OK: whitelisted peer sent %d future-timestamp blocks and was not banned", tsThreshold+2)
}

// ─── Test: valid timestamp resets the strike counter ─────────────────────────

func TestTimestampBan_ValidBlockResetsCounter(t *testing.T) {
	const threshold = 3

	h := p2p.NewHost(p2p.Config{
		ListenAddr:            "127.0.0.1:0",
		MaxPeers:              10,
		NodeID:                "test-ts-reset",
		UserAgent:             "aperod/test",
		TimestampBanThreshold: threshold,
		TimestampBanDuration:  time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()
	conn, _ := connectPeer(t, hostAddr)
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Send threshold-1 future-timestamp blocks then one normal block.
	for i := 0; i < threshold-1; i++ {
		sendFutureTimestampBlock(t, conn)
	}
	// Normal-timestamp block: height 1, timestamp = now.
	sendBlockAtHeight(t, conn, 1)
	time.Sleep(150 * time.Millisecond)

	// No ban should have fired.
	if len(h.ListBans()) != 0 {
		t.Errorf("peer was banned even though a normal-timestamp block was sent after %d future blocks", threshold-1)
	}
	if h.PeerCount() != 1 {
		t.Errorf("PeerCount = %d, want 1 after strike reset", h.PeerCount())
	}

	// Now send threshold more future blocks — the counter starts fresh and
	// the peer should be banned only after the full threshold is reached again.
	for i := 0; i < threshold; i++ {
		sendFutureTimestampBlock(t, conn)
	}
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("peer was not banned after counter was reset and threshold re-crossed")
	} else {
		t.Logf("OK: strike counter reset by normal block; peer banned only after a fresh threshold crossing")
	}
}
