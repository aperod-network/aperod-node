package api_test

import (
	"net/http"
	"testing"
	"time"
)

func TestStatus_NextRestartAtIsNullWhenSchedulerIsInactive(t *testing.T) {
	srv, _ := newTestServer(t)
	if err := srv.SetNightlyRestartSchedule(""); err != nil {
		t.Fatalf("SetNightlyRestartSchedule: %v", err)
	}

	code, resp := getStatus(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	value, present := resp["next_restart_at"]
	if !present {
		t.Fatal("next_restart_at field is missing")
	}
	if value != nil {
		t.Fatalf("next_restart_at = %#v, want null while scheduler is inactive", value)
	}
}

func TestStatus_NextRestartAtIsRFC3339UTC(t *testing.T) {
	srv, _ := newTestServer(t)
	if err := srv.SetNightlyRestartSchedule("04:00"); err != nil {
		t.Fatalf("SetNightlyRestartSchedule: %v", err)
	}

	before := time.Now().UTC()
	code, resp := getStatus(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	raw, ok := resp["next_restart_at"].(string)
	if !ok {
		t.Fatalf("next_restart_at = %#v, want RFC3339 string", resp["next_restart_at"])
	}
	next, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("next_restart_at %q is not RFC3339: %v", raw, err)
	}
	if next.Location() != time.UTC || raw != next.UTC().Format(time.RFC3339) {
		t.Fatalf("next_restart_at = %q, want canonical RFC3339 UTC ending in Z", raw)
	}
	if next.Hour() != 4 || next.Minute() != 0 || next.Second() != 0 {
		t.Fatalf("next_restart_at = %s, want the configured 04:00 UTC", next)
	}
	if !next.After(before) || next.After(before.Add(24*time.Hour)) {
		t.Fatalf("next_restart_at = %s, want the next occurrence within 24 hours of %s", next, before)
	}
}

func TestStatus_NextRestartAtRejectsInvalidSchedule(t *testing.T) {
	srv, _ := newTestServer(t)
	if err := srv.SetNightlyRestartSchedule("25:00"); err == nil {
		t.Fatal("SetNightlyRestartSchedule accepted an invalid time")
	}

	_, resp := getStatus(t, srv)
	if value, present := resp["next_restart_at"]; !present || value != nil {
		t.Fatalf("next_restart_at = %#v (present=%t), want null after rejected configuration", value, present)
	}
}
