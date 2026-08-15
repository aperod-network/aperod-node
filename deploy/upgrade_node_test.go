// Package deploy_test contains CI guards for the upgrade-node.sh upgrade script.
//
// This file adds an end-to-end smoke test that runs the real upgrade-node.sh
// inside sandboxed Docker containers under two scenarios.
//
// Scenario A — normal upgrade (drop-ins already present):
//  1. upgrade-node.sh exits 0.
//  2. ensure-dropin.sh was called (call record exists in the ordering log).
//  3. ensure-dropin.sh was called BEFORE the stub update-node.sh logged its
//     service-restart event (the ordering guarantee that prevents a node from
//     restarting without GOMEMLIMIT or TimeoutStopSec in place).
//  4. Both drop-in files exist in DROPIN_DIR after the run.
//
// Scenario B — pre-drop-in install (no drop-in files existed before the upgrade):
//  1. upgrade-node.sh exits 0.
//  2. ensure-dropin.sh was called.
//  3. ensure-dropin.sh was called BEFORE the service restart.
//  4. gomemlimit.conf was created by ensure-dropin.sh.
//  5. timeout.conf was created by ensure-dropin.sh.
//  6. gomemlimit.conf contains [Service] and a GOMEMLIMIT= directive.
//  7. timeout.conf contains [Service] and a TimeoutStopSec= directive.
//
// The test delegates to test-upgrade-node.sh, which builds a minimal Ubuntu 22.04
// Docker image pre-wired with:
//   - A "previously installed" node state (aperod user, binary, service file)
//     created by a preseed script.
//   - A stub ensure-dropin.sh that appends "ensure-dropin-called" to a sequence
//     log and then writes the drop-in files to the injected DROPIN_DIR.
//   - A stub update-node.sh that appends "service-restart-called" to the same
//     sequence log so the ordering assertion can compare line numbers.
//   - upgrade-node.sh forwards DROPIN_DIR and SYSTEMCTL through to ensure-dropin.sh
//     via the injectable seams defined in the script header.
//
// # Skip conditions
//
//   - Running on Windows (test-upgrade-node.sh requires bash).
//   - bash not found in PATH.
//   - Docker not found in PATH.
//   - Docker daemon not reachable (docker info fails).
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestUpgradeNodeE2E -v
//
//	# Or run the shell script directly:
//	bash blockchain/deploy/test-upgrade-node.sh
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestUpgradeNodeE2E runs test-upgrade-node.sh as a subprocess.
//
// The shell script:
//  1. Builds a disposable Ubuntu 22.04 Docker image containing the real
//     upgrade-node.sh, stub ensure-dropin.sh, stub update-node.sh, and a
//     preseed script that creates the "already installed" node state.
//  2. Runs Scenario A (normal upgrade — drop-ins already present) to verify
//     that ensure-dropin.sh is called and ordering is preserved.
//  3. Runs Scenario B (pre-drop-in install — no drop-ins before upgrade) to
//     verify that ensure-dropin.sh creates the drop-ins BEFORE update-node.sh
//     restarts the service, even on nodes installed before task-1429.
//
// Any regression that causes upgrade-node.sh to restart the service before
// ensure-dropin.sh has run — or to skip ensure-dropin.sh entirely — will
// cause this test to fail before it reaches a production server.
func TestUpgradeNodeE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-upgrade-node.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestUpgradeNodeE2E")
	}

	// The shell script itself performs a Docker availability check and exits 0
	// with a [SKIP] message when Docker is absent or the daemon is not running.
	// We replicate that check here so the Go test runner also reports a proper
	// skip rather than a failure.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping TestUpgradeNodeE2E")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not reachable (%v); skipping TestUpgradeNodeE2E\n%s",
			err, string(out))
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-upgrade-node.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-upgrade-node.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-upgrade-node.sh reported failures: %v\n"+
			"  Scenario A checks: ensure-dropin.sh called before service restart (drop-ins present).\n"+
			"  Scenario B checks: ensure-dropin.sh called before service restart (pre-drop-in install).\n"+
			"  Re-run with: bash blockchain/deploy/test-upgrade-node.sh",
			err)
	}
}
