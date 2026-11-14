//go:build linux

package p2p_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/p2p"
)

func TestLoadOrSaveP2PIdentityRejectsUnsafePermissions(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "p2p_identity.key")
	if _, _, _, err := p2p.LoadOrSaveP2PIdentity(keyPath, false); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatalf("make identity permissions unsafe: %v", err)
	}

	if _, _, _, err := p2p.LoadOrSaveP2PIdentity(keyPath, false); err == nil || !strings.Contains(err.Error(), "mode 0600") {
		t.Fatalf("expected unsafe permissions to be rejected, got %v", err)
	}
}

func TestLoadOrSaveP2PIdentityRejectsWrongOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing file owner requires root")
	}

	keyPath := filepath.Join(t.TempDir(), "p2p_identity.key")
	if _, _, _, err := p2p.LoadOrSaveP2PIdentity(keyPath, false); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if err := os.Chown(keyPath, 1, -1); err != nil {
		t.Fatalf("change test file owner: %v", err)
	}

	if _, _, _, err := p2p.LoadOrSaveP2PIdentity(keyPath, false); err == nil || !strings.Contains(err.Error(), "owned by the node process user") {
		t.Fatalf("expected wrong owner to be rejected, got %v", err)
	}
}
