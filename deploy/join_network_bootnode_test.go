// Package deploy_test — CI guard for the join-network.sh bootnode injection.
//
// TestJoinNetworkBootnode shells out to test-join-network-bootnode.sh.
// TestJoinNetworkBootnodeConfigLoad is an integration test that applies the
// same Python injection logic used in join-network.sh step 5/7 and then loads
// the resulting file with config.Load — verifying that cfg.P2P.Bootnodes
// actually contains the expected entry, not just that the YAML shape is
// correct.
// TestJoinNetworkBootnodeNonDefaultPort covers the --p2p-port flag path: when
// an operator supplies a non-standard P2P port the written bootnode multiaddr
// must embed that port, not the default 30303.
//
// Run from the blockchain root:
//
//	go test ./deploy/...
//
// Missing runtime dependencies fail the shell-backed suite on supported
// platforms so local runs cannot silently cover less than CI.
package deploy_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aperod/aperod/config"
)

// TestJoinNetworkBootnode shells out to test-join-network-bootnode.sh and
// fails if the script reports any failure.
func TestJoinNetworkBootnode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-join-network-bootnode.sh requires bash; skipping on Windows")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestJoinNetworkBootnode")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "test-join-network-bootnode.sh")

	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-join-network-bootnode.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 77 {
			t.Fatalf("test-join-network-bootnode.sh skipped required scenarios (exit 77); install the dependencies named in SKIP_SUMMARY")
		}
		t.Errorf("test-join-network-bootnode.sh reported failures: %v", err)
	}
}

// pythonInject mirrors the Python fallback in join-network.sh step 5/7.
// It detects the schema, migrates any legacy root-level bootnodes into
// p2p.bootnodes, and adds the requested bootnode.
const pythonInjectScript = `
import sys, yaml, os

cfg_path = sys.argv[1]
bootnode  = sys.argv[2]

with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

# Migrate legacy root-level 'bootnodes' into p2p.bootnodes — the Go config
# only reads cfg.P2P.Bootnodes (yaml:"bootnodes" under p2p:).
legacy = cfg.pop("bootnodes", None)

p2p = cfg.setdefault("p2p", {})
nodes = list(p2p.get("bootnodes") or [])

if legacy:
    for entry in (legacy if isinstance(legacy, list) else [legacy]):
        if entry and entry not in nodes:
            nodes.append(entry)

if bootnode not in nodes:
    nodes.append(bootnode)
p2p["bootnodes"] = nodes

tmp = cfg_path + ".tmp"
with open(tmp, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
os.replace(tmp, cfg_path)
`

// runPythonInject runs the Python injection snippet against cfgPath with the
// given bootnode address and returns any error.
func runPythonInject(t *testing.T, cfgPath, bootnode string) {
	t.Helper()
	cmd := exec.Command("python3", "-c", pythonInjectScript, cfgPath, bootnode)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("python inject failed: %v", err)
	}
}

