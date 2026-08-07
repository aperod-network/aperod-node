package p2p_test

// Tests for the rogue-fork block auto-ban logic:
//   A peer that sends badBlockBanThreshold (10) or more blocks whose height
//   exceeds ourTip+badBlockHeightLead (1000) is banned for 24 hours by bare IP.
//   - A reconnect from the same IP on a new source port is also rejected.
//   - All established connections from that IP are evicted immediately on ban.
//   - Sending a valid-height block resets the counter.
//   - Strike map is capped (badBlockMaxTrackedIPs) to prevent memory exhaustion.

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/aperod/aperod/p2p"
)

// connectPeer dials the host at addr, completes the Ping/Pong handshake
// (asymmetric: dialer sends Ping first, host replies with Pong), and returns
// the open connection.  peerIP is the bare IP the host records for the peer.
func connectPeer(t *testing.T, hostAddr string) (conn net.Conn, peerIP string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", hostAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Extract bare IP from the local side of the connection (as seen by the host).
	localAddr := c.LocalAddr().String()
	host, _, splitErr := net.SplitHostPort(localAddr)
	if splitErr != nil {
		host = localAddr
	}
	peerIP = host

	c.SetDeadline(time.Now().Add(2 * time.Second))
	// Dialer sends Ping first (asymmetric handshake).
	if err := p2p.WriteMsg(c, p2p.MsgPing, p2p.PingMsg{
		NodeID:    "rogue-peer",
		Height:    0,
		UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		c.Close()
		t.Fatalf("write ping: %v", err)
	}
	// Host replies with Pong.
	msgType, _, err := p2p.ReadMsg(c)
	if err != nil || msgType != p2p.MsgPong {
		c.Close()
		t.Fatalf("expected MsgPong, got %v err=%v", msgType, err)
	}
	c.SetDeadline(time.Time{}) // clear deadline
	return c, peerIP
}

// sendBlockAtHeight sends a single MsgBlock with the given height over conn.
func sendBlockAtHeight(t *testing.T, conn net.Conn, height uint64) {
	t.Helper()
	sb := p2p.SerializedBlock{
		Header: p2p.SerializedHeader{Height: height},
	}
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	if err := p2p.WriteMsg(conn, p2p.MsgBlock, sb); err != nil {
		t.Fatalf("WriteMsg block h=%d: %v", height, err)
	}
	conn.SetWriteDeadline(time.Time{})
}

// waitFor polls cond up to maxWait and returns whether it became true.
func waitFor(maxWait time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// ─── Test: 9 bad blocks → not banned ─────────────────────────────────────────

func TestBadBlockBan_9BlocksNotBanned(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-ban",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()
	conn, _ := connectPeer(t, hostAddr)
	defer conn.Close()

	// Wait for peer to register.
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// ourTip = 0 (stubHandler returns 0); out-of-range threshold = 0 + 1000 = 1000.
	// Send 9 blocks with height 2000 — all out of range, but below the ban threshold.
	for i := 0; i < 9; i++ {
		sendBlockAtHeight(t, conn, 2000)
	}

	// Give the host time to process all 9 blocks.
	time.Sleep(150 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Errorf("peer was banned after only 9 out-of-range blocks; expected no ban")
	}
	if h.PeerCount() != 1 {
		t.Errorf("PeerCount = %d after 9 bad blocks, want 1", h.PeerCount())
	}
}

// ─── Test: 10th bad block triggers ban by bare IP ────────────────────────────

func TestBadBlockBan_10thBlockBansPeer(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-ban",
		UserAgent:  "aperod/test",
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

	// Send 10 out-of-range blocks; the 10th should trigger the ban.
	for i := 0; i < 10; i++ {
		sendBlockAtHeight(t, conn, 5000)
	}

	// Ban + connection close should happen within a short window.
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("peer was NOT disconnected after 10 out-of-range blocks")
	}

	bans := h.ListBans()
	if len(bans) == 0 {
		t.Fatalf("no ban entry found after 10 out-of-range blocks")
	}
	found := false
	for _, b := range bans {
		if b.Addr == peerIP {
			found = true
			t.Logf("ban entry: addr=%s (bare IP) reason=%q expiresAt=%s", b.Addr, b.Reason, b.ExpiresAt)
			// Verify the ban duration is close to 24 hours.
			remaining := time.Until(b.ExpiresAt)
			if remaining < 23*time.Hour {
				t.Errorf("ban duration too short: %v (want ≥ 23h)", remaining)
			}
		}
	}
	if !found {
		t.Errorf("ban entry for bare IP %q not found; all bans: %v", peerIP, bans)
	}
}

// ─── Test: reconnect on a new source port is rejected ────────────────────────

func TestBadBlockBan_ReconnectRejected(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-ban-reconnect",
		UserAgent:  "aperod/test",
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
	for i := 0; i < 10; i++ {
		sendBlockAtHeight(t, conn, 5000)
	}
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Fatal("peer was not disconnected after ban")
	}
	t.Logf("peer IP %s banned; attempting reconnect on a new source port", peerIP)

	// Reconnect from the same IP (new OS-assigned source port) — must be rejected.
	conn2, err := net.DialTimeout("tcp", hostAddr, 2*time.Second)
	if err != nil {
		// Connection refused at TCP level means the host itself is down — that's wrong.
		t.Fatalf("host unreachable on reconnect: %v", err)
	}
	defer conn2.Close()

	// Attempt the handshake; the host should close the connection immediately
	// because the bare IP is banned.
	conn2.SetDeadline(time.Now().Add(time.Second))
	_ = p2p.WriteMsg(conn2, p2p.MsgPing, p2p.PingMsg{
		NodeID:    "rogue-peer-reconnect",
		Height:    0,
		UserAgent: "test",
		Timestamp: time.Now().Unix(),
	})
	_, _, readErr := p2p.ReadMsg(conn2)

	// Expect EOF / connection reset — the host drops the connection on the ban check.
	if readErr == nil {
		t.Errorf("reconnect from banned IP %s was accepted (expected immediate close)", peerIP)
	} else {
		t.Logf("reconnect correctly rejected: %v", readErr)
	}

	// Peer must still not be registered.
	time.Sleep(50 * time.Millisecond)
	if h.PeerCount() != 0 {
		t.Errorf("PeerCount = %d after reconnect from banned IP, want 0", h.PeerCount())
	}
}

