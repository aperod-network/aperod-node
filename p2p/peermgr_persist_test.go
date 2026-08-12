package p2p

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestPeerMgr_PersistAndRestore verifies the basic save → new-instance → load cycle.
func TestPeerMgr_PersistAndRestore(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bans.json")
	pm := newPeerMgrWithFile(f)

	pm.Ban("1.2.3.4", "wrong fork", time.Hour)
	pm.Ban("5.6.7.8:9000", "spam", 2*time.Hour)

	// Simulate a restart: fresh PeerMgr pointing at the same file.
	pm2 := newPeerMgrWithFile(f)
	if err := pm2.LoadBansFromFile(); err != nil {
		t.Fatalf("LoadBansFromFile: %v", err)
	}

	if !pm2.IsBanned("1.2.3.4") {
		t.Error("bare-IP ban must survive restart")
	}
	if !pm2.IsBanned("1.2.3.4:30303") {
		t.Error("bare-IP ban must block any port after restart")
	}
	if !pm2.IsBanned("5.6.7.8:9000") {
		t.Error("IP:port ban must survive restart")
	}
	if pm2.IsBanned("9.9.9.9") {
		t.Error("un-banned IP must not appear after restart")
	}
}

// TestPeerMgr_LiftBan_PersistAndReload is the persistence-reload regression
// test for the canonical bare-IP storage fix.  It exercises the full cycle:
//
//  1. Ban via "IP:port" (ephemeral connection address).
//  2. Confirm all ports on that IP are blocked.
//  3. Call LiftBan once — using the same "IP:port" form to verify canonical
//     normalisation in LiftBan, or the bare IP to mirror Admin Panel usage.
//  4. Verify all ports are immediately unblocked in memory.
//  5. Reload the ban file into a fresh PeerMgr and confirm no residual ban.
//
// Without the canonical-storage fix, Ban("IP:port") and LiftBan("IP:port")
// used different keys (full address vs bare IP), leaving a ghost entry after
// a lift that reappeared on the next node restart.
func TestPeerMgr_LiftBan_PersistAndReload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		banAddr  string
		liftAddr string
	}{
		{"ban_port_lift_port", "1.2.3.4:56789", "1.2.3.4:56789"},
		{"ban_port_lift_bare", "1.2.3.4:56789", "1.2.3.4"},
		{"ban_bare_lift_bare", "1.2.3.4", "1.2.3.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "bans.json")
			pm := newPeerMgrWithFile(f)

			pm.Ban(tc.banAddr, "rogue peer", time.Hour)

			// Pre-condition: multiple ports and bare IP are blocked.
			for _, check := range []string{"1.2.3.4:56789", "1.2.3.4:9333", "1.2.3.4"} {
				if !pm.IsBanned(check) {
					t.Fatalf("pre-condition failed: %s must be banned before lift", check)
				}
			}

			// Single LiftBan must remove the entire ban.
			if removed := pm.LiftBan(tc.liftAddr); !removed {
				t.Fatalf("LiftBan(%q) returned false — ban entry not found under canonical key", tc.liftAddr)
			}

			// In-memory: all ports unblocked immediately.
			for _, check := range []string{"1.2.3.4:56789", "1.2.3.4:9333", "1.2.3.4"} {
				if pm.IsBanned(check) {
					t.Errorf("after LiftBan: %s is still banned in memory — partial-lift regression", check)
				}
			}
			if n := pm.BannedCount(); n != 0 {
				t.Errorf("BannedCount = %d after LiftBan, want 0", n)
			}

			// Reload from persisted file: no ghost entries must survive.
			pm2 := newPeerMgrWithFile(f)
			if err := pm2.LoadBansFromFile(); err != nil {
				t.Fatalf("LoadBansFromFile after lift: %v", err)
			}
			for _, check := range []string{"1.2.3.4:56789", "1.2.3.4:9333", "1.2.3.4"} {
				if pm2.IsBanned(check) {
					t.Errorf("after reload: %s is still banned — ghost entry in ban file after LiftBan", check)
				}
			}
			if n := pm2.BannedCount(); n != 0 {
				t.Errorf("BannedCount after reload = %d, want 0", n)
			}
		})
	}
}

