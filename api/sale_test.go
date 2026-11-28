package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
	"github.com/aperod/aperod/store"
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

func TestSaleReadinessDistinguishesCheckpointAuthorityMismatch(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close sale readiness test database: %v", err)
		}
	})
	localKey, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_, checkpointPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := crypto.NewLockedValidatorKey(localKey.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(locked.Destroy)

	s := &Server{blockStore: db}
	s.SetValidatorKey(locked)
	s.SetSaleConfig(&store.SaleMigration{
		Checkpoint: sale.Checkpoint{SaleAuthority: checkpointPub},
	}, func() bool { return false }, func(uint64, crypto.Hash32) bool { return true })

	w := httptest.NewRecorder()
	s.restSaleReadiness(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/protocol-sales/readiness", nil))
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || result["code"] != "SALE_AUTHORITY_MISMATCH" || result["ready"] != false {
		t.Fatalf("unexpected readiness response: status=%d body=%s", w.Code, w.Body.String())
	}

	reserve := httptest.NewRecorder()
	s.restSaleReserve(reserve, httptest.NewRequest(http.MethodPost, "/api/v1/admin/protocol-sales/reserve", strings.NewReader("{}")))
	if reserve.Code != http.StatusServiceUnavailable || !strings.Contains(reserve.Body.String(), "AUTHORIZED_APRO_SETTLEMENT_UNAVAILABLE") {
		t.Fatalf("authority mismatch must keep reservation closed: status=%d body=%s", reserve.Code, reserve.Body.String())
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
