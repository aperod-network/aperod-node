package deploy_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aperod/aperod/p2p"
)

// Test the actual production heredocs in all join paths, not a copy of their
// Python implementation. The complete push flow is covered by MockSSH too.
func TestJoinNetworkWhitelist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("join scripts require bash")
	}
	cmd := exec.Command("bash", "test-join-network-whitelist.sh")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("production join whitelist writers: %v\n%s", err, output)
	}
}

// A YAML-only assertion misses the authoritative persisted whitelist. Run the
// production writer, then load its result through a real local Host twice.
func TestJoinNetworkWhitelistEffectiveAfterStartAndRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("join scripts require bash")
	}
	source, err := os.ReadFile("join-network.sh")
	if err != nil {
		t.Fatal(err)
	}
	marker := `ssh "root@${TARGET_IP}" python3 - "${SECONDARY_NODE_YAML}" "${PRIMARY_IP}" "${SECONDARY_DATA_DIR}" <<'PY'` + "\n"
	_, rest, found := strings.Cut(string(source), marker)
	if !found {
		t.Fatal("production push whitelist writer not found")
	}
	writer, _, found := strings.Cut(rest, "\nPY\n")
	if !found {
		t.Fatal("production whitelist heredoc has no terminator")
	}
	const validator = "198.51.100.8"
	const existing = "203.0.113.20"
	for _, variant := range []string{"fresh", "existing-default", "existing-custom"} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			sidecar := filepath.Join(dir, "p2p_whitelist.json")
			p2pConfig := map[string]any{"peer_whitelist": []string{validator}}
			if variant == "existing-custom" {
				sidecar = filepath.Join(dir, "operator-peers.json")
				p2pConfig["whitelist_file"] = sidecar
			}
			want := []string{validator}
			if variant != "fresh" {
				want = []string{existing, validator}
				if err := os.WriteFile(sidecar, []byte(`["`+existing+`"]`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			body, err := json.Marshal(map[string]any{"data_dir": dir, "p2p": p2pConfig})
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(dir, "node.yaml")
			if err := os.WriteFile(config, body, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("python3", "-", config, validator, dir)
			cmd.Stdin = strings.NewReader(writer)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("production writer failed: %v\n%s", err, out)
			}
			for boot := 0; boot < 2; boot++ {
				func() {
					host := p2p.NewHost(p2p.Config{
						ListenAddr: "127.0.0.1:0", MaxPeers: 2,
						NodeID: "join-whitelist-test", WhitelistFile: sidecar,
						PeerWhitelist: []string{validator},
					}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
					if err := host.Start(); err != nil {
						t.Fatalf("boot %d: %v", boot, err)
					}
					defer host.Stop()
					got := host.GetPeerWhitelist()
					if strings.Join(got, ",") != strings.Join(want, ",") {
						t.Fatalf("boot %d: effective whitelist %v, want %v", boot, got, want)
					}
				}()
			}
		})
	}
}

func TestJoinWhitelistMissingConfigDoesNotTouchData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("join scripts require bash")
	}
	for _, script := range []string{"join-network.sh", "aperod-join.sh"} {
		t.Run(script, func(t *testing.T) {
			dir := t.TempDir()
			data := filepath.Join(dir, "data")
			if err := os.MkdirAll(filepath.Join(data, "chain.db"), 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(data, "chain.db", "CURRENT")
			if err := os.WriteFile(marker, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			missing := filepath.Join(dir, "missing.yaml")
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(dir, "external-calls")
			for _, name := range []string{"ssh", "curl", "rsync", "systemctl"} {
				if err := os.WriteFile(filepath.Join(bin, name),
					[]byte("#!/bin/sh\nprintf called >> \"$JOIN_EXTERNAL_CALLS\"\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{script, "--bootstrap-from=192.0.2.10"}
			if script == "aperod-join.sh" {
				args = []string{script, "192.0.2.10:8545", "--tunnel", "root@192.0.2.10", "--data-dir", data, "--skip-start"}
			}
			cmd := exec.Command("bash", args...)
			cmd.Env = append(os.Environ(), "LOCAL_NODE_YAML="+missing,
				"APEROD_NODE_YAML="+missing, "SECONDARY_DATA_DIR="+data,
				"JOIN_EXTERNAL_CALLS="+calls, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "peer_whitelist") ||
				!strings.Contains(string(out), "install-node.sh") {
				t.Fatalf("missing config must fail with install guidance: %v\n%s", err, out)
			}
			body, err := os.ReadFile(marker)
			if err != nil || string(body) != "original" {
				t.Fatalf("existing chain data changed: %v %q", err, body)
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatalf("external operations attempted before rejecting missing config: %v", err)
			}
		})
	}
}
