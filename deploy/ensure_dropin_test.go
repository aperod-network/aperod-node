// Package deploy_test contains CI guards for the ensure-dropin.sh helper.
//
// TestEnsureDropin shells out to test-ensure-dropin.sh, which runs the
// real ensure-dropin.sh with injectable DROPIN_DIR and SYSTEMCTL seams
// against controlled temp directories.  Any future change that breaks
// drop-in generation, idempotence, or the daemon-reload call will fail
// here without requiring root access or a live systemd instance.
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

// TestEnsureDropin runs test-ensure-dropin.sh as a subprocess.
// The script exercises:
//   - Both drop-in files are created on the first run.
//   - timeout.conf has [Service] + TimeoutStopSec=900.
//   - gomemlimit.conf has [Service] + Environment="GOMEMLIMIT=5368709120".
//   - A second run is idempotent: file content is identical.
//   - daemon-reload is called on every run, even when nothing changes.
//   - Static checks: ensure-dropin.sh, install-validator.sh, and
//     join-network.sh all reference ensure-dropin.sh correctly.
//
// Skipped when bash is not available (e.g. Windows CI runners).
func TestEnsureDropin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-ensure-dropin.sh requires bash; skipping on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestEnsureDropin")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "test-ensure-dropin.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-ensure-dropin.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Errorf("test-ensure-dropin.sh reported failures: %v", err)
	}
}
