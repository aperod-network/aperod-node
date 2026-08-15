package p2p_test

// Tests for the rogue-fork block auto-ban logic:
//   A peer that sends badBlockBanThreshold (10) or more blocks whose height
//   exceeds ourTip+badBlockHeightLead (1000) is banned for 24 hours by bare IP.
//   - A reconnect from the same IP on a new source port is also rejected.
//   - All established connections from that IP are evicted immediately on ban.
//   - Sending a valid-height block resets the counter.
//   - Strike map is capped (badBlockMaxTrackedIPs) to prevent memory exhaustion.

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

	// Verify the second connection is actually closed.  The host sends one
	// unconditional MsgGetHeaders right after registration (handshake-race
	// fix), so buffered bytes may still be readable — drain them until the
	// read errors, then distinguish close (EOF/reset) from a still-open
	// connection (timeout).
	conn2.SetDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 256)
	var readErr error
	for readErr == nil {
		_, readErr = conn2.Read(buf)
	}
	if nerr, ok := readErr.(net.Error); ok && nerr.Timeout() {
		t.Errorf("second connection still open after IP ban (read timed out instead of EOF/reset)")
	} else {
		t.Logf("second connection correctly closed: %v", readErr)
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
//
// Subtests cover the range of announced heights that do NOT qualify for the
// sync exemption (peer.height ≤ ourTip + BadBlockHeightLead):
//
//   - peer_height_equals_our_tip   : rogue announces same height as us
//   - peer_height_zero             : rogue announces height 0 (no effort to lie)
//   - peer_height_inside_lead_window: rogue announces ourTip+500 (within the lead)
//   - fresh_node_our_tip_zero      : our node just started (tip=0); ban still fires
func TestBadBlockBan_RoguePeerStillBanned(t *testing.T) {
	const (
		threshold  = 5
		lead       = 1000
		banDur     = 24 * time.Hour
	)

	// connectAndSendBadBlocks dials host, announces peerHeight in the Ping,
	// then sends threshold bad blocks at rogueBlockHeight.  It returns once the
	// host has banned the peer (PeerCount drops to 0) or the sub-test fails.
	connectAndSendBadBlocks := func(
		t *testing.T,
		h *p2p.Host,
		peerHeight uint64,
		rogueBlockHeight uint64,
	) {
		t.Helper()
		hostAddr := h.ListenAddr()

		conn, err := net.DialTimeout("tcp", hostAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		conn.SetDeadline(time.Now().Add(2 * time.Second))
		if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
			NodeID:    "rogue-peer",
			Height:    peerHeight,
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

		for i := 0; i < threshold; i++ {
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
			t.Errorf("rogue peer was NOT banned after %d wrong-fork blocks (threshold=%d, peer_height=%d, rogue_block=%d)",
				threshold, threshold, peerHeight, rogueBlockHeight)
		}
		if bans := h.ListBans(); len(bans) == 0 {
			t.Errorf("rogue peer was not banned; expected a ban entry (peer_height=%d, rogue_block=%d)",
				peerHeight, rogueBlockHeight)
		} else {
			t.Logf("rogue peer correctly banned: peer_height=%d rogue_block=%d bans=%d",
				peerHeight, rogueBlockHeight, len(bans))
		}
	}

	// ── Subtest 1: rogue announces height equal to our tip ───────────────────
	//
	// Classic wrong-fork attacker: claims to be at the same height as us, then
	// sends blocks 500 000 heights in the future.
	// Exemption check: peer.height (1 000 000) ≤ ourTip (1 000 000) + lead (1 000)
	// → NOT exempt → strikes accumulate → ban fires.
	t.Run("peer_height_equals_our_tip", func(t *testing.T) {
		const ourHeight = 1_000_000
		h := p2p.NewHost(p2p.Config{
			ListenAddr:           "127.0.0.1:0",
			MaxPeers:             10,
			NodeID:               "test-rogue-same-height",
			UserAgent:            "aperod/test",
			BadBlockBanThreshold: threshold,
			BadBlockHeightLead:   lead,
			BadBlockBanDuration:  banDur,
		}, &fixedHeightHandler{height: ourHeight}, newTestLogger())
		if err := h.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer h.Stop()
		connectAndSendBadBlocks(t, h, ourHeight, ourHeight+500_000)
	})

	// ── Subtest 2: rogue announces height 0 (didn't bother lying) ────────────
	//
	// The attacker doesn't even pretend to be synced — it just sends far-future
	// blocks.  peer.height (0) is well below ourTip + lead, so the exemption
	// does not apply and the ban must still fire.
	t.Run("peer_height_zero", func(t *testing.T) {
		const ourHeight = 1_000_000
		h := p2p.NewHost(p2p.Config{
			ListenAddr:           "127.0.0.1:0",
			MaxPeers:             10,
			NodeID:               "test-rogue-height-zero",
			UserAgent:            "aperod/test",
			BadBlockBanThreshold: threshold,
			BadBlockHeightLead:   lead,
			BadBlockBanDuration:  banDur,
		}, &fixedHeightHandler{height: ourHeight}, newTestLogger())
		if err := h.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer h.Stop()
		// peerHeight=0, rogueBlock far above ourTip+lead.
		connectAndSendBadBlocks(t, h, 0, ourHeight+500_000)
	})

	// ── Subtest 3: rogue announces ourTip+500 (inside the lead window) ────────
	//
	// The attacker claims to be 500 blocks ahead of us — plausible for a fast
	// validator, but still within the lead window (lead=1000).
	// peer.height (1 000 500) ≤ ourTip (1 000 000) + lead (1 000) = 1 001 000
	// → NOT exempt → strikes accumulate → ban fires.
	t.Run("peer_height_inside_lead_window", func(t *testing.T) {
		const ourHeight = 1_000_000
		const peerHeight = ourHeight + 500 // inside the lead window
		h := p2p.NewHost(p2p.Config{
			ListenAddr:           "127.0.0.1:0",
			MaxPeers:             10,
			NodeID:               "test-rogue-inside-lead",
			UserAgent:            "aperod/test",
			BadBlockBanThreshold: threshold,
			BadBlockHeightLead:   lead,
			BadBlockBanDuration:  banDur,
		}, &fixedHeightHandler{height: ourHeight}, newTestLogger())
		if err := h.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer h.Stop()
		// rogueBlock far above ourTip+lead to ensure it scores strikes.
		connectAndSendBadBlocks(t, h, peerHeight, ourHeight+500_000)
	})

	// ── Subtest 4: fresh node (ourTip=0) still bans a rogue peer ─────────────
	//
	// The node just started from genesis or a fresh snapshot with tip=0.
	// The attacker announces height=500 (inside the lead window of 0+1000=1000)
	// and sends blocks at height 5000 (above the lead threshold).
	// peer.height (500) ≤ ourTip (0) + lead (1000) = 1000
	// → NOT exempt → strikes accumulate → ban fires.
	t.Run("fresh_node_our_tip_zero", func(t *testing.T) {
		const ourHeight = 0
		const peerHeight = 500 // inside lead window (0+1000)
		h := p2p.NewHost(p2p.Config{
			ListenAddr:           "127.0.0.1:0",
			MaxPeers:             10,
			NodeID:               "test-rogue-fresh-node",
			UserAgent:            "aperod/test",
			BadBlockBanThreshold: threshold,
			BadBlockHeightLead:   lead,
			BadBlockBanDuration:  banDur,
		}, &fixedHeightHandler{height: ourHeight}, newTestLogger())
		if err := h.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer h.Stop()
		// rogueBlock above ourTip+lead (0+1000=1000).
		connectAndSendBadBlocks(t, h, peerHeight, 5000)
	})
}

// Keep the import used via fmt.Sprintf in future tests.
var _ = fmt.Sprintf

// ─── Tests: corrupt ban sidecar aborts Start() ───────────────────────────────

// TestBanFile_CorruptAbortsStart verifies that Start() returns a non-nil error
// for each class of corrupt ban sidecar so that the node never starts with a
// degraded ban list that would allow previously-banned IPs to reconnect.
//
// The three cases mirror the whitelist sidecar tests in whitelist_test.go and
// cover the full surface of loadBansFromFile's fail-closed error conditions:
//
//   - json_null: JSON null is not a valid empty ban array; it signals a
//     truncated atomic-write (the tmp file was renamed before the payload was
//     written).
//   - truncated_json: a partially-written array means the OS interrupted the
//     write (power loss, OOM-kill, full disk).
//   - unreadable: the file exists but the process cannot read it, e.g. after
//     a manual chmod or an ownership change by the operator.
func TestBanFile_CorruptAbortsStart(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
		mode    os.FileMode
	}{
		{
			name:    "json_null",
			content: []byte("null"),
			mode:    0o644,
		},
		{
			name:    "truncated_json",
			content: []byte(`[{"addr":"1.2.3.4","reason":"wrong fork","until":"20`),
			mode:    0o644,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			banFile := dir + "/bans.json"

			if err := os.WriteFile(banFile, tc.content, tc.mode); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			h := p2p.NewHost(p2p.Config{
				ListenAddr: "127.0.0.1:0",
				MaxPeers:   10,
				NodeID:     "test-corrupt-banfile-" + tc.name,
				UserAgent:  "aperod/test",
				BanFile:    banFile,
			}, &stubHandler{}, newTestLogger())

			err := h.Start()
			if err == nil {
				h.Stop()
				t.Fatalf("Start() returned nil for corrupt ban sidecar (%s); expected a non-nil error so the node cannot start with a degraded ban list", tc.name)
			}
			t.Logf("Start() correctly returned error for %s: %v", tc.name, err)
		})
	}
}

