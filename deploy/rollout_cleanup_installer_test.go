package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

func TestRolloutCleanupHooksStandaloneLayout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rollout cleanup hook test requires Unix shell tools")
	}
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is unavailable", tool)
		}
	}
	parent := t.TempDir()
	node := filepath.Join(parent, "node-source")
	deploy := filepath.Join(node, "deploy")
	if err := os.MkdirAll(deploy, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"test-rollout-cleanup-hooks.sh", "update-node.sh", "setup-rollout-cleanup.sh",
		"aperod-rollout-cleanup.service", "aperod-rollout-cleanup.timer",
"aperod-historical-retirement.service", "aperod-historical-retirement.timer",
		"rollout_cleanup/__init__.py", "rollout_cleanup/cli.py",
"rollout_cleanup/runtime.py", "rollout_cleanup/provider.py", "rollout_cleanup/retirement.py",
	} {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(deploy, name)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(node, "go.mod"), []byte("module example.org/node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(override string) ([]byte, error) {
		cmd := exec.Command("bash", filepath.Join(deploy, "test-rollout-cleanup-hooks.sh"))
		for _, variable := range os.Environ() {
			if !strings.HasPrefix(variable, "PROD=") {
				cmd.Env = append(cmd.Env, variable)
			}
		}
		cmd.Env = append(cmd.Env, "PROD="+override)
		return cmd.CombinedOutput()
	}
	output, err := run("")
	if err != nil {
		t.Fatalf("standalone node safety checks failed: %v\n%s", err, output)
	}
	for _, required := range []string{
		"NOT_APPLICABLE:", "update-node registers only its owned candidate",
		"update-node completes only after bounded advancing readiness",
		"cleanup service keeps host visibility", "installer defaults to dry-run",
		"All rollout cleanup hook/installer tests passed.",
	} {
		if !strings.Contains(string(output), required) {
			t.Fatalf("standalone layout omitted required check %q:\n%s", required, output)
		}
	}
	// Explicit script paths and workspace-owned scripts must not silently skip.
	output, err = run(filepath.Join(parent, "missing-rollout.sh"))
	if err == nil || !strings.Contains(string(output), "required verified rollout script is missing") {
		t.Fatalf("missing explicit rollout script was not refused: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(parent, "pnpm-workspace.yaml"), []byte("packages: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err = run("")
	if err == nil || !strings.Contains(string(output), "required verified rollout script is missing") {
		t.Fatalf("missing workspace rollout script was not refused: %v\n%s", err, output)
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
