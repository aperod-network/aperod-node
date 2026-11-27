package deploy_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignedCheckpointScanExceptionsRemainExact(t *testing.T) {
	scanner, err := exec.LookPath("gitleaks")
	if err != nil {
		t.Skip("gitleaks unavailable; exact-exception scanner coverage not exercised")
	}
	config, err := os.ReadFile("../.gitleaks.toml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "deploy"), 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, ".gitleaks.toml")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mainnet-avm-checkpoint-4105317.json", "mainnet-bootstrap-4105317.json"} {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "deploy", name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	scan := func(wantFindings bool) {
		t.Helper()
		cmd := exec.Command(scanner, "dir", dir, "--config", configPath, "--redact=100", "--no-banner")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
		output, err := cmd.CombinedOutput()
		if wantFindings {
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				t.Fatalf("expected findings exit 1, got %v: %s", err, output)
			}
		} else if err != nil {
			t.Fatalf("public checkpoint metadata must scan cleanly: %v: %s", err, output)
		}
	}
	scan(false)
	bootstrap, err := os.ReadFile("mainnet-bootstrap-4105317.json")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Statement struct {
			AnchorHash string `json:"key_image_anchor_sha256"`
		} `json:"statement"`
	}
	if err := json.Unmarshal(bootstrap, &decoded); err != nil || len(decoded.Statement.AnchorHash) != 64 {
		t.Fatalf("invalid bootstrap digest fixture: %v", err)
	}
	// An unrelated path must not inherit an exception for the same public value.
	outside := filepath.Join(dir, "other.json")
	if err := os.WriteFile(outside, bootstrap, 0600); err != nil {
		t.Fatal(err)
	}
	scan(true)
	if err := os.Remove(outside); err != nil {
		t.Fatal(err)
	}
	// A different value at the approved path must still be detected.
	hash := decoded.Statement.AnchorHash
	replacement := "a"
	if hash[0] == 'a' {
		replacement = "b"
	}
	mutated := strings.Replace(string(bootstrap), hash, replacement+hash[1:], 1)
	if err := os.WriteFile(filepath.Join(dir, "deploy", "mainnet-bootstrap-4105317.json"), []byte(mutated), 0600); err != nil {
		t.Fatal(err)
	}
	scan(true)
}