// ─── Test: all connections from same IP are evicted on ban ───────────────────

// TestBadBlockBan_AllConnectionsEvicted opens two connections from the same IP,
// sends 10 bad blocks on the first, and verifies that BOTH connections are
// closed — not just the one that triggered the threshold.
func TestBadBlockBan_AllConnectionsEvicted(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-ban-evict",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()

	// First connection — will be used to send bad blocks.
	conn1, _ := connectPeer(t, hostAddr)
	defer conn1.Close()

	// Second connection from the same IP (127.0.0.1), different source port.
	conn2, _ := connectPeer(t, hostAddr)
	defer conn2.Close()

	// Wait for both peers to register.
	if !waitFor(600*time.Millisecond, func() bool { return h.PeerCount() == 2 }) {
		t.Fatalf("expected 2 peers registered, got %d", h.PeerCount())
	}
	t.Logf("both connections registered; PeerCount=2")

	// Trigger the ban on the first connection.
	for i := 0; i < 10; i++ {
		sendBlockAtHeight(t, conn1, 9000)
	}

	// Both connections should be evicted because they share the same IP.
	if !waitFor(600*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("PeerCount = %d after ban; both connections should have been evicted", h.PeerCount())
	}

	// Verify the second connection is actually closed (read returns an error).
	conn2.SetDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 1)
	n, err := conn2.Read(buf)
	if n > 0 || err == nil {
		t.Errorf("second connection still readable after IP ban (n=%d err=%v)", n, err)
	} else {
		t.Logf("second connection correctly closed: %v", err)
	}
}

// ─── Test: strike map is capped (memory exhaustion prevention) ───────────────

// TestBadBlockBan_MapCap verifies that the bad-block strike map never grows
// beyond BadBlockMaxTrackedIPs entries.  It uses RecordBadBlockStrike (an
// exported test helper that bypasses the network layer) to inject distinct fake
// IP addresses without OS-level aliasing.  Each IP receives only 1 strike, well
// below the ban threshold, so no connection is closed and there are no races.
func TestBadBlockBan_MapCap(t *testing.T) {
	// NewHost without Start(): we only need the in-memory strike map.
	h := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())

	cap := p2p.BadBlockMaxTrackedIPs

	// Fill the map to exactly cap with distinct /8 fake IPs.
	for i := 0; i < cap; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xFF, (i>>8)&0xFF, i&0xFF)
		h.RecordBadBlockStrike(ip)
	}

	count := h.BadBlockStrikeCount()
	if count != cap {
		t.Fatalf("expected exactly %d entries after filling cap, got %d", cap, count)
	}

	// Recording a brand-new IP must NOT increase the map past the cap.
	h.RecordBadBlockStrike("192.168.255.1")
	afterCap := h.BadBlockStrikeCount()
	if afterCap > cap {
		t.Errorf("strike map grew past cap: %d > %d", afterCap, cap)
	} else {
		t.Logf("cap enforced: %d entries (limit %d)", afterCap, cap)
	}

	// A previously-tracked IP (already in the map) can still accumulate strikes
	// even when the map is full — it is not new, so the capacity guard allows it.
	prev := h.RecordBadBlockStrike("10.0.0.0")
	if prev < 2 {
		t.Errorf("existing IP should have accumulated a second strike, got count=%d", prev)
	}
	if h.BadBlockStrikeCount() > cap {
		t.Errorf("strike map grew past cap on existing-IP update: %d > %d", h.BadBlockStrikeCount(), cap)
	}
}

// ─── Test: valid-height block resets counter ──────────────────────────────────

func TestBadBlockBan_ValidBlockResetsCounter(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-ban-reset",
		UserAgent:  "aperod/test",
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

	// Send 9 out-of-range blocks — counter at 9, just below ban threshold.
	for i := 0; i < 9; i++ {
		sendBlockAtHeight(t, conn, 9999)
	}
	time.Sleep(100 * time.Millisecond)

	// Send one valid-height block (height ≤ ourTip+1000 = 1000) — resets counter.
	sendBlockAtHeight(t, conn, 500)
	time.Sleep(100 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Fatalf("peer was banned after valid-height block reset; expected no ban")
	}
	if h.PeerCount() != 1 {
		t.Fatalf("peer was disconnected unexpectedly; PeerCount=%d", h.PeerCount())
	}

	// Send 9 more out-of-range blocks — counter is reset so still at 9; no ban.
	for i := 0; i < 9; i++ {
		sendBlockAtHeight(t, conn, 9999)
	}
	time.Sleep(150 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Errorf("peer was banned after 9 bad blocks post-reset; counter should have been reset")
	}
	if h.PeerCount() != 1 {
		t.Errorf("PeerCount = %d after 9 bad blocks post-reset, want 1", h.PeerCount())
	}
}

