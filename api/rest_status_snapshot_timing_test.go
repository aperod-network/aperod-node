package api_test

// rest_status_snapshot_timing_test.go — coverage for the snapshot_save_duration_ms
// field in GET /api/v1/status.
//
// Scenarios:
//   1. SetSnapshotTimings with a non-zero duration → snapshot_save_duration_ms appears.
//   2. No call to SetSnapshotTimings             → snapshot_save_duration_ms is absent.
//   3. SetSnapshotTimingsFromPreviousShutdown     → field present + from_previous flag set.
//   4. snapshot_ratio_pct is computed correctly  → ratio matches duration/timeout.

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// getStatus is a local helper (mirrors statusGet in rest_status_whitelist_test.go
// but avoids a redeclaration by using a different name).
func getStatus(t *testing.T, srv interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var resp map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	return rr.Code, resp
}

// TestStatus_SnapshotSaveDurationMS_PresentAfterSetSnapshotTimings confirms that
// snapshot_save_duration_ms is non-zero in /api/v1/status after
// SetSnapshotTimings is called — the primary assertion for this task.
func TestStatus_SnapshotSaveDurationMS_PresentAfterSetSnapshotTimings(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetSnapshotTimings(1234*time.Millisecond, 0)

	code, resp := getStatus(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	raw, ok := resp["snapshot_save_duration_ms"]
	if !ok {
		t.Fatal("snapshot_save_duration_ms missing from /api/v1/status after SetSnapshotTimings")
	}
	ms, ok := raw.(float64)
	if !ok {
		t.Fatalf("snapshot_save_duration_ms is not a number, got %T", raw)
	}
	if ms <= 0 {
		t.Errorf("snapshot_save_duration_ms = %v, want > 0", ms)
	}
}

// TestStatus_SnapshotSaveDurationMS_AbsentWithoutSetSnapshotTimings confirms that
// snapshot_save_duration_ms is absent when SetSnapshotTimings has never been called.
func TestStatus_SnapshotSaveDurationMS_AbsentWithoutSetSnapshotTimings(t *testing.T) {
	srv, _ := newTestServer(t)
	// Deliberately do NOT call SetSnapshotTimings.

	code, resp := getStatus(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, present := resp["snapshot_save_duration_ms"]; present {
		t.Error("snapshot_save_duration_ms should be absent before any snapshot timing is set")
	}
}

// TestStatus_SnapshotSaveDurationMS_FromPreviousShutdown confirms that
// SetSnapshotTimingsFromPreviousShutdown also makes snapshot_save_duration_ms
// appear and sets the snapshot_timing_from_previous_shutdown flag.
func TestStatus_SnapshotSaveDurationMS_FromPreviousShutdown(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetSnapshotTimingsFromPreviousShutdown(5678*time.Millisecond, 300)

	code, resp := getStatus(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, ok := resp["snapshot_save_duration_ms"]; !ok {
		t.Fatal("snapshot_save_duration_ms missing after SetSnapshotTimingsFromPreviousShutdown")
	}
	fromPrev, _ := resp["snapshot_timing_from_previous_shutdown"].(bool)
	if !fromPrev {
		t.Error("snapshot_timing_from_previous_shutdown should be true after SetSnapshotTimingsFromPreviousShutdown")
	}
}

// TestStatus_SnapshotRatioPct_ComputedCorrectly verifies that
// snapshot_ratio_pct equals duration/timeout*100, rounded to one decimal place.
// 60 000 ms / 300 s = 0.2 → 20.0 %
func TestStatus_SnapshotRatioPct_ComputedCorrectly(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetSnapshotTimings(60*time.Second, 300) // 60 s save, 300 s timeout → 20 %

	code, resp := getStatus(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	raw, ok := resp["snapshot_ratio_pct"]
	if !ok {
		t.Fatal("snapshot_ratio_pct missing when both duration and timeout are set")
	}
	pct, ok := raw.(float64)
	if !ok {
		t.Fatalf("snapshot_ratio_pct is not a number, got %T", raw)
	}
	want := 20.0
	if math.Abs(pct-want) > 0.05 {
		t.Errorf("snapshot_ratio_pct = %.2f, want %.2f", pct, want)
	}
}

// TestStatus_SnapshotTimeoutSec_PresentWithTimeout confirms that
// snapshot_timeout_sec is included in the response when a non-zero
// timeoutSec is supplied to SetSnapshotTimings.
func TestStatus_SnapshotTimeoutSec_PresentWithTimeout(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetSnapshotTimings(1*time.Second, 300)

	_, resp := getStatus(t, srv)
	raw, ok := resp["snapshot_timeout_sec"]
	if !ok {
		t.Fatal("snapshot_timeout_sec missing when timeout is set")
	}
	sec, _ := raw.(float64)
	if sec != 300 {
		t.Errorf("snapshot_timeout_sec = %v, want 300", sec)
	}
}
