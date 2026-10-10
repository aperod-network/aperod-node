// Package deploy_test contains a CI guard that shells out to
// test-backup.sh and fails the Go test suite when the script reports any
// failure.
//
// Run from the blockchain root:
//
//	go test ./deploy/...
//
// The test is skipped automatically when bash or python3 is not available
// (e.g. on Windows CI runners or stripped Docker images).
package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBackupSh runs the legacy regression suite.
func TestBackupSh(t *testing.T) {
	runBackupShellTest(t, "test-backup.sh")
}

// TestBackupLogicalSh runs the closed-checkpoint and remote-verification suite.
func TestBackupLogicalSh(t *testing.T) {
	runBackupShellTest(t, "test-backup-logical.sh")
}

func runBackupShellTest(t *testing.T, scriptName string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skipf("%s requires bash; skipping on Windows", scriptName)
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found in PATH; skipping %s", scriptName)
	}

	// Locate the shell suite relative to this test file's directory.
	// os.Getwd() during `go test ./deploy/...` is the package directory.
	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, scriptName)

	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("%s not found at %s", scriptName, scriptPath)
	}

	command := []string{"bash", scriptPath}
	if scriptName == "test-backup.sh" && os.Geteuid() != 0 {
		unsharePath, err := exec.LookPath("unshare")
		if err != nil {
			t.Skipf("rootless %s requires unshare for its root-only checkpoint fixture", scriptName)
		}
		probe := exec.Command(unsharePath, "--user", "--map-root-user", "true")
		if err := probe.Run(); err != nil {
			t.Skipf("user namespaces are unavailable for root-only %s fixture: %v", scriptName, err)
		}
		command = []string{unsharePath, "--user", "--map-root-user", "bash", scriptPath}
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	toolsDir := t.TempDir()
	verifierPath := filepath.Join(toolsDir, "aperod-backup-verify")
	verifier := `#!/usr/bin/env python3
import json, pathlib, sys
if len(sys.argv) != 3 or sys.argv[1] not in ("--stage", "--legacy-stage"):
    raise SystemExit("expected --stage or --legacy-stage directory")
stage = pathlib.Path(sys.argv[2])
if sys.argv[1] == "--legacy-stage":
    if not (stage / "testnet" / "chain.db" / "CURRENT").is_file():
        raise SystemExit("invalid extracted legacy chain fixture")
    if not (stage / "explorer_db.dump").is_file():
        raise SystemExit("invalid extracted legacy dump fixture")
    print(json.dumps({
        "success": True,
        "legacy": True,
        "tip_height": 42,
        "tip_hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    }))
else:
    with (stage / "manifest.json").open(encoding="utf-8") as source:
        manifest = json.load(source)
    if not (stage / "chain.db" / "CURRENT").is_file() or manifest.get("success") is not True:
        raise SystemExit("invalid extracted backup fixture")
    print(json.dumps({
        "success": True,
        "tip_height": manifest["tip_height"],
        "tip_hash": manifest["tip_hash"],
    }))
`
	if err := os.WriteFile(verifierPath, []byte(verifier), 0o755); err != nil {
		t.Fatalf("cannot create test backup verifier: %v", err)
	}
	pgRestorePath := filepath.Join(toolsDir, "pg_restore")
	pgRestore := `#!/bin/sh
[ "$#" -eq 3 ] && [ "$1" = "--file" ] && [ "$2" = "/dev/null" ] && [ -f "$3" ]
`
	if err := os.WriteFile(pgRestorePath, []byte(pgRestore), 0o755); err != nil {
		t.Fatalf("cannot create test pg_restore: %v", err)
	}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "APEROD_BACKUP_VERIFY_BIN=") ||
			strings.HasPrefix(entry, "APEROD_ROLLOUT_CLEANUP_BIN=") ||
			strings.HasPrefix(entry, "PATH=") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env,
		"APEROD_BACKUP_VERIFY_BIN="+verifierPath,
		"APEROD_ROLLOUT_CLEANUP_BIN="+filepath.Join(toolsDir, "rollout-cleanup-helper-not-installed"),
		"PATH="+toolsDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	if err := cmd.Run(); err != nil {
		t.Errorf("%s reported failures: %v", scriptName, err)
	}
}
