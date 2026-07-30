package p2p

import (
	"testing"
	"time"
)

func TestPeerMgr_BanAndCheck(t *testing.T) {
	pm := newPeerMgr()

	pm.Ban("1.2.3.4:9000", "duplicate block", time.Hour)
	if !pm.IsBanned("1.2.3.4:9000") {
		t.Error("banned peer should be detected")
	}
	if pm.IsBanned("5.6.7.8:9000") {
		t.Error("unrelated peer must not be banned")
	}
}

func TestPeerMgr_ExpiredBan(t *testing.T) {
	pm := newPeerMgr()
	pm.Ban("1.2.3.4:9000", "test", time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	if pm.IsBanned("1.2.3.4:9000") {
		t.Error("ban should have expired")
	}
}

func TestPeerMgr_Prune(t *testing.T) {
	pm := newPeerMgr()
	pm.Ban("a:1", "test", time.Millisecond)
	pm.Ban("b:2", "test", time.Hour)
	time.Sleep(5 * time.Millisecond)

	pm.Prune()
	if pm.BannedCount() != 1 {
		t.Errorf("expected 1 active ban after prune, got %d", pm.BannedCount())
	}
}

func TestPeerMgr_BannedCount(t *testing.T) {
	pm := newPeerMgr()
	if pm.BannedCount() != 0 {
		t.Error("fresh peermgr must have 0 bans")
	}
	pm.Ban("a:1", "x", time.Hour)
	pm.Ban("b:2", "y", time.Hour)
	if pm.BannedCount() != 2 {
		t.Errorf("expected 2 bans, got %d", pm.BannedCount())
	}
}

func TestPeerMgr_MultiBan_SameAddr(t *testing.T) {
	pm := newPeerMgr()
	pm.Ban("x:1", "first", time.Hour)
	pm.Ban("x:1", "second", 2*time.Hour) // should overwrite
	if !pm.IsBanned("x:1") {
		t.Error("peer must remain banned")
	}
	if pm.BannedCount() != 1 {
		t.Errorf("double-ban same addr: expected 1, got %d", pm.BannedCount())
	}
}

// TestPeerMgr_IPBan verifies that banning a bare IP address blocks any
// "IP:port" connection from that host, regardless of source port.
func TestPeerMgr_IPBan(t *testing.T) {
	pm := newPeerMgr()

	// Ban the bare IP (no port).
	pm.Ban("1.2.3.4", "ip-level ban", time.Hour)

	// Any connection from that IP — whatever port — must be blocked.
	if !pm.IsBanned("1.2.3.4:9000") {
		t.Error("IP ban must block 1.2.3.4:9000")
	}
	if !pm.IsBanned("1.2.3.4:54321") {
		t.Error("IP ban must block 1.2.3.4:54321")
	}
	// A different IP must not be affected.
	if pm.IsBanned("5.6.7.8:9000") {
		t.Error("IP ban on 1.2.3.4 must not affect 5.6.7.8")
	}
	// Looking up the bare IP itself must also return true.
	if !pm.IsBanned("1.2.3.4") {
		t.Error("IP ban must be visible when queried with the bare IP")
	}
}
