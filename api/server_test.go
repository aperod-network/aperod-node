package api_test

// Tests for JSON-RPC 2.0 API server (Phase 2, blocks 2.1.1-2.1.9).
// Uses net/http/httptest so no real TCP socket is needed.

import (
        "bytes"
        "encoding/json"
        "log/slog"
        "net/http"
        "net/http/httptest"
        "os"
        "testing"
        "time"

        "github.com/aperod/aperod/api"
        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// ─── Test helpers ─────────────────────────────────────────────────────────────

func newTestServer(t *testing.T) (*api.Server, *core.Chain) {
        t.Helper()
        priv, pub, _ := crypto.GenerateValidatorKey()
        hdr := core.BlockHeader{
                Height:       0,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: pub,
                MerkleRoot:   core.MerkleRoot(nil),
        }
        _ = hdr.Sign(priv)
        genesis := &core.Block{Header: hdr}

        chain := core.NewChain()
        _ = chain.SetGenesis(genesis)

        mp := core.NewMempool(core.DefaultMempoolConfig())
        utxos := core.NewUTXOSet()
        log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

        srv := api.NewServer(":0", chain, mp, utxos, log)
        return srv, chain
}

func rpcCall(t *testing.T, srv *api.Server, method string, params interface{}) map[string]interface{} {
        t.Helper()
        paramsJSON, _ := json.Marshal(params)
        body := map[string]interface{}{
                "jsonrpc": "2.0",
                "id":      1,
                "method":  method,
                "params":  json.RawMessage(paramsJSON),
        }
        reqBody, _ := json.Marshal(body)
        req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(reqBody))
        req.Header.Set("Content-Type", "application/json")
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)

        var resp map[string]interface{}
        _ = json.NewDecoder(rr.Body).Decode(&resp)
        return resp
}

// ─── /health ─────────────────────────────────────────────────────────────────

func TestHealth_ReturnsOK(t *testing.T) {
        srv, _ := newTestServer(t)
        req := httptest.NewRequest(http.MethodGet, "/health", nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)

        if rr.Code != http.StatusOK {
                t.Errorf("health status = %d, want 200", rr.Code)
        }
        var resp map[string]interface{}
        _ = json.NewDecoder(rr.Body).Decode(&resp)
        if resp["status"] != "ok" {
                t.Errorf("health status = %v, want ok", resp["status"])
        }
}

// ─── JSON-RPC basics ──────────────────────────────────────────────────────────

func TestRPC_UnknownMethod(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_unknownMethod", nil)
        if resp["error"] == nil {
                t.Error("expected error for unknown method")
        }
}

