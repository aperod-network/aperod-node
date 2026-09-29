package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const shutdownIntentMarkerName = "shutdown-intent"

// shutdownIntentMarkerPath returns the short-lived marker checked by the
// external watchdog while this process is saving its shutdown snapshot.
func shutdownIntentMarkerPath(dataDir string) string {
	return filepath.Join(dataDir, shutdownIntentMarkerName)
}

// writeShutdownIntentMarker atomically publishes this process's PID, Linux
// process start-time tick (to detect PID reuse), and Unix creation time.
func writeShutdownIntentMarker(dataDir string) error {
	startID, err := currentProcessStartID()
	if err != nil {
		return err
	}
	body := fmt.Sprintf("%d %s %d\n", os.Getpid(), startID, time.Now().Unix())
	tmp, err := os.CreateTemp(dataDir, shutdownIntentMarkerName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create shutdown intent marker: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write shutdown intent marker: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync shutdown intent marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close shutdown intent marker: %w", err)
	}
	if err := os.Rename(tmpName, shutdownIntentMarkerPath(dataDir)); err != nil {
		return fmt.Errorf("publish shutdown intent marker: %w", err)
	}
	return nil
}

// removeShutdownIntentMarker consumes an intent left by a previous process.
// Missing markers are expected on first startup.
func removeShutdownIntentMarker(dataDir string) error {
	err := os.Remove(shutdownIntentMarkerPath(dataDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// currentProcessStartID reads field 22 of /proc/<pid>/stat. The command name
// is parenthesized and may contain spaces, so parse after its final ')'.
func currentProcessStartID() (string, error) {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return "", fmt.Errorf("read process start identifier: %w", err)
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 || closeParen+1 >= len(stat) {
		return "", fmt.Errorf("parse process start identifier: malformed /proc/self/stat")
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	// The first field after ')' is field 3 (state); field 22 is index 19.
	if len(fields) <= 19 {
		return "", fmt.Errorf("parse process start identifier: incomplete /proc/self/stat")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", fmt.Errorf("parse process start identifier: %w", err)
	}
	return fields[19], nil
}
