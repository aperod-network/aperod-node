package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type failedMetricsWriter struct {
	headers http.Header
	writes  int
}

func (w *failedMetricsWriter) Header() http.Header { return w.headers }
func (*failedMetricsWriter) WriteHeader(int)       {}
func (w *failedMetricsWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, io.ErrClosedPipe
}

func TestMetricsWriteFailureStopsResponse(t *testing.T) {
	server, _ := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	control := httptest.NewRecorder()
	server.ServeHTTP(control, request)
	if control.Code != http.StatusOK {
		t.Fatalf("valid metrics request returned %d", control.Code)
	}
	body := control.Body.String()
	if strings.Count(body, "# TYPE ") != 15 || !strings.HasSuffix(body, "aperod_up 1\n") {
		t.Fatalf("valid metrics response lost its metrics or completion marker: %q", body)
	}

	failed := &failedMetricsWriter{headers: make(http.Header)}
	server.ServeHTTP(failed, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if failed.writes != 1 {
		t.Fatalf("attempted %d writes after the first connection failure", failed.writes)
	}
	if failed.Header().Get("Content-Type") != control.Header().Get("Content-Type") {
		t.Fatal("the failed writer did not reach the metrics handler")
	}
}
