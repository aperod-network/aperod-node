// Package deploy_test — mock-SSH end-to-end test for join-network.sh.
//
// TestJoinNetworkMockSSH runs test-join-network-mock-ssh.sh, which exercises
// the full join-network.sh script against a stubbed SSH target and asserts that
// the secondary node's node.yaml ends up with the correct bootnode written into
// p2p.bootnodes after the script exits.
//
// Unlike TestJoinNetworkBootnode (which tests the Python injection logic in
// isolation) and TestJoinNetworkRsyncGuard (which focuses on the rsync-safety
// guard), this test drives the complete script end-to-end:
//
//  1. All SSH, rsync, systemctl, and sleep calls are replaced with bash stubs.
//  2. The SSH stub intercepts the step-5/7 bootnode-injection heredoc
//     (detected by the remote command being exactly "bash") and executes it
//     locally against a real temp node.yaml — confirming that the quoting,
//     variable expansion, and Python/node-config.sh logic in the heredoc
//     actually work end-to-end.
//  3. The stub SSH returns JSON with peer_count >= 1 for the network/stats
//     health poll, allowing the script to exit 0 and report success.
//
// Scenarios covered:
//   - Python fallback path (node-config.sh absent) writes bootnode correctly.
//   - node-config.sh preferred path writes bootnode correctly (when present).
//   - Pre-existing p2p.bootnodes entries are preserved.
//   - Legacy root-level bootnodes key is migrated into p2p.bootnodes.
//   - Running the script twice does not produce duplicate bootnode entries.
//
// # Skip conditions
//
//   - Running on Windows (test-join-network-mock-ssh.sh requires bash).
//   - bash not found in PATH.
//   - python3 not found in PATH.
//   - python3 pyyaml library not available.
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestJoinNetworkMockSSH -v
//
//	# Or run the shell script directly:
//	bash blockchain/deploy/test-join-network-mock-ssh.sh
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestJoinNetworkMockSSH shells out to test-join-network-mock-ssh.sh and
// fails if the script reports any failure.
func TestJoinNetworkMockSSH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-join-network-mock-ssh.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestJoinNetworkMockSSH")
	}

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH; skipping TestJoinNetworkMockSSH")
	}

	// The script requires the pyyaml library; skip gracefully when absent.
	checkYaml := exec.Command("python3", "-c", "import yaml")
	if err := checkYaml.Run(); err != nil {
		t.Skip("python3 pyyaml not available; skipping TestJoinNetworkMockSSH " +
			"(install with: pip3 install pyyaml)")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-join-network-mock-ssh.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-join-network-mock-ssh.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-join-network-mock-ssh.sh reported failures: %v\n"+
			"  This means join-network.sh's bootnode injection (step 5/7)\n"+
			"  is broken in the full-script execution path.\n"+
			"  Re-run with: bash blockchain/deploy/test-join-network-mock-ssh.sh",
			err)
	}
}
