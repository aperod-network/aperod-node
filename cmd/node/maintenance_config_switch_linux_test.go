//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This helper executes the real dispatcher, not a copy of the argument parser.
// The parent puts ONLY the helper's /etc in a private mount namespace.
func TestMaintenanceConfigSwitchHelper(t *testing.T) {
	if os.Getenv("APEROD_MAINTENANCE_CONFIG_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"aperod-node"}, os.Args[i+1:]...)
			if err := run(); err != nil {
				fmt.Fprintf(os.Stderr, "aperod-node: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	t.Fatal("maintenance helper requires -- followed by node arguments")
}

func TestMaintenanceConfigAutoSwitchOpensSelectedChain(t *testing.T) {
	// A dedicated Linux CI job can make unavailable isolation a hard failure
	// instead of reporting a skipped integration test as complete coverage.
	unavailable := func(format string, args ...any) {
		t.Helper()
		if os.Getenv("APEROD_REQUIRE_MAINTENANCE_NAMESPACE") == "1" {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		unavailable("mount-namespace integration test requires unshare: %v", err)
	}
	if _, err := exec.LookPath("mount"); err != nil {
		unavailable("mount-namespace integration test requires mount: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, unshare, "--user", "--map-root-user", "--mount",
		"sh", "-eu", "-c", "mount --make-rprivate /")
	if output, err := probe.CombinedOutput(); err != nil {
		unavailable("private user/mount namespaces unavailable: %v\n%s", err, output)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name           string
		mode           string
		explicit       bool
		configFirst    bool
		missingSystem  bool
		corruptSystem  bool
		requireDataDir bool
	}{
		{name: "repair without config opens system chain"},
		{name: "reset-tip without config opens system chain", mode: "--reset-tip"},
		{name: "key-image rebuild without config opens system chain", mode: "--rebuild-key-images"},
		{name: "spent-index rebuild without config opens system chain", mode: "--rebuild-spent-index"},
		{name: "explicit config before repair wins", explicit: true, configFirst: true},
		{name: "explicit config after repair ignores broken system config", explicit: true, corruptSystem: true},
		{name: "missing system config refuses local fallback", missingSystem: true},
		{name: "compact refuses implicit data directory despite system config", mode: "--compact-db", requireDataDir: true},
		{name: "height-index repair refuses implicit data directory despite system config", mode: "--repair-height-index", requireDataDir: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fakeEtc := filepath.Join(root, "etc")
			localData := filepath.Join(root, "local-chain")
			systemData := filepath.Join(root, "system-chain")
			explicitData := filepath.Join(root, "explicit-chain")
			before := make(map[string]map[string][32]byte)
			for i, dataDir := range []string{localData, systemData, explicitData} {
				createMaintenanceTestDB(t, dataDir, uint64((i+1)*100))
				// Stop at the read-only stale-target preflight, after the real
				// LevelDB tip has been read but before RecoverFile or networking.
				marker := filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-20000.json.gz", snapVersion))
				if err := os.WriteFile(marker, []byte("stale-target marker"), 0600); err != nil {
					t.Fatal(err)
				}
				before[dataDir] = maintenanceDBState(t, dataDir)
			}
			writeConfig := func(path, dataDir string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				content := fmt.Sprintf("network: testnet\ndata_dir: %q\n", dataDir)
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// A valid local default makes removal of the auto-switch guard
			// observable: it would inspect tip 100 instead of the system tip 200.
			writeConfig(filepath.Join(root, "config", "testnet.yaml"), localData)
			explicitConfig := filepath.Join(root, "explicit.yaml")
			writeConfig(explicitConfig, explicitData)
			if err := os.MkdirAll(fakeEtc, 0700); err != nil {
				t.Fatal(err)
			}
			if !tc.missingSystem {
				systemConfig := filepath.Join(fakeEtc, "aperod", "node.yaml")
				writeConfig(systemConfig, systemData)
				if tc.corruptSystem {
					if err := os.WriteFile(systemConfig, []byte("invalid: [yaml"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}

			mode := tc.mode
			if mode == "" {
				mode = "--repair-db"
			}
			nodeArgs := []string{mode}
			if tc.explicit {
				if tc.configFirst {
					nodeArgs = []string{"--config", explicitConfig, mode}
				} else {
					nodeArgs = append(nodeArgs, "--config", explicitConfig)
				}
			}
			// All mounts are private and disappear with the child. Never write
			// the host's /etc/aperod/node.yaml, even when it already exists.
			script := `mount --make-rprivate /
mount --bind "$1" /etc
shift
exec "$@"`
			args := []string{"--user", "--map-root-user", "--mount", "sh", "-eu", "-c",
				script, "maintenance-config-test", fakeEtc, executable,
				"-test.run=^TestMaintenanceConfigSwitchHelper$", "--"}
			args = append(args, nodeArgs...)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, unshare, args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "APEROD_MAINTENANCE_CONFIG_HELPER=1", "APEROD_DATA_DIR=")
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("maintenance subprocess timed out: %v\n%s", ctx.Err(), output)
			}
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				t.Fatalf("expected read-only maintenance refusal (exit 1), got %v\n%s", err, output)
			}
			text := string(output)
			warned := strings.Contains(text, "WARNING: --config was not supplied; defaulting to /etc/aperod/node.yaml")
			if warned != (!tc.explicit && !tc.missingSystem && !tc.requireDataDir) {
				t.Fatalf("unexpected auto-switch warning (present=%v):\n%s", warned, text)
			}
			if tc.requireDataDir {
				if !strings.Contains(text, mode+" requires --data-dir=<path>") ||
					strings.Contains(text, "MAINTENANCE PREFLIGHT") {
					t.Fatalf("early maintenance command did not refuse an implicit target:\n%s", text)
				}
			} else if tc.missingSystem {
				if !strings.Contains(text, "MAINTENANCE REFUSED: --config was not supplied") ||
					strings.Contains(text, "MAINTENANCE PREFLIGHT") {
					t.Fatalf("missing system config did not refuse before opening a chain:\n%s", text)
				}
			} else {
				wantConfig, wantData, wantHeight := "/etc/aperod/node.yaml", systemData, 200
				if tc.explicit {
					wantConfig, wantData, wantHeight = explicitConfig, explicitData, 300
				}
				for _, want := range []string{
					"resolved_config: " + wantConfig,
					"absolute_data_dir: " + wantData,
					fmt.Sprintf("tip_height: %d\n", wantHeight),
					"appears stale or internally inconsistent",
				} {
					if !strings.Contains(text, want) {
						t.Errorf("selected-chain evidence missing %q:\n%s", want, text)
					}
				}
				if strings.Contains(text, "absolute_data_dir: "+localData) {
					t.Errorf("maintenance selected the local default chain:\n%s", text)
				}
			}
			for dataDir, state := range before {
				if after := maintenanceDBState(t, dataDir); !reflect.DeepEqual(after, state) {
					t.Errorf("maintenance changed fixture database %s before refusing", dataDir)
				}
			}
		})
	}
}
