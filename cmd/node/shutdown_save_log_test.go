package main

// Task #1625: a mempool Save() failure during graceful shutdown must be visible
// in the logs at WARN level.  Losing the mempool silently on shutdown makes it
// impossible to tell — from the journal alone — whether pending transactions
// were preserved across a restart.
//
// These tests exercise saveMempoolOnShutdown, the exact helper the SIGTERM /
// SIGINT handler in run() invokes.  A save is forced to fail by pointing it at a
// read-only directory (os.Rename into it returns EACCES); the test then asserts
// the failure was logged via slog at WARN level, captured with the JSON handler.

import (
	"bytes"
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
)

// logRecordLevel scans JSON log lines for an entry whose "msg" field equals
// wantMsg and returns its "level" field.  Returns ("", false) if not found.
func logRecordLevel(buf *bytes.Buffer, wantMsg string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var rec map[string]interface{}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if msg, ok := rec["msg"].(string); !ok || msg != wantMsg {
			continue
		}
		lvl, _ := rec["level"].(string)
		return lvl, true
	}
	return "", false
}

// TestSaveMempoolOnShutdown_LogsWarnOnFailure verifies that when Save() fails
// (non-writable data directory) the helper emits the shutdown-save-failure
// message at WARN level and includes an err field.
func TestSaveMempoolOnShutdown_LogsWarnOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0500 does not prevent writes, cannot force Save() failure")
	}

	// Create a directory and strip write permission so os.WriteFile /
	// os.Rename inside it fail with EACCES.
	dir := t.TempDir()
	roDir := filepath.Join(dir, "readonly")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatalf("mkdir readonly: %v", err)
	}
	// Restore permissions on cleanup so t.TempDir removal succeeds.
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	mempool := core.NewMempool(core.DefaultMempoolConfig(), discardLogger())

	// Sanity: confirm Save() actually fails against this directory.
	if err := mempool.Save(roDir); err == nil {
		t.Skip("Save() unexpectedly succeeded in a read-only dir on this platform; cannot exercise failure path")
	}

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	saveMempoolOnShutdown(mempool, roDir, log)

	const wantMsg = "shutdown: failed to save mempool"
	if !logContainsMsg(&buf, wantMsg) {
		t.Fatalf("expected log %q, got:\n%s", wantMsg, buf.String())
	}
	level, ok := logRecordLevel(&buf, wantMsg)
	if !ok {
		t.Fatalf("failure log record not found; full output:\n%s", buf.String())
	}
	if level != "WARN" {
		t.Errorf("save-failure logged at level %q, want WARN; full output:\n%s", level, buf.String())
	}

	// The success message must NOT be present when Save() failed.
	if logContainsMsg(&buf, "shutdown: mempool saved") {
		t.Errorf("unexpected success log emitted on Save() failure:\n%s", buf.String())
	}
}

// TestSaveMempoolOnShutdown_LogsInfoOnSuccess verifies the happy path: a
// writable directory yields the INFO-level success message and no WARN.
func TestSaveMempoolOnShutdown_LogsInfoOnSuccess(t *testing.T) {
	dir := t.TempDir()
	mempool := core.NewMempool(core.DefaultMempoolConfig(), discardLogger())

	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	saveMempoolOnShutdown(mempool, dir, log)

	const wantMsg = "shutdown: mempool saved"
	if !logContainsMsg(&buf, wantMsg) {
		t.Fatalf("expected log %q, got:\n%s", wantMsg, buf.String())
	}
	level, _ := logRecordLevel(&buf, wantMsg)
	if level != "INFO" {
		t.Errorf("success logged at level %q, want INFO", level)
	}
	if logContainsMsg(&buf, "shutdown: failed to save mempool") {
		t.Errorf("unexpected failure log on successful save:\n%s", buf.String())
	}
}
