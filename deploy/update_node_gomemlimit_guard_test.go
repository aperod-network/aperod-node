// Package deploy_test contains CI guards for the gomemlimit.conf preflight
// check in update-node.sh.
//
// TestUpdateNodeGomemlimitGuardScript shells out to
// test-update-node-gomemlimit-guard.sh, which extracts and runs the real
// Step 0d block from update-node.sh in an isolated subshell — no root, no
// real systemd, no real GOMEMLIMIT canonical file — and asserts six
// invariants:
//
//  1. (T1) When the canonical gomemlimit.conf is absent the guard exits
//     non-zero, prints "NOT stopped" to stderr, and does not invoke systemctl,
//     confirming the service is never stopped before the OOM guard is verified.
//
//  2. (T2) When the canonical file exists but contains no GOMEMLIMIT=<digits>
//     sequence the guard exits non-zero, prints "could not parse GOMEMLIMIT"
//     and "NOT stopped" to stderr, and does not invoke systemctl — a corrupt
//     or empty file is treated identically to an absent one.
//
//  3. (T3) When the canonical file is valid the guard exits 0, writes a
//     correct [Service] / Environment="GOMEMLIMIT=<N>" drop-in, and calls
//     systemctl daemon-reload exactly once.
//
//  4. (T4) Static analysis: the Step 0d block is still present in
//     update-node.sh and contains the expected sentinel strings, so a future
//     refactor cannot silently remove the guard without failing CI.
//
//  5. (T5) Static ordering: the GOMEMLIMIT_CANONICAL= preflight line appears
//     before the first `systemctl stop` in update-node.sh, confirming that a
//     corrupt or absent canonical file is detected before the node is ever
//     stopped.
//
//  6. (T6) Self-check: a synthetic update-node.sh with `systemctl stop` moved
//     above Step 0d is correctly identified as a regression, so the T5
//     ordering detection cannot be silently bypassed.
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