// TestPeerMgr_LegacySidecar_MigratedOnLoad is the regression fixture for the
// LoadBansFromFile canonicalisation fix.  It crafts a ban sidecar that
// contains legacy "IP:port" keys — exactly what older node versions wrote —
// and verifies that after loading:
//
//   - All ports on the banned IP are blocked (cross-port guarantee).
//   - LiftBan works with both the port form and the bare-IP form.
//   - No ban entry survives after a successful LiftBan + reload.
//
// Without the canonicalisation in LoadBansFromFile, legacy entries remained
// stored under their original "IP:port" key, making LiftBan("IP") a no-op and
// leaving the ghost ban after every node restart.
func TestPeerMgr_LegacySidecar_MigratedOnLoad(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bans.json")

	// Write a legacy-format sidecar: entries keyed by "IP:port" as older
	// node versions wrote them.  Also include a bare-IP entry to verify that
	// the collision-resolution logic (keep furthest expiry) works correctly.
	legacy := []persistedBan{
		{Addr: "1.2.3.4:56789", Reason: "rogue fork (legacy)", Until: time.Now().Add(20 * time.Hour)},
		{Addr: "1.2.3.4", Reason: "rogue fork (bare, shorter)", Until: time.Now().Add(10 * time.Hour)},
		{Addr: "5.6.7.8:9000", Reason: "spam (legacy, only entry)", Until: time.Now().Add(time.Hour)},
	}
	data, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.WriteFile(f, data, 0600); err != nil {
		t.Fatal(err)
	}

	// ── Step 1: load — migration must happen transparently ───────────────────
	pm := newPeerMgrWithFile(f)
	if err := pm.LoadBansFromFile(); err != nil {
		t.Fatalf("LoadBansFromFile: %v", err)
	}

	// 1.2.3.4 collision: legacy entry expiry (20h) > bare-IP entry (10h) →
	// canonical key "1.2.3.4" must carry the 20-hour ban.
	for _, check := range []string{"1.2.3.4:56789", "1.2.3.4:9333", "1.2.3.4"} {
		if !pm.IsBanned(check) {
			t.Errorf("after load: %s must be banned (migrated from legacy sidecar)", check)
		}
	}
	if !pm.IsBanned("5.6.7.8:9000") {
		t.Error("5.6.7.8:9000 must be banned after legacy sidecar load")
	}
	if !pm.IsBanned("5.6.7.8:1234") {
		t.Error("5.6.7.8:1234 must also be banned (cross-port from canonicalised 5.6.7.8)")
	}
	// Exactly 2 canonical entries ("1.2.3.4" and "5.6.7.8") — not 3 (the
	// collision reduces the two 1.2.3.4 entries to one).
	if n := pm.BannedCount(); n != 2 {
		t.Errorf("BannedCount = %d after legacy load, want 2 (canonical entries only)", n)
	}

	// ── Step 2: LiftBan via port form must fully remove the 1.2.3.4 ban ─────
	if removed := pm.LiftBan("1.2.3.4:56789"); !removed {
		t.Error("LiftBan(IP:port) must find and remove the canonical bare-IP entry")
	}
	for _, check := range []string{"1.2.3.4:56789", "1.2.3.4:9333", "1.2.3.4"} {
		if pm.IsBanned(check) {
			t.Errorf("after LiftBan(port form): %s still banned — partial-lift regression", check)
		}
	}

	// ── Step 3: re-persist (happens inside LiftBan) then reload ─────────────
	pm2 := newPeerMgrWithFile(f)
	if err := pm2.LoadBansFromFile(); err != nil {
		t.Fatalf("LoadBansFromFile after lift: %v", err)
	}
	for _, check := range []string{"1.2.3.4:56789", "1.2.3.4:9333", "1.2.3.4"} {
		if pm2.IsBanned(check) {
			t.Errorf("after reload: %s still banned — ghost entry in sidecar after LiftBan", check)
		}
	}
	// 5.6.7.8 is still banned.
	if !pm2.IsBanned("5.6.7.8") {
		t.Error("5.6.7.8 must still be banned after reload")
	}
	if n := pm2.BannedCount(); n != 1 {
		t.Errorf("BannedCount after reload = %d, want 1", n)
	}
}

// TestPeerMgr_ExpiredBansFilteredOnLoad verifies that bans that expired while
// the node was down are silently discarded and do not block legitimate peers.
func TestPeerMgr_ExpiredBansFilteredOnLoad(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bans.json")

	// Write a ban file manually with one already-expired entry.
	expired := []persistedBan{
		{Addr: "1.2.3.4", Reason: "old ban", Until: time.Now().Add(-time.Hour)},
		{Addr: "5.6.7.8", Reason: "active ban", Until: time.Now().Add(time.Hour)},
	}
	data, _ := json.MarshalIndent(expired, "", "  ")
	if err := os.WriteFile(f, data, 0600); err != nil {
		t.Fatal(err)
	}

	pm := newPeerMgrWithFile(f)
	if err := pm.LoadBansFromFile(); err != nil {
		t.Fatalf("LoadBansFromFile: %v", err)
	}

	if pm.IsBanned("1.2.3.4") {
		t.Error("expired ban must not be restored")
	}
	if !pm.IsBanned("5.6.7.8") {
		t.Error("active ban must be restored")
	}
}

// TestPeerMgr_CorruptFileReturnsFatalError verifies that a corrupt ban file
// causes LoadBansFromFile to return a non-nil error so that Start() aborts
// rather than running with an empty ban list (fail-closed semantics).
func TestPeerMgr_CorruptFileReturnsFatalError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bans.json")
	if err := os.WriteFile(f, []byte("{not valid json at all!!!"), 0600); err != nil {
		t.Fatal(err)
	}

	pm := newPeerMgrWithFile(f)
	if err := pm.LoadBansFromFile(); err == nil {
		t.Error("corrupt ban file must return a non-nil error — node must not start with a degraded ban list")
	}
}