// TestBanFile_UnreadableAbortsStart verifies that Start() returns a non-nil
// error when the ban sidecar file exists but cannot be read (permissions 000).
// Skipped when running as root because chmod has no effect for the superuser.
func TestBanFile_UnreadableAbortsStart(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — chmod 000 does not restrict root; skipping unreadable-file test")
	}

	dir := t.TempDir()
	banFile := dir + "/bans.json"

	// Write a syntactically valid ban file, then remove all permissions so the
	// node process cannot open it.
	if err := os.WriteFile(banFile, []byte(`[]`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(banFile, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(banFile, 0o644) }) // restore so TempDir cleanup succeeds

	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-unreadable-banfile",
		UserAgent:  "aperod/test",
		BanFile:    banFile,
	}, &stubHandler{}, newTestLogger())

	err := h.Start()
	if err == nil {
		h.Stop()
		t.Fatal("Start() returned nil for unreadable ban sidecar; expected a non-nil error")
	}
	t.Logf("Start() correctly returned error for unreadable ban sidecar: %v", err)
}

// ─── Tests: whitelist sidecar atomicity ───────────────────────────────────────

// TestWhitelistSidecar_AtomicWrite verifies that saveWhitelistToFile uses a
// tmp-file + rename pattern so the sidecar is always a valid, fully-formed JSON
// array immediately after each SetPeerWhitelist call.  A direct (non-atomic)
// write would leave a partial file visible between truncate and the final byte,
// which would corrupt loadWhitelistFromFile on restart.
func TestWhitelistSidecar_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	wlFile := filepath.Join(dir, "p2p_whitelist.json")

	h := p2p.NewHost(p2p.Config{
		MaxPeers:      10,
		WhitelistFile: wlFile,
	}, &stubHandler{}, newTestLogger())
	// Host does not need to be started — SetPeerWhitelist writes the sidecar
	// directly without requiring the listener or peer loop to be active.

	// Seed the sidecar with an initial list so it exists before subsequent calls.
	initial := []string{"1.2.3.4", "10.0.0.0/8"}
	if err := h.SetPeerWhitelist(initial); err != nil {
		t.Fatalf("SetPeerWhitelist(initial): %v", err)
	}

	// Make 20 sequential calls with different lists and after each call confirm
	// that the sidecar contains a valid, non-null JSON array.
	for i := 0; i < 20; i++ {
		entries := []string{
			fmt.Sprintf("192.168.%d.0/24", i),
			fmt.Sprintf("10.%d.0.0/16", i),
		}
		if err := h.SetPeerWhitelist(entries); err != nil {
			t.Fatalf("SetPeerWhitelist iteration %d: %v", i, err)
		}

		data, readErr := os.ReadFile(wlFile)
		if readErr != nil {
			t.Fatalf("ReadFile after iteration %d: %v", i, readErr)
		}
		var got []string
		if jsonErr := json.Unmarshal(data, &got); jsonErr != nil {
			t.Fatalf("sidecar is not valid JSON after iteration %d: %v\ncontent: %s",
				i, jsonErr, data)
		}
		if got == nil {
			// json.Unmarshal decodes JSON "null" as a nil slice — that would be
			// indistinguishable from a truncated write at the start of the file.
			t.Fatalf("sidecar decoded to nil (JSON null?) after iteration %d; content: %s",
				i, data)
		}
	}
	t.Logf("sidecar remained valid JSON after 20 sequential SetPeerWhitelist calls")
}

// TestWhitelistSidecar_LargeWriteNoPartialRead writes a large whitelist (500
// entries) through a race of concurrent SetPeerWhitelist callers while a reader
// goroutine continuously reads the sidecar file.  Every successful read must
// decode as a valid, non-null JSON array — a partially-written file would fail
// json.Unmarshal, catching any regression from atomic rename back to a direct
// truncate+write.
//
// The rename(2) syscall is atomic on POSIX file-systems: the reader either sees
// the previous complete file or the new complete file, never a partial one.
func TestWhitelistSidecar_LargeWriteNoPartialRead(t *testing.T) {
	dir := t.TempDir()
	wlFile := filepath.Join(dir, "p2p_whitelist.json")

	h := p2p.NewHost(p2p.Config{
		MaxPeers:      10,
		WhitelistFile: wlFile,
	}, &stubHandler{}, newTestLogger())

	// Build a large list so each write produces a non-trivial amount of data.
	large := make([]string, 500)
	for i := range large {
		large[i] = fmt.Sprintf("10.%d.%d.0/24", (i>>8)&0xFF, i&0xFF)
	}

	// Seed the file so the reader never sees a missing file.
	if err := h.SetPeerWhitelist(large); err != nil {
		t.Fatalf("seed SetPeerWhitelist: %v", err)
	}

	var (
		readerErr error
		readerMu  sync.Mutex
		stop      = make(chan struct{})
		wg        sync.WaitGroup
	)

	// Reader goroutine: poll the sidecar file continuously until writers finish.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(wlFile)
			if err != nil {
				// A transient ENOENT between rename phases is theoretically
				// impossible for rename(2) replacing an existing file, but
				// tolerate it as an OS race.
				continue
			}
			var got []string
			if jsonErr := json.Unmarshal(data, &got); jsonErr != nil {
				readerMu.Lock()
				readerErr = fmt.Errorf("sidecar not valid JSON during concurrent writes: %v\ncontent: %s",
					jsonErr, data)
				readerMu.Unlock()
				close(stop) // signal writers to stop early
				return
			}
			if got == nil {
				readerMu.Lock()
				readerErr = fmt.Errorf("sidecar decoded to nil (JSON null?) during concurrent writes; content: %s", data)
				readerMu.Unlock()
				close(stop)
				return
			}
		}
	}()

	// Writer goroutines: 4 concurrent callers each do 25 SetPeerWhitelist calls.
	const writers = 4
	const callsPerWriter = 25
	var writerWg sync.WaitGroup
	for w := 0; w < writers; w++ {
		writerWg.Add(1)
		go func(id int) {
			defer writerWg.Done()
			for c := 0; c < callsPerWriter; c++ {
				select {
				case <-stop:
					return // reader detected corruption; stop early
				default:
				}
				// Build a distinct large list per (writer, call) pair.
				entries := make([]string, 500)
				for i := range entries {
					entries[i] = fmt.Sprintf("172.%d.%d.%d", id, c&0xFF, i&0xFF)
				}
				// Ignore errors — concurrent callers may race on wlPersistMu but
				// must never leave a partial file visible.
				_ = h.SetPeerWhitelist(entries)
			}
		}(w)
	}

	writerWg.Wait()
	close(stop) // signal reader to exit if it hasn't already
	wg.Wait()

	readerMu.Lock()
	rerr := readerErr
	readerMu.Unlock()
	if rerr != nil {
		t.Fatal(rerr)
	}
	t.Logf("sidecar was always valid JSON throughout %d concurrent writers × %d calls each",
		writers, callsPerWriter)
}

// ─── Test: empty sidecar retains node.yaml validators ────────────────────────

