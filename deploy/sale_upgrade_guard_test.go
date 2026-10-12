package deploy

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaleUpgradeGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires bash")
	}
	_, file, _, _ := runtime.Caller(0)
	cmd := exec.Command("bash", filepath.Join(filepath.Dir(file), "test-sale-upgrade-guard.sh"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sale upgrade guard: %v\n%s", err, output)
	}
	t.Log(string(output))
}
