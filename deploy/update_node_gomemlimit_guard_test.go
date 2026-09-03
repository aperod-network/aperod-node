// Package deploy_test contains CI guards for update-node's host-aware
// GOMEMLIMIT policy.
//
// TestUpdateNodeGomemlimitGuardScript shells out to
// test-update-node-gomemlimit-guard.sh, which verifies that update-node uses
// the shared policy and that a 2 GiB relay cannot resolve to the primary cap.
//
// # Skip conditions
//
//   - Running on Windows (bash not available).
//   - bash not found in PATH.
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestUpdateNodeGomemlimitGuardScript -v
//
//	# Or run the shell script directly:
//	bash blockchain/deploy/test-update-node-gomemlimit-guard.sh
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestUpdateNodeGomemlimitGuardScript runs test-update-node-gomemlimit-guard.sh
// as a subprocess.  The shell script is stub-based (no root, no real systemd,
// no real canonical file) and is self-contained — it requires only bash and
// a writable /tmp.
func TestUpdateNodeGomemlimitGuardScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-update-node-gomemlimit-guard.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestUpdateNodeGomemlimitGuardScript")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-update-node-gomemlimit-guard.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-update-node-gomemlimit-guard.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Errorf("test-update-node-gomemlimit-guard.sh reported failures: %v", err)
	}
}
