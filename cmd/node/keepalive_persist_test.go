package main

// keepalive_persist_test.go — tests for persistKeepaliveInterval and
// readYAMLKeepaliveInterval (Task: persist Admin-Panel keepalive tuning to
// node.yaml so it survives a restart).
//
// Every persistence test re-loads the rewritten file through config.Load
// (via readYAMLKeepaliveInterval) — proving the output is a valid, loadable
// config across YAML layouts: 2-space block, 4-space block, flow-style
// mappings, null p2p sections, and absent p2p sections.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/config"
)

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return path
}

// persistAndReload runs persistKeepaliveInterval then re-loads the result
// via config.Load, failing the test if the rewritten file is not loadable
// or does not round-trip the new value.
func persistAndReload(t *testing.T, path string, d time.Duration) string {
	t.Helper()
	if err := persistKeepaliveInterval(path, d); err != nil {
		t.Fatalf("persist: %v", err)
	}
	got, err := readYAMLKeepaliveInterval(path)
	if err != nil {
		t.Fatalf("rewritten config not loadable: %v", err)
	}
	if got != d {
		t.Fatalf("round-trip mismatch: got %v want %v", got, d)
	}
	data, _ := os.ReadFile(path)
	return string(data)
}

func TestPersistKeepalive_ReplacesExistingValue(t *testing.T) {
	s := persistAndReload(t, writeTempYAML(t, `network: testnet
data_dir: /tmp/x

p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
  # tuned for slow links
  keepalive_interval: 10s
  max_peers: 50

api:
  listen_addr: 127.0.0.1:8545
`), 5*time.Second)
	if !strings.Contains(s, "keepalive_interval: 5s") {
		t.Fatalf("value not replaced:\n%s", s)
	}
	if strings.Contains(s, "10s") {
		t.Fatalf("old value still present:\n%s", s)
	}
	// Comments and other keys preserved by the AST round-trip.
	for _, want := range []string{"# tuned for slow links", "max_peers: 50", "api:", "network: testnet"} {
		if !strings.Contains(s, want) {
			t.Fatalf("lost %q:\n%s", want, s)
		}
	}
}

func TestPersistKeepalive_InsertsIntoExistingP2PSection(t *testing.T) {
	s := persistAndReload(t, writeTempYAML(t, `network: testnet
p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
api:
  listen_addr: 127.0.0.1:8545
`), 7*time.Second)
	if !strings.Contains(s, "keepalive_interval: 7s") {
		t.Fatalf("key not inserted:\n%s", s)
	}
	if !strings.Contains(s, "listen_addr: /ip4/0.0.0.0/tcp/30303") {
		t.Fatalf("existing p2p key lost:\n%s", s)
	}
}

func TestPersistKeepalive_FourSpaceIndent(t *testing.T) {
	// Reviewer regression: block mapping with 4-space indentation must stay
	// a valid, loadable config after insertion.
	s := persistAndReload(t, writeTempYAML(t, `network: testnet
p2p:
    listen_addr: /ip4/0.0.0.0/tcp/30303
    max_peers: 50
api:
    listen_addr: 127.0.0.1:8545
`), 6*time.Second)
	cfg, err := config.Load(writeTempYAML(t, s))
	if err != nil {
		t.Fatalf("4-space rewrite not loadable: %v\n%s", err, s)
	}
	if cfg.P2P.MaxPeers != 50 {
		t.Fatalf("max_peers lost: got %d\n%s", cfg.P2P.MaxPeers, s)
	}
}

func TestPersistKeepalive_FlowStyleP2PMapping(t *testing.T) {
	// Reviewer regression: flow-style p2p mapping must remain valid YAML.
	s := persistAndReload(t, writeTempYAML(t,
		"network: testnet\np2p: {listen_addr: /ip4/0.0.0.0/tcp/30303, max_peers: 25}\n",
	), 4*time.Second)
	cfg, err := config.Load(writeTempYAML(t, s))
	if err != nil {
		t.Fatalf("flow-style rewrite not loadable: %v\n%s", err, s)
	}
	if cfg.P2P.MaxPeers != 25 {
		t.Fatalf("max_peers lost: got %d\n%s", cfg.P2P.MaxPeers, s)
	}
	if cfg.P2P.KeepaliveInterval != 4*time.Second {
		t.Fatalf("keepalive not set: got %v", cfg.P2P.KeepaliveInterval)
	}
}

func TestPersistKeepalive_NullP2PSection(t *testing.T) {
	// "p2p:" with no children parses as a null value — must be upgraded to a
	// mapping, not corrupted.
	persistAndReload(t, writeTempYAML(t, "network: testnet\np2p:\n"), 8*time.Second)
}

func TestPersistKeepalive_AppendsP2PSectionWhenMissing(t *testing.T) {
	s := persistAndReload(t, writeTempYAML(t, "network: testnet\ndata_dir: /tmp/x\n"), 3*time.Second)
	if !strings.Contains(s, "keepalive_interval: 3s") {
		t.Fatalf("p2p section not appended:\n%s", s)
	}
}

func TestPersistKeepalive_DoesNotTouchOtherSectionsKey(t *testing.T) {
	// A keepalive_interval-like key in another section must not be modified.
	s := persistAndReload(t, writeTempYAML(t, `other:
  keepalive_interval: 99s
p2p:
  keepalive_interval: 10s
`), 4*time.Second)
	if !strings.Contains(s, "keepalive_interval: 99s") {
		t.Fatalf("other section modified:\n%s", s)
	}
	if !strings.Contains(s, "keepalive_interval: 4s") {
		t.Fatalf("p2p value not replaced:\n%s", s)
	}
}

func TestPersistKeepalive_PreservesFileMode(t *testing.T) {
	path := writeTempYAML(t, "p2p:\n  keepalive_interval: 10s\n")
	persistAndReload(t, path, 6*time.Second)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode not preserved: got %v want 0640", info.Mode().Perm())
	}
	// No tmp leftovers.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".node.yaml.tmp-") {
			t.Fatalf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestPersistKeepalive_MissingFileErrors(t *testing.T) {
	if err := persistKeepaliveInterval(filepath.Join(t.TempDir(), "absent.yaml"), 5*time.Second); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestPersistKeepalive_UnsupportedShapeLeavesFileUntouched(t *testing.T) {
	// A p2p value that is a sequence cannot be safely mutated — the helper
	// must refuse and leave the original file byte-identical.
	orig := "p2p:\n  - not\n  - a\n  - mapping\n"
	path := writeTempYAML(t, orig)
	if err := persistKeepaliveInterval(path, 5*time.Second); err == nil {
		t.Fatal("expected error for non-mapping p2p value")
	}
	data, _ := os.ReadFile(path)
	if string(data) != orig {
		t.Fatalf("original file modified on failure:\n%s", string(data))
	}
}

func TestReadYAMLKeepalive_RoundTripAndDefault(t *testing.T) {
	path := writeTempYAML(t, "p2p:\n  keepalive_interval: 10s\n")
	persistAndReload(t, path, 5*time.Second)

	// Zero / absent value normalizes to the built-in 10s default.
	path2 := writeTempYAML(t, "network: testnet\n")
	d2, err := readYAMLKeepaliveInterval(path2)
	if err != nil {
		t.Fatalf("read default: %v", err)
	}
	if d2 != 10*time.Second {
		t.Fatalf("default got %v want 10s", d2)
	}
}