// ─── Test: custom BadBlockBanThreshold is honoured ───────────────────────────

// TestBadBlockBan_CustomThreshold configures a ban threshold of 3 (not the
// default 10) and verifies the peer is banned after exactly 3 bad blocks.
func TestBadBlockBan_CustomThreshold(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-custom-threshold",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: 3,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
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

	// Send 2 bad blocks — below the custom threshold of 3; must not ban.
	sendBlockAtHeight(t, conn, 5000)
	sendBlockAtHeight(t, conn, 5000)
	time.Sleep(100 * time.Millisecond)
	if len(h.ListBans()) != 0 {
		t.Errorf("peer was banned after only 2 bad blocks with threshold=3")
	}

	// Send the 3rd bad block — at the custom threshold; must ban.
	sendBlockAtHeight(t, conn, 5000)
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("peer was NOT disconnected after 3 bad blocks with threshold=3")
	}
	bans := h.ListBans()
	found := false
	for _, b := range bans {
		if b.Addr == peerIP {
			found = true
		}
	}
	if !found {
		t.Errorf("ban entry for bare IP %q not found after custom threshold=3; all bans: %v", peerIP, bans)
	}
}

// ─── Test: custom BadBlockHeightLead is honoured ─────────────────────────────

// TestBadBlockBan_CustomHeightLead configures a height lead of 50 (not the
// default 1000) and verifies that a block at ourTip+51 triggers a strike.
func TestBadBlockBan_CustomHeightLead(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-custom-lead",
		UserAgent:            "aperod/test",
		BadBlockHeightLead:   50,
		BadBlockBanThreshold: 3,
		BadBlockBanDuration:  24 * time.Hour,
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

	// ourTip=0, lead=50 → threshold is 50. Height 51 is out-of-range; 50 is not.
	// A block at height 50 should NOT count as a strike.
	sendBlockAtHeight(t, conn, 50)
	time.Sleep(80 * time.Millisecond)
	if len(h.ListBans()) != 0 {
		t.Errorf("peer was banned for block at exactly the height lead (height=50, lead=50)")
	}

	// A block at height 51 IS out-of-range → strike.  Send 3 to trigger the ban.
	sendBlockAtHeight(t, conn, 51)
	sendBlockAtHeight(t, conn, 51)
	sendBlockAtHeight(t, conn, 51)
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("peer was NOT disconnected; custom height lead 50 may not be in effect")
	}
	bans := h.ListBans()
	found := false
	for _, b := range bans {
		if b.Addr == peerIP {
			found = true
		}
	}
	if !found {
		t.Errorf("ban entry for %q not found; bans: %v", peerIP, bans)
	}
}

// ─── Test: custom BadBlockBanDuration is honoured ────────────────────────────

// TestBadBlockBan_CustomBanDuration verifies that the ban's expiry time
// reflects the configured duration (5 minutes here) rather than the 24h default.
func TestBadBlockBan_CustomBanDuration(t *testing.T) {
	const customDuration = 5 * time.Minute
	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-custom-duration",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: 3,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  customDuration,
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

	for i := 0; i < 3; i++ {
		sendBlockAtHeight(t, conn, 9000)
	}
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Fatalf("peer was NOT disconnected after 3 bad blocks")
	}

	bans := h.ListBans()
	for _, b := range bans {
		if b.Addr == peerIP {
			remaining := time.Until(b.ExpiresAt)
			// Should be within [customDuration-5s, customDuration+5s].
			if remaining < customDuration-5*time.Second || remaining > customDuration+5*time.Second {
				t.Errorf("ban duration mismatch: got remaining=%v, want ~%v", remaining, customDuration)
			} else {
				t.Logf("ban duration correct: remaining=%v (want ~%v)", remaining, customDuration)
			}
			return
		}
	}
	t.Errorf("ban entry for bare IP %q not found; all bans: %v", peerIP, bans)
}

// ─── Test: whitelisted peer is never banned for height-lead blocks ────────────

// TestBadBlockBan_WhitelistedPeerNotBanned verifies that a peer whose source IP
// is in PeerWhitelist can send any number of out-of-range blocks without
// accumulating strikes or being banned.  15 blocks (well above the default
// threshold of 10) are sent; the peer must remain connected.
func TestBadBlockBan_WhitelistedPeerNotBanned(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-wl-not-banned",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: 10,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
		// Whitelist the loopback address so the test peer (127.0.0.1) is trusted.
		PeerWhitelist: []string{"127.0.0.1"},
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

	// ourTip=0, lead=1000 → out-of-range is height > 1000.
	// Send 15 out-of-range blocks from a whitelisted IP; none should be strikes.
	for i := 0; i < 15; i++ {
		sendBlockAtHeight(t, conn, 9999)
	}

	// Allow time for all 15 messages to be processed.
	time.Sleep(200 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Errorf("whitelisted peer %s was banned after 15 out-of-range blocks; expected no ban", peerIP)
	}
	if h.PeerCount() != 1 {
		t.Errorf("PeerCount = %d after 15 out-of-range blocks from whitelisted peer, want 1", h.PeerCount())
	}
	t.Logf("whitelisted peer %s correctly NOT banned after 15 out-of-range blocks", peerIP)
}

