// Package deploy_test — CI guard for the aperod-watchdog-set-interval helper.
//
// Run from the blockchain root:
//
//	go test ./deploy/...
//
// The test shells out to test-watchdog-set-interval.sh and fails the Go
// suite when the script reports any failure.  It is automatically skipped on
// Windows or when bash is not in PATH.
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWatchdogSetInterval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-watchdog-set-interval.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestWatchdogSetInterval")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "test-watchdog-set-interval.sh")

	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-watchdog-set-interval.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-watchdog-set-interval.sh reported failures: %v", err)
	}
}
