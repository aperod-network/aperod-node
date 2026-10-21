package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSaleReadinessIsPrivateAndClosed(t *testing.T) {
	s := &Server{}
	s.mux = http.NewServeMux()
	s.registerRESTRoutes()
	path := "/api/v1/admin/protocol-sales/readiness"
	for _, tc := range []struct {
		remote, host string
		status       int
	}{
		{"203.0.113.9:1234", "localhost", http.StatusForbidden},
		{"127.0.0.1:1234", "localhost", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+path, nil)
		req.RemoteAddr = tc.remote
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("private readiness status %d, expected %d", w.Code, tc.status)
		}
	}
	w := httptest.NewRecorder()
	s.restSaleReadiness(w, httptest.NewRequest(http.MethodGet, path, nil))
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result["code"] != "SALE_NOT_ACTIVATED" || result["ready"] != false {
		t.Fatal(result)
	}
}

func TestSaleEndpointsFailClosedBeforeConfiguration(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{"GET", "/api/v1/admin/protocol-sales/status", s.restSaleStatus},
		{"POST", "/api/v1/admin/protocol-sales/reserve", s.restSaleReserve},
		{"POST", "/api/v1/admin/protocol-sales/paid", s.restSalePaid},
		{"POST", "/api/v1/admin/protocol-sales/invoice", s.restSaleInvoice},
		{"POST", "/api/v1/admin/protocol-sales/recover", s.restSaleRecover},
		{"GET", "/api/v1/admin/protocol-sales/receipt?order=" + strings.Repeat("a", 64), s.restSaleReceipt},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		tc.handler(w, r)
		if w.Code != 503 {
			t.Fatalf("%s gave %d before configuration", tc.path, w.Code)
		}
		if !strings.Contains(w.Body.String(), "AUTHORIZED_APRO_SETTLEMENT_UNAVAILABLE") {
			t.Fatal("silent fallback")
		}
	}
}

func TestSaleExactRequestDecoderRejectsNumbersAndTrailingData(t *testing.T) {
	for _, raw := range []string{`{"amount_napro":1}`, `{"amount_napro":"1"} {}`, `{"amount_napro":"1","unknown":true}`} {
		var input struct {
			Amount string `json:"amount_napro"`
		}
		if saleBody(httptest.NewRequest("POST", "/", strings.NewReader(raw)), &input) == nil {
			t.Fatalf("ambiguous request accepted: %s", raw)
		}
	}
}