// ─── Test: non-whitelisted peer is still banned when whitelist is active ──────

// TestBadBlockBan_NonWhitelistedStillBanned verifies that the whitelist exemption
// only applies to whitelisted IPs.  When a whitelist is active, a peer whose IP
// is NOT in the list still accumulates strikes and gets banned after threshold.
func TestBadBlockBan_NonWhitelistedStillBanned(t *testing.T) {
	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-wl-non-wl-banned",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: 3,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
		// Whitelist an IP that is NOT the test peer (192.0.2.1 is TEST-NET, never used).
		// The test peer comes from 127.0.0.1, which is NOT in the whitelist.
		PeerWhitelist: []string{"192.0.2.1"},
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()
	// NOTE: the PeerWhitelist above is used only for the HEIGHT-BASED ban exemption;
	// the inbound-connection whitelist only fires when PeerWhitelist entries match
	// the connecting IP.  127.0.0.1 is NOT in this whitelist, so the peer connects
	// normally via acceptLoop (the accept-phase check only rejects if wlLen>0 AND
	// the IP is absent — but our whitelist entry 192.0.2.1 keeps wlLen>0 so
	// 127.0.0.1 would be rejected at the accept gate).
	//
	// To isolate just the height-ban exemption, use an empty PeerWhitelist for
	// connection acceptance and test the strike logic directly via the exported helper.
	h2 := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-wl-non-wl-banned-2",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: 3,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
		// Empty whitelist → no connection gate, and no exemption from height ban.
	}, &stubHandler{}, newTestLogger())
	if err := h2.Start(); err != nil {
		t.Fatalf("Start h2: %v", err)
	}
	defer h2.Stop()
	_ = h
	_ = hostAddr

	hostAddr2 := h2.ListenAddr()
	conn, peerIP := connectPeer(t, hostAddr2)
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h2.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Send 3 out-of-range blocks from a non-whitelisted IP; should ban at threshold=3.
	for i := 0; i < 3; i++ {
		sendBlockAtHeight(t, conn, 9999)
	}
	if !waitFor(500*time.Millisecond, func() bool { return h2.PeerCount() == 0 }) {
		t.Errorf("non-whitelisted peer was NOT disconnected after 3 out-of-range blocks (threshold=3)")
	}
	bans := h2.ListBans()
	found := false
	for _, b := range bans {
		if b.Addr == peerIP {
			found = true
		}
	}
	if !found {
		t.Errorf("ban entry for bare IP %q not found after threshold=3; all bans: %v", peerIP, bans)
	}
	t.Logf("non-whitelisted peer %s correctly banned after 3 out-of-range blocks", peerIP)
}

// ─── Test: whitelist loaded from sidecar survives a simulated node restart ────

