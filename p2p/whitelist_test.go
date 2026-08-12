package p2p

import (
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func wlTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newWLTestHost returns a minimal Host suitable for whitelist unit tests.
// It does NOT start a listener.
func newWLTestHost(t *testing.T, cfgOpts func(*Config)) *Host {
	t.Helper()
	cfg := Config{
		ListenAddr:           "127.0.0.1:0",
		BadBlockHeightLead:   1000,
		BadBlockBanThreshold: 10,
	}
	if cfgOpts != nil {
		cfgOpts(&cfg)
	}
	return NewHost(cfg, nil, wlTestLogger())
}

// TestWhitelist_AddRemove verifies basic add/remove/idempotent behaviour.
func TestWhitelist_AddRemove(t *testing.T) {
	h := newWLTestHost(t, nil)

	if err := h.AddToWhitelist("1.2.3.4"); err != nil {
		t.Fatalf("AddToWhitelist: %v", err)
	}
	if got := h.GetPeerWhitelist(); len(got) != 1 || got[0] != "1.2.3.4" {
		t.Fatalf("expected [1.2.3.4], got %v", got)
	}

	// Idempotent add — list must not grow.
	if err := h.AddToWhitelist("1.2.3.4"); err != nil {
		t.Fatalf("duplicate AddToWhitelist: %v", err)
	}
	if n := len(h.GetPeerWhitelist()); n != 1 {
		t.Fatalf("expected 1 entry after duplicate add, got %d", n)
	}

	// Add a CIDR.
	if err := h.AddToWhitelist("10.0.0.0/8"); err != nil {
		t.Fatalf("CIDR AddToWhitelist: %v", err)
	}
	if n := len(h.GetPeerWhitelist()); n != 2 {
		t.Fatalf("expected 2 entries, got %d", n)
	}

	// Remove the bare IP.
	if ok, err := h.RemoveFromWhitelist("1.2.3.4"); err != nil || !ok {
		t.Fatalf("RemoveFromWhitelist: expected (true, nil), got (%v, %v)", ok, err)
	}
	if got := h.GetPeerWhitelist(); len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Fatalf("expected [10.0.0.0/8], got %v", got)
	}

	// Remove non-existent entry → (false, nil).
	if ok, err := h.RemoveFromWhitelist("9.9.9.9"); ok || err != nil {
		t.Fatalf("RemoveFromWhitelist of absent entry: expected (false, nil), got (%v, %v)", ok, err)
	}
}

// TestWhitelist_InvalidEntry verifies that AddToWhitelist rejects garbage.
func TestWhitelist_InvalidEntry(t *testing.T) {
	h := newWLTestHost(t, nil)
	if err := h.AddToWhitelist("not-an-ip"); err == nil {
		t.Fatal("expected error for invalid entry, got nil")
	}
	if n := len(h.GetPeerWhitelist()); n != 0 {
		t.Fatalf("invalid entry must not modify list; got %d entries", n)
	}
}

// TestWhitelist_ConcurrentAdd exercises simultaneous AddToWhitelist calls.
// After N goroutines each add a unique IP, the list must contain all N IPs
// with no duplicates or missing entries.
func TestWhitelist_ConcurrentAdd(t *testing.T) {
	h := newWLTestHost(t, nil)

	const n = 50
	ips := make([]string, n)
	for i := 0; i < n; i++ {
		ips[i] = "10.0.0." + string(rune('0'+i%10)) + "0" // unique enough for test
	}
	// Use unique IPs to avoid idempotent path.
	addrs := make([]string, n)
	for i := 0; i < n; i++ {
		addrs[i] = "192.0.2." + itoa(i+1) // 192.0.2.1 … 192.0.2.50
	}

	var wg sync.WaitGroup
	for _, addr := range addrs {
		addr := addr
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.AddToWhitelist(addr); err != nil {
				t.Errorf("AddToWhitelist(%s): %v", addr, err)
			}
		}()
	}
	wg.Wait()

	got := h.GetPeerWhitelist()
	if len(got) != n {
		t.Fatalf("expected %d entries after concurrent adds, got %d: %v", n, len(got), got)
	}
	seen := make(map[string]int)
	for _, e := range got {
		seen[e]++
	}
	for _, addr := range addrs {
		if seen[addr] != 1 {
			t.Errorf("entry %s appears %d times (want 1)", addr, seen[addr])
		}
	}
}

