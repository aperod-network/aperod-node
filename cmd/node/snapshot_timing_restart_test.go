package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

func fetchSnapshotTimingStatus(t *testing.T, client *http.Client, url string) map[string]interface{} {
	t.Helper()

	resp, err := client.Get(url + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/v1/status status=%d, want %d: %s", resp.StatusCode, http.StatusOK, body)
	}

	var status map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode /api/v1/status response: %v", err)
	}
	return status
}

func statusNumber(t *testing.T, status map[string]interface{}, field string) float64 {
	t.Helper()
	value, ok := status[field].(float64)
	if !ok {
		t.Fatalf("%s = %T (%v), want number", field, status[field], status[field])
	}
	return value
}

// TestSnapshotTimingFromPreviousShutdownOnRestart exercises the previous-run
// metadata round-trip and the API behavior observed immediately after restart:
// persisted timing is visible and marked as previous-run data until this
// process completes its first snapshot.
func TestSnapshotTimingFromPreviousShutdownOnRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chain.db")
	const (
		previousSaveMs = int64(57_000)
		timeoutSec     = 300.0
	)

	// Simulate the prior process persisting the successful shutdown snapshot.
	priorDB, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open prior-run DB: %v", err)
	}
	if err := priorDB.StoreSnapshotSaveDuration(previousSaveMs); err != nil {
		_ = priorDB.Close()
		t.Fatalf("persist prior snapshot duration: %v", err)
	}
	if err := priorDB.StoreSnapshotTimeoutSec(timeoutSec); err != nil {
		_ = priorDB.Close()
		t.Fatalf("persist prior snapshot timeout: %v", err)
	}
	if err := priorDB.Close(); err != nil {
		t.Fatalf("close prior-run DB: %v", err)
	}

	// Opening a fresh handle represents the new node process after restart.
	restartedDB, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open restarted DB: %v", err)
	}
	defer restartedDB.Close()

	savedMs, found, err := restartedDB.LoadSnapshotSaveDuration()
	if err != nil {
		t.Fatalf("load prior snapshot duration: %v", err)
	}
	if !found || savedMs != previousSaveMs {
		t.Fatalf("loaded duration=(%d, found=%t), want (%d, true)", savedMs, found, previousSaveMs)
	}
	savedTimeout, timeoutFound, err := restartedDB.LoadSnapshotTimeoutSec()
	if err != nil {
		t.Fatalf("load prior snapshot timeout: %v", err)
	}
	if !timeoutFound || savedTimeout != timeoutSec {
		t.Fatalf("loaded timeout=(%v, found=%t), want (%v, true)", savedTimeout, timeoutFound, timeoutSec)
	}

	apiServer := api.NewServer(
		":0",
		core.NewChain(),
		core.NewMempool(core.DefaultMempoolConfig()),
		core.NewUTXOSet(),
		slog.Default(),
	)
	apiServer.SetStore(restartedDB)
	apiServer.SetSnapshotTimingsFromPreviousShutdown(
		time.Duration(savedMs)*time.Millisecond,
		savedTimeout,
	)
	httpServer := httptest.NewServer(apiServer)
	defer httpServer.Close()

	client := httpServer.Client()
	status := fetchSnapshotTimingStatus(t, client, httpServer.URL)
	if got, ok := status["snapshot_timing_from_previous_shutdown"].(bool); !ok || !got {
		t.Fatalf("snapshot_timing_from_previous_shutdown = %v, want true", status["snapshot_timing_from_previous_shutdown"])
	}
	if got := statusNumber(t, status, "snapshot_save_duration_ms"); got != float64(previousSaveMs) {
		t.Fatalf("snapshot_save_duration_ms = %v, want %d", got, previousSaveMs)
	}
	if got := statusNumber(t, status, "snapshot_timeout_sec"); got != timeoutSec {
		t.Fatalf("snapshot_timeout_sec = %v, want %v", got, timeoutSec)
	}
	if got := statusNumber(t, status, "snapshot_ratio_pct"); got != 19.0 {
		t.Fatalf("snapshot_ratio_pct = %v, want 19.0", got)
	}

	// The first successful snapshot in this process replaces the restored
	// values and clears the previous-shutdown marker in the live status API.
	const currentSaveDuration = 12_345 * time.Millisecond
	apiServer.SetSnapshotTimings(currentSaveDuration, timeoutSec)
	status = fetchSnapshotTimingStatus(t, client, httpServer.URL)
	if _, present := status["snapshot_timing_from_previous_shutdown"]; present {
		t.Fatalf("snapshot_timing_from_previous_shutdown should be omitted after first in-process snapshot, got %v", status["snapshot_timing_from_previous_shutdown"])
	}
	if got := statusNumber(t, status, "snapshot_save_duration_ms"); got != float64(currentSaveDuration.Milliseconds()) {
		t.Fatalf("snapshot_save_duration_ms after first snapshot = %v, want %d", got, currentSaveDuration.Milliseconds())
	}
	if got := statusNumber(t, status, "snapshot_timeout_sec"); got != timeoutSec {
		t.Fatalf("snapshot_timeout_sec after first snapshot = %v, want %v", got, timeoutSec)
	}
}