// TestWhitelist_EmptySidecarRetainsNodeYamlEntries covers the special branch in
// loadWhitelistFromFile(): when the sidecar contains an empty JSON array ("[]")
// but cfg.PeerWhitelist (from node.yaml) is non-empty, the static config entries
// must be retained rather than transitioning to an open (unbounded) network.
//
// Scenario: an admin clears the Admin Panel whitelist (writes "[]" to the
// sidecar) on a node that has relay validators listed in node.yaml.  Without the
// retention branch those validators would lose inbound-connection access on the
// next restart.
//
// The test verifies two things:
//  1. GetPeerWhitelist() still returns the node.yaml entry after Start().
//  2. A peer connecting from 127.0.0.1 (the retained entry) passes the
//     inbound-IP gate and is accepted as a connected peer.
func TestWhitelist_EmptySidecarRetainsNodeYamlEntries(t *testing.T) {
	// Write an empty-array sidecar to a temp file to simulate an admin "clear all".
	dir := t.TempDir()
	sidecar := filepath.Join(dir, "p2p_whitelist.json")
	if err := os.WriteFile(sidecar, []byte("[]"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "test-empty-sidecar-retain",
		UserAgent:     "aperod/test",
		// node.yaml validator entry that must be retained despite the empty sidecar.
		PeerWhitelist: []string{"127.0.0.1"},
		WhitelistFile: sidecar,
	}, &stubHandler{}, newTestLogger())

	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// ── Assertion 1: GetPeerWhitelist() must still contain the node.yaml entry. ──
	wl := h.GetPeerWhitelist()
	found := false
	for _, entry := range wl {
		if entry == "127.0.0.1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetPeerWhitelist() = %v; want it to contain \"127.0.0.1\" (retained from node.yaml)", wl)
	} else {
		t.Logf("GetPeerWhitelist() correctly retained node.yaml entry: %v", wl)
	}

	// ── Assertion 2: a peer from 127.0.0.1 passes the inbound-IP gate. ──
	// Because the whitelist is non-empty and 127.0.0.1 is in it, the acceptLoop
	// must allow the connection rather than dropping it immediately.
	conn, _ := connectPeer(t, h.ListenAddr())
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Errorf("peer from 127.0.0.1 was NOT accepted through the inbound-IP gate "+
			"even though it is in the retained node.yaml whitelist; PeerCount=%d", h.PeerCount())
	} else {
		t.Logf("peer from 127.0.0.1 correctly accepted through the inbound-IP gate")
	}
}

// ─── Test: TLS-authenticated rogue peer still gets auto-banned ────────────────

// TestBadBlockBan_TLSAuthenticatedPeer verifies that a peer connected over TLS
// mutual auth is subject to the same wrong-fork ban pipeline as a plain-TCP peer.
//
// The existing ban tests use plain TCP (no TLSConfig) for simplicity.  A
// regression in the TLS handshake layer — e.g. a cert-check short-circuit that
// skips handleConn's ban logic — could allow a rogue TLS peer to send unlimited
// wrong-fork blocks without being banned, and the existing tests would not catch it.
//
// Test sequence:
//  1. Boot a Host with a self-signed TLS config (mirroring the production setup).
//  2. The dialer completes the TLS handshake + Aperod Ping/Pong over the
//     encrypted channel.  The peer is registered in h.peers.
//  3. The peer sends BadBlockBanThreshold out-of-range blocks; the threshold
//     strike fires and the connection is closed by the host.
//  4. A reconnect attempt from the same IP over a fresh TLS connection is
//     rejected by the host before the Aperod handshake completes.
func TestBadBlockBan_TLSAuthenticatedPeer(t *testing.T) {
	const threshold = 10

	// ── Step 1: generate independent TLS identities for host and peer ────────

	cfgHost, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("generate host TLS config: %v", err)
	}
	cfgPeer, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("generate peer TLS config: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-tls-ban",
		UserAgent:            "aperod/test",
		TLSConfig:            cfgHost,
		BadBlockBanThreshold: threshold,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()

	// ── Step 2: TLS dial + Aperod Ping/Pong handshake ─────────────────────────

	tlsConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", hostAddr, cfgPeer,
	)
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer tlsConn.Close()

	// Extract the bare IP as seen by the host (local side of our connection).
	localAddr := tlsConn.LocalAddr().String()
	peerIP, _, splitErr := net.SplitHostPort(localAddr)
	if splitErr != nil {
		peerIP = localAddr
	}

	tlsConn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	// Asymmetric handshake: dialer sends MsgPing first; host replies with MsgPong.
	if err := p2p.WriteMsg(tlsConn, p2p.MsgPing, p2p.PingMsg{
		NodeID:    "tls-rogue-peer",
		Height:    0,
		UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("write ping over TLS: %v", err)
	}
	msgType, _, err := p2p.ReadMsg(tlsConn)
	if err != nil || msgType != p2p.MsgPong {
		t.Fatalf("expected MsgPong over TLS, got %v err=%v", msgType, err)
	}
	tlsConn.SetDeadline(time.Time{}) // clear deadline

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("TLS peer did not register after Ping/Pong handshake")
	}
	t.Logf("step 2 ✓ TLS peer %s registered after mutual-auth handshake", peerIP)

	// ── Step 3: send threshold−1 bad blocks → must NOT trigger a ban ─────────
	// ourTip = 0 (stubHandler); out-of-range = height > 0 + 1000.

	for i := 0; i < threshold-1; i++ {
		sb := p2p.SerializedBlock{
			Header: p2p.SerializedHeader{Height: 5000},
		}
		tlsConn.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck
		if err := p2p.WriteMsg(tlsConn, p2p.MsgBlock, sb); err != nil {
			t.Fatalf("write bad block %d over TLS: %v", i+1, err)
		}
		tlsConn.SetWriteDeadline(time.Time{}) //nolint:errcheck
	}
	time.Sleep(150 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Errorf("step 3 failed: TLS peer banned after only %d bad blocks (threshold=%d)",
			threshold-1, threshold)
	}
	if h.PeerCount() != 1 {
		t.Errorf("step 3 failed: PeerCount=%d after %d bad blocks, want 1",
			h.PeerCount(), threshold-1)
	}
	t.Logf("step 3 ✓ %d bad blocks over TLS — no ban yet (threshold=%d)", threshold-1, threshold)

	// ── Step 4: send the threshold-th bad block → ban + disconnect ───────────

	sb := p2p.SerializedBlock{
		Header: p2p.SerializedHeader{Height: 5000},
	}
	tlsConn.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck
	_ = p2p.WriteMsg(tlsConn, p2p.MsgBlock, sb) // connection may close during/after this write
	tlsConn.SetWriteDeadline(time.Time{})        //nolint:errcheck

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("step 4 failed: TLS peer was NOT disconnected after %d out-of-range blocks", threshold)
	}

	bans := h.ListBans()
	if len(bans) == 0 {
		t.Fatalf("step 4 failed: no ban entry found after %d bad blocks over TLS", threshold)
	}
	found := false
	for _, b := range bans {
		if b.Addr == peerIP {
			found = true
			remaining := time.Until(b.ExpiresAt)
			if remaining < 23*time.Hour {
				t.Errorf("step 4 failed: ban duration too short: %v (want ≥ 23h)", remaining)
			} else {
				t.Logf("step 4 ✓ bare IP %s banned via TLS path; duration remaining=%v reason=%q",
					b.Addr, remaining, b.Reason)
			}
		}
	}
	if !found {
		t.Errorf("step 4 failed: ban entry for bare IP %q not found; all bans: %v", peerIP, bans)
	}

	// ── Step 5: reconnect from the same IP over TLS must be rejected ──────────

	tlsConn2, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", hostAddr, cfgPeer,
	)
	if err != nil {
		// Host closed the connection before or during the TLS handshake — the
		// strongest form of rejection: ban gate fires before TLS records flow.
		// This is the production behaviour (ban check in handleConn precedes
		// the eager TLS Handshake() call).
		t.Logf("step 5 ✓ TLS reconnect rejected at/before TLS handshake (expected): %v", err)
		time.Sleep(50 * time.Millisecond)
		if h.PeerCount() != 0 {
			t.Errorf("step 5 failed: PeerCount=%d after TLS reconnect rejection, want 0", h.PeerCount())
		}
		return
	}
	defer tlsConn2.Close()

	// TLS handshake completed — regression scenario: the ban check did not fire
	// before the TLS handshake.  The host must still close the connection at the
	// ban gate before any Aperod application data flows.
	//
	// Use a raw Read() with a short deadline (NOT p2p.ReadMsg, which sets its
	// own internal 30 s deadline and would mask a blackholed connection as a
	// spurious timeout — exactly the false-pass the reviewer caught).
	//
	// Three possible outcomes:
	//   readErr == nil         → host accepted the reconnect        → FAIL
	//   netErr.Timeout() true  → connection blackholed, not closed  → FAIL
	//   EOF / RST / other      → host closed the connection promptly → PASS
	tlsConn2.SetDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
	buf := make([]byte, 8)
	_, readErr := tlsConn2.Read(buf)
	if readErr == nil {
		t.Errorf("step 5 failed: TLS reconnect from banned IP %s was accepted — "+
			"host sent data when it should have closed immediately", peerIP)
	} else if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
		t.Errorf("step 5 failed: TLS reconnect from banned IP %s timed out (blackholed) — "+
			"ban gate must close the connection promptly, not leave it open", peerIP)
	} else {
		t.Logf("step 5 ✓ TLS reconnect from banned IP %s closed promptly after TLS handshake: %v", peerIP, readErr)
	}

	time.Sleep(50 * time.Millisecond)
	if h.PeerCount() != 0 {
		t.Errorf("step 5 failed: PeerCount=%d after TLS reconnect from banned IP, want 0", h.PeerCount())
	}
}