// TestWhitelist_ConcurrentAddRemove exercises concurrent add and remove
// without a crash or deadlock.  It does not assert a specific final state
// (last-writer-wins), but verifies the invariant that the list never
// contains duplicates or invalid strings.
func TestWhitelist_ConcurrentAddRemove(t *testing.T) {
	h := newWLTestHost(t, nil)

	// Seed a few IPs.
	for i := 0; i < 5; i++ {
		_ = h.AddToWhitelist("10.1.1." + itoa(i+1))
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = h.AddToWhitelist("10.2.2." + itoa(i+1))
		}()
		go func() {
			defer wg.Done()
			_, _ = h.RemoveFromWhitelist("10.1.1." + itoa(i%5+1))
		}()
	}
	wg.Wait()

	// Invariant: no duplicates in final list.
	got := h.GetPeerWhitelist()
	seen := make(map[string]bool)
	for _, e := range got {
		if seen[e] {
			t.Errorf("duplicate entry %q in whitelist", e)
		}
		seen[e] = true
	}
}

// TestWhitelist_Persistence verifies that:
//  1. Calling AddToWhitelist writes the entry to the sidecar file.
//  2. A new Host loaded from the same file (simulating restart) sees the entry.
//  3. RemoveFromWhitelist persists the removal; a new Host does not see the
//     removed entry even when it was originally present in cfg.PeerWhitelist.
func TestWhitelist_Persistence(t *testing.T) {
	dir := t.TempDir()
	wlFile := filepath.Join(dir, "whitelist.json")

	// Phase 1 — first boot: no sidecar; cfg has one static entry.
	h1 := newWLTestHost(t, func(cfg *Config) {
		cfg.PeerWhitelist = []string{"5.5.5.5"}
		cfg.WhitelistFile = wlFile
	})
	if err := h1.loadWhitelistFromFile(); err != nil { // simulates Start()
		t.Fatalf("loadWhitelistFromFile (first boot): %v", err)
	}

	// Sidecar should have been seeded from cfg.
	if _, err := os.Stat(wlFile); err != nil {
		t.Fatalf("sidecar file not created after seeding: %v", err)
	}

	// Add a dynamic entry.
	if err := h1.AddToWhitelist("9.9.9.9"); err != nil {
		t.Fatalf("AddToWhitelist: %v", err)
	}

	// Phase 2 — restart simulation: new Host, same sidecar file.
	// cfg has the original static entry; sidecar is authoritative and
	// must override cfg (so both "5.5.5.5" and "9.9.9.9" are present).
	h2 := newWLTestHost(t, func(cfg *Config) {
		cfg.PeerWhitelist = []string{"5.5.5.5"}
		cfg.WhitelistFile = wlFile
	})
	if err := h2.loadWhitelistFromFile(); err != nil {
		t.Fatalf("loadWhitelistFromFile (restart): %v", err)
	}

	got := h2.GetPeerWhitelist()
	has := func(s string) bool {
		for _, e := range got {
			if e == s {
				return true
			}
		}
		return false
	}
	if !has("5.5.5.5") {
		t.Errorf("restart: expected 5.5.5.5 in whitelist, got %v", got)
	}
	if !has("9.9.9.9") {
		t.Errorf("restart: expected 9.9.9.9 in whitelist, got %v", got)
	}

	// Phase 3 — remove a cfg-sourced entry and restart again.
	// The removed entry must NOT come back from cfg on restart.
	if ok, err := h2.RemoveFromWhitelist("5.5.5.5"); err != nil || !ok {
		t.Fatalf("RemoveFromWhitelist(5.5.5.5): expected (true, nil), got (%v, %v)", ok, err)
	}
	h3 := newWLTestHost(t, func(cfg *Config) {
		cfg.PeerWhitelist = []string{"5.5.5.5"} // still in node.yaml
		cfg.WhitelistFile = wlFile
	})
	if err := h3.loadWhitelistFromFile(); err != nil {
		t.Fatalf("loadWhitelistFromFile (after removal): %v", err)
	}

	got3 := h3.GetPeerWhitelist()
	for _, e := range got3 {
		if e == "5.5.5.5" {
			t.Errorf("restart after removal: 5.5.5.5 should NOT be in whitelist (cfg entry removed by admin)")
		}
	}
	hasEntry := func(sl []string, s string) bool {
		for _, e := range sl {
			if e == s {
				return true
			}
		}
		return false
	}
	if !hasEntry(got3, "9.9.9.9") {
		t.Errorf("restart after removal: 9.9.9.9 should still be in whitelist, got %v", got3)
	}
}

