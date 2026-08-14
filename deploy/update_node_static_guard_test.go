// Package deploy_test contains CI guards for the static-link check in
// update-node.sh.
//
// TestUpdateNodeStaticGuardScript shells out to
// test-update-node-static-guard.sh, which exercises the Step 2b ldd/readelf
// guard in an isolated subshell — no root, no real systemd, no network.
//
// The shell script verifies three invariants:
//
//  1. (G1) When BINARY_SRC points to a known dynamically-linked system binary
//     (e.g. /bin/ls), the guard sets _binary_is_dynamic=true, prints the
//     "Static-link check FAILED" error on stderr, confirms the service was NOT
//     stopped, and exits non-zero.
//
//  2. (G2) When ldd is mocked to output "not a dynamic executable" (PATH
//     injection), the guard correctly identifies the binary as static and
//     exits 0 without printing any error — the happy path must not be broken.
//
//  3. (G3) The guard block is syntactically present in update-node.sh and
//     contains the expected variable name, error string, and exit statement,
//     so a future refactor cannot silently remove the check without failing CI.
//
// # Skip conditions
//
//   - Running on Windows (bash not available).
//   - bash not found in PATH.
//   - systemctl not found (handled inside the shell script — exits 0 with SKIP).
//   - ldd and readelf both absent (handled inside the shell script — exits 0 with SKIP).
//   - No known dynamic binary found (handled inside the shell script — exits 0 with SKIP).
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestUpdateNodeStaticGuardScript -v
//
//	# Or run the shell script directly:
//	bash blockchain/deploy/test-update-node-static-guard.sh
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestUpdateNodeStaticGuardScript runs test-update-node-static-guard.sh as a
// subprocess.  The shell script is stub-based (no root, no real systemd, no
// real binary build) and self-skips gracefully when the host environment lacks
// systemctl, ldd/readelf, or a suitable dynamic binary.
func TestUpdateNodeStaticGuardScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-update-node-static-guard.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestUpdateNodeStaticGuardScript")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-update-node-static-guard.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-update-node-static-guard.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Errorf("test-update-node-static-guard.sh reported failures: %v", err)
	}
}
