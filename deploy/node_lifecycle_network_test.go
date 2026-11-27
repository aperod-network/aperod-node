package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLifecycleUpdateFixture_UsesVerifiedTestnetWithoutBypassingReleaseGate(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "node.yaml")
	// Extract the actual replacement config used by update/upgrade, without
	// running the systemd guest or the rest of its privileged fixture.
	program := `
import ast, sys
from pathlib import Path
tree = ast.parse(Path(sys.argv[1]).read_text())
calls = [n for n in ast.walk(tree) if isinstance(n, ast.Call)
    and isinstance(n.func, ast.Attribute) and n.func.attr == "write_text"
    and isinstance(n.func.value, ast.Name) and n.func.value.id == "config"]
assert len(calls) == 1, "lifecycle replacement config must be unambiguous"
Path(sys.argv[2]).write_text(ast.literal_eval(calls[0].args[0]))
`
	extract := exec.Command("python3", "-c", program, "test-node-service-lifecycle.py", config)
	if out, err := extract.CombinedOutput(); err != nil {
		t.Fatalf("extract actual lifecycle config: %v\n%s", err, out)
	}
	fixture, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(fixture), "network: testnet\n") {
		t.Fatal("update fixture does not explicitly identify its synthetic network")
	}
	for _, tc := range []struct {
		name, content string
		pass          bool
	}{
		{"actual fixture", string(fixture), true},
		{"mainnet still requires approval", strings.Replace(string(fixture), "network: testnet", "network: mainnet", 1), false},
		{"missing network still rejected", strings.Replace(string(fixture), "network: testnet\n", "", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(config, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c",
				`. "$1"; mainnet_payout_release_gate "$2" "$3"`,
				"lifecycle-network-control", "mainnet-payout-release-gate.sh", config, filepath.Join(dir, "absent-mainnet.yaml"))
			// In particular, do not inherit an operator's approval marker.
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
			out, err := cmd.CombinedOutput()
			if tc.pass && err != nil {
				t.Fatalf("explicit testnet fixture rejected: %v\n%s", err, out)
			}
			if !tc.pass && (err == nil || !strings.Contains(string(out), "RELEASE BLOCKED:")) {
				t.Fatalf("release guard bypassed: %v\n%s", err, out)
			}
		})
	}
}
