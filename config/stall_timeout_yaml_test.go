package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGetBlockStallTimeoutParseFromYAML verifies that a node.yaml-style config
// with a raised get_block_stall_timeout (60s, for slow links) is parsed into
// P2PConfig.GetBlockStallTimeout — and that an absent key stays 0 so the p2p
// host applies its 15s built-in default.
func TestGetBlockStallTimeoutParseFromYAML(t *testing.T) {
	dir := t.TempDir()

	p := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(p, []byte("p2p:\n  get_block_stall_timeout: 60s\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.P2P.GetBlockStallTimeout != 60*time.Second {
		t.Fatalf("get_block_stall_timeout: got %v, want 60s", c.P2P.GetBlockStallTimeout)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected a valid 60s stall timeout: %v", err)
	}

	// Absent key → zero value ("use built-in default 15s" downstream).
	p2 := filepath.Join(dir, "node-default.yaml")
	if err := os.WriteFile(p2, []byte("p2p:\n  max_peers: 10\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	c2, err := Load(p2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c2.P2P.GetBlockStallTimeout != 0 {
		t.Fatalf("absent get_block_stall_timeout: got %v, want 0", c2.P2P.GetBlockStallTimeout)
	}
}

// TestValidate_GetBlockStallTimeoutNegative verifies that a negative
// get_block_stall_timeout is rejected at config-load time (a negative
// duration would panic in time.NewTicker inside the p2p host).
func TestValidate_GetBlockStallTimeoutNegative(t *testing.T) {
	c := DefaultConfig()
	c.P2P.GetBlockStallTimeout = -1 * time.Second
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a negative get_block_stall_timeout")
	}
	if !strings.Contains(err.Error(), "get_block_stall_timeout") {
		t.Fatalf("error does not mention get_block_stall_timeout: %v", err)
	}
}
