// Package deploy_test contains a CI guard for the install-node.sh --primary-ip
// flag.
//
// This file wraps test-install-node-primary-ip.sh, which runs the real
// install-node.sh inside a disposable Docker container (Ubuntu 22.04) wired
// with stub commands and a spy wrapper on node-config.sh.  The spy records
// every subcommand + argument passed to node-config.sh, letting the test
// verify that:
//
//   - With --primary-ip <IP>:
//     1. node-config.sh add-bootnode was called exactly once.
//     2. The multiaddr passed was /ip4/<IP>/tcp/30303.
//     3. node.yaml contains the bootnode entry.
//     4. The node service was enabled and started.
//
//   - Without --primary-ip:
//     5. node-config.sh add-bootnode was NOT called.
//     6. The warning block "ВНИМАНИЕ: bootnode не настроен" appears in stdout.
//     7. node.yaml p2p.bootnodes is empty (no phantom entries).
//     8. The node service was NOT started (safety hold).
//
// # Skip conditions
//
//   - Running on Windows (bash unavailable).
//   - bash not found in PATH.
//   - Docker not found in PATH.
//   - Docker daemon not reachable (docker info fails).
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestInstallNodePrimaryIP -v
//
//	# Or run the shell script directly:
//	bash blockchain/deploy/test-install-node-primary-ip.sh
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInstallNodePrimaryIP verifies that install-node.sh --primary-ip
// immediately wires the node into the network by calling
// node-config.sh add-bootnode with the correct multiaddr, and that omitting
// the flag prints a prominent warning and leaves the service stopped.
//
// The test delegates all setup and assertions to
// test-install-node-primary-ip.sh, which builds a disposable Docker image
// containing the real installer plus stub commands and a spy wrapper on
// node-config.sh.
func TestInstallNodePrimaryIP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-install-node-primary-ip.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestInstallNodePrimaryIP")
	}

	// The shell script exits 0 with a [SKIP] message when Docker is absent.
	// Mirror that check here so the Go test runner reports a clean skip.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping TestInstallNodePrimaryIP")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not reachable (%v); skipping TestInstallNodePrimaryIP\n%s",
			err, string(out))
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-install-node-primary-ip.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-install-node-primary-ip.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-install-node-primary-ip.sh reported failures: %v\n"+
			"  Run manually to see the full output:\n"+
			"    bash blockchain/deploy/test-install-node-primary-ip.sh",
			err)
	}
}
