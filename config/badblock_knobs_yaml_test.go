package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBadBlockKnobsParseFromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "node.yaml")
	os.WriteFile(p, []byte("p2p:\n  bad_block_height_lead: 500\n  bad_block_ban_threshold: 7\n  bad_block_ban_duration: 12h\n"), 0644)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.P2P.BadBlockHeightLead != 500 || c.P2P.BadBlockBanThreshold != 7 || c.P2P.BadBlockBanDuration != 12*time.Hour {
		t.Fatalf("got lead=%d thr=%d dur=%v", c.P2P.BadBlockHeightLead, c.P2P.BadBlockBanThreshold, c.P2P.BadBlockBanDuration)
	}
}