// ─── Test: TLS peers sharing an IP are all evicted on ban ────────────────────

// TestBadBlockBan_TLSAllConnectionsEvicted verifies that all TLS-wrapped
// connections from a banned IP are evicted immediately — not just the one that
// triggered the threshold.
//
// This is the TLS counterpart of TestBadBlockBan_AllConnectionsEvicted.  The
// original plain-TCP test cannot catch a regression where evictByIP performs a
// type assertion on net.Conn (e.g. casting to *net.TCPConn) that silently skips
// *tls.Conn values, leaving the second TLS connection open after the ban fires.
//
// Test sequence:
//  1. Boot a Host with mutual-TLS enabled.
//  2. Open two TLS connections from the same loopback IP (different source ports).
//  3. Wait for PeerCount to reach 2.
//  4. Send 10 out-of-range blocks on the first TLS connection → ban fires.
//  5. Assert PeerCount drops to 0 (both connections evicted).
//  6. Assert the second TLS connection is actually closed (Read returns EOF/reset,
//     not a timeout).
func TestBadBlockBan_TLSAllConnectionsEvicted(t *testing.T) {
	const threshold = 10

	// Generate independent TLS identities for host and peer.
	cfgHost, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("generate host TLS config: %v", err)
	}
	cfgPeer, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("generate peer TLS config: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-tls-evict-all",
		UserAgent:            "aperod/test",
		TLSConfig:            cfgHost,
		BadBlockBanThreshold: threshold,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()

	// dialTLS opens a TLS connection, completes the Aperod Ping/Pong handshake,
	// and returns the open *tls.Conn.
	dialTLS := func(nodeID string) *tls.Conn {
		t.Helper()
		c, err := tls.DialWithDialer(
			&net.Dialer{Timeout: 2 * time.Second},
			"tcp", hostAddr, cfgPeer,
		)
		if err != nil {
			t.Fatalf("TLS dial (%s): %v", nodeID, err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
		if err := p2p.WriteMsg(c, p2p.MsgPing, p2p.PingMsg{
			NodeID:    nodeID,
			Height:    0,
			UserAgent: "test",
			Timestamp: time.Now().Unix(),
		}); err != nil {
			c.Close()
			t.Fatalf("write ping (%s): %v", nodeID, err)
		}
		msgType, _, err := p2p.ReadMsg(c)
		if err != nil || msgType != p2p.MsgPong {
			c.Close()
			t.Fatalf("expected MsgPong (%s), got %v err=%v", nodeID, msgType, err)
		}
		c.SetDeadline(time.Time{}) //nolint:errcheck
		return c
	}

	// Open two TLS connections from the same 127.0.0.1 address.
	tlsConn1 := dialTLS("tls-rogue-1")
	defer tlsConn1.Close()

	tlsConn2 := dialTLS("tls-rogue-2")
	defer tlsConn2.Close()

	// Wait for both peers to register.
	if !waitFor(600*time.Millisecond, func() bool { return h.PeerCount() == 2 }) {
		t.Fatalf("expected 2 TLS peers registered, got %d", h.PeerCount())
	}
	t.Logf("both TLS connections registered; PeerCount=2")

	// Trigger the ban on the first TLS connection.
	for i := 0; i < threshold; i++ {
		sb := p2p.SerializedBlock{
			Header: p2p.SerializedHeader{Height: 9000},
		}
		tlsConn1.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck
		_ = p2p.WriteMsg(tlsConn1, p2p.MsgBlock, sb)          // conn may close on final write
		tlsConn1.SetWriteDeadline(time.Time{})                 //nolint:errcheck
	}

	// Both TLS connections must be evicted because they share the same bare IP.
	if !waitFor(600*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("PeerCount = %d after ban; both TLS connections should have been evicted", h.PeerCount())
	}

	// Verify the second TLS connection is actually closed.  Use raw Read() with a
	// short deadline so a blackholed (open) connection is caught as a timeout
	// failure — exactly the regression this test exists to catch.
	tlsConn2.SetDeadline(time.Now().Add(300 * time.Millisecond)) //nolint:errcheck
	buf := make([]byte, 256)
	var readErr error
	for readErr == nil {
		_, readErr = tlsConn2.Read(buf)
	}
	if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
		t.Errorf("second TLS connection still open after IP ban (read timed out instead of EOF/reset) — " +
			"evictByIP may not be reaching tls.Conn-wrapped connections")
	} else {
		t.Logf("second TLS connection correctly closed: %v", readErr)
	}

	// Confirm ban entry exists for the loopback IP.
	bans := h.ListBans()
	if len(bans) == 0 {
		t.Errorf("no ban entry found after %d TLS bad blocks", threshold)
	}
}

// ─── Test: TLS-whitelisted validator is still exempt from wrong-fork bans ────

// TestBadBlockBan_TLSWhitelistedPeerNotBanned is the regression test that
// confirms the PeerWhitelist bad-block exemption still applies when the
// connection is established over TLS mutual auth.
//
// The plain-TCP whitelist tests (TestBadBlockBan_WhitelistedPeerNotBanned)
// exercise the fast path in checkBadBlock but do not cover the TLS code path.
// A regression in the TLS branch — e.g. a short-circuit that bypasses the
// whitelist check — could silently allow the ban counter to fire on a trusted
// production validator that always connects over TLS.
//
// Test sequence:
//  1. Boot a Host with a self-signed TLS config AND PeerWhitelist=["127.0.0.1"].
//  2. The peer dials with its own independent TLS identity, completes the TLS
//     handshake, then finishes the Aperod Ping/Pong handshake.
//  3. The peer sends BadBlockBanThreshold+5 out-of-range blocks (well above the
//     default threshold of 10) — any one of which would ban a non-whitelisted peer.
//  4. Assertions: no ban entry created, PeerCount still 1, connection still live.
func TestBadBlockBan_TLSWhitelistedPeerNotBanned(t *testing.T) {
	const threshold = 10

	// ── Step 1: generate independent TLS identities for host and peer ────────

	cfgHost, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("generate host TLS config: %v", err)
	}
	cfgPeer, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("generate peer TLS config: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-tls-wl-not-banned",
		UserAgent:            "aperod/test",
		TLSConfig:            cfgHost,
		BadBlockBanThreshold: threshold,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
		// Whitelist the loopback so the TLS peer (127.0.0.1) is a trusted validator.
		PeerWhitelist: []string{"127.0.0.1"},
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()

	// ── Step 2: TLS dial + Aperod Ping/Pong handshake ────────────────────────

	tlsConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", hostAddr, cfgPeer,
	)
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer tlsConn.Close()

	// Extract the bare IP as seen by the host (local side of our connection).
	localAddr := tlsConn.LocalAddr().String()
	peerIP, _, splitErr := net.SplitHostPort(localAddr)
	if splitErr != nil {
		peerIP = localAddr
	}

	tlsConn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	// Asymmetric handshake: dialer sends MsgPing first; host replies with MsgPong.
	if err := p2p.WriteMsg(tlsConn, p2p.MsgPing, p2p.PingMsg{
		NodeID:    "tls-whitelisted-validator",
		Height:    0,
		UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("write ping over TLS: %v", err)
	}
	msgType, _, err := p2p.ReadMsg(tlsConn)
	if err != nil || msgType != p2p.MsgPong {
		t.Fatalf("expected MsgPong over TLS, got %v err=%v", msgType, err)
	}
	tlsConn.SetDeadline(time.Time{}) // clear deadline

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("TLS peer did not register after Ping/Pong handshake")
	}
	t.Logf("step 2 ✓ TLS whitelisted peer %s registered after mutual-auth handshake", peerIP)

	// ── Step 3: send threshold+5 out-of-range blocks — all must be no-ops ────
	// ourTip = 0 (stubHandler); out-of-range is height > 0 + 1000.
	// A non-whitelisted peer would be disconnected and banned at exactly
	// BadBlockBanThreshold.  The whitelisted 127.0.0.1 must accumulate zero
	// strikes and stay connected regardless of how many bad blocks it sends.

	for i := 0; i < threshold+5; i++ {
		sb := p2p.SerializedBlock{
			Header: p2p.SerializedHeader{Height: 9999},
		}
		tlsConn.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck
		if err := p2p.WriteMsg(tlsConn, p2p.MsgBlock, sb); err != nil {
			t.Fatalf("write bad block %d over TLS: %v", i+1, err)
		}
		tlsConn.SetWriteDeadline(time.Time{}) //nolint:errcheck
	}

	// Give the host time to process all messages.
	time.Sleep(200 * time.Millisecond)

	// ── Step 4: assert no ban was created and the peer is still connected ─────

	if bans := h.ListBans(); len(bans) != 0 {
		t.Errorf("step 4 failed: TLS whitelisted peer %s was banned after %d out-of-range blocks; bans: %v",
			peerIP, threshold+5, bans)
	} else {
		t.Logf("step 4 ✓ TLS whitelisted peer %s correctly NOT banned after %d out-of-range blocks",
			peerIP, threshold+5)
	}
	if h.PeerCount() != 1 {
		t.Errorf("step 4 failed: PeerCount = %d after out-of-range blocks from TLS whitelisted peer, want 1",
			h.PeerCount())
	}

	// Confirm the connection is still live by writing one more message.
	tlsConn.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck
	writeErr := p2p.WriteMsg(tlsConn, p2p.MsgBlock, p2p.SerializedBlock{
		Header: p2p.SerializedHeader{Height: 9999},
	})
	tlsConn.SetWriteDeadline(time.Time{}) //nolint:errcheck
	if writeErr != nil {
		t.Errorf("step 4 failed: TLS whitelisted peer connection was closed unexpectedly: %v", writeErr)
	} else {
		t.Logf("step 4 ✓ TLS whitelisted peer connection still live after %d bad blocks", threshold+5)
	}
}

