// Package deploy_test contains CI guards for the update-node.sh upgrade script.
//
// This file adds an end-to-end smoke test that runs the real update-node.sh
// inside a sandboxed Docker container under two scenarios.
//
// Scenario 1 — normal upgrade:
//  1. update-node.sh exits 0.
//  2. `systemctl is-active aperod-node` returns 0 (service still active).
//  3. `curl http://127.0.0.1:8545/api/v1/status` returns HTTP 200.
//  4. /usr/local/bin/aperod-node is executable.
//  5. The systemd service file is still present.
//
// Scenario 2 — broken install (cp fails mid-install after service is stopped):
//  R1. update-node.sh exits non-zero (install failure is detected).
//  R2. /usr/local/bin/aperod-node is still executable (old binary restored).
//  R3. `systemctl is-active aperod-node` returns 0 (rollback restarted it).
//  R4. The systemd service file is still present.
//
// The test delegates to test-update-node-e2e.sh, which builds a minimal
// Ubuntu 22.04 Docker image pre-wired with:
//   - A "previously installed" node state (aperod user, service file, old
//     binary, node.yaml) created by a preseed script.
//   - Stub commands (fake systemctl, sudo, git, make) that prevent real
//     network I/O and system calls while faithfully exercising the upgrade
//     logic (stop → build → install → start → health-check).
//   - A Python3 aperod-node stub that serves the health endpoint on :8545
//     so the post-upgrade health check can succeed.
//   - A failing cp stub (/stubs-rollback/cp) that exits 1 for every call;
//     update-node.sh uses /bin/cp (absolute path) for backup and restore so
//     those succeed, and only the PATH-resolved install cp is intercepted.
//
// # Skip conditions
//
//   - Running on Windows (test-update-node-e2e.sh requires bash).
//   - bash not found in PATH.
//   - Docker not found in PATH.
//   - Docker daemon not reachable (docker info fails).
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestUpdateNodeE2E -v
//
//	# Or run the shell script directly:
//	bash blockchain/deploy/test-update-node-e2e.sh
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestUpdateNodeE2E runs test-update-node-e2e.sh as a subprocess.
//
// The shell script:
//  1. Builds a disposable Ubuntu 22.04 Docker image containing the real
//     update-node.sh, all sibling deploy files (including peer-check.sh),
//     stub commands, and a preseed script that creates the "already installed"
//     node state inside the container.
//  2. Runs update-node.sh non-interactively with SKIP_PEER_CHECK=1 so the
//     30-second peer-wait is bypassed (no actual P2P peers in a test env).
//  3. Asserts that the service is active and the health endpoint responds
//     after the upgrade.
//
// Any regression in the upgrade path — broken binary swap, missed
// daemon-reload, wrong file permissions, missing stop-before-copy step —
// will cause this test to fail before it reaches a production server.
func TestUpdateNodeE2E(t *testing.T) {
if os.Getenv("APEROD_RUN_DOCKER_E2E") != "1" {
t.Skip("Docker E2E disabled by default; set APEROD_RUN_DOCKER_E2E=1 to run TestUpdateNodeE2E")
}

	if runtime.GOOS == "windows" {
		t.Skip("test-update-node-e2e.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestUpdateNodeE2E")
	}

	// The shell script itself performs a Docker availability check and exits 0
	// with a [SKIP] message when Docker is absent or the daemon is not running.
	// We replicate that check here so the Go test runner also reports a proper
	// skip rather than a failure.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping TestUpdateNodeE2E")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not reachable (%v); skipping TestUpdateNodeE2E\n%s",
			err, string(out))
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-update-node-e2e.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-update-node-e2e.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-update-node-e2e.sh reported failures: %v\n"+
			"  This means update-node.sh left the node in a broken state.\n"+
			"  Re-run with: bash blockchain/deploy/test-update-node-e2e.sh",
			err)
	}
}
