// Package deploy_test contains a CI guard that shells out to
// test-backup.sh and fails the Go test suite when the script reports any
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

// TestBackupSh runs test-backup.sh as a subprocess and fails if the
// script exits with a non-zero status.
//
// The test verifies that aperod_backup.sh sends a Telegram failure alert
// via curl (directly from the bash EXIT trap) when the backup fails —
// independently of the API server's 30-minute monitor cycle.
//
// The test is skipped when:
//   - the OS is Windows (bash unavailable)
//   - bash is not found in PATH
func TestBackupSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-backup.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestBackupSh")
	}

	// Locate test-backup.sh relative to this test file's directory.
	// os.Getwd() during `go test ./deploy/...` is the package directory.
	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "test-backup.sh")

	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-backup.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Errorf("test-backup.sh reported failures: %v", err)
	}
}
