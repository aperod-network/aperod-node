// Package deploy_test contains CI guards for the deploy scripts.
//
// This file adds an end-to-end smoke test that runs the real
// uninstall-validator.sh inside a sandboxed Docker container and asserts that:
//
//  1. The uninstaller exits 0.
//  2. `systemctl is-active aperod-node` returns non-zero (service stopped).
//  3. The systemd service unit file is absent.
//  4. The aperod-node binary is absent.
//  5. The aperod binary is absent.
//  6. The config directory /etc/aperod is absent.
//  7. The data directory /var/lib/aperod is absent.
//  8. The install directory /opt/aperod is absent.
//  9. The system user "aperod" is absent.
//
// The test delegates to test-uninstall-validator-e2e.sh, which builds a
// minimal Ubuntu 22.04 Docker image pre-seeded with the expected post-install
// file layout (service file, stub binaries, config, data dirs, system user,
// and a running fake process tracked by a PID file).  A fake systemctl stub
// handles stop/is-active/disable/daemon-reload without systemd itself.
//
// # Skip conditions
//
//   - Running on Windows (test-uninstall-validator-e2e.sh requires bash).
//   - bash not found in PATH.
//   - Docker not found in PATH.
//   - Docker daemon not reachable (docker info fails).
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestUninstallValidatorE2E -v
//
//	# Or run the shell script directly:
//	bash blockchain/deploy/test-uninstall-validator-e2e.sh
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestUninstallValidatorE2E runs test-uninstall-validator-e2e.sh as a subprocess.
//
// The shell script:
//  1. Builds a disposable Ubuntu 22.04 Docker image containing the real
//     uninstall-validator.sh, all sibling deploy files, and a fake systemctl
//     stub that simulates a running aperod-node service via PID file.
//  2. Pre-seeds the container with the expected post-install layout:
//     service unit, binaries, config dir, data dir, install dir, and system
//     user.  The fake aperod-node process is forked so the stub reports the
//     service as active before the uninstaller runs.
//  3. Runs uninstall-validator.sh non-interactively
//     (APEROD_UNINSTALL_CONFIRM=YES).
//  4. Asserts that every artifact the uninstaller is responsible for removing
//     is gone, and that the service is no longer active.
//
// Any regression in the uninstaller — service left running, unit file not
// removed, directory not deleted, user not removed — will cause this test to
// fail before it reaches production.
func TestUninstallValidatorE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-uninstall-validator-e2e.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestUninstallValidatorE2E")
	}

	// The shell script itself performs a Docker availability check and exits 0
	// with a [SKIP] message when Docker is absent or the daemon is not running.
	// We replicate that check here so the Go test runner also reports a proper
	// skip rather than a failure.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping TestUninstallValidatorE2E")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not reachable (%v); skipping TestUninstallValidatorE2E\n%s",
			err, string(out))
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-uninstall-validator-e2e.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-uninstall-validator-e2e.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-uninstall-validator-e2e.sh reported failures: %v\n"+
			"  This means uninstall-validator.sh left aperod artifacts behind.\n"+
			"  Re-run with: bash blockchain/deploy/test-uninstall-validator-e2e.sh",
			err)
	}
}
