package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandshakeTimeoutProductionDefault(t *testing.T) {
	if got := DefaultConfig().P2P.HandshakeTimeout; got != 3*time.Second {
		t.Fatalf("default handshake timeout = %v, want 3s", got)
	}
}

func TestHandshakeTimeoutYAML(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"omitted p2p", "log_level: info\n", 3 * time.Second},
		{"omitted key", "p2p:\n  max_peers: 10\n", 3 * time.Second},
		{"custom", "p2p:\n  handshake_timeout: 2s\n", 2 * time.Second},
		{"fractional seconds", "p2p:\n  handshake_timeout: 1500ms\n", 1500 * time.Millisecond},
		{"explicit host fallback", "p2p:\n  handshake_timeout: 0s\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("valid handshake timeout rejected: %v", err)
			}
			if got := cfg.P2P.HandshakeTimeout; got != tc.want {
				t.Fatalf("handshake_timeout = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHandshakeTimeoutNegativeRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(path, []byte("p2p:\n  handshake_timeout: -1s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "p2p.handshake_timeout") {
		t.Fatalf("expected a named validation error, got %v", err)
	}
}

func TestHandshakeTimeoutMalformedYAMLRejected(t *testing.T) {
	for _, value := range []string{"not-a-duration", "3", "999999999999999999999h"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.yaml")
			if err := os.WriteFile(path, []byte("p2p:\n  handshake_timeout: "+value+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("malformed handshake timeout was silently accepted")
			}
		})
	}
}

func TestHandshakeTimeoutExampleYAML(t *testing.T) {
	cfg, err := Load("../node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.P2P.HandshakeTimeout; got != 3*time.Second {
		t.Fatalf("example handshake timeout = %v, want 3s", got)
	}
}
