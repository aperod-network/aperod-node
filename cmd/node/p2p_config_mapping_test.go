package main

// Verifies the REAL node.yaml → p2p.Config mapping used at node startup
// (buildP2PConfig, invoked from run()).  Guards the config-to-host wiring of
// p2p.get_block_stall_timeout: an operator on a slow link who raises the
// value to 60s must get 60s at the p2p host, not the 15s built-in default.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aperod/aperod/config"
)

func TestBuildP2PConfig_GetBlockStallTimeoutFromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(yamlPath, []byte(
		"data_dir: "+dir+"\np2p:\n  get_block_stall_timeout: 60s\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cfg, err := config.Load(yamlPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.P2P.GetBlockStallTimeout != 60*time.Second {
		t.Fatalf("config: get_block_stall_timeout = %v, want 60s", cfg.P2P.GetBlockStallTimeout)
	}

	// Invoke the production mapping (the exact function run() calls when
	// constructing the p2p host).
	p2pCfg := buildP2PConfig(cfg, "127.0.0.1:0", nil, "test-node", nil, "")
	if p2pCfg.GetBlockStallTimeout != 60*time.Second {
		t.Fatalf("buildP2PConfig: GetBlockStallTimeout = %v, want 60s — "+
			"the node.yaml → p2p.Config mapping dropped the configured value",
			p2pCfg.GetBlockStallTimeout)
	}

	// Absent key → zero value ("use the p2p host's built-in 15s default").
	yamlPath2 := filepath.Join(dir, "node-default.yaml")
	if err := os.WriteFile(yamlPath2, []byte("data_dir: "+dir+"\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cfg2, err := config.Load(yamlPath2)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	p2pCfg2 := buildP2PConfig(cfg2, "127.0.0.1:0", nil, "test-node", nil, "")
	if p2pCfg2.GetBlockStallTimeout != 0 {
		t.Fatalf("buildP2PConfig: absent key should map to 0 (use default), got %v",
			p2pCfg2.GetBlockStallTimeout)
	}

	// Sanity: the mapping must also carry the other stall-adjacent defaults
	// (ban/whitelist file defaulting) without disturbing the timeout field.
	wantBan := filepath.Join(dir, "p2p_bans.json")
	if p2pCfg.BanFile != wantBan {
		t.Fatalf("buildP2PConfig: BanFile = %q, want %q", p2pCfg.BanFile, wantBan)
	}
}
