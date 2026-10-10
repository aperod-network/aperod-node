package api_test

// TestRegisterRoutes_NoPanic verifies that calling registerRoutes() (exercised
// via NewServer) does not panic due to duplicate ServeMux registrations.
// Go's http.ServeMux panics at registration time when the same pattern is
// registered twice — this test catches that class of bug before it reaches
// production and takes down the node.
//
// The test also confirms that each key endpoint returns a non-404 status so we
// know the route was actually registered and dispatched correctly.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRegisterRoutes_NoPanic constructs a Server and asserts that
// registerRoutes() does not panic.  Because NewServer calls registerRoutes()
// internally, a panic here means a duplicate route was registered.
func TestRegisterRoutes_NoPanic(t *testing.T) {
	// Should not panic — use a defer/recover to turn a panic into a test failure
	// with a useful message instead of a raw goroutine dump.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewServer panicked (likely duplicate route registration): %v", r)
		}
	}()

	srv, _ := newTestServer(t)
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
}

// TestRegisterRoutes_KeyEndpointsReachable verifies that the routes registered
// by registerRoutes() and registerRESTRoutes() are actually reachable — i.e.
// each well-known path returns something other than 404.
func TestRegisterRoutes_KeyEndpointsReachable(t *testing.T) {
	srv, _ := newTestServer(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/health"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/api/v1/blocks"},
		{http.MethodGet, "/api/v1/network/stats"},
		{http.MethodGet, "/api/v1/status"},
		{http.MethodGet, "/api/v1/fee-estimate"},
		{http.MethodGet, "/api/v1/validators"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code == http.StatusNotFound {
				t.Errorf("%s %s returned 404 — route may not be registered", ep.method, ep.path)
			}
		})
	}
}
