// Package deploy_test — CI guard for the rsync-safety guard in join-network.sh.
//
// TestJoinNetworkRsyncGuard shells out to test-join-network-rsync-guard.sh
// which exercises two scenarios without requiring live infrastructure:
//
//  1. Negative path: a stubbed systemctl returns non-zero for
//     "stop aperod-node".  The script must abort before running rsync and
//     print the expected error message.
//
//  2. Positive path: all external commands (systemctl, ssh, rsync) are
//     replaced with stubs that simulate a clean run.  The stub ssh returns
//     valid JSON for the network/stats poll so the script detects height > 0
//     and peer_count > 0 and exits 0.
//
// Run from the blockchain root:
//
//	go test ./deploy/...
//
// The test is skipped automatically when bash or python3 is unavailable
// (e.g. on Windows CI runners or stripped Docker images).
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestJoinNetworkRsyncGuard shells out to test-join-network-rsync-guard.sh
// and fails if the script reports any failure.
func TestJoinNetworkRsyncGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-join-network-rsync-guard.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestJoinNetworkRsyncGuard")
	}

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH; skipping TestJoinNetworkRsyncGuard")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-join-network-rsync-guard.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-join-network-rsync-guard.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-join-network-rsync-guard.sh reported failures: %v", err)
	}
}
