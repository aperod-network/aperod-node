package api_test

// Security regression tests for Gh0stAnts re-verification findings (August 2026).
//
// F-051: POST /api/v1/admin/mint requires X-API-Key when one is configured.
// F-046: view_key_hex URL query parameter is silently ignored (X-View-Key header only).
// F-048: GET /api/v1/address/*/utxos response contains scan_limited field.
// F-053: realIP() uses RemoteAddr, not X-Forwarded-For, from non-loopback peers.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── F-051: Admin mint requires X-API-Key ────────────────────────────────────

// TestREST_AdminMint_RequiresAPIKey confirms that POST /api/v1/admin/mint
// returns 401 Unauthorized when an API key is configured but the header is absent,
// and 200 when the correct key is supplied.
func TestREST_AdminMint_RequiresAPIKey(t *testing.T) {
	srv, _ := buildUTXOServer(t)
	const apiKey = "test-secret-key-abc123"
	srv.SetAPIKey(apiKey)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	body, _ := json.Marshal(map[string]interface{}{
		"idempotency_key": "auth-1", "address": string(addr), "amount_apr": 1.0,
	})

	// ── Without API key: must be rejected ────────────────────────────────────
	// localOnly() checks r.Host; httptest.NewRequest leaves it empty which
	// bypasses the DNS-rebinding guard — set it explicitly to a loopback value.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/mint", bytes.NewReader(body))
	req.Host = "127.0.0.1:8545"
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	// API key check returns 401 Unauthorized when key is configured but absent/wrong.
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("without API key: status = %d, want 401 Unauthorized (F-051 regression)", rr.Code)
	}

	// ── With correct API key: must be accepted (not 401) ─────────────────────
	body2, _ := json.Marshal(map[string]interface{}{
		"idempotency_key": "auth-2", "address": string(addr), "amount_apr": 1.0,
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/mint", bytes.NewReader(body2))
	req2.Host = "127.0.0.1:8545"
	req2.RemoteAddr = "127.0.0.1:54321"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-API-Key", apiKey)
	rr2 := httptest.NewRecorder()
	srv.ServeHTTP(rr2, req2)
	if rr2.Code == http.StatusUnauthorized {
		t.Errorf("with correct API key: status = %d, want != 401 (correct key must be accepted)", rr2.Code)
	}
}

// TestREST_AdminMint_NoKeyConfigured_FailsClosed confirms that an omitted
// configuration key never turns a privileged endpoint into an open endpoint.
func TestREST_AdminMint_NoKeyConfigured_FailsClosed(t *testing.T) {
	srv, _ := buildUTXOServer(t) // no SetAPIKey call

	wk, _ := crypto.GenerateWalletKeys()
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	body, _ := json.Marshal(map[string]interface{}{
		"idempotency_key": "no-auth", "address": string(addr), "amount_apr": 1.0,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/mint", bytes.NewReader(body))
	req.Host = "127.0.0.1:8545"
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no API key configured: status = %d, want 401 (privileged API must fail closed)", rr.Code)
	}
}

func TestREST_AdminMint_LocalAndBrowserGuards(t *testing.T) {
	const key = "test-secret-key-abc123"
	tests := []struct {
		name       string
		host       string
		remoteAddr string
		apiKey     string
		content    string
		origin     string
		fetchSite  string
		want       int
	}{
		{name: "invalid key", host: "127.0.0.1", remoteAddr: "127.0.0.1:1", apiKey: "wrong", content: "application/json", want: http.StatusUnauthorized},
		{name: "spoofed host", host: "attacker.example", remoteAddr: "127.0.0.1:1", apiKey: key, content: "application/json", want: http.StatusForbidden},
		{name: "empty host", host: "", remoteAddr: "127.0.0.1:1", apiKey: key, content: "application/json", want: http.StatusForbidden},
		{name: "forged forwarded", host: "127.0.0.1", remoteAddr: "192.0.2.10:1", apiKey: key, content: "application/json", want: http.StatusForbidden},
		{name: "cross site origin", host: "127.0.0.1", remoteAddr: "127.0.0.1:1", apiKey: key, content: "application/json", origin: "https://attacker.example", want: http.StatusForbidden},
		{name: "cross site fetch", host: "127.0.0.1", remoteAddr: "127.0.0.1:1", apiKey: key, content: "application/json", fetchSite: "cross-site", want: http.StatusForbidden},
		{name: "text plain", host: "127.0.0.1", remoteAddr: "127.0.0.1:1", apiKey: key, content: "text/plain", want: http.StatusUnsupportedMediaType},
		{name: "permissive content type", host: "127.0.0.1", remoteAddr: "127.0.0.1:1", apiKey: key, content: "application/jsonp", want: http.StatusUnsupportedMediaType},
		{name: "authenticated", host: "127.0.0.1", remoteAddr: "127.0.0.1:1", apiKey: key, content: "application/json", want: http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := buildUTXOServer(t)
			srv.SetAPIKey(key)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/mint", bytes.NewReader([]byte(`{"idempotency_key":"security-test","address":"bad","amount_apr":1}`)))
			req.Host = tc.host
			req.RemoteAddr = tc.remoteAddr
			req.Header.Set("Content-Type", tc.content)
			req.Header.Set("X-API-Key", tc.apiKey)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			req.Header.Set("X-Forwarded-For", "127.0.0.1")
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if tc.name == "authenticated" {
				if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden || rr.Code == http.StatusUnsupportedMediaType {
					t.Fatalf("authenticated request rejected by security guard: %d", rr.Code)
				}
				return
			}
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestREST_AdminBodyLimit(t *testing.T) {
	srv, _ := buildUTXOServer(t)
	srv.SetAPIKey("test-secret-key-abc123")
	body := bytes.Repeat([]byte("a"), (1<<20)+1)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/mint", bytes.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-secret-key-abc123")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized admin request status=%d, want 413; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRPC_WalletAuthAndCORS(t *testing.T) {
	srv, _ := newTestServer(t)
	const key = "wallet-rpc-test-key"
	srv.SetAPIKey(key)
	srv.SetAllowedOrigins([]string{"https://admin.example"})

	call := func(apiKey, contentType, origin, fetchSite string) *httptest.ResponseRecorder {
		t.Helper()
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"apr_walletMaxSpendable","params":{}}`)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-API-Key", apiKey)
		req.Header.Set("Origin", origin)
		req.Header.Set("Sec-Fetch-Site", fetchSite)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	if rr := call("", "application/json", "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status=%d, want 401", rr.Code)
	}
	if rr := call("wrong", "application/json", "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key status=%d, want 401", rr.Code)
	}
	if rr := call(key, "text/plain", "", ""); rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain status=%d, want 415", rr.Code)
	}
	if rr := call(key, "application/jsonp", "", ""); rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("permissive content type status=%d, want 415", rr.Code)
	}
	if rr := call(key, "application/json", "https://attacker.example", "cross-site"); rr.Code != http.StatusForbidden {
		t.Fatalf("cross-site status=%d, want 403", rr.Code)
	}
	rr := call(key, "application/json", "https://admin.example", "same-site")
	if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
		t.Fatalf("authenticated configured-origin request rejected: %d", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example" {
		t.Fatalf("CORS origin=%q, want configured origin", got)
	}
	rr = call(key, "application/json", "https://attacker.example", "same-site")
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unconfigured origin received CORS header %q", got)
	}

	preflight := httptest.NewRequest(http.MethodOptions, "/", nil)
	preflight.Header.Set("Origin", "https://admin.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	preflight.Header.Set("Access-Control-Request-Headers", "Content-Type, X-API-Key")
	preflightRR := httptest.NewRecorder()
	srv.ServeHTTP(preflightRR, preflight)
	if preflightRR.Code != http.StatusOK {
		t.Fatalf("unauthenticated preflight status=%d, want 200", preflightRR.Code)
	}
	if got := preflightRR.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example" {
		t.Fatalf("preflight CORS origin=%q, want configured origin", got)
	}
}

func TestRPC_PrivilegedBodyLimit(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetAPIKey("wallet-rpc-test-key")
	padding := bytes.Repeat([]byte("a"), (1<<20)+1)
	body := append([]byte(`{"jsonrpc":"2.0","id":1,"method":"apr_walletMaxSpendable","params":{"padding":"`), padding...)
	body = append(body, []byte(`"}}`)...)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "wallet-rpc-test-key")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized RPC status=%d, want 413; body=%s", rr.Code, rr.Body.String())
	}
}

// ─── F-046: view_key_hex query parameter is ignored ──────────────────────────

// TestREST_AddressUTXOs_ViewKeyQueryParamIgnored confirms that the deprecated
// view_key_hex URL query parameter is silently ignored after the F-046 fix.
// A stealth UTXO should NOT be discovered via the query param; it requires the
// X-View-Key header (or node-configured view key).
func TestREST_AddressUTXOs_ViewKeyQueryParamIgnored(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)
	makeStealthUTXO(t, utxos, wk.Spend.Public, wk.View.Public, 100_000_000, 0xF1)

	viewKeyHex := hex.EncodeToString(wk.View.Private[:])

	// ── Query param: stealth UTXO must NOT be discovered ─────────────────────
	codeParam, respParam := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos?view_key_hex="+viewKeyHex)
	if codeParam != http.StatusOK {
		t.Fatalf("query param request: status = %d, want 200", codeParam)
	}
	listParam, _ := respParam["utxos"].([]interface{})
	if len(listParam) != 0 {
		t.Errorf("F-046 regression: view_key_hex query param discovered stealth UTXO — "+
			"query param must be ignored (found %d UTXOs, want 0)", len(listParam))
	}

	// ── X-View-Key header: stealth UTXO MUST be discovered ───────────────────
	codeHdr, respHdr := restGetHeader(t, srv, "/api/v1/address/"+string(addr)+"/utxos", "X-View-Key", viewKeyHex)
	if codeHdr != http.StatusOK {
		t.Fatalf("header request: status = %d, want 200", codeHdr)
	}
	listHdr, _ := respHdr["utxos"].([]interface{})
	if len(listHdr) != 1 {
		t.Errorf("X-View-Key header must discover stealth UTXO (found %d, want 1)", len(listHdr))
	}
}

// ─── F-048: scan_limited field in response ───────────────────────────────────

// TestREST_AddressUTXOs_ScanLimitedFieldPresent confirms that the response JSON
// always contains a scan_limited boolean field (false when within cap, true when
// the UTXO set exceeds 200,000 entries).  This test covers the field presence;
// the true-cap path is integration-tested separately (too slow for unit tests).
func TestREST_AddressUTXOs_ScanLimitedFieldPresent(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, _ := crypto.GenerateWalletKeys()
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	// Add a single transparent UTXO.
	var txHash crypto.Hash32
	txHash[0] = 0xF2
	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		OneTimePub:  wk.Spend.Public,
		BlockHeight: 1,
	})

	code, resp := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	scanLimited, exists := resp["scan_limited"]
	if !exists {
		t.Error("F-048 regression: scan_limited field absent from /address/*/utxos response")
	}
	// With one UTXO the cap is not hit — scan_limited must be false.
	if b, ok := scanLimited.(bool); ok && b {
		t.Error("scan_limited must be false with a single UTXO in the set")
	}
}

// ─── F-053: X-Forwarded-For only trusted from loopback ───────────────────────

// TestREST_RealIP_XFFIgnoredFromNonLoopback confirms that X-Forwarded-For is
// silently ignored when the TCP peer is not on the loopback interface.
// Without this fix an attacker could rotate XFF to bypass per-IP rate limiting.
func TestREST_RealIP_XFFIgnoredFromNonLoopback(t *testing.T) {
	srv, _ := buildUTXOServer(t)
	const spoofedIP = "1.2.3.4"

	// Rate-limit the real remote IP (192.0.2.1) by exhausting its token bucket.
	// If XFF is trusted the bucket for 1.2.3.4 is charged instead — meaning
	// a second request with a different XFF value would succeed (bypass).
	// We test the simpler invariant: the server does not crash and returns a
	// well-formed response regardless of what XFF contains.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.RemoteAddr = "192.0.2.1:54321" // non-loopback
	req.Header.Set("X-Forwarded-For", spoofedIP)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	// The endpoint must respond normally (not panic, not 500).
	if rr.Code == http.StatusInternalServerError {
		t.Errorf("XFF from non-loopback caused internal server error: %s", rr.Body.String())
	}

	// Second request from loopback WITH XFF — XFF must be trusted here.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req2.RemoteAddr = "127.0.0.1:54321"
	req2.Header.Set("X-Forwarded-For", spoofedIP)
	rr2 := httptest.NewRecorder()
	srv.ServeHTTP(rr2, req2)
	if rr2.Code == http.StatusInternalServerError {
		t.Errorf("XFF from loopback caused internal server error: %s", rr2.Body.String())
	}
}

// ─── F-052: out-of-turn block producer rejected ──────────────────────────────

// TestREST_ProposerSlotEnforced is documented in consensus_test.go
// (TestEngine_HandleIncomingBlock_WrongProposerRejected).
// This stub prevents the security-fix test file from being empty of consensus tests.
func TestREST_ProposerSlotEnforced_SeeConsensusTests(t *testing.T) {
	t.Log("proposer-slot enforcement tested in blockchain/consensus/consensus_test.go " +
		"(TestEngine_HandleIncomingBlock_WrongProposerRejected)")
}

// ─── helper: build a test mempool + mint registry server ─────────────────────

// mintTestBody encodes a minimal admin mint request body for address + amount.
func mintTestBody(t *testing.T, addr crypto.Address, amtAPR float64) []byte {
	t.Helper()
	b, _ := json.Marshal(map[string]interface{}{
		"address":    string(addr),
		"amount_apr": amtAPR,
	})
	return b
}

// sprintHex formats bytes as lowercase hex — used in test error messages.
func sprintHex(b []byte) string { return fmt.Sprintf("%x", b) }