func TestRPC_InvalidJSON(t *testing.T) {
        srv, _ := newTestServer(t)
        req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{bad json`)))
        req.Header.Set("Content-Type", "application/json")
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)

        var resp map[string]interface{}
        _ = json.NewDecoder(rr.Body).Decode(&resp)
        errObj, ok := resp["error"].(map[string]interface{})
        if !ok || errObj == nil {
                t.Error("expected parse error")
        }
}

func TestRPC_NonPostMethod(t *testing.T) {
        srv, _ := newTestServer(t)
        req := httptest.NewRequest(http.MethodGet, "/", nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)

        var resp map[string]interface{}
        _ = json.NewDecoder(rr.Body).Decode(&resp)
        if resp["error"] == nil {
                t.Error("GET to / should return error")
        }
}

func TestRPC_OptionsPreFlight(t *testing.T) {
        srv, _ := newTestServer(t)
        req := httptest.NewRequest(http.MethodOptions, "/", nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)
        if rr.Code != http.StatusOK {
                t.Errorf("OPTIONS status = %d, want 200", rr.Code)
        }
}

func TestRPC_WrongJSONRPCVersion(t *testing.T) {
        srv, _ := newTestServer(t)
        body := `{"jsonrpc":"1.0","id":1,"method":"apr_getNodeInfo","params":null}`
        req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(body)))
        req.Header.Set("Content-Type", "application/json")
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)
        var resp map[string]interface{}
        _ = json.NewDecoder(rr.Body).Decode(&resp)
        if resp["error"] == nil {
                t.Error("expected error for jsonrpc != 2.0")
        }
}

// ─── apr_getNodeInfo ─────────────────────────────────────────────────────────

func TestRPC_GetNodeInfo(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_getNodeInfo", nil)
        if resp["error"] != nil {
                t.Fatalf("apr_getNodeInfo error: %v", resp["error"])
        }
        result, ok := resp["result"].(map[string]interface{})
        if !ok {
                t.Fatal("result is not an object")
        }
        if result["version"] == nil {
                t.Error("expected version in node info")
        }
        if result["chain_id"] == nil {
                t.Error("expected chain_id in node info")
        }
}

// ─── apr_getBlockByHeight ─────────────────────────────────────────────────────

func TestRPC_GetBlockByHeight_Genesis(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_getBlockByHeight", map[string]interface{}{"height": 0})
        if resp["error"] != nil {
                t.Fatalf("apr_getBlockByHeight(0) error: %v", resp["error"])
        }
        result, ok := resp["result"].(map[string]interface{})
        if !ok {
                t.Fatal("result is not an object")
        }
        if result["height"] != float64(0) {
                t.Errorf("height = %v, want 0", result["height"])
        }
}

func TestRPC_GetBlockByHeight_NotFound(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_getBlockByHeight", map[string]interface{}{"height": 9999})
        if resp["error"] == nil {
                t.Error("expected error for non-existent height")
        }
}

// ─── apr_getBlockByHash ───────────────────────────────────────────────────────

func TestRPC_GetBlockByHash_Genesis(t *testing.T) {
        srv, chain := newTestServer(t)
        genesis := chain.GetByHeight(0)
        hash := genesis.Hash()
        hashHex := string(make([]byte, 64))
        {
                b := make([]byte, 64)
                for i, v := range hash {
                        b[i*2] = "0123456789abcdef"[v>>4]
                        b[i*2+1] = "0123456789abcdef"[v&0xf]
                }
                hashHex = string(b)
        }

        resp := rpcCall(t, srv, "apr_getBlockByHash", map[string]interface{}{"hash": hashHex})
        if resp["error"] != nil {
                t.Fatalf("apr_getBlockByHash error: %v", resp["error"])
        }
}

// ─── apr_getMempoolInfo / apr_getMempoolTxs ───────────────────────────────────

func TestRPC_GetMempoolInfo(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_getMempoolInfo", nil)
        if resp["error"] != nil {
                t.Fatalf("apr_getMempoolInfo error: %v", resp["error"])
        }
        result, ok := resp["result"].(map[string]interface{})
        if !ok {
                t.Fatal("result is not an object")
        }
        if result["count"] == nil {
                t.Error("expected count in mempool info")
        }
}

func TestRPC_GetMempoolTxs_Empty(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_getMempoolTxs", nil)
        if resp["error"] != nil {
                t.Fatalf("apr_getMempoolTxs error: %v", resp["error"])
        }
        result, ok := resp["result"].([]interface{})
        if !ok {
                t.Fatalf("result is not an array: %T", resp["result"])
        }
        if len(result) != 0 {
                t.Errorf("empty mempool should return [] not %v", result)
        }
}

// ─── /metrics ──────────────────────────────────────────────────────────────

func TestMetrics_ExposesCoreGauges(t *testing.T) {
        srv, _ := newTestServer(t)

        req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)

        if rr.Code != http.StatusOK {
                t.Fatalf("expected 200, got %d", rr.Code)
        }
        body := rr.Body.String()
        for _, want := range []string{
                "aperod_chain_height",
                "aperod_mempool_size",
                "aperod_utxo_count",
                "aperod_peer_count",
                "aperod_up 1",
        } {
                if !bytes.Contains([]byte(body), []byte(want)) {
                        t.Errorf("metrics output missing %q; body:\n%s", want, body)
                }
        }
}

func TestMetrics_PeerCounterWired(t *testing.T) {
        srv, _ := newTestServer(t)
        srv.SetPeerCounter(func() int { return 7 })

        req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)

        if !bytes.Contains(rr.Body.Bytes(), []byte("aperod_peer_count 7")) {
                t.Errorf("expected aperod_peer_count 7, got:\n%s", rr.Body.String())
        }
}

// ─── apr_validateAddress ──────────────────────────────────────────────────────

func TestRPC_ValidateAddress_Valid(t *testing.T) {
        srv, _ := newTestServer(t)
        wk, _ := crypto.GenerateWalletKeys()
        addr := crypto.AddressFromKeys(crypto.MainnetByte, wk)

        resp := rpcCall(t, srv, "apr_validateAddress", map[string]interface{}{"address": addr.String()})
        if resp["error"] != nil {
                t.Fatalf("apr_validateAddress error: %v", resp["error"])
        }
        result, ok := resp["result"].(map[string]interface{})
        if !ok {
                t.Fatal("result is not an object")
        }
        if result["valid"] != true {
                t.Errorf("valid = %v, want true", result["valid"])
        }
}

func TestRPC_ValidateAddress_Invalid(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_validateAddress", map[string]interface{}{"address": "not-an-address"})
        if resp["error"] != nil {
                t.Fatalf("apr_validateAddress error: %v", resp["error"])
        }
        result := resp["result"].(map[string]interface{})
        if result["valid"] != false {
                t.Errorf("valid = %v, want false", result["valid"])
        }
}

// ─── apr_getBalance ────────────────────────────────────────────────────────────

func TestRPC_GetBalance_ValidAddress(t *testing.T) {
        srv, _ := newTestServer(t)
        wk, _ := crypto.GenerateWalletKeys()
        addr := crypto.AddressFromKeys(crypto.MainnetByte, wk)

        resp := rpcCall(t, srv, "apr_getBalance", map[string]interface{}{
                "address": addr.String(),
        })
        if resp["error"] != nil {
                t.Fatalf("apr_getBalance error: %v", resp["error"])
        }
}

func TestRPC_GetBalance_InvalidAddress(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_getBalance", map[string]interface{}{
                "address": "garbage-addr",
        })
        if resp["error"] == nil {
                t.Error("expected error for invalid address in apr_getBalance")
        }
}