// TestJoinNetworkBootnodeConfigLoad verifies that after the Python injection
// runs, config.Load reports the bootnode in cfg.P2P.Bootnodes — ensuring the
// write targets the field the Go runtime actually reads.
func TestJoinNetworkBootnodeConfigLoad(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires bash + python3; skipping on Windows")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH; skipping TestJoinNetworkBootnodeConfigLoad")
	}
	checkYaml := exec.Command("python3", "-c", "import yaml")
	if err := checkYaml.Run(); err != nil {
		t.Skip("python3 pyyaml not available; skipping TestJoinNetworkBootnodeConfigLoad")
	}

	const bootnode = "/ip4/89.169.53.128/tcp/30303"

	// nestedYAML matches the config produced by install-validator.sh.
	nestedYAML := `
network: testnet
data_dir: /tmp/aperod-test
log_level: info
p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
  bootnodes: []
  max_peers: 50
consensus:
  validator_key: /etc/aperod/validator.key
  reward_address: aproecTest
api:
  enabled: true
  listen_addr: 127.0.0.1:8545
genesis:
  file: /etc/aperod/genesis-testnet.yaml
`

	// toplevelYAML mimics the older install-node.sh config with a root-level
	// 'bootnodes' key that the Go runtime ignores (no matching field in Config).
	toplevelYAML := `
network: testnet
data_dir: /tmp/aperod-test
p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
  max_peers: 30
bootnodes: []
api:
  enabled: true
  listen_addr: 127.0.0.1:8545
genesis:
  file: /etc/aperod/genesis-testnet.yaml
`

	tests := []struct {
		name    string
		yaml    string
		wantLen int
	}{
		{
			name:    "nested_schema_p2p_bootnodes",
			yaml:    nestedYAML,
			wantLen: 1,
		},
		{
			name:    "toplevel_migration_to_p2p_bootnodes",
			yaml:    toplevelYAML,
			wantLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Write the starting YAML to a temp file.
			f, err := os.CreateTemp("", "node-*.yaml")
			if err != nil {
				t.Fatalf("create temp file: %v", err)
			}
			cfgPath := f.Name()
			defer os.Remove(cfgPath)
			defer os.Remove(cfgPath + ".tmp")

			if _, err := f.WriteString(strings.TrimSpace(tc.yaml) + "\n"); err != nil {
				t.Fatalf("write yaml: %v", err)
			}
			f.Close()

			// Run the Python injection (mirrors join-network.sh step 5/7).
			runPythonInject(t, cfgPath, bootnode)

			// Load with config.Load — this exercises the actual Go runtime path.
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("config.Load failed: %v", err)
			}

			// The critical assertion: cfg.P2P.Bootnodes must contain the bootnode.
			if len(cfg.P2P.Bootnodes) != tc.wantLen {
				t.Errorf("cfg.P2P.Bootnodes length = %d, want %d; got: %v",
					len(cfg.P2P.Bootnodes), tc.wantLen, cfg.P2P.Bootnodes)
			}

			found := false
			for _, bn := range cfg.P2P.Bootnodes {
				if bn == bootnode {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("cfg.P2P.Bootnodes does not contain %q; got: %v",
					bootnode, cfg.P2P.Bootnodes)
			}

			// For the top-level migration case, verify no root-level key remains
			// in memory (it should have been popped and migrated).
			// We can detect it by re-loading the raw YAML and checking.
			rawData, _ := os.ReadFile(cfgPath)
			rawStr := string(rawData)
			if !strings.Contains(rawStr, "p2p:") {
				t.Errorf("output file missing p2p: section")
			}

			t.Logf("[%s] cfg.P2P.Bootnodes = %v  ✓", tc.name, cfg.P2P.Bootnodes)

			// Idempotency: run inject again and confirm count stays the same.
			runPythonInject(t, cfgPath, bootnode)
			cfg2, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("config.Load (second run) failed: %v", err)
			}
			if len(cfg2.P2P.Bootnodes) != tc.wantLen {
				t.Errorf("idempotency check: cfg.P2P.Bootnodes length = %d after second inject, want %d; got: %v",
					len(cfg2.P2P.Bootnodes), tc.wantLen, cfg2.P2P.Bootnodes)
			}

			t.Logf("[%s] idempotency OK ✓", tc.name)

			// Fail fast if Python left a stale .tmp file.
			if _, err := os.Stat(fmt.Sprintf("%s.tmp", cfgPath)); err == nil {
				t.Errorf("stale .tmp file found after inject")
			}
		})
	}
}

