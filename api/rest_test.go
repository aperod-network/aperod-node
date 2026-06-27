package api_test

// Tests for Phase 2 REST endpoints and new JSON-RPC methods (apr_getTransaction, apr_estimateFee).

import (
        "encoding/hex"
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

// ─── helpers ─────────────────────────────────────────────────────────────────

func testLogger() *slog.Logger {
        return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// buildChainServer creates a server backed by a chain with genesis + n blocks.
func buildChainServer(t *testing.T, extraBlocks int) (*api.Server, *core.Chain) {
        t.Helper()
        priv, pub, _ := crypto.GenerateValidatorKey()

        makeBlock := func(height uint64, prevHash crypto.Hash32) *core.Block {
                cb := core.CoinbaseTx(crypto.Point32(pub), 1_000_000)
                txs := []core.Transaction{cb}
                hdr := core.BlockHeader{
                        Height:       height,
                        PrevHash:     prevHash,
                        Timestamp:    time.Now().UnixNano(),
                        ValidatorPub: pub,
                        MerkleRoot:   core.MerkleRoot(txs),
                }
                _ = hdr.Sign(priv)
                return &core.Block{Header: hdr, Txs: txs}
        }

        var prev crypto.Hash32
        genesis := makeBlock(0, prev)
        chain := core.NewChain()
        _ = chain.SetGenesis(genesis)

        for i := 1; i <= extraBlocks; i++ {
                parent := chain.GetByHeight(uint64(i - 1))
                prev = parent.Hash()
                _ = chain.AddBlock(makeBlock(uint64(i), prev))
        }

        mp := core.NewMempool(core.DefaultMempoolConfig())
        utxos := core.NewUTXOSet()
        srv := api.NewServer(":0", chain, mp, utxos, testLogger())
        return srv, chain
}

func restGet(t *testing.T, srv *api.Server, path string) (int, map[string]interface{}) {
        t.Helper()
        req := httptest.NewRequest(http.MethodGet, path, nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)
        var resp map[string]interface{}
        _ = json.NewDecoder(rr.Body).Decode(&resp)
        return rr.Code, resp
}

// ─── /api/v1/blocks ───────────────────────────────────────────────────────────

func TestREST_Blocks_WithGenesis(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        code, resp := restGet(t, srv, "/api/v1/blocks")
        if code != http.StatusOK {
                t.Errorf("status = %d, want 200", code)
        }
        if resp["total"] == nil {
                t.Error("expected total in response")
        }
        blocks, _ := resp["blocks"].([]interface{})
        if len(blocks) == 0 {
                t.Error("expected at least genesis block")
        }
}

func TestREST_Blocks_Pagination(t *testing.T) {
        srv, _ := buildChainServer(t, 5)
        code, resp := restGet(t, srv, "/api/v1/blocks?limit=3&offset=0")
        if code != http.StatusOK {
                t.Errorf("status = %d, want 200", code)
        }
        blocks, ok := resp["blocks"].([]interface{})
        if !ok {
                t.Fatalf("blocks not an array: %T", resp["blocks"])
        }
        if len(blocks) != 3 {
                t.Errorf("got %d blocks, want 3", len(blocks))
        }
}

func TestREST_Blocks_OffsetPastEnd(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        code, resp := restGet(t, srv, "/api/v1/blocks?offset=99999")
        if code != http.StatusOK {
                t.Errorf("status = %d, want 200", code)
        }
        blocks := resp["blocks"].([]interface{})
        if len(blocks) != 0 {
                t.Errorf("expected empty blocks slice, got %d", len(blocks))
        }
}

func TestREST_Blocks_MethodNotAllowed(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        req := httptest.NewRequest(http.MethodPost, "/api/v1/blocks", nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)
        if rr.Code != http.StatusMethodNotAllowed {
                t.Errorf("status = %d, want 405", rr.Code)
        }
}

// ─── /api/v1/blocks/{id} ─────────────────────────────────────────────────────

func TestREST_BlockByHeight(t *testing.T) {
        srv, _ := buildChainServer(t, 3)
        code, resp := restGet(t, srv, "/api/v1/blocks/0")
        if code != http.StatusOK {
                t.Errorf("status = %d, want 200 for block 0", code)
        }
        if resp["height"] != float64(0) {
                t.Errorf("height = %v, want 0", resp["height"])
        }
}

func TestREST_BlockByHash(t *testing.T) {
        srv, chain := buildChainServer(t, 1)
        genesis := chain.GetByHeight(0)
        gh := genesis.Hash()
        hashHex := hex.EncodeToString(gh[:])
        code, resp := restGet(t, srv, "/api/v1/blocks/"+hashHex)
        if code != http.StatusOK {
                t.Errorf("status = %d, want 200", code)
        }
        if resp["height"] != float64(0) {
                t.Errorf("height = %v, want 0", resp["height"])
        }
}

func TestREST_BlockByID_NotFoundHeight(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        code, _ := restGet(t, srv, "/api/v1/blocks/9999")
        if code != http.StatusNotFound {
                t.Errorf("status = %d, want 404", code)
        }
}

func TestREST_BlockByID_InvalidID(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        code, _ := restGet(t, srv, "/api/v1/blocks/notahash")
        if code != http.StatusBadRequest {
                t.Errorf("status = %d, want 400", code)
        }
}

func TestREST_BlockByID_NotFoundHash(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        // All-0xFF hash is unlikely to match any real block
        notFound := make([]byte, 32)
        for i := range notFound {
                notFound[i] = 0xFF
        }
        code, _ := restGet(t, srv, "/api/v1/blocks/"+hex.EncodeToString(notFound))
        if code != http.StatusNotFound {
                t.Errorf("status = %d, want 404", code)
        }
}

func TestREST_BlockByID_MethodNotAllowed(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        req := httptest.NewRequest(http.MethodPost, "/api/v1/blocks/0", nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)
        if rr.Code != http.StatusMethodNotAllowed {
                t.Errorf("status = %d, want 405", rr.Code)
        }
}

// ─── /api/v1/transactions/{hash} ─────────────────────────────────────────────

func TestREST_Transaction_Confirmed(t *testing.T) {
        srv, chain := buildChainServer(t, 0)
        genesis := chain.GetByHeight(0)
        txHash := genesis.Txs[0].Hash()
        txHashArr := txHash
        code, resp := restGet(t, srv, "/api/v1/transactions/"+hex.EncodeToString(txHashArr[:]))
        if code != http.StatusOK {
                t.Errorf("status = %d, want 200", code)
        }
        if resp["is_coinbase"] != true {
                t.Errorf("is_coinbase = %v, want true", resp["is_coinbase"])
        }
}

func TestREST_Transaction_NotFound(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        code, _ := restGet(t, srv, "/api/v1/transactions/"+hex.EncodeToString(make([]byte, 32)))
        if code != http.StatusNotFound {
                t.Errorf("status = %d, want 404", code)
        }
}

func TestREST_Transaction_InvalidHash(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        code, _ := restGet(t, srv, "/api/v1/transactions/notahash")
        if code != http.StatusBadRequest {
                t.Errorf("status = %d, want 400", code)
        }
}

func TestREST_Transaction_MethodNotAllowed(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        req := httptest.NewRequest(http.MethodPost, "/api/v1/transactions/"+hex.EncodeToString(make([]byte, 32)), nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)
        if rr.Code != http.StatusMethodNotAllowed {
                t.Errorf("status = %d, want 405", rr.Code)
        }
}

// ─── /api/v1/address/{addr}/transactions ─────────────────────────────────────

func TestREST_AddressTxs_ValidAddress(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        wk, _ := crypto.GenerateWalletKeys()
        addr := crypto.AddressFromKeys(crypto.MainnetByte, wk)
        code, resp := restGet(t, srv, "/api/v1/address/"+addr.String()+"/transactions")
        if code != http.StatusOK {
                t.Errorf("status = %d, want 200", code)
        }
        if resp["transactions"] == nil {
                t.Error("expected transactions field")
        }
}

func TestREST_AddressTxs_InvalidAddress(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        code, _ := restGet(t, srv, "/api/v1/address/garbage-addr/transactions")
        if code != http.StatusBadRequest {
                t.Errorf("status = %d, want 400", code)
        }
}

func TestREST_AddressTxs_CoinbaseMatch(t *testing.T) {
        // Build chain where coinbase output.OneTimePub == spendPub of query address
        priv, pub, _ := crypto.GenerateValidatorKey()
        spendPt := crypto.Point32(pub)
        cb := core.CoinbaseTx(spendPt, 1_000_000)
        txs := []core.Transaction{cb}
        hdr := core.BlockHeader{
                Height:       0,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: pub,
                MerkleRoot:   core.MerkleRoot(txs),
        }
        _ = hdr.Sign(priv)
        genesis := &core.Block{Header: hdr, Txs: txs}
        chain := core.NewChain()
        _ = chain.SetGenesis(genesis)

        mp := core.NewMempool(core.DefaultMempoolConfig())
        utxos := core.NewUTXOSet()
        srv := api.NewServer(":0", chain, mp, utxos, testLogger())

        // Encode spendPt as an address (use same point for spend+view)
        addr := crypto.EncodeAddress(crypto.MainnetByte, spendPt, spendPt)
        code, resp := restGet(t, srv, "/api/v1/address/"+string(addr)+"/transactions")
        if code != http.StatusOK {
                t.Errorf("status = %d, want 200", code)
        }
        txList, _ := resp["transactions"].([]interface{})
        if len(txList) == 0 {
                t.Error("expected at least 1 coinbase output matching the address")
        }
}

func TestREST_AddressTxs_MethodNotAllowed(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        wk, _ := crypto.GenerateWalletKeys()
        addr := crypto.AddressFromKeys(crypto.MainnetByte, wk)
        req := httptest.NewRequest(http.MethodPost, "/api/v1/address/"+addr.String()+"/transactions", nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)
        if rr.Code != http.StatusMethodNotAllowed {
                t.Errorf("status = %d, want 405", rr.Code)
        }
}

// ─── /api/v1/network/stats ────────────────────────────────────────────────────

func TestREST_NetworkStats_Genesis(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        code, resp := restGet(t, srv, "/api/v1/network/stats")
        if code != http.StatusOK {
                t.Errorf("status = %d, want 200", code)
        }
        if resp["height"] == nil {
                t.Error("expected height in stats")
        }
}

func TestREST_NetworkStats_MultiBlock(t *testing.T) {
        srv, _ := buildChainServer(t, 5)
        code, resp := restGet(t, srv, "/api/v1/network/stats")
        if code != http.StatusOK {
                t.Errorf("status = %d, want 200", code)
        }
        if resp["height"] != float64(5) {
                t.Errorf("height = %v, want 5", resp["height"])
        }
        if resp["total_txs"] == nil {
                t.Error("expected total_txs")
        }
}

func TestREST_NetworkStats_MethodNotAllowed(t *testing.T) {
        srv, _ := buildChainServer(t, 0)
        req := httptest.NewRequest(http.MethodPost, "/api/v1/network/stats", nil)
        rr := httptest.NewRecorder()
        srv.ServeHTTP(rr, req)
        if rr.Code != http.StatusMethodNotAllowed {
                t.Errorf("status = %d, want 405", rr.Code)
        }
}

// ─── apr_getTransaction RPC ───────────────────────────────────────────────────

func TestRPC_GetTransaction_Confirmed(t *testing.T) {
        srv, chain := buildChainServer(t, 0)
        genesis := chain.GetByHeight(0)
        txHash := genesis.Txs[0].Hash()
        hashHex := hex.EncodeToString(txHash[:])

        resp := rpcCall(t, srv, "apr_getTransaction", map[string]string{"hash": hashHex})
        if resp["error"] != nil {
                t.Fatalf("apr_getTransaction error: %v", resp["error"])
        }
        result := resp["result"].(map[string]interface{})
        if result["is_coinbase"] != true {
                t.Errorf("is_coinbase = %v, want true", result["is_coinbase"])
        }
        if result["block_height"] != float64(0) {
                t.Errorf("block_height = %v, want 0", result["block_height"])
        }
}

func TestRPC_GetTransaction_NotFound(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_getTransaction", map[string]string{
                "hash": hex.EncodeToString(make([]byte, 32)),
        })
        if resp["error"] == nil {
                t.Error("expected error for unknown tx")
        }
}

