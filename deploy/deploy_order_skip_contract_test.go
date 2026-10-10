package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDeployOrderSkipContract drives the configured workflow entry point and
// confirms that incomplete local coverage cannot be reported as a full pass.
func TestDeployOrderSkipContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy/test-deploy-order.sh requires bash; skipping on Windows")
	}

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH; skipping TestDeployOrderSkipContract")
	}

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve deploy-order test source path")
	}
	scriptPath := filepath.Join(filepath.Dir(testFile), "test-deploy-order.sh")
	cmd := exec.Command(bashPath, scriptPath)
	cmd.Env = append(os.Environ(), "DEPLOY_TEST_FORCE_SKIP_T29=1")
	output, runErr := cmd.CombinedOutput()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 77 {
		t.Fatalf("configured deploy-order entry point must exit 77 on a skip, got %v\n%s", runErr, output)
	}
	for _, marker := range []string{
		"1 skipped",
		"SKIPPED SCENARIOS:",
		"T29: forced missing Go dependency",
		"SKIP_SUMMARY count=1",
	} {
		if !strings.Contains(string(output), marker) {
			t.Errorf("skip output missing %q:\n%s", marker, output)
		}
	}
}
