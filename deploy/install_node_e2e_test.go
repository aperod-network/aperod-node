// Package deploy_test contains CI guards for the install-node.sh installer.
//
// This file adds an end-to-end smoke test that runs the real install-node.sh
// inside a sandboxed Docker container and asserts that:
//
//  1. The installer exits 0.
//  2. `systemctl is-active aperod-node` returns 0 (service active).
//  3. `curl http://127.0.0.1:8545/health` returns HTTP 200.
//  4. The systemd service file is present.
//  5. The GOMEMLIMIT drop-in is present.
//  6. The node config file is present.
//
// The test delegates to test-install-node-e2e.sh, which builds a minimal
// Ubuntu 22.04 Docker image pre-wired with stub commands (fake systemctl,
// wget, make, go, ufw) and a Python3 aperod-node stub that serves the health
// endpoint.  This means no real network access, Go toolchain download, or
// blockchain binary compilation is required.
//
// # Skip conditions
//
//   - Running on Windows (test-install-node-e2e.sh requires bash).
//   - bash not found in PATH.
//   - Docker not found in PATH.
//   - Docker daemon not reachable (docker info fails).
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestInstallNodeE2E -v
//
//	# Or run the shell script directly:
//	bash blockchain/deploy/test-install-node-e2e.sh
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInstallNodeE2E runs test-install-node-e2e.sh as a subprocess.
//
// The shell script:
//  1. Builds a disposable Ubuntu 22.04 Docker image containing the real
//     install-node.sh, all sibling deploy files, and a set of stub commands
//     that prevent network I/O and simulate systemd.
//  2. Runs the installer non-interactively inside the container (stdin is
//     piped with a wallet-choice answer; external IP lookup is stubbed so
//     the IP read prompt is never reached).
//  3. Asserts service-active status and health-endpoint reachability.
//
// Any regression in the installer — broken user creation, missing file copy,
// wrong config path, removed service file — will cause this test to fail
// before it reaches production.
func TestInstallNodeE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-install-node-e2e.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestInstallNodeE2E")
	}

	// The shell script itself performs a Docker availability check and exits 0
	// with a [SKIP] message when Docker is absent or the daemon is not running.
	// We replicate that check here so the Go test runner also reports a proper
	// skip rather than a failure.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping TestInstallNodeE2E")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not reachable (%v); skipping TestInstallNodeE2E\n%s",
			err, string(out))
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-install-node-e2e.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-install-node-e2e.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-install-node-e2e.sh reported failures: %v\n"+
			"  This means install-node.sh left the node in a broken state.\n"+
			"  Re-run with: bash blockchain/deploy/test-install-node-e2e.sh",
			err)
	}
}
