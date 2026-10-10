package main

// rsync_sentinel_test.go — unit tests for checkRsyncSentinel.
//
// Validates that the node refuses to start when .rsync-in-progress is present
// in the data directory, and starts normally when it is absent.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckRsyncSentinel_Absent verifies that checkRsyncSentinel returns nil
// when no sentinel file exists — the normal (safe) startup path.
func TestCheckRsyncSentinel_Absent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := checkRsyncSentinel(dir); err != nil {
		t.Fatalf("expected nil when sentinel is absent, got: %v", err)
	}
}

// TestCheckRsyncSentinel_Present verifies that checkRsyncSentinel returns a
// non-nil error whose message names the sentinel file and explains how to
// unblock startup when the sentinel exists.
func TestCheckRsyncSentinel_Present(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sentinel := filepath.Join(dir, ".rsync-in-progress")

	if err := os.WriteFile(sentinel, []byte("1"), 0644); err != nil {
		t.Fatalf("setup: write sentinel: %v", err)
	}

	err := checkRsyncSentinel(dir)
	if err == nil {
		t.Fatal("expected non-nil error when sentinel is present, got nil")
	}

	msg := err.Error()

	// The error must name the sentinel file so operators can find it.
	if !strings.Contains(msg, ".rsync-in-progress") {
		t.Errorf("error message does not mention .rsync-in-progress; got: %s", msg)
	}

	// The error must contain the word "blocked" so log scanners that grep for
	// startup failures surface this as a blocked (not crashed) startup.
	if !strings.Contains(msg, "blocked") {
		t.Errorf("error message does not contain 'blocked'; got: %s", msg)
	}

	// The error must give operators an actionable way to unblock:
	// manual removal of the sentinel file.
	if !strings.Contains(msg, "rm ") {
		t.Errorf("error message does not include 'rm' (removal instruction); got: %s", msg)
	}
}

// TestRsyncSentinelPath verifies the sentinel lives directly inside the data
// directory (not inside chain.db/ or any subdirectory), so that an rsync of
// chain.db/ alone does not copy the sentinel from the source node.
func TestRsyncSentinelPath(t *testing.T) {
	t.Parallel()

	dir := "/opt/aperod/data/testnet"
	want := "/opt/aperod/data/testnet/.rsync-in-progress"

	if got := rsyncSentinelPath(dir); got != want {
		t.Errorf("rsyncSentinelPath(%q) = %q, want %q", dir, got, want)
	}
}

// TestCheckRsyncSentinel_SentinelRemoved verifies that after the sentinel is
// removed checkRsyncSentinel returns nil again, confirming the node would be
// allowed to start after join-network.sh completes successfully.
func TestCheckRsyncSentinel_SentinelRemoved(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sentinel := filepath.Join(dir, ".rsync-in-progress")

	// Write sentinel (simulating join-network.sh before rsync).
	if err := os.WriteFile(sentinel, []byte("1"), 0644); err != nil {
		t.Fatalf("setup: write sentinel: %v", err)
	}

	// Confirm the sentinel blocks startup.
	if err := checkRsyncSentinel(dir); err == nil {
		t.Fatal("expected error while sentinel is present, got nil")
	}

	// Remove sentinel (simulating join-network.sh after successful rsync).
	if err := os.Remove(sentinel); err != nil {
		t.Fatalf("setup: remove sentinel: %v", err)
	}

	// Now the node should be allowed to start.
	if err := checkRsyncSentinel(dir); err != nil {
		t.Fatalf("expected nil after sentinel removed, got: %v", err)
	}
}