func TestRPC_GetTransaction_InvalidHash(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_getTransaction", map[string]string{"hash": "tooshort"})
        if resp["error"] == nil {
                t.Error("expected error for bad hash")
        }
}

// ─── apr_estimateFee RPC ──────────────────────────────────────────────────────

func TestRPC_EstimateFee_Default(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_estimateFee", nil)
        if resp["error"] != nil {
                t.Fatalf("apr_estimateFee error: %v", resp["error"])
        }
        result := resp["result"].(map[string]interface{})
        if result["fee"] == nil {
                t.Error("expected fee in response")
        }
        if result["unit"] != "nAPR" {
                t.Errorf("unit = %v, want nAPR", result["unit"])
        }
}

func TestRPC_EstimateFee_WithSize(t *testing.T) {
        srv, _ := newTestServer(t)
        // size_bytes is ignored — flat fee is always returned
        resp := rpcCall(t, srv, "apr_estimateFee", map[string]interface{}{"size_bytes": 1000})
        if resp["error"] != nil {
                t.Fatalf("apr_estimateFee error: %v", resp["error"])
        }
        result := resp["result"].(map[string]interface{})
        fee := result["fee"].(float64)
        if fee != float64(core.FlatFee) {
                t.Errorf("fee = %v, want %d (flat fee)", fee, core.FlatFee)
        }
}

