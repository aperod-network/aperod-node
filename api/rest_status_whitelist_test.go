package api_test

// Tests for the peer_whitelist field in GET /api/v1/status.
//
// Scenarios:
//   1. SetPeerWhitelist with entries → peer_whitelist appears in the response.
//   2. No whitelist configured      → peer_whitelist is absent from the response.
//   3. Live whitelistGetFn wired    → live list is preferred over the startup snapshot.
//   4. Live fn returns empty slice  → peer_whitelist is absent (empty is treated as "no whitelist").

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// statusGet performs GET /api/v1/status and returns the decoded JSON body.
func statusGet(t *testing.T, srv interface {
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

// TestStatus_WithWhitelist confirms that peer_whitelist appears in
// /api/v1/status when SetPeerWhitelist is called with one or more entries.
func TestStatus_WithWhitelist(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetPeerWhitelist([]string{"192.168.1.0/24", "10.0.0.5"})

	code, resp := statusGet(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	raw, ok := resp["peer_whitelist"]
	if !ok {
		t.Fatal("peer_whitelist missing from /api/v1/status when whitelist is configured")
	}
	entries, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("peer_whitelist is not an array, got %T", raw)
	}
	if len(entries) != 2 {
		t.Errorf("peer_whitelist len = %d, want 2", len(entries))
	}
	want := map[string]bool{"192.168.1.0/24": true, "10.0.0.5": true}
	for _, e := range entries {
		s, _ := e.(string)
		if !want[s] {
			t.Errorf("unexpected whitelist entry %q", s)
		}
	}
}

// TestStatus_WithoutWhitelist confirms that peer_whitelist is absent from
// /api/v1/status when no whitelist has been configured.
func TestStatus_WithoutWhitelist(t *testing.T) {
	srv, _ := newTestServer(t)
	// Do NOT call SetPeerWhitelist.

	code, resp := statusGet(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, present := resp["peer_whitelist"]; present {
		t.Error("peer_whitelist should be absent when no whitelist is configured, but it appeared")
	}
}

// TestStatus_EmptyWhitelist confirms that peer_whitelist is absent when
// SetPeerWhitelist is called with an empty slice (same semantics as "none").
func TestStatus_EmptyWhitelist(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetPeerWhitelist([]string{})

	code, resp := statusGet(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, present := resp["peer_whitelist"]; present {
		t.Error("peer_whitelist should be absent for empty slice, but it appeared")
	}
}

// TestStatus_LiveWhitelistFnPreferred confirms that when whitelistGetFn is
// wired the live list is used instead of the startup snapshot set via
// SetPeerWhitelist.
func TestStatus_LiveWhitelistFnPreferred(t *testing.T) {
	srv, _ := newTestServer(t)
	// Startup snapshot has one entry …
	srv.SetPeerWhitelist([]string{"10.0.0.1"})
	// … but the live P2P layer reports a different, extended list.
	liveList := []string{"10.0.0.1", "172.16.0.0/12"}
	srv.SetWhitelistGetFunc(func() []string { return liveList })

	code, resp := statusGet(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	raw, ok := resp["peer_whitelist"]
	if !ok {
		t.Fatal("peer_whitelist missing from response when live fn is wired")
	}
	entries, _ := raw.([]interface{})
	if len(entries) != 2 {
		t.Errorf("expected 2 entries from live fn, got %d", len(entries))
	}
}

// TestStatus_LiveWhitelistFnEmpty confirms that peer_whitelist is absent
// when the live whitelistGetFn returns an empty slice even if SetPeerWhitelist
// was called with entries (live fn always wins).
func TestStatus_LiveWhitelistFnEmpty(t *testing.T) {
	srv, _ := newTestServer(t)
	// Startup snapshot has entries …
	srv.SetPeerWhitelist([]string{"10.0.0.1"})
	// … but live P2P layer reports an empty whitelist.
	srv.SetWhitelistGetFunc(func() []string { return []string{} })

	code, resp := statusGet(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, present := resp["peer_whitelist"]; present {
		t.Error("peer_whitelist should be absent when live fn returns empty slice")
	}
}

// TestStatus_BaseFieldsAlwaysPresent confirms that the mandatory ok/height/syncing
// fields are always returned regardless of whitelist configuration.
func TestStatus_BaseFieldsAlwaysPresent(t *testing.T) {
	srv, _ := newTestServer(t)

	code, resp := statusGet(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, field := range []string{"ok", "height", "syncing"} {
		if _, ok := resp[field]; !ok {
			t.Errorf("mandatory field %q missing from /api/v1/status", field)
		}
	}
}
