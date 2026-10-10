package api_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/aperod/aperod/api"
)

func TestStatusStartupSnapshotOutcome(t *testing.T) {
	tests := []struct {
		name    string
		reason  string
		err     error
		wantErr string
	}{
		{name: "loaded", reason: api.StartupSnapshotLoaded},
		{name: "absent", reason: api.StartupNoSnapshot},
		{
			name:    "corrupt",
			reason:  api.StartupCorruptSnapshot,
			err:     errors.New("checksum mismatch"),
			wantErr: "checksum mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			srv.SetStartupSnapshotOutcome(tt.reason, 1989, tt.err)

			code, resp := getStatus(t, srv)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if got := resp["snapshot_startup_reason"]; got != tt.reason {
				t.Errorf("snapshot_startup_reason = %v, want %q", got, tt.reason)
			}
			if got := resp["startup_reason"]; got != tt.reason {
				t.Errorf("startup_reason = %v, want %q", got, tt.reason)
			}
			if got := resp["snapshot_startup_tip_height"]; got != float64(1989) {
				t.Errorf("snapshot_startup_tip_height = %v, want 1989", got)
			}
			if got := resp["startup_tip_height"]; got != float64(1989) {
				t.Errorf("startup_tip_height = %v, want 1989", got)
			}
			if got := resp["startup_id"]; got == nil || got == "" {
				t.Error("startup_id is empty")
			}
			gotErr, hasErr := resp["snapshot_startup_error"]
			if tt.wantErr == "" {
				if hasErr {
					t.Errorf("snapshot_startup_error unexpectedly present: %v", gotErr)
				}
			} else if gotErr != tt.wantErr {
				t.Errorf("snapshot_startup_error = %v, want %q", gotErr, tt.wantErr)
			}
			canonicalErr, hasCanonicalErr := resp["startup_error"]
			if tt.wantErr == "" && hasCanonicalErr {
				t.Errorf("startup_error unexpectedly present: %v", canonicalErr)
			} else if tt.wantErr != "" && canonicalErr != tt.wantErr {
				t.Errorf("startup_error = %v, want %q", canonicalErr, tt.wantErr)
			}
		})
	}
}

func TestStatusStartupSnapshotOutcomeReplacesError(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetStartupSnapshotOutcome(api.StartupCorruptSnapshot, 100, errors.New("truncated gzip"))
	srv.SetStartupSnapshotOutcome(api.StartupSnapshotLoaded, 101, nil)

	_, resp := getStatus(t, srv)
	if got := resp["snapshot_startup_reason"]; got != api.StartupSnapshotLoaded {
		t.Errorf("snapshot_startup_reason = %v, want %q", got, api.StartupSnapshotLoaded)
	}
	if _, ok := resp["snapshot_startup_error"]; ok {
		t.Error("snapshot_startup_error retained from previous outcome")
	}
}
