package api_test

// Tests for GET /api/v1/admin/utxo-audit — the local-only endpoint exposing
// the latest background UTXO-store integrity audit result.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
)

// restGetLocal sends a GET with a loopback Host so localOnly() passes.
func restGetLocal(t *testing.T, srv *api.Server, path string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var resp map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	return rr.Code, resp
}

// TestRESTUTXOAudit_PendingBeforeFirstCycle — before any audit cycle has
// completed the endpoint returns status=pending.
func TestRESTUTXOAudit_PendingBeforeFirstCycle(t *testing.T) {
	srv, _ := buildChainServer(t, 1)
	code, resp := restGetLocal(t, srv, "/api/v1/admin/utxo-audit")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp["status"] != "pending" {
		t.Fatalf("status field = %v, want pending", resp["status"])
	}
}

// TestRESTUTXOAudit_ReturnsStoredResult — after SetUTXOAuditResult the
// endpoint returns status=ok with the audit payload.
func TestRESTUTXOAudit_ReturnsStoredResult(t *testing.T) {
	srv, _ := buildChainServer(t, 1)
	srv.SetUTXOAuditResult(&api.UTXOAuditResult{
		CompletedAt:          time.Now(),
		DurationMs:           42,
		TipHeight:            7,
		SampledChecked:       100,
		RecentBlocksChecked:  50,
		RecentOutputsChecked: 60,
		Mismatches:           2,
		Skipped:              1,
		MismatchDetails: []api.UTXOAuditMismatch{
			{TxHash: "aa", OutputIndex: 0, Height: 3, StoreCommit: "bb", BlockCommit: "cc"},
		},
	})
	code, resp := restGetLocal(t, srv, "/api/v1/admin/utxo-audit")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp["status"] != "ok" {
		t.Fatalf("status field = %v, want ok", resp["status"])
	}
	audit, ok := resp["audit"].(map[string]interface{})
	if !ok {
		t.Fatalf("audit field missing or wrong type: %v", resp["audit"])
	}
	if audit["mismatches"].(float64) != 2 {
		t.Fatalf("mismatches = %v, want 2", audit["mismatches"])
	}
	if audit["sampled_checked"].(float64) != 100 {
		t.Fatalf("sampled_checked = %v, want 100", audit["sampled_checked"])
	}
	details, ok := audit["mismatch_details"].([]interface{})
	if !ok || len(details) != 1 {
		t.Fatalf("mismatch_details = %v, want 1 entry", audit["mismatch_details"])
	}
}

// TestRESTUTXOAudit_LocalOnly — a non-loopback Host header is rejected by the
// localOnly guard.
func TestRESTUTXOAudit_LocalOnly(t *testing.T) {
	srv, _ := buildChainServer(t, 1)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/utxo-audit", nil)
	req.Host = "attacker.example.com"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

// TestRESTUTXOAudit_APIKeyRequired — when the node has an API key configured,
// a request without X-API-Key gets 401 and a request with the key succeeds
// (this is what the Node.js api-server monitor sends via GO_NODE_API_KEY).
func TestRESTUTXOAudit_APIKeyRequired(t *testing.T) {
	srv, _ := buildChainServer(t, 1)
	srv.SetAPIKey("audit-key")
	srv.SetUTXOAuditResult(&api.UTXOAuditResult{Mismatches: 1})

	// Without the key → 401.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/utxo-audit", nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("without key: status = %d, want 401", rr.Code)
	}

	// With the key → 200 + audit payload.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/utxo-audit", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("X-API-Key", "audit-key")
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("with key: status = %d, want 200", rr.Code)
	}
	var resp map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Fatalf("with key: status field = %v, want ok", resp["status"])
	}
}

// TestRESTUTXOAudit_MethodNotAllowed — POST is rejected.
func TestRESTUTXOAudit_MethodNotAllowed(t *testing.T) {
	srv, _ := buildChainServer(t, 1)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/utxo-audit", nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}
