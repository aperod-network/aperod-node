// Package deploy_test contains a CI guard that shells out to
// test-watchdog.sh and fails the Go test suite when the script reports any
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

// TestWatchdogSh runs test-watchdog.sh as a subprocess and fails if the
// script exits with a non-zero status.
//
// The test is skipped when:
//   - the OS is Windows (bash unavailable)
//   - bash is not found in PATH
//   - python3 is not found in PATH (needed for the mock HTTP server)
func TestWatchdogSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-watchdog.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestWatchdogSh")
	}

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH; skipping TestWatchdogSh (needed for mock HTTP server)")
	}

	// Locate test-watchdog.sh relative to this test file's directory.
	// os.Getwd() during `go test ./deploy/...` is the package directory.
	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "test-watchdog.sh")

	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-watchdog.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-watchdog.sh reported failures: %v", err)
	}
}