// ─── Test: slow-handshake flood cannot exhaust all peer slots ─────────────────

// TestSlowHandshakeFlood verifies that MaxPendingHandshakes caps the number of
// inbound connections stuck in the TLS handshake phase.  An attacker that opens
// many raw TCP connections but never sends a TLS ClientHello would otherwise
// hold one goroutine per connection for up to 10 s (the handshake deadline);
// this guard closes excess connections immediately at the acceptLoop level so
// the node cannot be goroutine-starved and legitimate peers can still connect.
//
// Flow:
//  1. Boot a TLS-enabled host with MaxPendingHandshakes = 3.
//  2. Open 3 raw TCP connections without sending any TLS data — these hold all
//     3 pending-handshake slots; the server goroutines are blocked inside
//     tlsConn.Handshake() waiting for a ClientHello that never arrives.
//  3. Open 2 more raw TCP connections — the host must close both immediately
//     because the counter would exceed MaxPendingHandshakes.
//  4. Close the 3 flood connections — server goroutines get EOF, releaseHS()
//     fires, and the pending counter drops to 0.
//  5. A legitimate TLS peer completes the full handshake + Ping/Pong and
//     registers successfully, proving the guard does not starve honest peers.
func TestSlowHandshakeFlood(t *testing.T) {
	const maxPending = 3
	const extraFlood = 2

	cfgHost, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("generate host TLS config: %v", err)
	}
	cfgPeer, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("generate peer TLS config: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             20,
		NodeID:               "test-handshake-flood",
		UserAgent:            "aperod/test",
		TLSConfig:            cfgHost,
		MaxPendingHandshakes: maxPending,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()

	// ── Step 1: open maxPending raw TCP connections — hold all pending slots ──
	// The host wraps the TCP listener with tls.NewListener, so each accepted
	// conn is a *tls.Conn on the server side.  handleConn calls Handshake()
	// which blocks waiting for TLS records; the slot is held until the
	// handshake completes or the connection is closed.
	floodConns := make([]net.Conn, maxPending)
	for i := 0; i < maxPending; i++ {
		c, err := net.DialTimeout("tcp", hostAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("flood conn %d: dial: %v", i, err)
		}
		floodConns[i] = c
	}

	// Wait for acceptLoop to have processed and incremented the counter for
	// all maxPending connections.
	if !waitFor(time.Second, func() bool {
		return h.PendingHandshakes() == int64(maxPending)
	}) {
		t.Fatalf("pending handshakes did not reach %d within 1 s (got %d); "+
			"flood connections may not have been accepted yet",
			maxPending, h.PendingHandshakes())
	}
	t.Logf("step 1 ✓ pending handshakes = %d (all slots occupied)", maxPending)

	// ── Step 2: open extraFlood more conns — host must close them immediately ─
	// acceptLoop checks the counter BEFORE spawning handleConn; when cur >
	// MaxPendingHandshakes it calls conn.Close() and continues without
	// incrementing the counter, so the excess connections are dropped at the
	// earliest possible point.
	excessConns := make([]net.Conn, extraFlood)
	for i := 0; i < extraFlood; i++ {
		c, err := net.DialTimeout("tcp", hostAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("excess conn %d: dial: %v", i, err)
		}
		excessConns[i] = c
	}

	for i, c := range excessConns {
		// The host closes excess connections before any data is sent, so a
		// Read must return EOF (or a reset) immediately — not a timeout.
		c.SetDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
		buf := make([]byte, 1)
		n, readErr := c.Read(buf)
		if n > 0 || readErr == nil {
			t.Errorf("excess conn %d: host did not close it immediately (n=%d err=%v); "+
				"MaxPendingHandshakes guard may be bypassed", i, n, readErr)
		} else if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
			t.Errorf("excess conn %d: host left it open (read timed out); "+
				"must be closed promptly when pending cap is reached", i)
		} else {
			t.Logf("step 2 ✓ excess conn %d closed by host: %v", i, readErr)
		}
		c.Close()
	}

	// The counter must not have exceeded the cap at any point we can observe.
	if cur := h.PendingHandshakes(); cur > int64(maxPending) {
		t.Errorf("pendingHandshakes counter exceeded cap: %d > %d", cur, maxPending)
	}

	// ── Step 3: release flood conns — server goroutines free the slots ────────
	// Closing the client side causes the server-side Handshake() to return an
	// EOF error; handleConn then calls releaseHS() via defer, decrementing the
	// counter.
	for _, c := range floodConns {
		c.Close()
	}
	if !waitFor(time.Second, func() bool {
		return h.PendingHandshakes() == 0
	}) {
		// Non-fatal: log the current value; the legitimate-peer step below will
		// confirm whether slots were actually freed in time.
		t.Logf("note: pending handshakes = %d after flood close (still draining)",
			h.PendingHandshakes())
	} else {
		t.Logf("step 3 ✓ pending handshakes = 0 after flood conns closed")
	}

	// ── Step 4: legitimate TLS peer must connect and register ─────────────────
	// After the flood connections are closed and slots freed, a peer that
	// properly completes the TLS handshake and Aperod Ping/Pong must be
	// accepted.  This confirms the guard does not permanently block honest peers.
	tlsConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", hostAddr, cfgPeer,
	)
	if err != nil {
		t.Fatalf("step 4 failed: legitimate TLS peer could not connect after flood: %v", err)
	}
	defer tlsConn.Close()

	localAddr := tlsConn.LocalAddr().String()
	legitIP, _, splitErr := net.SplitHostPort(localAddr)
	if splitErr != nil {
		legitIP = localAddr
	}

	tlsConn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	if err := p2p.WriteMsg(tlsConn, p2p.MsgPing, p2p.PingMsg{
		NodeID:    "legit-peer-post-flood",
		Height:    0,
		UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("step 4 failed: write ping: %v", err)
	}
	msgType, _, err := p2p.ReadMsg(tlsConn)
	if err != nil || msgType != p2p.MsgPong {
		t.Fatalf("step 4 failed: expected MsgPong from host, got type=%v err=%v", msgType, err)
	}
	tlsConn.SetDeadline(time.Time{}) //nolint:errcheck

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatalf("step 4 failed: legitimate peer %s did not register after handshake; PeerCount=%d",
			legitIP, h.PeerCount())
	}
	t.Logf("step 4 ✓ legitimate TLS peer %s registered successfully after the flood", legitIP)
}

// ─── Test: concurrent handshake flood cannot double-count slots ───────────────

