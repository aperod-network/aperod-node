package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// These are host-service tests, not consensus or production rehearsals.
// Opt-in requires actual complete coverage: unavailable Docker is a failure,
// never a successful skipped subprocess.
func runNodeLifecycle(t *testing.T, mode string) {
	t.Helper()
	if os.Getenv("APEROD_RUN_DOCKER_E2E") != "1" {
		t.Skip("Set APEROD_RUN_DOCKER_E2E=1 for the disposable CI systemd guest")
	}
	cmd := exec.Command("bash", filepath.Join(".", "test-node-service-lifecycle.sh"), mode)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("Incomplete or failed real host lifecycle (%s): %v", mode, err)
	}
}

func TestInstallValidatorLifecycleE2E(t *testing.T) {
	runNodeLifecycle(t, "validator")
}
