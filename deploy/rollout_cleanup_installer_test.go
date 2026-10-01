package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRolloutCleanupHooksAndInstaller(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rollout cleanup hook test requires bash")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	script := filepath.Join(".", "test-rollout-cleanup-hooks.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("rollout cleanup hook test not found: %v", err)
	}
	cmd := exec.Command("bash", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("rollout cleanup hook/installer test failed: %v", err)
	}
}

func TestRolloutCleanupCore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rollout cleanup safety tests require Unix file descriptors")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable; rollout cleanup safety coverage is incomplete")
	}
	script := filepath.Join(".", "test-rollout-cleanup.py")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("rollout cleanup safety test not found: %v", err)
	}
	cmd := exec.Command("python3", script)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("rollout cleanup safety test failed: %v", err)
	}
}