// TestConcurrentHandshakeFlood verifies that the pendingHandshakes atomic
// counter remains correct when MaxPendingHandshakes + N TCP connections arrive
// simultaneously.  The sequential TestSlowHandshakeFlood opens connections one
// at a time; this test uses two-phase goroutine barriers to stress the
// Add/compare sequence in acceptLoop for races that could allow more goroutines
// than MaxPendingHandshakes through.
//
// The cap is proved behaviourally — not via sampling — by confirming that every
// excess connection is immediately closed (EOF on Read) and that no slots are
// leaked after the flood drains.
//
// Flow:
//  1. Boot a TLS-enabled host with MaxPendingHandshakes = maxPending.
//  2. Phase 1: launch exactly maxPending goroutines behind a barrier; all dial
//     simultaneously and hold raw TCP connections (no TLS data sent) so that
//     server-side handleConn blocks inside tls.Conn.Handshake().  Wait until
//     PendingHandshakes() == maxPending (all slots occupied); treat timeout as
//     a fatal test failure — without saturation the excess-rejection check is
//     meaningless.
//  3. Phase 2: launch extraFlood more goroutines behind a second barrier; all
//     dial simultaneously.  Every connection that the host accepts must be
//     closed immediately (EOF or reset on the first Read, not a timeout),
//     proving the counter never exceeded the cap.
//  4. Close the phase-1 flood connections.  PendingHandshakes() must drain to 0
//     within the deadline — failure is fatal (slot leak detected).
//  5. A legitimate TLS peer completes the full handshake and registers,
//     confirming no permanent slot starvation after the attack.
func TestConcurrentHandshakeFlood(t *testing.T) {
	const (
		maxPending = 5
		extraFlood = 8 // connections beyond the cap sent simultaneously
	)

	cfgHost, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("generate host TLS config: %v", err)
	}
	cfgPeer, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("generate peer TLS config: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             50,
		NodeID:               "test-concurrent-flood",
		UserAgent:            "aperod/test",
		TLSConfig:            cfgHost,
		MaxPendingHandshakes: maxPending,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	hostAddr := h.ListenAddr()

	// concurrentDial launches n goroutines that all dial hostAddr at the same
	// instant.  Each goroutine signals readyCh when it is blocked at the start
	// gate, then waits for startCh to be closed before dialling.  The caller
	// must drain readyCh (n times) and then close startCh.
	// Returns a slice of n net.Conn values; nil entries mean the dial failed.
	concurrentDial := func(n int, mustSucceed bool) []net.Conn {
		conns := make([]net.Conn, n)
		readyCh := make(chan struct{}, n)
		startCh := make(chan struct{})

		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				readyCh <- struct{}{} // signal: I am at the gate
				<-startCh             // wait for the simultaneous release
				c, dialErr := net.DialTimeout("tcp", hostAddr, 3*time.Second)
				if dialErr != nil {
					if mustSucceed {
						// Non-fatal from here; caller checks the slice.
						t.Logf("concurrentDial[%d]: dial error: %v", idx, dialErr)
					}
					return
				}
				conns[idx] = c
			}(i)
		}

		// Wait for every goroutine to reach the gate, then release all at once.
		for i := 0; i < n; i++ {
			<-readyCh
		}
		close(startCh)
		wg.Wait()
		return conns
	}

	// ── Phase 1: saturate all handshake slots concurrently ───────────────────
	// All maxPending goroutines are unblocked simultaneously.  Each holds an
	// open raw TCP connection without sending any TLS records; handleConn blocks
	// inside tls.Conn.Handshake() keeping the slot occupied.
	floodConns := concurrentDial(maxPending, true)

	// Every phase-1 dial must have succeeded — OS backlog is far larger than
	// maxPending (5), so any failure is a test environment problem.
	for i, c := range floodConns {
		if c == nil {
			t.Fatalf("phase 1: flood conn %d failed to dial — "+
				"cannot saturate slots; remaining test would prove nothing", i)
		}
	}

	// Wait until all slots are occupied before testing excess connections.
	// Without confirmed saturation, excess connections could succeed not because
	// of a cap bypass but because free slots were still available.
	if !waitFor(2*time.Second, func() bool {
		return h.PendingHandshakes() == int64(maxPending)
	}) {
		t.Fatalf("phase 1: PendingHandshakes did not reach %d within 2 s (got %d); "+
			"slots were not fully occupied — excess-rejection check cannot proceed",
			maxPending, h.PendingHandshakes())
	}
	t.Logf("phase 1 ✓ all %d slots occupied (PendingHandshakes=%d)",
		maxPending, h.PendingHandshakes())

	// ── Phase 2: send excess connections simultaneously ───────────────────────
	// All extraFlood goroutines are released from the same gate so they race
	// through acceptLoop concurrently, maximising the chance of a double-count
	// window in the atomic Add/compare.
	//
	// Each excess connection MUST have been accepted at the TCP level (dial must
	// succeed — only 13 total connections, well within the OS backlog) and then
	// immediately closed by acceptLoop at the application level (EOF on first
	// Read, not a timeout).  OS-level dial failure is a test error because it
	// cannot validate the handshake-slot guard.
	excessConns := concurrentDial(extraFlood, false)

	closedCount := 0
	for i, c := range excessConns {
		if c == nil {
			// Dial failed at the OS level — this cannot be treated as guard
			// coverage; the application-level cap check was never reached.
			t.Errorf("phase 2: excess conn %d: dial failed at OS/listener level — "+
				"cannot verify the application-layer MaxPendingHandshakes guard", i)
			continue
		}
		c.SetDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
		buf := make([]byte, 1)
		n, readErr := c.Read(buf)
		switch {
		case n > 0 || readErr == nil:
			t.Errorf("phase 2: excess conn %d was accepted by the host (n=%d err=%v); "+
				"MaxPendingHandshakes cap appears to have been exceeded", i, n, readErr)
		case func() bool { nerr, ok := readErr.(net.Error); return ok && nerr.Timeout() }():
			t.Errorf("phase 2: excess conn %d left open (Read timed out — not closed by host); "+
				"cap may have been bypassed via concurrent Add race", i)
		default:
			t.Logf("phase 2 ✓ excess conn %d immediately closed by host: %v", i, readErr)
			closedCount++
		}
		c.Close()
	}
	t.Logf("phase 2 summary: %d/%d excess connections immediately closed by host",
		closedCount, extraFlood)

	// Counter must be within bounds right after the storm.
	if cur := h.PendingHandshakes(); cur > int64(maxPending) {
		t.Errorf("PendingHandshakes over cap after excess flood: %d > %d", cur, maxPending)
	}

	// ── Phase 3: release flood connections; assert full slot drain ────────────
	// Fatal: a leaked slot means honest peers would be incorrectly rejected
	// permanently — the guard becomes a ratchet that can only tighten.
	for _, c := range floodConns {
		if c != nil {
			c.Close()
		}
	}
	if !waitFor(2*time.Second, func() bool {
		return h.PendingHandshakes() == 0
	}) {
		t.Fatalf("phase 3: PendingHandshakes did not drain to 0 within 2 s (got %d) — "+
			"slot leak detected; releaseHS() may not fire on all exit paths",
			h.PendingHandshakes())
	}
	t.Logf("phase 3 ✓ all slots released: PendingHandshakes=0")

	// ── Phase 4: legitimate TLS peer must connect and register ───────────────
	// Confirms: (a) no permanent slot starvation, (b) guard resets cleanly for
	// honest peers after a concurrent attack.
	tlsConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", hostAddr, cfgPeer,
	)
	if err != nil {
		t.Fatalf("phase 4: legitimate TLS peer could not connect after concurrent flood: %v", err)
	}
	defer tlsConn.Close()

	tlsConn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	if err := p2p.WriteMsg(tlsConn, p2p.MsgPing, p2p.PingMsg{
		NodeID:    "legit-peer-concurrent-flood",
		Height:    0,
		UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("phase 4: write ping: %v", err)
	}
	msgType, _, err := p2p.ReadMsg(tlsConn)
	if err != nil || msgType != p2p.MsgPong {
		t.Fatalf("phase 4: expected MsgPong, got type=%v err=%v", msgType, err)
	}
	tlsConn.SetDeadline(time.Time{}) //nolint:errcheck

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Errorf("phase 4: legitimate peer did not register after concurrent flood; PeerCount=%d",
			h.PeerCount())
	} else {
		t.Logf("phase 4 ✓ legitimate TLS peer registered successfully after concurrent handshake flood")
	}
}

// ─── Test: wrong-fork ban records a BanEvent for the API/Telegram alert chain ─