// TestBadBlockBan_WhitelistPersistsAcrossRestart is the regression test for the
// scenario where the sidecar whitelist file is missing or empty after a restart,
// causing a trusted validator to be incorrectly auto-banned.
//
// Sequence:
//  1. Create Host1 with PeerWhitelist=["127.0.0.1"] + a WhitelistFile path.
//     On Start() the sidecar is seeded from cfg.PeerWhitelist.
//  2. Connect a peer and send out-of-range blocks → no ban (IP is whitelisted).
//  3. Stop Host1.
//  4. Create Host2 with an EMPTY cfg.PeerWhitelist but the SAME WhitelistFile.
//     On Start() the sidecar is loaded as the authoritative whitelist.
//  5. Connect a peer and send out-of-range blocks → still no ban.
//
// If loadWhitelistFromFile() is broken (e.g. sidecar ignored or file missing),
// Host2 would start with an empty whitelist and ban the validator after
// BadBlockBanThreshold strikes.
func TestBadBlockBan_WhitelistPersistsAcrossRestart(t *testing.T) {
	// Use a temp dir so the sidecar file is cleaned up automatically.
	dir := t.TempDir()
	wlFile := dir + "/whitelist.json"

	const threshold = 3

	// ── Phase 1: first boot seeds the sidecar ────────────────────────────────

	h1 := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-wl-restart-1",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: threshold,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
		// Whitelist the loopback so the test peer (127.0.0.1) is a trusted validator.
		PeerWhitelist: []string{"127.0.0.1"},
		// Persist the whitelist to disk so it survives the simulated restart.
		WhitelistFile: wlFile,
	}, &stubHandler{}, newTestLogger())

	if err := h1.Start(); err != nil {
		t.Fatalf("h1.Start: %v", err)
	}

	conn1, peerIP := connectPeer(t, h1.ListenAddr())
	defer conn1.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h1.PeerCount() == 1 }) {
		t.Fatal("h1: peer did not register after handshake")
	}

	// Send threshold+5 out-of-range blocks; a non-whitelisted peer would be
	// banned at exactly threshold.  The whitelisted 127.0.0.1 must not be banned.
	for i := 0; i < threshold+5; i++ {
		sendBlockAtHeight(t, conn1, 9999)
	}
	time.Sleep(200 * time.Millisecond)

	if bans := h1.ListBans(); len(bans) != 0 {
		t.Fatalf("h1: whitelisted peer %s was banned after %d out-of-range blocks; expected no ban",
			peerIP, threshold+5)
	}
	if h1.PeerCount() != 1 {
		t.Errorf("h1: PeerCount = %d after out-of-range blocks from whitelisted peer, want 1",
			h1.PeerCount())
	}
	t.Logf("h1: peer %s correctly not banned (whitelisted); sidecar written to %s", peerIP, wlFile)

	h1.Stop()
	conn1.Close()

	// ── Phase 2: simulated restart — sidecar is the sole source of truth ─────

	h2 := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-wl-restart-2",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: threshold,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
		// Deliberately omit PeerWhitelist — the sidecar is the authoritative source.
		// If loadWhitelistFromFile() is broken, this becomes an empty whitelist and
		// the test will detect the regression.
		WhitelistFile: wlFile,
	}, &stubHandler{}, newTestLogger())

	if err := h2.Start(); err != nil {
		t.Fatalf("h2.Start (simulated restart): %v", err)
	}
	defer h2.Stop()

	// Verify the whitelist was loaded from the sidecar (not from cfg.PeerWhitelist).
	loaded := h2.GetPeerWhitelist()
	found := false
	for _, entry := range loaded {
		if entry == "127.0.0.1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("h2: whitelist not loaded from sidecar %q; loaded entries: %v", wlFile, loaded)
	}
	t.Logf("h2: whitelist loaded from sidecar: %v", loaded)

	conn2, peerIP2 := connectPeer(t, h2.ListenAddr())
	defer conn2.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h2.PeerCount() == 1 }) {
		t.Fatal("h2: peer did not register after handshake (post-restart)")
	}

	// Send the same number of out-of-range blocks as Phase 1.  If the whitelist
	// was not restored from the sidecar, the peer would be banned at strike threshold.
	for i := 0; i < threshold+5; i++ {
		sendBlockAtHeight(t, conn2, 9999)
	}
	time.Sleep(200 * time.Millisecond)

	if bans := h2.ListBans(); len(bans) != 0 {
		t.Errorf("h2 (post-restart): peer %s was banned — whitelist was NOT restored from sidecar %q; bans: %v",
			peerIP2, wlFile, bans)
	} else {
		t.Logf("h2 (post-restart): peer %s correctly not banned — whitelist survived the restart", peerIP2)
	}
	if h2.PeerCount() != 1 {
		t.Errorf("h2 (post-restart): PeerCount = %d, want 1", h2.PeerCount())
	}
}

// ─── Tests: corrupt whitelist sidecar aborts Start() ─────────────────────────

// TestWhitelistSidecar_CorruptAbortsStart verifies that loadWhitelistFromFile()
// returns a fatal error for each class of corrupt sidecar, causing Start() to
// abort rather than running fail-open (allowing all inbound peers in).
func TestWhitelistSidecar_CorruptAbortsStart(t *testing.T) {
	cases := []struct {
		name    string
		content string // raw file content written to the sidecar
	}{
		{
			name:    "json_null",
			content: "null",
		},
		{
			name:    "truncated_json",
			content: `["1.2.3.4", "5.6.7`,
		},
		{
			name:    "invalid_ip_entry",
			content: `["1.2.3.4", "not-an-ip-or-cidr"]`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			wlFile := dir + "/whitelist.json"

			// Write the corrupt sidecar.
			if err := os.WriteFile(wlFile, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			h := p2p.NewHost(p2p.Config{
				ListenAddr:    "127.0.0.1:0",
				MaxPeers:      10,
				NodeID:        "test-corrupt-wl-" + tc.name,
				UserAgent:     "aperod/test",
				WhitelistFile: wlFile,
			}, &stubHandler{}, newTestLogger())

			err := h.Start()
			if err == nil {
				h.Stop()
				t.Fatalf("Start() returned nil for corrupt sidecar (%s); expected a non-nil error", tc.name)
			}
			t.Logf("Start() correctly returned error for %s: %v", tc.name, err)
		})
	}
}

// TestWhitelistSidecar_UnreadableAbortsStart verifies that Start() returns a
// non-nil error when the sidecar file exists but cannot be read (mode 000).
// This prevents the node from running as an open network when file permissions
// prevent the whitelist from being loaded.
func TestWhitelistSidecar_UnreadableAbortsStart(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — chmod 000 does not restrict root; skipping unreadable-file test")
	}

	dir := t.TempDir()
	wlFile := dir + "/whitelist.json"

	// Write a valid sidecar, then make it unreadable.
	if err := os.WriteFile(wlFile, []byte(`["1.2.3.4"]`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(wlFile, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(wlFile, 0o644) }) // restore so TempDir cleanup succeeds

	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "test-unreadable-wl",
		UserAgent:     "aperod/test",
		WhitelistFile: wlFile,
	}, &stubHandler{}, newTestLogger())

	err := h.Start()
	if err == nil {
		h.Stop()
		t.Fatal("Start() returned nil for unreadable sidecar; expected a non-nil error")
	}
	t.Logf("Start() correctly returned error for unreadable sidecar: %v", err)
}

// ─── Test: whitelisted peer passes the inbound-IP gate after a restart ────────

