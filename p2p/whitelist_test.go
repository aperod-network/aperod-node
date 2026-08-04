package p2p

import (
	"encoding/json"
	"log/slog"
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
	if ok := h.RemoveFromWhitelist("1.2.3.4"); !ok {
		t.Fatal("RemoveFromWhitelist: expected true, got false")
	}
	if got := h.GetPeerWhitelist(); len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Fatalf("expected [10.0.0.0/8], got %v", got)
	}

	// Remove non-existent entry → false.
	if ok := h.RemoveFromWhitelist("9.9.9.9"); ok {
		t.Fatal("RemoveFromWhitelist of absent entry: expected false, got true")
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
			_ = h.RemoveFromWhitelist("10.1.1." + itoa(i%5+1))
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
	h1.loadWhitelistFromFile() // simulates Start()

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
	h2.loadWhitelistFromFile()

	got := h2.GetPeerWhitelist()
	has := func(s string) bool {
		for _, e := range got { if e == s { return true } }
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
	if ok := h2.RemoveFromWhitelist("5.5.5.5"); !ok {
		t.Fatal("RemoveFromWhitelist(5.5.5.5): expected true")
	}
	h3 := newWLTestHost(t, func(cfg *Config) {
		cfg.PeerWhitelist = []string{"5.5.5.5"} // still in node.yaml
		cfg.WhitelistFile = wlFile
	})
	h3.loadWhitelistFromFile()

	got3 := h3.GetPeerWhitelist()
	for _, e := range got3 {
		if e == "5.5.5.5" {
			t.Errorf("restart after removal: 5.5.5.5 should NOT be in whitelist (cfg entry removed by admin)")
		}
	}
	if !func() bool { for _, e := range got3 { if e == "9.9.9.9" { return true } }; return false }() {
		t.Errorf("restart after removal: 9.9.9.9 should still be in whitelist, got %v", got3)
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
	h.SetPeerWhitelist(entries)

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
