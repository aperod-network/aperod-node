// Package deploy_test contains a CI guard that shells out to
// test-node-config.sh and fails the Go test suite when the script reports any
// failure.
//
// Run from the blockchain root:
//
//	go test ./deploy/...
//
// The test is skipped automatically when bash or python3 is not available
// (e.g. on Windows CI runners or stripped Docker images).
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestNodeConfigSh runs test-node-config.sh as a subprocess and fails if the
// script exits with a non-zero status.
//
// The test is skipped when:
//   - the OS is Windows (bash unavailable)
//   - bash is not found in PATH
//   - python3 is not found in PATH (pyyaml check is inside the script itself)
func TestNodeConfigSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("node-config.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestNodeConfigSh")
	}

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH; skipping TestNodeConfigSh")
	}

	// The script requires the pyyaml library; skip gracefully when it is absent.
	checkYaml := exec.Command("python3", "-c", "import yaml")
	if err := checkYaml.Run(); err != nil {
		t.Skip("python3 pyyaml not available; skipping TestNodeConfigSh (install with: pip3 install pyyaml)")
	}

	// Locate test-node-config.sh relative to this test file's directory.
	// os.Getwd() during `go test ./deploy/...` is the package directory.
	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "test-node-config.sh")

	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-node-config.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-node-config.sh reported failures: %v", err)
	}
}