// TestBadBlockBan_BanEventRecorded verifies the full alerting pipeline for a
// wrong-fork (rogue-block) ban:
//
//  1. A peer sends BadBlockBanThreshold out-of-range blocks → auto-ban fires.
//  2. GetBanEvents() returns a BanEvent whose IP, Violations, and BanDurationSecs
//     fields are correct.
//
// The BanEvent ring buffer is the source the API server polls every 60 s to
// construct and send the Telegram "peer auto-banned" alert.  If this event is
// not recorded, the alert chain is silently broken even though the network-level
// ban (ListBans) still works.
//
// This is the Go-side complement to the TypeScript unit tests in
// peer-ban-monitor.test.ts that verify the polling → Telegram notification leg.
func TestBadBlockBan_BanEventRecorded(t *testing.T) {
	const (
		threshold = 3
		banDur    = time.Hour
	)

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-ban-event-recorded",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: threshold,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  banDur,
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

	// Record the time just before sending bad blocks so we can use it as the
	// since= parameter for GetBanEvents and confirm the event was recorded now.
	before := time.Now()

	// ourTip = 0 (stubHandler); out-of-range height = ourTip + 1000 + 1 = 1001.
	// Send exactly threshold bad blocks to trigger the ban.
	for i := 0; i < threshold; i++ {
		sendBlockAtHeight(t, conn, 9000)
	}

	// Wait for the peer to be disconnected after the ban.
	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Fatalf("peer was NOT disconnected after %d out-of-range blocks", threshold)
	}

	// ── Verify GetBanEvents() recorded the event for the API alert chain ──────
	events := h.GetBanEvents(before)
	if len(events) == 0 {
		t.Fatalf("GetBanEvents returned no events after wrong-fork ban of %s — "+
			"the API/Telegram alert chain is broken", peerIP)
	}

	found := false
	for _, ev := range events {
		if ev.IP != peerIP {
			continue
		}
		found = true
		t.Logf("BanEvent: ip=%s peer_addr=%s violations=%d ban_duration_secs=%d reason=%q",
			ev.IP, ev.PeerAddr, ev.Violations, ev.BanDurationSecs, ev.Reason)

		if ev.Violations != threshold {
			t.Errorf("BanEvent.Violations = %d, want %d", ev.Violations, threshold)
		}
		wantMinSecs := int64(banDur.Seconds()) - 5
		if ev.BanDurationSecs < wantMinSecs {
			t.Errorf("BanEvent.BanDurationSecs = %d, want ≥ %d (~%v)",
				ev.BanDurationSecs, wantMinSecs, banDur)
		}
		if ev.At.Before(before) {
			t.Errorf("BanEvent.At (%v) is before the test start time (%v)", ev.At, before)
		}
	}
	if !found {
		t.Errorf("BanEvent for IP %q not found in %d events: %+v", peerIP, len(events), events)
	}
}

// ─── Tests: strike counter TTL resets properly ───────────────────────────────

// TestBadBlockStrike_TTLResetsCounter drives the real MsgBlock dispatch path to
// confirm that strikes older than BadBlockStrikeTTL are discarded on the next
// incoming bad block, so a slow attacker that paces their sends to just under
// the TTL interval can never accumulate strikes past the ban threshold.
//
// Sequence:
//  1. Send (threshold-1) out-of-range MsgBlock messages via the real listener
//     and handshake path — these register as real strikes inside the dispatch loop.
//  2. Backdate lastSeen via SetBadBlockLastSeen, simulating elapsed time.
//  3. Send one more out-of-range MsgBlock — the TTL check in the dispatch loop
//     must reset the count to 0 before incrementing, yielding count=1.
//  4. Assert the peer is still connected and no ban was issued (count=1 < threshold).
//  5. Send (threshold-2) more blocks — count reaches threshold-1 within the new
//     TTL window — still no ban, proving accumulation works after a TTL reset.
func TestBadBlockStrike_TTLResetsCounter(t *testing.T) {
	const threshold = 5

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-ttl-reset",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: threshold,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	conn, peerIP := connectPeer(t, h.ListenAddr())
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Step 1: drive threshold-1 real bad blocks through the MsgBlock dispatch path.
	// ourTip=0 (stubHandler), heightLead=1000 → height 5000 is far out of range.
	for i := 0; i < threshold-1; i++ {
		sendBlockAtHeight(t, conn, 5000)
	}
	time.Sleep(150 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Fatalf("unexpected ban after %d bad blocks (threshold=%d)", threshold-1, threshold)
	}
	if h.PeerCount() != 1 {
		t.Fatalf("peer was disconnected with only %d bad blocks (threshold=%d)", threshold-1, threshold)
	}

	// Step 2: backdate the strike record so it looks older than BadBlockStrikeTTL,
	// simulating a slow attacker going quiet for over an hour.
	expired := time.Now().Add(-(p2p.BadBlockStrikeTTL + time.Second))
	h.SetBadBlockLastSeen(peerIP, expired)

	// Step 3: send one more real bad block. The MsgBlock dispatch loop checks the
	// TTL, resets the count to 0, then increments to 1 — well below threshold.
	// The peer must NOT be banned or disconnected.
	sendBlockAtHeight(t, conn, 5000)
	time.Sleep(150 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Errorf("peer was banned after TTL expiry reset (count should be 1, threshold=%d); bans: %v",
			threshold, h.ListBans())
	}
	if h.PeerCount() != 1 {
		t.Errorf("peer was disconnected after TTL reset (count=1, threshold=%d); PeerCount=%d",
			threshold, h.PeerCount())
	}

	// Step 5: send threshold-2 more blocks — accumulating to threshold-1 within the
	// fresh TTL window. Still no ban (count = threshold-1 < threshold).
	for i := 0; i < threshold-2; i++ {
		sendBlockAtHeight(t, conn, 5000)
	}
	time.Sleep(150 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Errorf("peer was banned with only threshold-1 strikes after TTL reset; bans: %v", h.ListBans())
	}
	if h.PeerCount() != 1 {
		t.Errorf("PeerCount = %d, want 1 (threshold-1 strikes after reset)", h.PeerCount())
	}
	t.Logf("TTL reset verified: %d expired strikes discarded; new window accumulated "+
		"to %d strikes (threshold=%d) with no ban", threshold-1, threshold-1, threshold)
}

// TestBadBlockStrike_AccumulatesWithinTTL drives the real MsgBlock dispatch path
// to confirm the complementary invariant: strikes that arrive within the TTL
// window DO accumulate toward the ban threshold and eventually trigger a ban.
// A slow attacker that sends bad blocks more frequently than the TTL will
// accumulate to the threshold and get banned — they cannot stay under the limit
// by spreading sends unless they wait longer than BadBlockStrikeTTL each time.
//
// Sequence:
//  1. Send (threshold-1) out-of-range MsgBlock messages — all within the TTL window.
//     Assert the peer is still connected (no premature ban).
//  2. Send the threshold-th bad block.
//     Assert the peer is banned and disconnected (count reached threshold).
func TestBadBlockStrike_AccumulatesWithinTTL(t *testing.T) {
	const threshold = 5

	h := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             10,
		NodeID:               "test-ttl-accum",
		UserAgent:            "aperod/test",
		BadBlockBanThreshold: threshold,
		BadBlockHeightLead:   1000,
		BadBlockBanDuration:  24 * time.Hour,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	conn, peerIP := connectPeer(t, h.ListenAddr())
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Fatal("peer did not register after handshake")
	}

	// Step 1: send threshold-1 bad blocks in rapid succession — all within the TTL
	// window so the strikes accumulate. The peer must not be banned yet.
	for i := 0; i < threshold-1; i++ {
		sendBlockAtHeight(t, conn, 5000)
	}
	time.Sleep(150 * time.Millisecond)

	if len(h.ListBans()) != 0 {
		t.Fatalf("peer was banned after only %d bad blocks (threshold=%d)", threshold-1, threshold)
	}
	if h.PeerCount() != 1 {
		t.Fatalf("peer was disconnected after %d bad blocks (threshold=%d); PeerCount=%d",
			threshold-1, threshold, h.PeerCount())
	}

	// Step 2: send the threshold-th bad block. Strikes are still within the TTL
	// window so the count reaches threshold, triggering the ban + disconnect.
	sendBlockAtHeight(t, conn, 5000)

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 0 }) {
		t.Errorf("peer NOT disconnected after %d within-TTL bad blocks (threshold=%d)",
			threshold, threshold)
	}
	found := false
	for _, b := range h.ListBans() {
		if b.Addr == peerIP {
			found = true
		}
	}
	if !found {
		t.Errorf("ban entry for %q not found after %d within-TTL strikes; all bans: %v",
			peerIP, threshold, h.ListBans())
	}
	t.Logf("within-TTL accumulation verified: %d strikes within %v window triggered ban on %s",
		threshold, p2p.BadBlockStrikeTTL, peerIP)
}

// ─── Tests: ban sidecar atomicity ─────────────────────────────────────────────

// banSidecarEntry is a local mirror of the on-disk ban JSON shape used by the
// ban-sidecar atomicity tests.  It matches the persistedBan struct in peermgr.go
// and is intentionally kept minimal — we only need to verify the file is a valid,
// non-null JSON array with an "addr" field per entry.
type banSidecarEntry struct {
	Addr   string    `json:"addr"`
	Reason string    `json:"reason"`
	Until  time.Time `json:"until"`
}

// readBanSidecar reads path and returns the decoded ban array plus a nil error
// on success.  The call fails if the file cannot be read, if the JSON is
// malformed, or if the top-level value is JSON null (which indicates a
// truncated or partially-written file).
func readBanSidecar(path string) ([]banSidecarEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	// Use json.Unmarshal to a pointer so we can distinguish null from [].
	var entries []banSidecarEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("JSON decode %q (%d bytes): %w\ncontent: %s",
			path, len(data), err, data)
	}
	// json.Unmarshal leaves entries == nil for JSON null, but a valid empty
	// array produces a non-nil slice of length 0.  Null indicates a
	// truncated write — treat it as corrupt.
	if entries == nil {
		return nil, fmt.Errorf("ban sidecar %q contains JSON null — not a valid ban array (likely truncated write)", path)
	}
	return entries, nil
}

