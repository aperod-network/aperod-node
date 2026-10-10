package api_test

// Tests that /api/v1/network/stats and /metrics expose mempool depth, byte
// size and the evictions counter so operators can detect mempool floods.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestREST_NetworkStats_MempoolFields(t *testing.T) {
	srv, _ := buildChainServer(t, 0)
	code, resp := restGet(t, srv, "/api/v1/network/stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, field := range []string{"mempool_count", "mempool_bytes", "mempool_evictions_total"} {
		v, ok := resp[field]
		if !ok {
			t.Errorf("missing %q in network stats response", field)
			continue
		}
		if _, isNum := v.(float64); !isNum {
			t.Errorf("%q = %v (%T), want a number", field, v, v)
		}
	}
	// Fresh chain server: empty mempool, zero evictions.
	if resp["mempool_count"] != float64(0) {
		t.Errorf("mempool_count = %v, want 0", resp["mempool_count"])
	}
	if resp["mempool_bytes"] != float64(0) {
		t.Errorf("mempool_bytes = %v, want 0", resp["mempool_bytes"])
	}
	if resp["mempool_evictions_total"] != float64(0) {
		t.Errorf("mempool_evictions_total = %v, want 0", resp["mempool_evictions_total"])
	}
}

func TestMetrics_MempoolGauges(t *testing.T) {
	srv, _ := buildChainServer(t, 0)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	text := string(body)
	for _, metric := range []string{
		"aperod_mempool_size 0",
		"aperod_mempool_bytes 0",
		"aperod_mempool_evictions_total 0",
	} {
		if !strings.Contains(text, metric) {
			t.Errorf("/metrics output missing %q\n---\n%s", metric, text)
		}
	}
}
