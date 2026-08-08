// Package deploy_test — CI guard for P2P identity isolation in join-network.sh.
//
// TestJoinNetworkIdentity shells out to test-join-network-identity.sh, which
// verifies that join-network.sh never lets a relay node inherit the source
// node's p2p_identity.key after an rsync-based bootstrap.
//
// Two complementary defences are tested:
//
//  1. Defence A — rsync --exclude='p2p_identity.key'
//     The source key is never transferred to the target during rsync.
//
//  2. Defence B — SSH rm -f <target>/p2p_identity.key (Step 4)
//     Any key already on the target (from a previous install or a hypothetical
//     broken rsync without --exclude) is deleted so the node generates a fresh
//     TLS identity on first start.
//
// Test matrix (all run inside the shell script with bash stubs — no root
// access, real SSH target, or Docker is required):
//
//	I1  Normal path: rsync excludes key; SSH rm -f deletes old target key.
//	I2  Safety-net: SSH rm -f still fires even if rsync copied the key.
//	I3  Fresh-key: stub node generates a key that differs from the source key.
//
// # Skip conditions
//
//   - Running on Windows (test-join-network-identity.sh requires bash).
//   - bash not found in PATH.
//   - python3 not found in PATH (used by join-network.sh bootnode injection).
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestJoinNetworkIdentity -v
//
//	# Or run the shell script directly:
//	bash blockchain/deploy/test-join-network-identity.sh
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestJoinNetworkIdentity shells out to test-join-network-identity.sh and
// fails if any of the 10 identity-isolation assertions fail.
//
// The test guards against a class of misconfiguration where a relay node
// bootstrapped via rsync ends up sharing a TLS fingerprint with its peer,
// causing both nodes to log "p2p identity conflict detected — peer shares our
// TLS fingerprint" every 30 s and preventing stable P2P connections.
func TestJoinNetworkIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-join-network-identity.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestJoinNetworkIdentity")
	}

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH; skipping TestJoinNetworkIdentity")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}

	scriptPath := filepath.Join(scriptDir, "test-join-network-identity.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-join-network-identity.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-join-network-identity.sh reported failures: %v\n"+
			"  This means join-network.sh's p2p identity isolation (step 3/4)\n"+
			"  is broken — a relay node may inherit the source node's TLS key.\n"+
			"  Re-run with: bash blockchain/deploy/test-join-network-identity.sh",
			err)
	}
}
