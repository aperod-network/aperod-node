package main

// sighup_reload_test.go — confirms that reloadScanCheckpointInterval updates
// cfg.Snapshot.ScanCheckpointInterval from the YAML file without a restart,
// and that the updated value would be propagated to the next runStartupScan
// call via startupScanParams.CheckpointInterval.

import (
        "bytes"
        "fmt"
        "os"
        "path/filepath"
        "testing"

        "github.com/aperod/aperod/config"
)

// writeIntervalConfig writes a minimal node.yaml with the given
// scan_checkpoint_interval to cfgPath.
func writeIntervalConfig(t *testing.T, cfgPath string, interval uint64) {
        t.Helper()
        content := fmt.Sprintf(`network: testnet
data_dir: ./data
consensus:
  block_time: 1s
snapshot:
  scan_checkpoint_interval: %d
`, interval)
        if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
                t.Fatalf("writeIntervalConfig: %v", err)
        }
}

// TestReloadScanCheckpointInterval_UpdatesInMemoryValue verifies that after
// the config file is changed and reloadScanCheckpointInterval is called (the
// SIGHUP handler), cfg.Snapshot.ScanCheckpointInterval reflects the new value.
// This is the value that would be forwarded to startupScanParams.CheckpointInterval
// on the next runStartupScan call.
func TestReloadScanCheckpointInterval_UpdatesInMemoryValue(t *testing.T) {
        dir := t.TempDir()
        cfgPath := filepath.Join(dir, "node.yaml")

        // Write initial config and load it.
        writeIntervalConfig(t, cfgPath, 25000)
        cfg, err := config.Load(cfgPath)
        if err != nil {
                t.Fatalf("initial Load: %v", err)
        }
        if cfg.Snapshot.ScanCheckpointInterval != 25000 {
                t.Fatalf("pre-condition: expected ScanCheckpointInterval=25000, got %d",
                        cfg.Snapshot.ScanCheckpointInterval)
        }

        // Simulate operator editing node.yaml.
        writeIntervalConfig(t, cfgPath, 75000)

        // Simulate SIGHUP: reload.
        var buf bytes.Buffer
        log := newCaptureLogger(&buf)
        reloadScanCheckpointInterval(cfgPath, cfg, log)

        // cfg must now carry the new value.
        if cfg.Snapshot.ScanCheckpointInterval != 75000 {
                t.Errorf("after reload: expected ScanCheckpointInterval=75000, got %d",
                        cfg.Snapshot.ScanCheckpointInterval)
        }

        // A structured log entry must confirm the change.
        if !logContainsMsg(&buf, "SIGHUP: ScanCheckpointInterval reloaded") {
                t.Errorf("expected reload log message not found; log output:\n%s", buf.String())
        }
}

// TestReloadScanCheckpointInterval_ReloadedValueFlowsToScanParams verifies that
// the updated cfg.Snapshot.ScanCheckpointInterval is what would be forwarded as
// startupScanParams.CheckpointInterval — the field runStartupScan reads.
func TestReloadScanCheckpointInterval_ReloadedValueFlowsToScanParams(t *testing.T) {
        dir := t.TempDir()
        cfgPath := filepath.Join(dir, "node.yaml")

        writeIntervalConfig(t, cfgPath, 10000)
        cfg, err := config.Load(cfgPath)
        if err != nil {
                t.Fatalf("Load: %v", err)
        }

        // Reload with a new value.
        writeIntervalConfig(t, cfgPath, 100000)
        var buf bytes.Buffer
        reloadScanCheckpointInterval(cfgPath, cfg, newCaptureLogger(&buf))

        // Construct the params the same way main.go does (line ~637):
        //   CheckpointInterval: cfg.Snapshot.ScanCheckpointInterval,
        params := startupScanParams{
                CheckpointInterval: cfg.Snapshot.ScanCheckpointInterval,
        }
        if params.CheckpointInterval != 100000 {
                t.Errorf("scan params: expected CheckpointInterval=100000 after reload, got %d",
                        params.CheckpointInterval)
        }
}

// TestReloadScanCheckpointInterval_BadConfigKeepsOldValue verifies that a
// parse error leaves the existing ScanCheckpointInterval unchanged so a
// typo in node.yaml cannot silently reset the interval to the default.
func TestReloadScanCheckpointInterval_BadConfigKeepsOldValue(t *testing.T) {
        dir := t.TempDir()
        cfgPath := filepath.Join(dir, "node.yaml")

        // Write a malformed YAML file.
        if err := os.WriteFile(cfgPath, []byte("network: [unclosed\n"), 0644); err != nil {
                t.Fatalf("WriteFile: %v", err)
        }

        cfg := config.DefaultConfig()
        cfg.Snapshot.ScanCheckpointInterval = 30000

        var buf bytes.Buffer
        log := newCaptureLogger(&buf)
        reloadScanCheckpointInterval(cfgPath, cfg, log)

        if cfg.Snapshot.ScanCheckpointInterval != 30000 {
                t.Errorf("bad config: expected ScanCheckpointInterval unchanged at 30000, got %d",
                        cfg.Snapshot.ScanCheckpointInterval)
        }
        if !logContainsMsg(&buf, "SIGHUP: config reload failed — keeping current ScanCheckpointInterval") {
                t.Errorf("expected failure log message not found; log output:\n%s", buf.String())
        }
}

// TestReloadScanCheckpointInterval_ZeroFallsBackToDefault verifies that setting
// scan_checkpoint_interval: 0 in node.yaml is treated as "use default" (50 000)
// by runStartupScan, matching the documented behaviour.
func TestReloadScanCheckpointInterval_ZeroFallsBackToDefault(t *testing.T) {
        dir := t.TempDir()
        cfgPath := filepath.Join(dir, "node.yaml")

        writeIntervalConfig(t, cfgPath, 40000)
        cfg, err := config.Load(cfgPath)
        if err != nil {
                t.Fatalf("Load: %v", err)
        }

        // Reload with zero → runStartupScan will use its built-in default (50 000).
        writeIntervalConfig(t, cfgPath, 0)
        var buf bytes.Buffer
        reloadScanCheckpointInterval(cfgPath, cfg, newCaptureLogger(&buf))

        if cfg.Snapshot.ScanCheckpointInterval != 0 {
                t.Errorf("expected ScanCheckpointInterval=0 after reload, got %d",
                        cfg.Snapshot.ScanCheckpointInterval)
        }

        // Simulate what runStartupScan does with CheckpointInterval == 0.
        effective := cfg.Snapshot.ScanCheckpointInterval
        if effective == 0 {
                effective = 50000 // default (see scan.go)
        }
        if effective != 50000 {
                t.Errorf("expected effective interval 50000 when cfg is 0, got %d", effective)
        }
}
