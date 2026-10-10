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

// ---------------------------------------------------------------------------
// Back-off tests
// ---------------------------------------------------------------------------

func TestBackoffDuration_Progression(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{5, 80 * time.Second},
		{6, 160 * time.Second},
		{7, 5 * time.Minute}, // capped
		{20, 5 * time.Minute},
	}
	for _, c := range cases {
		got := backoffDuration(c.failures)
		if got != c.want {
			t.Errorf("backoffDuration(%d) = %v, want %v", c.failures, got, c.want)
		}
	}
}

func TestPeerMgr_CanDial_BackoffWindow(t *testing.T) {
	pm := newPeerMgr()
	addr := "1.2.3.4:9000"

	// No failures yet — CanDial must be true.
	if !pm.CanDial(addr) {
		t.Fatal("CanDial must be true before any failures")
	}

	// Record one failure — back-off window starts (5 s).
	pm.OnDialFail(addr)

	// CanDial must be false immediately (window has not elapsed).
	if pm.CanDial(addr) {
		t.Fatal("CanDial must be false while inside back-off window")
	}
}

func TestPeerMgr_OnDialSuccess_ResetsBackoff(t *testing.T) {
	pm := newPeerMgr()
	addr := "1.2.3.4:9000"

	pm.OnDialFail(addr)
	pm.OnDialFail(addr)
	if pm.CanDial(addr) {
		t.Fatal("should be in back-off after two failures")
	}

	// Success resets the state.
	pm.OnDialSuccess(addr)
	if !pm.CanDial(addr) {
		t.Fatal("CanDial must be true after OnDialSuccess resets back-off")
	}
}

// TestPeerMgr_RapidDisconnect_Throttled verifies that a peer whose connection
// keeps dropping quickly (< stableConnTime) cannot be dialled more than a
// small number of times within a short observation window.
//
// In the real host, OnDialFail is called both for TCP/TLS dial errors AND when
// an outbound connection closes before stableConnTime has elapsed.  This test
// exercises the failure-accumulation path directly.
func TestPeerMgr_RapidDisconnect_Throttled(t *testing.T) {
	pm := newPeerMgr()
	addr := "5.5.5.5:9000"

	// Accumulate 7 failures quickly (7th failure reaches the 5-minute cap).
	// Failure progression: 5s, 10s, 20s, 40s, 80s, 160s, 5min (cap).
	const rapidFails = 7
	for i := 0; i < rapidFails; i++ {
		pm.OnDialFail(addr)
	}

	// After 7 rapid failures the back-off is capped at 5 min.
	// CanDial must be false immediately.
	if pm.CanDial(addr) {
		t.Fatal("after 7 rapid failures CanDial must be blocked (back-off not applied)")
	}

	// Further failures must not reset things.
	pm.OnDialFail(addr)
	if pm.CanDial(addr) {
		t.Fatal("CanDial must remain blocked after further failure")
	}
}

// TestPeerMgr_ShortConnection_IncreasesBackoff verifies the lifecycle used by
// host.go: a peer that connects successfully at the TCP level but disconnects
// before stableConnTime causes OnDialFail to be called (not OnDialSuccess),
// so that repeated short connections progressively lengthen the back-off.
func TestPeerMgr_ShortConnection_IncreasesBackoff(t *testing.T) {
	pm := newPeerMgr()
	addr := "7.7.7.7:9000"

	// First attempt — no failures yet, CanDial is open.
	if !pm.CanDial(addr) {
		t.Fatal("CanDial must be true before any failures")
	}

	// Simulate 3 short-lived successful connections (connect, drop quickly).
	// In host.go this path calls OnDialFail because time.Since(connectedAt) < stableConnTime.
	pm.OnDialFail(addr) // short connection 1 → back-off 5 s
	if pm.CanDial(addr) {
		t.Fatal("CanDial must be blocked after 1st short connection")
	}

	pm.OnDialSuccess(addr) // pretend back-off elapsed (simulate host reset for test clarity)
	pm.OnDialFail(addr)    // short connection 2 → back-off 5 s (counter reset by success)
	pm.OnDialFail(addr)    // short connection 3 → back-off 10 s
	if pm.CanDial(addr) {
		t.Fatal("CanDial must be blocked after accumulated short connections")
	}

	// A stable connection (host calls OnDialSuccess) clears everything.
	pm.OnDialSuccess(addr)
	if !pm.CanDial(addr) {
		t.Fatal("CanDial must be true after OnDialSuccess following a stable connection")
	}
}