// TestWhitelist_NullSidecarFails verifies that a sidecar containing JSON null
// (which json.Unmarshal decodes into a nil []string) is treated as invalid and
// causes loadWhitelistFromFile to return an error.  A null sidecar must not
// silently produce an empty whitelist that admits all inbound peers.
func TestWhitelist_NullSidecarFails(t *testing.T) {
	dir := t.TempDir()
	wlFile := filepath.Join(dir, "whitelist.json")

	if err := os.WriteFile(wlFile, []byte("null"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	h := newWLTestHost(t, func(cfg *Config) {
		cfg.PeerWhitelist = []string{"3.3.3.3"} // cfg has an entry — must not be used
		cfg.WhitelistFile = wlFile
	})
	if err := h.loadWhitelistFromFile(); err == nil {
		t.Fatal("expected error for null sidecar, got nil")
	}
}

// TestWhitelist_FirstBootSeedFailurePropagates verifies that when the sidecar
// directory doesn't exist (so the first-boot seed write fails), Start() receives
// a non-nil error rather than silently starting without durable whitelist state.
func TestWhitelist_FirstBootSeedFailurePropagates(t *testing.T) {
	// Point WhitelistFile at a path whose parent directory does not exist.
	nonExistentDir := filepath.Join(t.TempDir(), "no-such-subdir", "whitelist.json")

	h := newWLTestHost(t, func(cfg *Config) {
		cfg.PeerWhitelist = []string{"5.5.5.5"} // non-empty → seed is attempted
		cfg.WhitelistFile = nonExistentDir
	})
	if err := h.loadWhitelistFromFile(); err == nil {
		t.Fatal("expected error when first-boot sidecar seed fails, got nil")
	}
}

// TestWhitelist_CorruptSidecarFails verifies that a corrupt (unparseable JSON)
// sidecar causes loadWhitelistFromFile to return a non-nil error so that
// Start() aborts rather than running fail-open.
func TestWhitelist_CorruptSidecarFails(t *testing.T) {
	dir := t.TempDir()
	wlFile := filepath.Join(dir, "whitelist.json")

	// Write truncated / corrupt JSON.
	if err := os.WriteFile(wlFile, []byte(`["1.1.1.1", "2.2`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	h := newWLTestHost(t, func(cfg *Config) {
		cfg.PeerWhitelist = []string{"3.3.3.3"} // cfg has an entry — must not be used
		cfg.WhitelistFile = wlFile
	})
	if err := h.loadWhitelistFromFile(); err == nil {
		t.Fatal("expected error for corrupt sidecar, got nil")
	}
}

// TestWhitelist_ValidJSONInvalidEntriesFails verifies that a sidecar with
// syntactically valid JSON but containing invalid IP/CIDR entries causes
// loadWhitelistFromFile to return an error (fail-closed), not silently produce
// an empty whitelist that admits all inbound peers.
func TestWhitelist_ValidJSONInvalidEntriesFails(t *testing.T) {
	dir := t.TempDir()
	wlFile := filepath.Join(dir, "whitelist.json")

	// Valid JSON array, but entries are garbage strings.
	if err := os.WriteFile(wlFile, []byte(`["not-an-ip", "also-bad"]`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	h := newWLTestHost(t, func(cfg *Config) {
		cfg.PeerWhitelist = []string{"3.3.3.3"} // cfg has an entry — must not be used
		cfg.WhitelistFile = wlFile
	})
	if err := h.loadWhitelistFromFile(); err == nil {
		t.Fatal("expected error for valid JSON sidecar with invalid IP entries, got nil")
	}

	// The in-memory whitelist must not be modified (stays at the cfg value or empty).
	// Critically, it must NOT be set to an empty list that would allow all inbound peers.
	// Since loadWhitelistFromFile errors out before overwriting, wlNets/wlIPs are whatever
	// NewHost set from cfg.PeerWhitelist, which is fine — Start() would abort anyway.
}

// TestWhitelist_UnreadableSidecarFails verifies that a sidecar that exists but
// cannot be read (e.g. permission denied) causes loadWhitelistFromFile to return
// a non-nil error so that Start() aborts (fail-closed).
func TestWhitelist_UnreadableSidecarFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; permission checks do not apply")
	}
	dir := t.TempDir()
	wlFile := filepath.Join(dir, "whitelist.json")

	if err := os.WriteFile(wlFile, []byte(`["1.1.1.1"]`), 0o000); err != nil {
		t.Fatalf("setup: %v", err)
	}

	h := newWLTestHost(t, func(cfg *Config) {
		cfg.PeerWhitelist = []string{"3.3.3.3"}
		cfg.WhitelistFile = wlFile
	})
	if err := h.loadWhitelistFromFile(); err == nil {
		t.Fatal("expected error for unreadable sidecar, got nil")
	}
}

// TestWhitelist_EmptyYAMLFirstBoot verifies that when cfg.PeerWhitelist is
// empty and no sidecar exists, loadWhitelistFromFile succeeds without creating
// a sidecar file (open-network case).
func TestWhitelist_EmptyYAMLFirstBoot(t *testing.T) {
	dir := t.TempDir()
	wlFile := filepath.Join(dir, "whitelist.json")

	h := newWLTestHost(t, func(cfg *Config) {
		// cfg.PeerWhitelist deliberately empty (no whitelist configured).
		cfg.WhitelistFile = wlFile
	})
	if err := h.loadWhitelistFromFile(); err != nil {
		t.Fatalf("unexpected error for empty cfg + no sidecar: %v", err)
	}
	if _, err := os.Stat(wlFile); !os.IsNotExist(err) {
		t.Fatal("sidecar must NOT be created for an open-network (empty) whitelist")
	}
	if n := len(h.GetPeerWhitelist()); n != 0 {
		t.Fatalf("expected empty whitelist, got %d entries", n)
	}
}

// TestWhitelist_SidecarRoundtrip verifies the JSON file round-trip (atomic write → read).
func TestWhitelist_SidecarRoundtrip(t *testing.T) {
	dir := t.TempDir()
	wlFile := filepath.Join(dir, "whitelist.json")

	h := newWLTestHost(t, func(cfg *Config) {
		cfg.WhitelistFile = wlFile
	})

	entries := []string{"1.1.1.1", "2.2.2.2", "10.0.0.0/8"}
	if err := h.SetPeerWhitelist(entries); err != nil {
		t.Fatalf("SetPeerWhitelist: %v", err)
	}

	raw, err := os.ReadFile(wlFile)
	if err != nil {
		t.Fatalf("reading sidecar: %v", err)
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("sidecar: want %d entries, got %d: %v", len(entries), len(got), got)
	}
	for i, e := range entries {
		if got[i] != e {
			t.Errorf("sidecar[%d]: want %s, got %s", i, e, got[i])
		}
	}
}

// TestWhitelist_PersistFailureDoesNotModifyInMemory verifies that when the
// sidecar file cannot be written (e.g. the parent directory does not exist),
// AddToWhitelist and RemoveFromWhitelist return an error AND leave the
// in-memory whitelist unchanged — no false-success, no silent divergence.
func TestWhitelist_PersistFailureDoesNotModifyInMemory(t *testing.T) {
	// Point WhitelistFile at a directory that does not exist so CreateTemp fails.
	nonExistentDir := filepath.Join(t.TempDir(), "no-such-subdir", "whitelist.json")

	h := newWLTestHost(t, func(cfg *Config) {
		// Seed with one valid IP so we can verify the list is unchanged after failure.
		cfg.PeerWhitelist = []string{"1.1.1.1"}
		cfg.WhitelistFile = nonExistentDir
	})
	// Don't call loadWhitelistFromFile — we only care about add/remove atomicity.

	// AddToWhitelist must return a non-nil error.
	if err := h.AddToWhitelist("2.2.2.2"); err == nil {
		t.Fatal("AddToWhitelist: expected persist error, got nil")
	}
	// In-memory list must be unchanged (still the cfg seed value "1.1.1.1").
	got := h.GetPeerWhitelist()
	if len(got) != 1 || got[0] != "1.1.1.1" {
		t.Fatalf("AddToWhitelist persist failure: in-memory list modified unexpectedly: %v", got)
	}

	// RemoveFromWhitelist must also return a non-nil error.
	if ok, err := h.RemoveFromWhitelist("1.1.1.1"); err == nil {
		t.Fatalf("RemoveFromWhitelist: expected persist error, got nil (ok=%v)", ok)
	}
	// In-memory list must still be unchanged.
	got2 := h.GetPeerWhitelist()
	if len(got2) != 1 || got2[0] != "1.1.1.1" {
		t.Fatalf("RemoveFromWhitelist persist failure: in-memory list modified unexpectedly: %v", got2)
	}
}

// TestIpInWhitelist_CIDR verifies that ipInWhitelist accepts an IP that falls
// inside a CIDR range and rejects one that falls outside it.
func TestIpInWhitelist_CIDR(t *testing.T) {
	_, net10, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	wlNets := []*net.IPNet{net10}

	cases := []struct {
		ip   string
		want bool
		desc string
	}{
		{"10.0.0.1", true, "first host in subnet"},
		{"10.255.255.255", true, "last host in subnet"},
		{"10.1.2.3", true, "arbitrary host inside 10.0.0.0/8"},
		{"11.0.0.1", false, "just outside subnet"},
		{"192.168.1.1", false, "entirely different range"},
		{"0.0.0.0", false, "zero address outside 10/8"},
	}

	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("net.ParseIP(%q) returned nil", tc.ip)
		}
		got := ipInWhitelist(ip, wlNets, nil)
		if got != tc.want {
			t.Errorf("ipInWhitelist(%s, 10.0.0.0/8, nil) = %v, want %v (%s)",
				tc.ip, got, tc.want, tc.desc)
		}
	}
}

// TestIpInWhitelist_MixedCIDRAndIP verifies that a whitelist combining a CIDR
// range and individual IP entries correctly accepts IPs covered by either
// entry and rejects IPs covered by neither.
func TestIpInWhitelist_MixedCIDRAndIP(t *testing.T) {
	_, net192, err := net.ParseCIDR("192.168.0.0/16")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	wlNets := []*net.IPNet{net192}
	wlIPs := []net.IP{net.ParseIP("10.0.0.1")}

	cases := []struct {
		ip   string
		want bool
		desc string
	}{
		// Accepted via CIDR entry.
		{"192.168.0.1", true, "first host in 192.168.0.0/16"},
		{"192.168.5.10", true, "arbitrary host inside CIDR"},
		{"192.168.255.254", true, "last host in CIDR"},
		// Accepted via individual IP entry.
		{"10.0.0.1", true, "exact individual IP match"},
		// Rejected — covered by neither entry.
		{"10.0.0.2", false, "adjacent to individual IP, not in CIDR"},
		{"172.16.0.1", false, "unrelated address"},
		{"193.168.0.1", false, "just outside CIDR"},
	}

	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("net.ParseIP(%q) returned nil", tc.ip)
		}
		got := ipInWhitelist(ip, wlNets, wlIPs)
		if got != tc.want {
			t.Errorf("ipInWhitelist(%s) = %v, want %v (%s)", tc.ip, got, tc.want, tc.desc)
		}
	}
}

