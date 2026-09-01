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
// Missing runtime dependencies are failures on supported platforms: silently
// skipping this suite locally previously allowed CI-only failures to ship.
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
	"strings"
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
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 77 {
			t.Fatalf("test-join-network-mock-ssh.sh skipped required scenarios (exit 77); install the dependencies named in SKIP_SUMMARY")
		}
		t.Errorf("test-join-network-mock-ssh.sh reported failures: %v\n"+
			"  This means join-network.sh's bootnode injection (step 5/7)\n"+
			"  is broken in the full-script execution path.\n"+
			"  Re-run with: bash blockchain/deploy/test-join-network-mock-ssh.sh",
			err)
	}
}

// TestJoinNetworkMockSSHSkipContract ensures a missing optional dependency can
// never look like a successful full run to either humans or test wrappers.
func TestJoinNetworkMockSSHSkipContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-join-network-mock-ssh.sh requires bash; skipping on Windows")
	}

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH; skipping TestJoinNetworkMockSSHSkipContract")
	}
	dirnamePath, err := exec.LookPath("dirname")
	if err != nil {
		t.Skip("dirname not found in PATH; skipping TestJoinNetworkMockSSHSkipContract")
	}

	binDir := t.TempDir()
	if err := os.Symlink(bashPath, filepath.Join(binDir, "bash")); err != nil {
		t.Fatalf("link bash stub: %v", err)
	}
	if err := os.Symlink(dirnamePath, filepath.Join(binDir, "dirname")); err != nil {
		t.Fatalf("link dirname stub: %v", err)
	}

	scriptPath, err := filepath.Abs("test-join-network-mock-ssh.sh")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	cmd := exec.Command(bashPath, scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir)
	output, runErr := cmd.CombinedOutput()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 77 {
		t.Fatalf("missing python3 must exit 77, got %v\n%s", runErr, output)
	}
	for _, marker := range []string{
		"0 passed",
		"0 failed",
		"1 skipped",
		"M1-M6: python3 not found in PATH",
		"SKIP_SUMMARY count=1",
	} {
		if !strings.Contains(string(output), marker) {
			t.Errorf("skip output missing %q:\n%s", marker, output)
		}
	}
}