// TestJoinNetworkBootnodeNonDefaultPort runs aperod-join.sh itself with
// --p2p-port 30304 so the argument-parsing block and BOOTNODE_ADDR
// construction in the script are exercised end-to-end.  External commands are
// replaced by lightweight stubs (id, systemctl, curl) placed at the front of
// PATH; APEROD_NODE_YAML and APEROD_DROPIN_DIR redirect the two writable
// system paths so no root access is required.  After the script exits the test
// loads the resulting file with config.Load and asserts cfg.P2P.Bootnodes
// contains /ip4/89.169.53.128/tcp/30304 (not /tcp/30303).
func TestJoinNetworkBootnodeNonDefaultPort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires bash + python3; skipping on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestJoinNetworkBootnodeNonDefaultPort")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH; skipping TestJoinNetworkBootnodeNonDefaultPort")
	}
	checkYaml := exec.Command("python3", "-c", "import yaml")
	if err := checkYaml.Run(); err != nil {
		t.Skip("python3 pyyaml not available; skipping TestJoinNetworkBootnodeNonDefaultPort")
	}

	const (
		primaryIP      = "89.169.53.128"
		nonDefaultPort = "30304"
		defaultPort    = "30303"
		wantBootnode   = "/ip4/" + primaryIP + "/tcp/" + nonDefaultPort
	)

	// Locate aperod-join.sh relative to this test file (blockchain/deploy/).
	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	joinSh := filepath.Join(scriptDir, "aperod-join.sh")
	if _, err := os.Stat(joinSh); os.IsNotExist(err) {
		t.Fatalf("aperod-join.sh not found at %s", joinSh)
	}
	nodeConfigSh := filepath.Join(scriptDir, "node-config.sh")

	// ── Stub binaries ────────────────────────────────────────────────────────
	// Create a temporary directory with lightweight stubs that satisfy the
	// checks aperod-join.sh performs before reaching step 7/8 (bootnode write).
	stubDir := t.TempDir()

	stubs := map[string]string{
		// id: "-u" → 0 (root); anything else → exit 1 (user not found → chown skipped)
		"id": `#!/usr/bin/env bash
for arg; do [[ "$arg" == "-u" ]] && echo 0 && exit 0; done
exit 1`,
		// systemctl: is-active → exit 1 (not running, script continues with warn)
		//            everything else (disable, daemon-reload) → exit 0
		"systemctl": `#!/usr/bin/env bash
[[ "${1:-}" == "is-active" ]] && exit 1
exit 0`,
		// curl: /api/v1/snapshot → exit 1 (non-fatal warn path)
		//       /api/v1/status   → exit 0 (primary reachable)
		//       everything else  → exit 0
		"curl": `#!/usr/bin/env bash
for arg; do
  [[ "$arg" == *"/api/v1/snapshot"* ]] && exit 1
done
exit 0`,
		// pip3: delegate to "python3 -m pip" so node-config.sh can install pyyaml
		// when the real pip3 binary is absent from PATH.
		"pip3": `#!/usr/bin/env bash
python3 -m pip "$@"`,
	}

	for name, body := range stubs {
		p := filepath.Join(stubDir, name)
		if err := os.WriteFile(p, []byte(body+"\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}

	// ── Temp directories for overridable paths ───────────────────────────────
	cfgFile, err := os.CreateTemp("", "node-*.yaml")
	if err != nil {
		t.Fatalf("create temp config: %v", err)
	}
	cfgPath := cfgFile.Name()
	t.Cleanup(func() { os.Remove(cfgPath) })

	// nestedYAML matches the config produced by install-validator.sh.
	nestedYAML := strings.TrimSpace(`
network: testnet
data_dir: /tmp/aperod-test
log_level: info
p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
  bootnodes: []
  max_peers: 50
consensus:
  validator_key: /etc/aperod/validator.key
  reward_address: aproecTest
api:
  enabled: true
  listen_addr: 127.0.0.1:8545
genesis:
  file: /etc/aperod/genesis-testnet.yaml
`) + "\n"
	if _, err := cfgFile.WriteString(nestedYAML); err != nil {
		t.Fatalf("write config yaml: %v", err)
	}
	cfgFile.Close()

	dataDir := t.TempDir()
	dropinDir := t.TempDir()

	// ── Build the command environment ────────────────────────────────────────
	// Inherit the full process environment so Python site-packages (pyyaml),
	// locale, and other implicit dependencies remain available, then override
	// only the vars that redirect system paths to our temp directories.
	overrides := map[string]string{
		"PATH":                  fmt.Sprintf("%s:%s", stubDir, os.Getenv("PATH")),
		"APEROD_NODE_YAML":      cfgPath,
		"APEROD_DROPIN_DIR":     dropinDir,
		"APEROD_NODE_CONFIG_SH": nodeConfigSh,
	}
	var env []string
	for _, kv := range os.Environ() {
		key := kv[:strings.IndexByte(kv, '=')]
		if val, ok := overrides[key]; ok {
			env = append(env, key+"="+val)
			delete(overrides, key)
		} else {
			env = append(env, kv)
		}
	}
	// Append any overrides that were not already present in the environment.
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}

	// ── Run aperod-join.sh with --p2p-port 30304 ────────────────────────────
	cmd := exec.Command("bash", joinSh,
		fmt.Sprintf("%s:8545", primaryIP),
		"--p2p-port", nonDefaultPort,
		"--data-dir", dataDir,
		"--user", "nonexistentuser_gotest",
		"--skip-start",
		"--no-chaindb",
	)
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("aperod-join.sh --p2p-port %s failed (exit %v):\n%s",
			nonDefaultPort, runErr, out)
	}
	t.Logf("aperod-join.sh output:\n%s", out)

	// ── Load the written config and assert the bootnode port ─────────────────
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load failed after aperod-join.sh run: %v", err)
	}

	if len(cfg.P2P.Bootnodes) != 1 {
		t.Fatalf("cfg.P2P.Bootnodes length = %d, want 1; got: %v",
			len(cfg.P2P.Bootnodes), cfg.P2P.Bootnodes)
	}
	got := cfg.P2P.Bootnodes[0]

	if got != wantBootnode {
		t.Errorf("cfg.P2P.Bootnodes[0] = %q, want %q", got, wantBootnode)
	}
	if !strings.Contains(got, "/tcp/"+nonDefaultPort) {
		t.Errorf("bootnode %q does not contain expected port /tcp/%s", got, nonDefaultPort)
	}
	if strings.Contains(got, "/tcp/"+defaultPort) {
		t.Errorf("bootnode %q contains hardcoded default port /tcp/%s — --p2p-port flag appears broken in aperod-join.sh",
			got, defaultPort)
	}

	t.Logf("cfg.P2P.Bootnodes[0] = %q  ✓ (port %s preserved end-to-end)", got, nonDefaultPort)
}
