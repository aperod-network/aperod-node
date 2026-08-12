package p2p_test

// Test for TASK #1770: fail-closed behaviour when the whitelist sidecar file
// exists but cannot be read (permission denied).
//
// loadWhitelistFromFile must return a fatal error in that case so Start()
// aborts rather than silently running fail-open (accepting every inbound IP,
// which would defeat the whitelist's access-control purpose).
//
// NOTE: an EMPTY or ABSENT sidecar intentionally falls back to the node.yaml
// peer_whitelist entries; that behaviour is covered elsewhere and is not
// exercised here.  Only the unreadable-file case must fail closed.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aperod/aperod/p2p"
)

// TestStart_FailsClosedOnUnreadableWhitelistSidecar creates a sidecar file,
// chmods it to 0000 so it cannot be read, then asserts Start() returns a
// non-nil error instead of opening the network.
func TestStart_FailsClosedOnUnreadableWhitelistSidecar(t *testing.T) {
	// File-permission enforcement is only meaningful for a non-root user on a
	// POSIX filesystem.  Root bypasses the permission bits entirely, and the
	// chmod semantics do not apply on non-Linux platforms in CI.
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permission bits do not apply")
	}
	if runtime.GOOS != "linux" {
		t.Skip("permission-denied semantics only reliably testable on linux")
	}

	dir := t.TempDir()
	sidecar := filepath.Join(dir, "whitelist.json")

	// Write a syntactically valid sidecar so the ONLY failure is the read
	// permission, not JSON corruption.
	if err := os.WriteFile(sidecar, []byte(`["10.0.0.0/8"]`), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	// Make the file unreadable: permission denied on ReadFile.
	if err := os.Chmod(sidecar, 0o000); err != nil {
		t.Fatalf("chmod 0000: %v", err)
	}
	t.Cleanup(func() {
		// Restore perms so t.TempDir cleanup can remove the file.
		_ = os.Chmod(sidecar, 0o600)
	})

	host := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      10,
		NodeID:        "host",
		UserAgent:     "aperod/test",
		WhitelistFile: sidecar,
	}, &stubHandler{}, newTestLogger())

	err := host.Start()
	if err == nil {
		// Start opened the network despite the unreadable whitelist — this is
		// the fail-open regression the task guards against.
		host.Stop()
		t.Fatal("Start() returned nil for an unreadable whitelist sidecar; " +
			"expected a fatal error (fail-closed)")
	}
}