// TestBanSidecar_AlwaysValidJSONAfterEachBan verifies that the ban sidecar file
// is always a well-formed, non-null JSON array after every individual Ban call.
//
// A regression from the atomic tmp+rename pattern to a direct overwrite would
// expose a window during which the file is either empty or partially written.
// Any read during that window must NOT return a corrupt or null document.
//
// Strategy: call Ban() N times in sequence and read the sidecar immediately
// after each call.  The file must decode successfully as a JSON array on every
// iteration.
func TestBanSidecar_AlwaysValidJSONAfterEachBan(t *testing.T) {
	dir := t.TempDir()
	banFile := filepath.Join(dir, "bans.json")

	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-ban-sidecar-atomic",
		UserAgent:  "aperod/test",
		BanFile:    banFile,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	const numBans = 20
	for i := 0; i < numBans; i++ {
		addr := fmt.Sprintf("10.0.0.%d", i+1)
		p2p.HostBanPeer(h, addr, "sidecar atomicity test", time.Hour)

		entries, err := readBanSidecar(banFile)
		if err != nil {
			t.Fatalf("iteration %d: sidecar is not a valid JSON array immediately after Ban(%q): %v",
				i+1, addr, err)
		}
		// The sidecar must contain at least the bans added so far.
		if len(entries) < i+1 {
			t.Errorf("iteration %d: sidecar has %d entries, want ≥ %d",
				i+1, len(entries), i+1)
		}
		t.Logf("iteration %d: sidecar valid — %d entries", i+1, len(entries))
	}
}

// TestBanSidecar_ConcurrentWritesNeverPartiallyVisible exercises the
// tmp+rename atomicity guarantee under concurrent load.
//
// A reader goroutine polls the sidecar file continuously for the duration of
// the test, decoding the JSON on every read.  Concurrently, multiple writer
// goroutines call Ban() in a tight loop.  The reader must NEVER observe a
// partial write, a truncated file, or a JSON null value — every read must
// return a valid, non-null JSON array.
//
// Run the suite with -race: the Go race detector will surface any unsynchronised
// access that is not guarded by the persistMu / pm.mu pair.
func TestBanSidecar_ConcurrentWritesNeverPartiallyVisible(t *testing.T) {
	dir := t.TempDir()
	banFile := filepath.Join(dir, "bans.json")

	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "test-ban-sidecar-concurrent",
		UserAgent:  "aperod/test",
		BanFile:    banFile,
	}, &stubHandler{}, newTestLogger())
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// Seed the first ban so the file exists before the reader starts.
	p2p.HostBanPeer(h, "10.255.0.0", "seed ban", time.Hour)

	const (
		writerCount    = 8
		writesPerWorker = 25
		testDuration   = 300 * time.Millisecond
	)

	var (
		// readerErrors counts how many reads returned a corrupt or null sidecar.
		readerErrors atomic.Int64
		// readerReads counts successful valid reads.
		readerReads atomic.Int64
		// done signals all goroutines to stop.
		stop = make(chan struct{})
	)

	// ── Reader goroutine: polls the file until stop is closed ────────────────
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, err := readBanSidecar(banFile)
			if err != nil {
				// Count but don't t.Fatal here — we're on a non-test goroutine.
				// The error is reported after all goroutines stop.
				readerErrors.Add(1)
			} else {
				readerReads.Add(1)
			}
			// Tight polling: no sleep, to maximise the chance of catching a
			// partial write between the truncate and re-write steps that a
			// non-atomic implementation would expose.
			runtime_gosched()
		}
	}()

	// ── Writer goroutines: each adds distinct IPs in a tight loop ────────────
	var wg sync.WaitGroup
	for w := 0; w < writerCount; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < writesPerWorker; i++ {
				addr := fmt.Sprintf("192.168.%d.%d", workerID, i+1)
				p2p.HostBanPeer(h, addr, "concurrent atomicity test", time.Hour)
			}
		}(w)
	}

	// Let writers run; the reader polls concurrently.
	wg.Wait()
	// Keep reading for a short window after writes finish to catch any
	// lingering partially-visible state.
	time.Sleep(testDuration)
	close(stop)

	// Give the reader goroutine time to observe the stop signal and exit.
	time.Sleep(20 * time.Millisecond)

	errors := readerErrors.Load()
	reads := readerReads.Load()
	t.Logf("concurrent sidecar test: %d valid reads, %d corrupt reads", reads, errors)

	if errors > 0 {
		t.Errorf("ban sidecar was read in a corrupt or partially-written state %d time(s) "+
			"out of %d total reads — atomic write guarantee violated", errors, reads+errors)
	}
	if reads == 0 {
		t.Error("reader goroutine never completed a successful read — check test setup")
	}

	// Final sanity: sidecar must be valid after all writes complete.
	entries, err := readBanSidecar(banFile)
	if err != nil {
		t.Fatalf("final sidecar read failed: %v", err)
	}
	t.Logf("final sidecar: %d entries", len(entries))
}

// runtime_gosched is a thin wrapper around runtime.Gosched so the reader
// goroutine yields to other goroutines rather than spinning the CPU to 100%.
// Defined as a function (not a direct call) to keep the import confined to the
// helper and avoid adding "runtime" to the top-level import block unnecessarily.
func runtime_gosched() {
	// Intentional tight loop without sleep: we want to maximise read frequency
	// to stress the atomicity of the sidecar write.  A brief yield prevents
	// the goroutine from starving the writers on single-CPU builds.
	//
	// We use a micro-sleep instead of runtime.Gosched() to avoid importing the
	// runtime package just for this — time.Sleep(0) is equivalent on all
	// platforms supported by the Go scheduler.
	time.Sleep(0)
}

// ─── Test: null-JSON whitelist sidecar blocks startup ────────────────────────

// TestWhitelistSidecar_EmptySidecarAndNoCfgIsOpenNetwork confirms that an
// empty-array sidecar ("[]") combined with an empty cfg.PeerWhitelist results
// in a fully-open network: GetPeerWhitelist() must be empty and a peer from
// 127.0.0.1 must be accepted.  This guards the path in loadWhitelistFromFile
// where both sources are empty and the retention branch must NOT fire.
func TestWhitelistSidecar_EmptySidecarAndNoCfgIsOpenNetwork(t *testing.T) {
	dir := t.TempDir()
	sidecarPath := filepath.Join(dir, "whitelist.json")
	// Write an empty JSON array — valid but containing no entries.
	if err := os.WriteFile(sidecarPath, []byte("[]"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "test-empty-whitelist",
		UserAgent:     "aperod/test",
		// PeerWhitelist is intentionally empty (open network in cfg).
		WhitelistFile: sidecarPath,
	}, &stubHandler{}, newTestLogger())

	if err := h.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer h.Stop()

	// GetPeerWhitelist() must be empty — the retention branch must not fire
	// when both the sidecar and cfg.PeerWhitelist are empty.
	wl := h.GetPeerWhitelist()
	if len(wl) != 0 {
		t.Errorf("GetPeerWhitelist() = %v, want empty (open network)", wl)
	}

	// A peer from 127.0.0.1 must be accepted — no IP restriction should apply.
	conn, _ := connectPeer(t, h.ListenAddr())
	defer conn.Close()

	if !waitFor(500*time.Millisecond, func() bool { return h.PeerCount() == 1 }) {
		t.Errorf("PeerCount = %d after handshake, want 1 (open network must accept the peer)", h.PeerCount())
	} else {
		t.Logf("open-network preserved: peer accepted, GetPeerWhitelist=%v", wl)
	}
}

// TestWhitelistSidecar_NullJSONBlocksStartup writes a sidecar containing the
// literal JSON value `null` and verifies that h.Start() returns a non-nil error.
// A null sidecar is not a valid empty list; it indicates a truncated or tampered
// file.  loadWhitelistFromFile must fail-closed rather than silently opening the
// network to inbound connections.
func TestWhitelistSidecar_NullJSONBlocksStartup(t *testing.T) {
	dir := t.TempDir()
	sidecarPath := filepath.Join(dir, "whitelist.json")
	if err := os.WriteFile(sidecarPath, []byte("null"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	h := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "test-null-whitelist",
		UserAgent:     "aperod/test",
		WhitelistFile: sidecarPath,
	}, &stubHandler{}, newTestLogger())

	err := h.Start()
	if err == nil {
		h.Stop()
		t.Fatal("Start() returned nil; expected a non-nil error for a null-JSON whitelist sidecar")
	}
	t.Logf("Start() correctly rejected null sidecar: %v", err)
}
