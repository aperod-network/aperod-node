package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestShutdownIntentMarkerRoundTripAndConsume(t *testing.T) {
	dir := t.TempDir()
	marker := shutdownIntentMarkerPath(dir)

	if err := writeShutdownIntentMarker(dir); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	fields := strings.Fields(string(mustReadFile(t, marker)))
	if len(fields) != 3 {
		t.Fatalf("marker has %d fields, want 3: %q", len(fields), fields)
	}
	if fields[0] != strconv.Itoa(os.Getpid()) {
		t.Errorf("marker PID = %q, want %d", fields[0], os.Getpid())
	}
	startID, err := currentProcessStartID()
	if err != nil {
		t.Fatalf("read current process start ID: %v", err)
	}
	if fields[1] != startID {
		t.Errorf("marker start ID = %q, want %q", fields[1], startID)
	}
	if _, err := strconv.ParseInt(fields[2], 10, 64); err != nil {
		t.Errorf("marker timestamp is not an integer: %q", fields[2])
	}

	if err := removeShutdownIntentMarker(dir); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker remains after consume (stat err: %v)", err)
	}
	if err := removeShutdownIntentMarker(dir); err != nil {
		t.Errorf("removing absent marker should be harmless: %v", err)
	}
}

func TestShutdownIntentMarkerIsNotPublishedOnMissingDirectory(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "missing")
	if err := writeShutdownIntentMarker(missingDir); err == nil {
		t.Fatal("expected marker write to fail for missing directory")
	}
	if _, err := os.Stat(shutdownIntentMarkerPath(missingDir)); !os.IsNotExist(err) {
		t.Fatalf("marker unexpectedly exists (stat err: %v)", err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