func TestRPC_EstimateFee_SmallTx_MinFee(t *testing.T) {
        srv, _ := newTestServer(t)
        resp := rpcCall(t, srv, "apr_estimateFee", map[string]interface{}{"size_bytes": 1})
        result := resp["result"].(map[string]interface{})
        fee := result["fee"].(float64)
        if fee < float64(core.FlatFee) {
                t.Errorf("fee = %v, should be >= %d (flat fee)", fee, core.FlatFee)
        }
}

// ─── WebSocket hub ────────────────────────────────────────────────────────────

func TestHub_ClientCount_Initial(t *testing.T) {
        srv, _ := newTestServer(t)
        if srv.Hub().ClientCount() != 0 {
                t.Errorf("ClientCount = %d, want 0", srv.Hub().ClientCount())
        }
}

func TestHub_BroadcastBlock_NoPeers(t *testing.T) {
        srv, _ := newTestServer(t)
        priv, pub, _ := crypto.GenerateValidatorKey()
        hdr := core.BlockHeader{Height: 0, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
        _ = hdr.Sign(priv)
        b := &core.Block{Header: hdr}
        srv.Hub().BroadcastBlock(b) // must not panic
}

func TestHub_BroadcastTx_NoPeers(t *testing.T) {
        srv, _ := newTestServer(t)
        tx := &core.Transaction{Version: core.TxVersionBase}
        srv.Hub().BroadcastTx(tx) // must not panic
}

func TestHub_BroadcastConfirmed_NoPeers(t *testing.T) {
        srv, _ := newTestServer(t)
        tx := &core.Transaction{Version: core.TxVersionBase}
        srv.Hub().BroadcastConfirmed(tx, 5) // must not panic
}

func TestWS_Endpoint_Registered(t *testing.T) {
        // golang.org/x/net/websocket requires http.Hijacker; use a real test server.
        srv, _ := newTestServer(t)
        ts := httptest.NewServer(srv)
        defer ts.Close()

        // Plain GET without WS upgrade headers — server may return any non-404 status.
        resp, err := http.Get(ts.URL + "/ws")
        if err != nil {
                t.Fatalf("GET /ws: %v", err)
        }
        defer resp.Body.Close()
        if resp.StatusCode == http.StatusNotFound {
                t.Error("/ws route not registered (got 404)")
        }
}