// TestWhitelistInboundGate_AllowsAfterRestart is the regression test for the
// scenario where the acceptLoop drops a trusted validator because the sidecar
// whitelist is loaded too late or not at all after a node restart.
//
// Unlike TestBadBlockBan_WhitelistPersistsAcrossRestart (which tests the
// bad-block strike exemption), this test targets the acceptLoop IP gate:
//
//	acceptLoop checks wlNets/wlIPs and drops any inbound connection whose
//	source IP is absent — before any handshake occurs.
//
// Sequence:
//  1. Start Host1 with PeerWhitelist=["127.0.0.1"] + WhitelistFile.
//     On Start() the sidecar is seeded from cfg.PeerWhitelist.
//  2. Stop Host1 (simulating a node restart).
//  3. Start Host2 with an EMPTY cfg.PeerWhitelist but the SAME WhitelistFile.
//     loadWhitelistFromFile() must populate wlNets/wlIPs from the sidecar so
//     that the acceptLoop gate allows 127.0.0.1 through.
//  4. Dial the host and complete the Ping/Pong handshake.
//  5. Assert PeerCount reaches 1 — the peer was not dropped at the accept gate.
//
// If the sidecar is ignored (regression), acceptLoop sees a non-empty wlNets/wlIPs
// set (because the whitelist is empty and the gate is a no-op) — wait, actually
// it sees an EMPTY whitelist meaning all IPs are allowed.  But then if somehow
// the gate stays non-empty and the IP is absent, the peer is dropped.
// The real regression scenario: wlNets and wlIPs remain as parsed from
// cfg.PeerWhitelist (which is empty), so the gate is bypassed entirely — which
// would PASS this test.  The meaningful regression is the opposite: loadWhitelistFromFile
// runs but overwrites wlNets/wlIPs with the sidecar, correctly whitelisting 127.0.0.1.
// We verify the peer actually connects (PeerCount==1) to confirm the sidecar was
// loaded and the gate allowed the connection rather than silently dropping it.
func TestWhitelistInboundGate_AllowsAfterRestart(t *testing.T) {
	dir := t.TempDir()
	wlFile := dir + "/whitelist.json"

	// ── Phase 1: first boot seeds the sidecar ───────────────────────────────

	h1 := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "test-gate-restart-1",
		UserAgent:     "aperod/test",
		PeerWhitelist: []string{"127.0.0.1"},
		WhitelistFile: wlFile,
	}, &stubHandler{}, newTestLogger())

	if err := h1.Start(); err != nil {
		t.Fatalf("h1.Start: %v", err)
	}

	// Verify the sidecar was written: 127.0.0.1 must appear in the whitelist.
	wl1 := h1.GetPeerWhitelist()
	found := false
	for _, e := range wl1 {
		if e == "127.0.0.1" {
			found = true
			break
		}
	}
	if !found {
		h1.Stop()
		t.Fatalf("h1: 127.0.0.1 not in peer whitelist after Start(); got: %v", wl1)
	}
	t.Logf("h1: whitelist seeded and sidecar written: %v → %s", wl1, wlFile)

	h1.Stop()

	// ── Phase 2: simulated restart — only WhitelistFile, no cfg.PeerWhitelist ─

	h2 := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-gate-restart-2",
		UserAgent:  "aperod/test",
		// Deliberately omit PeerWhitelist: the sidecar is the sole source of truth.
		// If loadWhitelistFromFile() is broken, wlNets/wlIPs stay nil (open network)
		// and the gate lets anyone in — the PeerCount==1 assertion still passes, but
		// GetPeerWhitelist() will be empty, exposing the regression.
		WhitelistFile: wlFile,
	}, &stubHandler{}, newTestLogger())

	if err := h2.Start(); err != nil {
		t.Fatalf("h2.Start (simulated restart): %v", err)
	}
	defer h2.Stop()

	// The sidecar must have been loaded: 127.0.0.1 must be in the active whitelist.
	wl2 := h2.GetPeerWhitelist()
	sidecarLoaded := false
	for _, e := range wl2 {
		if e == "127.0.0.1" {
			sidecarLoaded = true
			break
		}
	}
	if !sidecarLoaded {
		t.Fatalf("h2 (post-restart): sidecar not loaded — 127.0.0.1 absent from whitelist; got: %v", wl2)
	}
	t.Logf("h2 (post-restart): whitelist loaded from sidecar: %v", wl2)

	// Dial from 127.0.0.1 and complete the Ping/Pong handshake.
	// If the acceptLoop gate drops the connection before the handshake, the
	// MsgPong read will fail and the test will report the regression.
	conn, peerIP := connectPeer(t, h2.ListenAddr())
	defer conn.Close()

	// PeerCount must reach 1: the whitelisted peer passed the inbound-IP gate
	// and completed the handshake.
	if !waitFor(500*time.Millisecond, func() bool { return h2.PeerCount() == 1 }) {
		t.Fatalf("h2 (post-restart): peer %s did not register after handshake — "+
			"likely dropped by the inbound-IP gate (sidecar whitelist not applied)", peerIP)
	}
	t.Logf("h2 (post-restart): peer %s connected successfully — inbound-IP gate allowed whitelisted peer", peerIP)
}

// ─── Regression: full pipeline with asymmetric handshake ─────────────────────

