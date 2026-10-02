package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHistoricalRetirementSafety(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 is required for historical retirement safety checks")
	}
	script, err := filepath.Abs("test-historical-retirement.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("historical retirement safety test not found: %v", err)
	}
	cmd := exec.Command("python3", script)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("historical retirement safety checks failed: %v\n%s", err, output)
	}
}