// TestStart_WhitelistLoadedBeforeListenerOpens is a regression guard for the
// ordering invariant in Start():
//
//	loadWhitelistFromFile  →  net.Listen  →  tls.NewListener
//
// If a future refactor moves net.Listen above the whitelist load, inbound
// connections can arrive with an empty whitelist (fail-open) before the load
// finishes.
//
// The test detects this by replacing h.listenFunc (the injectable TCP-listen
// factory) with a custom factory that captures GetPeerWhitelist() at the
// exact moment of the bind call.  Because the factory IS the listen call, it
// executes at the true bind boundary — not at an independently-positioned hook.
// If net.Listen is ever moved before loadWhitelistFromFile, this factory runs
// before the whitelist is populated and the assertion inside it fails
// immediately, catching the regression in CI.
func TestStart_WhitelistLoadedBeforeListenerOpens(t *testing.T) {
	dir := t.TempDir()
	wlFile := filepath.Join(dir, "whitelist.json")

	// Write a known whitelist to the sidecar so loadWhitelistFromFile has
	// something to load (the open-network case would not distinguish orderings).
	if err := os.WriteFile(wlFile, []byte(`["203.0.113.7", "198.51.100.0/24"]`), 0o600); err != nil {
		t.Fatalf("setup: write sidecar: %v", err)
	}

	h := newWLTestHost(t, func(cfg *Config) {
		cfg.WhitelistFile = wlFile
	})

	// wlAtBindTime holds the whitelist snapshot captured inside the listen
	// factory — at the real TCP bind point, not a separate hook position.
	var wlAtBindTime []string

	SetListenFunc(h, func(network, addr string) (net.Listener, error) {
		// This executes at the exact point Start() calls h.listenFunc.
		// If loadWhitelistFromFile ran before us, the whitelist is already
		// populated.  If not, we capture an empty list and the assertions below
		// fail, exposing the ordering regression.
		wlAtBindTime = h.GetPeerWhitelist()
		// Delegate to real net.Listen so Start() gets a working listener.
		return net.Listen(network, addr)
	})

	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// The factory must have been called (Start must have invoked h.listenFunc).
	if wlAtBindTime == nil {
		t.Fatal("listen factory was never called — listenFunc may not be wired in Start()")
	}

	// Both sidecar entries must be present at bind time, proving
	// loadWhitelistFromFile completed before the listener opened.
	has := func(want string) bool {
		for _, e := range wlAtBindTime {
			if e == want {
				return true
			}
		}
		return false
	}
	if !has("203.0.113.7") {
		t.Errorf("ordering regression: 203.0.113.7 not in whitelist at net.Listen time; got %v", wlAtBindTime)
	}
	if !has("198.51.100.0/24") {
		t.Errorf("ordering regression: 198.51.100.0/24 not in whitelist at net.Listen time; got %v", wlAtBindTime)
	}
}

// itoa is a tiny helper so the test file has no strconv import.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