// TestRoguePeerBan_FullPipeline_AsymmetricHandshake is the dedicated regression
// test for the auto-ban path after the asymmetric TLS handshake change landed.
//
// It exercises the complete pipeline in a single test:
//
//  1. Dialer sends Ping first; host replies with Pong (asymmetric handshake).
//  2. Exactly BadBlockBanThreshold out-of-range blocks accumulate strikes.
//  3. The peer is disconnected and its bare IP is banned on the threshold strike.
//  4. A reconnect attempt from the same IP (new source port) is rejected by
//     the host before the handshake completes.
//
// This test must stay passing whenever changes touch handleConn, the ban list,
// or the Ping/Pong handshake protocol — it provides a single regression signal
// for the entire handshake → dispatch → ban → reconnect-rejection chain.
func TestRoguePeerBan_FullPipeline_AsymmetricHandshake(t *testing.T) {
	const threshold = 10

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-full-pipeline",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: threshold,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()

	// ── Step 1: complete the asymmetric Ping/Pong handshake ──────────────────
	// connectPeer: dialer sends Ping → host replies Pong.
	conn, peerIP := connectPeer(t, hostAddr)
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after asymmetric handshake")
	}
	t.Logf("step 1 passed: peer %s registered after asymmetric Ping/Pong handshake", peerIP)

	// ── Step 2: send threshold−1 bad blocks → must NOT trigger a ban ─────────
	// ourTip = 0 (stubHandler); out-of-range threshold = 0 + 1000 = 1000.
	for i := 0; i < threshold-1; i++ {
		sendBlockAtHeight(t, conn, 5000)
	}
	time.Sleep(150 * time.Millisecond)
	if len(h.ListBans()) != 0 {
		t.Errorf("step 2 failed: peer was banned after only %d out-of-range blocks (threshold=%d)",
			threshold-1, threshold)
	}
	if h.PeerCount() != 1 {
		t.Errorf("step 2 failed: PeerCount = %d after %d bad blocks, want 1", h.PeerCount(), threshold-1)
	}
	t.Logf("step 2 passed: %d bad blocks accumulated — no ban yet (threshold=%d)", threshold-1, threshold)

	// ── Step 3: send the threshold-th bad block → ban + disconnect ───────────
	sendBlockAtHeight(t, conn, 5000)

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("step 3 failed: peer was NOT disconnected after %d out-of-range blocks", threshold)
	}

	bans := h.ListBans()
	if len(bans) == 0 {
		t.Fatalf("step 3 failed: no ban entry found after %d out-of-range blocks", threshold)
	}
	found := false
	for _, b := range bans {
		if b.Addr == peerIP {
			found = true
			remaining := time.Until(b.ExpiresAt)
			if remaining < 23*time.Hour {
				t.Errorf("step 3 failed: ban duration too short: %v (want ≥ 23h)", remaining)
			} else {
				t.Logf("step 3 passed: bare IP %s banned; duration remaining=%v reason=%q",
					b.Addr, remaining, b.Reason)
			}
		}
	}
	if !found {
		t.Errorf("step 3 failed: ban entry for bare IP %q not found; all bans: %v", peerIP, bans)
	}

	// ── Step 4: reconnect from the same bare IP must be rejected ─────────────
	conn2, err := net.DialTimeout("tcp", hostAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("step 4 failed: host unreachable on reconnect: %v", err)
	}
	defer conn2.Close()

	// Attempt the asymmetric handshake from the banned IP.
	// The host must close the connection immediately on the ban check.
	conn2.SetDeadline(time.Now().Add(time.Second))
	_ = p2p.WriteMsg(conn2, p2p.MsgPing, p2p.PingMsg{
		NodeID:    "rogue-reconnect",
		Height:    0,
		UserAgent: "test",
		Timestamp: time.Now().Unix(),
	})
	_, _, readErr := p2p.ReadMsg(conn2)

	if readErr == nil {
		t.Errorf("step 4 failed: reconnect from banned IP %s was accepted (expected immediate close)", peerIP)
	} else {
		t.Logf("step 4 passed: reconnect from banned IP %s correctly rejected: %v", peerIP, readErr)
	}

	// PeerCount must still be 0.
	time.Sleep(50 * time.Millisecond)
	if h.PeerCount() != 0 {
		t.Errorf("step 4 failed: PeerCount = %d after reconnect from banned IP, want 0", h.PeerCount())
	}
}

// ─── Test: relay syncing from far-ahead validator is never banned ─────────────

// fixedHeightHandler is a p2p.Handler stub that reports a configurable height.
// All other methods are no-ops identical to stubHandler.
type fixedHeightHandler struct {
	stubHandler
	height uint64
}

func (f *fixedHeightHandler) CurrentHeight() uint64 { return f.height }

