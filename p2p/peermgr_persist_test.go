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