// TestPeerMgr_MissingFileHandledGracefully verifies that a missing ban file
// (first boot) does not prevent startup.
func TestPeerMgr_MissingFileHandledGracefully(t *testing.T) {
	f := filepath.Join(t.TempDir(), "nonexistent_bans.json")
	pm := newPeerMgrWithFile(f)
	if err := pm.LoadBansFromFile(); err != nil {
		t.Fatalf("missing file must not return an error (first boot): %v", err)
	}

	if pm.BannedCount() != 0 {
		t.Error("missing file must result in an empty ban list")
	}
}

// TestPeerMgr_PersistenceDisabledByDash verifies that setting banFile to "-"
// disables persistence entirely — no file is created.
func TestPeerMgr_PersistenceDisabledByDash(t *testing.T) {
	pm := newPeerMgrWithFile("-")
	pm.Ban("1.2.3.4", "test", time.Hour)

	// No file should have been created anywhere under tmp.
	tmp := t.TempDir()
	entries, _ := os.ReadDir(tmp)
	if len(entries) != 0 {
		t.Error("no file should be created when ban file is '-'")
	}

	// LoadBansFromFile must be a silent no-op.
	pm2 := newPeerMgrWithFile("-")
	if err := pm2.LoadBansFromFile(); err != nil {
		t.Fatalf("LoadBansFromFile with '-' must not return an error: %v", err)
	}
	if pm2.BannedCount() != 0 {
		t.Error("no bans should be loaded when persistence is disabled")
	}
}

// TestPeerMgr_LiftBanPersisted verifies that lifting a ban is reflected in the
// file and not re-introduced by a subsequent restart.
func TestPeerMgr_LiftBanPersisted(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bans.json")
	pm := newPeerMgrWithFile(f)

	pm.Ban("1.2.3.4", "wrong fork", time.Hour)
	pm.LiftBan("1.2.3.4")

	pm2 := newPeerMgrWithFile(f)
	if err := pm2.LoadBansFromFile(); err != nil {
		t.Fatalf("LoadBansFromFile: %v", err)
	}

	if pm2.IsBanned("1.2.3.4") {
		t.Error("lifted ban must not re-appear after restart")
	}
}

// TestPeerMgr_ConcurrentBansPersistCorrectly exercises concurrent Ban calls
// and verifies that after a simulated restart all bans that were active at
// shutdown time are present (and no extra entries appear).
//
// The test is run with -race by the CI pipeline; any data race in persistBans
// or LoadBansFromFile will surface as a race-detector failure.
func TestPeerMgr_ConcurrentBansPersistCorrectly(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bans.json")
	pm := newPeerMgrWithFile(f)

	const workers = 20
	addrs := make([]string, workers)
	for i := range addrs {
		// Use distinct IPs so bans don't overwrite each other.
		addrs[i] = "10.0.0." + string(rune('0'+i%10)) + ":9000"
		if i >= 10 {
			addrs[i] = "10.0.1." + string(rune('0'+i%10)) + ":9000"
		}
	}

	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			pm.Ban(a, "concurrent test", time.Hour)
		}(addr)
	}
	wg.Wait()

	// Simulate restart.
	pm2 := newPeerMgrWithFile(f)
	if err := pm2.LoadBansFromFile(); err != nil {
		t.Fatalf("LoadBansFromFile: %v", err)
	}

	for _, addr := range addrs {
		if !pm2.IsBanned(addr) {
			t.Errorf("ban for %s must survive restart after concurrent writes", addr)
		}
	}
}

// TestPeerMgr_ConcurrentBanAndLift exercises the race between Ban and LiftBan
// calls.  After all goroutines finish the final persisted state must be
// internally consistent (no panic, valid JSON, no entries that both banned and
// lifted the same address simultaneously).
func TestPeerMgr_ConcurrentBanAndLift(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bans.json")
	pm := newPeerMgrWithFile(f)

	// First ban the address so LiftBan has something to remove.
	pm.Ban("1.2.3.4", "base ban", time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			pm.Ban("1.2.3.4", "concurrent ban", time.Hour)
		}()
		go func() {
			defer wg.Done()
			pm.LiftBan("1.2.3.4")
		}()
	}
	wg.Wait()

	// After all goroutines: final file must be valid JSON.
	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("ban file missing after concurrent run: %v", err)
	}
	var entries []persistedBan
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("ban file corrupt after concurrent run: %v\n%s", err, data)
	}

	// The final in-memory state and the file must agree.
	pm2 := newPeerMgrWithFile(f)
	if err := pm2.LoadBansFromFile(); err != nil {
		t.Fatalf("LoadBansFromFile: %v", err)
	}
	inMemory := pm.IsBanned("1.2.3.4")
	inFile := pm2.IsBanned("1.2.3.4")
	if inMemory != inFile {
		t.Errorf("in-memory ban (%v) disagrees with persisted state (%v) after concurrent ban/lift",
			inMemory, inFile)
	}
}