// TestPeerMgr_BanIPPort_BlocksAllPorts verifies that banning via an "IP:port"
// address (e.g. the peer's ephemeral connection address) blocks every port on
// that IP — not just the specific port that was in the ban call.  This is the
// canonical-storage guarantee: Ban("IP:port") stores under the bare IP so
// that future dials to the peer's listen port are also rejected.
func TestPeerMgr_BanIPPort_BlocksAllPorts(t *testing.T) {
	pm := newPeerMgr()

	// Ban via the ephemeral connection port that the node observes.
	pm.Ban("1.2.3.4:56789", "rogue fork", time.Hour)

	// The specific port used in the ban call must be blocked.
	if !pm.IsBanned("1.2.3.4:56789") {
		t.Error("IsBanned must be true for the exact port used in Ban()")
	}
	// A different port on the same IP must also be blocked.
	if !pm.IsBanned("1.2.3.4:9333") {
		t.Error("IsBanned must be true for any other port on the same IP")
	}
	// The bare IP must also be blocked.
	if !pm.IsBanned("1.2.3.4") {
		t.Error("IsBanned must be true for the bare IP")
	}
	// A completely different IP must not be affected.
	if pm.IsBanned("2.3.4.5:9333") {
		t.Error("IsBanned must be false for an unrelated IP")
	}
	// Exactly one ban entry must be in the map (canonical storage: no duplicates).
	if n := pm.BannedCount(); n != 1 {
		t.Errorf("BannedCount = %d after one Ban(IP:port) call, want 1 — canonical storage must produce a single entry", n)
	}
}

// TestPeerMgr_LiftBan_IPPort_LiftsByBareIP verifies that lifting a ban using
// either the full "IP:port" or the bare "IP" address removes the single
// canonical ban entry, leaving every port on that host unblocked.
// This is the key regression guard for the reviewer's concern: one LiftBan
// call must fully reverse a Ban call regardless of which port form is used.
func TestPeerMgr_LiftBan_IPPort_LiftsByBareIP(t *testing.T) {
	for _, liftAddr := range []string{"1.2.3.4:56789", "1.2.3.4"} {
		t.Run("lift_via="+liftAddr, func(t *testing.T) {
			pm := newPeerMgr()
			pm.Ban("1.2.3.4:56789", "rogue fork", time.Hour)

			// Pre-condition: multiple ports must be blocked.
			if !pm.IsBanned("1.2.3.4:56789") || !pm.IsBanned("1.2.3.4:9333") {
				t.Fatal("pre-condition: ban must be in effect before lift")
			}

			// One LiftBan call — regardless of whether we pass the port form
			// or the bare-IP form — must remove the ban completely.
			removed := pm.LiftBan(liftAddr)
			if !removed {
				t.Errorf("LiftBan(%q) returned false; expected true (ban should have existed)", liftAddr)
			}

			// All ports must now be unblocked.
			if pm.IsBanned("1.2.3.4:56789") {
				t.Error("port :56789 still banned after LiftBan — partial lift regression")
			}
			if pm.IsBanned("1.2.3.4:9333") {
				t.Error("port :9333 still banned after LiftBan — partial lift regression")
			}
			if pm.IsBanned("1.2.3.4") {
				t.Error("bare IP still banned after LiftBan — partial lift regression")
			}
			if n := pm.BannedCount(); n != 0 {
				t.Errorf("BannedCount = %d after LiftBan, want 0", n)
			}
		})
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