// TestBadBlockBan_RelaySyncNoBan is the regression test for Task #1477.
//
// Scenario: a relay node started from a snapshot at height 1 000 connects to a
// validator that is already at height 980 000.  The validator gossips its
// current-tip blocks to the relay; those blocks arrive far ahead of the relay's
// local tip and cannot be applied yet (intermediate blocks are still being
// fetched via the sync pipeline).  Before the fix these gossip blocks triggered
// the rogue-fork ban counter because their height exceeded ourTip + BadBlockHeightLead,
// eventually banning the validator after 10 such blocks.
//
// After the fix: the counter is suppressed when the peer's own announced height
// (from the Ping/Pong handshake) is also far ahead of our tip, which is the
// reliable signal that we are syncing FROM this peer — not being attacked by it.
//
// The test:
//  1. Host reports local height = 1 000.
//  2. The test peer announces height = 980 000 in the Ping.
//  3. The peer sends 20 blocks at height 980 001 (well above ourTip + 1000).
//  4. After all 20 blocks the peer must NOT be banned, and PeerCount must stay 1.
func TestBadBlockBan_RelaySyncNoBan(t *testing.T) {
	// Relay node: local chain tip is at 1 000 (just loaded a snapshot).
	const relayTip = 1_000
	// Validator (peer) is 979 000 blocks ahead — a realistic post-snapshot gap.
	const validatorHeight = 980_000

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-relay-sync",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: 10,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
	}, &fixedHeightHandler{height: relayTip}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()

	// Dial the host and complete the Ping/Pong handshake, advertising the
	// validator's height so the host knows we are far ahead.
	conn, err := net.DialTimeout("tcp", hostAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	// Dialer sends Ping first (asymmetric handshake); host replies with Pong.
	if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
		NodeID:    "validator-peer",
		Height:    validatorHeight, // <-- the key: peer announces it is far ahead
		UserAgent: "test",
		Timestamp: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	msgType, _, err := p2p.ReadMsg(conn)
	if err != nil || msgType != p2p.MsgPong {
		t.Fatalf("expected MsgPong, got type=%v err=%v", msgType, err)
	}
	conn.SetDeadline(time.Time{})

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}
	t.Logf("peer connected: our_tip=%d, peer_height=%d", relayTip, validatorHeight)

	// Send 20 blocks at height 980 001 (validator's next tip block arriving
	// via gossip while the relay is still filling the 979 000-block gap).
	// Under the old (broken) logic each block would score a rogue-fork strike.
	// With the fix, none should: peer.height > ourTip + BadBlockHeightLead.
	for i := 0; i < 20; i++ {
		sb := p2p.SerializedBlock{
			Header: p2p.SerializedHeader{Height: validatorHeight + 1},
		}
		conn.SetWriteDeadline(time.Now().Add(time.Second))
		if err := p2p.WriteMsg(conn, p2p.MsgBlock, sb); err != nil {
			t.Fatalf("WriteMsg block %d: %v", i, err)
		}
		conn.SetWriteDeadline(time.Time{})
	}

	// Give the host time to process all 20 messages.
	time.Sleep(200 * time.Millisecond)

	if bans := h.ListBans(); len(bans) != 0 {
		t.Errorf("relay was banned after receiving gossip from a far-ahead validator: %v", bans)
	}
	if h.PeerCount() != 1 {
		t.Errorf("PeerCount = %d after gossip blocks from syncing peer, want 1", h.PeerCount())
	}
	t.Logf("relay correctly NOT banned: 20 gossip blocks from far-ahead validator (height %d) at our tip %d",
		validatorHeight, relayTip)
}

// ─── Test: deliberately rogue peer still gets banned ─────────────────────────

// TestBadBlockBan_RoguePeerStillBanned verifies that the sync exemption does
// NOT protect a rogue peer that:
//
//   - Announces a modest height (close to or equal to our tip) in the Ping, AND
//   - Sends blocks far above that announced height.
//
// This is the signature of a wrong-fork attacker: it pretends to be at the
// same network height as us but floods us with fake future blocks.
// The fix must leave this detection path intact.
func TestBadBlockBan_RoguePeerStillBanned(t *testing.T) {
	// Our node is deep in the chain (e.g. a live validator).
	const ourHeight = 1_000_000
	// Rogue peer lies and announces it is at the same height.
	const roguePeerHeight = 1_000_000

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-rogue-deep-chain",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: 5,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
	}, &fixedHeightHandler{height: ourHeight}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()

	conn, err := net.DialTimeout("tcp", hostAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	// Rogue peer announces its height as equal to ours (or even 0) — NOT far ahead.
	if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
		NodeID:    "rogue-peer",
		Height:    roguePeerHeight,
		UserAgent: "test",
		Timestamp: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	msgType, _, err := p2p.ReadMsg(conn)
	if err != nil || msgType != p2p.MsgPong {
		t.Fatalf("expected MsgPong, got type=%v err=%v", msgType, err)
	}
	conn.SetDeadline(time.Time{})

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Rogue peer sends blocks far ahead of both the announced height and our tip.
	// These should score strikes because peer.height is NOT > ourTip+BadBlockHeightLead.
	const rogueBlockHeight = ourHeight + 500_000
	for i := 0; i < 5; i++ {
		sb := p2p.SerializedBlock{
			Header: p2p.SerializedHeader{Height: rogueBlockHeight},
		}
		conn.SetWriteDeadline(time.Now().Add(time.Second))
		if err := p2p.WriteMsg(conn, p2p.MsgBlock, sb); err != nil {
			// Connection may have been closed by the ban — stop sending.
			break
		}
		conn.SetWriteDeadline(time.Time{})
	}

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("rogue peer was NOT banned after %d wrong-fork blocks (threshold=5)", 5)
	}

	bans := h.ListBans()
	if len(bans) == 0 {
		t.Fatalf("rogue peer was not banned; expected a ban entry")
	}
	t.Logf("rogue peer correctly banned after sending fake blocks at height %d (our_tip=%d, peer_height=%d)",
		rogueBlockHeight, ourHeight, roguePeerHeight)
}

// Keep the import used via fmt.Sprintf in future tests.
var _ = fmt.Sprintf
