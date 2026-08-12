// Package deploy_test contains CI guards for the validator-key permission
// preflight in update-node.sh.
//
// TestUpdateNodeKeyPreflight shells out to test-update-node-key-preflight.sh,
// which extracts the _resolve_validator_key_path and preflight_validator_key
// helpers verbatim from update-node.sh and exercises them with stubbed
// chmod / chown / stat.  It verifies that an unsafe (644-style) or wrongly
// owned validator key is auto-fixed with chmod 600 + chown aperod:aperod, that
// an already-safe key is left untouched (permissions are never loosened), that
// an unfixable key aborts the deploy, and that the whole preflight is wired to
// run BEFORE the service is stopped so a bad key never causes downtime.
//
// Run from the blockchain root:
//
//	go test ./deploy/...
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestUpdateNodeKeyPreflight runs test-update-node-key-preflight.sh as a
// subprocess.  It is stub-based (no root, no real systemd, no real key files)
// and is skipped when bash is unavailable (e.g. Windows CI runners).
func TestUpdateNodeKeyPreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-update-node-key-preflight.sh requires bash; skipping on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestUpdateNodeKeyPreflight")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "test-update-node-key-preflight.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-update-node-key-preflight.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Errorf("test-update-node-key-preflight.sh reported failures: %v", err)
	}
}
