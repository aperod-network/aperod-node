// Package deploy_test — CI guard for the EXIT/ERR cleanup trap in the
// --bootstrap-from (bootstrap) mode of join-network.sh.
//
// TestJoinNetworkBootstrapTrap shells out to
// test-join-network-bootstrap-trap.sh which exercises three scenarios without
// requiring live infrastructure:
//
//  1. Rsync failure → trap fires
//     rsync exits non-zero after both nodes were stopped.  The trap must call
//     `systemctl start aperod-node` (local node) and
//     `ssh root@VALIDATOR systemctl start aperod-node` (remote validator).
//     The [TRAP] banner must appear in the output.
//
//  2. Successful run → trap cleared, NOT fired on exit 0
//     A clean bootstrap completes and calls `trap - EXIT ERR` before exit.
//     `systemctl start aperod-node` must be called exactly once (from the
//     normal `systemctl enable --now` at step 8) and the [TRAP] banner must
//     be absent from the output.
//
//  3. Chain.db rsync exits with a non-zero code (e.g. 11 = partial transfer)
//     Same trap-fires assertions as scenario 1.
//
// Run from the blockchain root:
//
//	go test ./deploy/... -run TestJoinNetworkBootstrapTrap
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

// TestJoinNetworkBootstrapTrap shells out to
// test-join-network-bootstrap-trap.sh and fails if the script reports any
// test failure.
func TestJoinNetworkBootstrapTrap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-join-network-bootstrap-trap.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestJoinNetworkBootstrapTrap")
	}

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH; skipping TestJoinNetworkBootstrapTrap")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-join-network-bootstrap-trap.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-join-network-bootstrap-trap.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-join-network-bootstrap-trap.sh reported failures: %v", err)
	}
}
