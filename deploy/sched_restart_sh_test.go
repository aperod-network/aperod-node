// Package deploy_test contains a CI guard that shells out to
// test-sched-restart.sh and fails the Go test suite when the script reports
// any failure.
//
// Run from the blockchain root:
//
//	go test ./deploy/...
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSchedRestartSh runs test-sched-restart.sh as a subprocess and fails if
// the script exits with a non-zero status.
//
// The test is skipped when:
//   - the OS is Windows (bash unavailable)
//   - bash is not found in PATH
//   - python3 is not found in PATH (needed for mock HTTP servers)
func TestSchedRestartSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-sched-restart.sh requires bash; skipping on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestSchedRestartSh")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH; skipping TestSchedRestartSh (needed for mock HTTP servers)")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "test-sched-restart.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-sched-restart.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-sched-restart.sh reported failures: %v", err)
	}
}
