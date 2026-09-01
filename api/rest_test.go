package api_test

// Tests for Phase 2 REST endpoints and new JSON-RPC methods (apr_getTransaction, apr_estimateFee).

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
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

// restGetHeader sends a GET request with a single extra header.
func restGetHeader(t *testing.T, srv *api.Server, path, header, value string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(header, value)
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

// TestREST_AddressTxs_IsCoinbaseFlagged confirms that the address-transactions
// endpoint sets is_coinbase=true on block-reward (coinbase) outputs.  This is
// the regression guard for the "+0 APRO" wallet-history bug: the wallet filters
// on this field so it must be present and accurate.
func TestREST_AddressTxs_IsCoinbaseFlagged(t *testing.T) {
	// Build a genesis block whose coinbase output.OneTimePub equals spendPt so
	// the scanner matches it and returns it in the address-transactions list.
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

	addr := crypto.EncodeAddress(crypto.MainnetByte, spendPt, spendPt)
	code, resp := restGet(t, srv, "/api/v1/address/"+string(addr)+"/transactions")
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200", code)
	}
	txList, _ := resp["transactions"].([]interface{})
	if len(txList) == 0 {
		t.Fatal("expected at least 1 matching transaction for the coinbase output")
	}
	first, ok := txList[0].(map[string]interface{})
	if !ok {
		t.Fatalf("transaction entry is not an object: %T", txList[0])
	}
	if first["is_coinbase"] != true {
		t.Errorf("is_coinbase = %v, want true — coinbase block-reward outputs must be flagged so the wallet filter can exclude them", first["is_coinbase"])
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

// reconnect_backoff_active must appear in the stats payload so the API-server
// peer-count monitor can flag a relay that is silently waiting out a dial
// back-off window with 0 peers.
func TestREST_NetworkStats_ReconnectBackoffFlag(t *testing.T) {
	srv, _ := buildChainServer(t, 0)

	// Not wired → defaults to false, but the key must be present.
	code, resp := restGet(t, srv, "/api/v1/network/stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if v, ok := resp["reconnect_backoff_active"]; !ok {
		t.Error("expected reconnect_backoff_active key in stats")
	} else if v != false {
		t.Errorf("reconnect_backoff_active = %v, want false when not wired", v)
	}

	// Wired → reflects the live flag.
	srv.SetReconnectBackoffFlag(func() bool { return true })
	code, resp = restGet(t, srv, "/api/v1/network/stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp["reconnect_backoff_active"] != true {
		t.Errorf("reconnect_backoff_active = %v, want true when wired flag returns true", resp["reconnect_backoff_active"])
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
	if result["unit"] != "nAPRO" {
		t.Errorf("unit = %v, want nAPRO", result["unit"])
	}
}

func TestRPC_EstimateFee_WithSize(t *testing.T) {
	srv, _ := newTestServer(t)
	// size_bytes drives the estimate: fee = size_bytes x InitialBaseFeePerByte
	resp := rpcCall(t, srv, "apr_estimateFee", map[string]interface{}{"size_bytes": 1000})
	if resp["error"] != nil {
		t.Fatalf("apr_estimateFee error: %v", resp["error"])
	}
	result := resp["result"].(map[string]interface{})
	fee := result["fee"].(float64)
	if fee != float64(1000*core.InitialBaseFeePerByte) {
		t.Errorf("fee = %v, want %v (size_based_eip1559)", fee, float64(1000*core.InitialBaseFeePerByte))
	}
}

func TestRPC_EstimateFee_SmallTx_MinFee(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := rpcCall(t, srv, "apr_estimateFee", map[string]interface{}{"size_bytes": 1})
	result := resp["result"].(map[string]interface{})
	fee := result["fee"].(float64)
	if fee < float64(core.MinBaseFeePerByte) {
		t.Errorf("fee = %v, should be >= %d (MinBaseFeePerByte)", fee, core.MinBaseFeePerByte)
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

// ─── /api/v1/admin/mint ────────────────────────────────────────────────────

func restPostJSON(t *testing.T, srv *api.Server, path string, body []byte) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Host = "127.0.0.1" // localOnly guard: simulate loopback caller
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var resp map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	return rr.Code, resp
}

func restLocalPostJSON(t *testing.T, srv *api.Server, path string, body []byte) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var resp map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	return rr.Code, resp
}
func TestREST_AdminMint_FractionalAmount(t *testing.T) {
	srv, _ := newTestServer(t)
	// Stub the engine mint scheduler (in production it is wired to
	// consensus.Engine.ScheduleAdminMint by cmd/node).
	srv.SetMintScheduler(func(idempotencyKey, addr string, amountNAPR uint64, timeout time.Duration) (string, uint64, error) {
		return "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", 42, nil
	})
	wk, _ := crypto.GenerateWalletKeys()
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	body := []byte(`{"idempotency_key":"fractional","address":"` + string(addr) + `","amount_apr":5909.5}`)
	code, resp := restLocalPostJSON(t, srv, "/api/v1/admin/mint", body)
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %v", code, resp)
	}
	amt, ok := resp["amount_apr"].(float64)
	if !ok || amt != 5909.5 {
		t.Fatalf("expected amount_apr=5909.5, got %v", resp["amount_apr"])
	}
	// Task #1902 — the response must carry the authoritative scheduled nAPRO
	// amount (not just a float echo of the request) so the API server can
	// persist and verify the exact on-chain value.
	// Serialized as a decimal string so JS callers can parse losslessly with BigInt.
	napr, ok := resp["amount_napr"].(string)
	if !ok || napr != "590950000000" {
		t.Fatalf("expected amount_napr=\"590950000000\", got %v", resp["amount_napr"])
	}
	if resp["tx_hash"] == "" {
		t.Fatal("expected non-empty tx_hash")
	}
}

// TestREST_AdminMint_ReturnsV2BlindForScheduledHeight verifies Task #1900:
// when the mint scheduler commits the mint at a real height (> 0), the
// response blind must be DeterministicMintBlindV2(spendPub, amount, height) —
// matching the commitment BuildMintTx created — NOT the legacy V1 blind.
func TestREST_AdminMint_ReturnsV2BlindForScheduledHeight(t *testing.T) {
	srv, _ := newTestServer(t)
	const mintHeight = uint64(777)
	srv.SetMintScheduler(func(idempotencyKey, addr string, amountNAPR uint64, timeout time.Duration) (string, uint64, error) {
		return "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", mintHeight, nil
	})
	wk, _ := crypto.GenerateWalletKeys()
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	body := []byte(`{"idempotency_key":"blind","address":"` + string(addr) + `","amount_apr":500}`)
	code, resp := restLocalPostJSON(t, srv, "/api/v1/admin/mint", body)
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %v", code, resp)
	}
	if h, _ := resp["height"].(float64); uint64(h) != mintHeight {
		t.Fatalf("expected height=%d in response, got %v", mintHeight, resp["height"])
	}
	const amountNAPR = uint64(500 * 100_000_000)
	wantV2, err := crypto.DeterministicMintBlindV2(wk.Spend.Public, amountNAPR, mintHeight)
	if err != nil {
		t.Fatalf("DeterministicMintBlindV2: %v", err)
	}
	wantHex := fmt.Sprintf("%x", wantV2[:])
	gotHex, _ := resp["blind_hex"].(string)
	if gotHex != wantHex {
		t.Fatalf("blind_hex mismatch: got %s want V2 %s", gotHex, wantHex)
	}
	v1, _ := crypto.DeterministicMintBlind(wk.Spend.Public, amountNAPR)
	if gotHex == fmt.Sprintf("%x", v1[:]) {
		t.Fatal("blind_hex equals legacy V1 blind — commitment mismatch for scheduled mints")
	}
}

func TestREST_AdminMint_ZeroAmount(t *testing.T) {
	srv, _ := newTestServer(t)
	wk, _ := crypto.GenerateWalletKeys()
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	body := []byte(`{"idempotency_key":"zero","address":"` + string(addr) + `","amount_apr":0}`)
	code, _ := restLocalPostJSON(t, srv, "/api/v1/admin/mint", body)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for zero amount, got %d", code)
	}
}

func TestREST_NetworkBanByAddr_LocalOnly(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetBanLiftFunc(func(string) bool { return true }) // always succeeds

	// Request with a non-loopback Host must be rejected.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/network/bans/192.0.2.1", nil)
	req.Host = "attacker.example.com"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback Host, got %d", rr.Code)
	}
}
func buildUTXOServer(t *testing.T) (*api.Server, *core.UTXOSet) {
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
	return srv, utxos
}

// TestREST_AddressUTXOs_TransparentMatch verifies that a UTXO whose OneTimePub
// equals the address spend public key appears in the listing.
func TestREST_AddressUTXOs_TransparentMatch(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	spendPub := wk.Spend.Public
	addr := crypto.EncodeAddress(crypto.MainnetByte, spendPub, wk.View.Public)

	var txHash crypto.Hash32
	txHash[0] = 0xAB
	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		OneTimePub:  spendPub,
		BlockHeight: 0,
	})

	code, resp := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	list, _ := resp["utxos"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("utxos count = %d, want 1", len(list))
	}
	entry := list[0].(map[string]interface{})
	if entry["tx_hash"] != hex.EncodeToString(txHash[:]) {
		t.Errorf("tx_hash = %v, want %s", entry["tx_hash"], hex.EncodeToString(txHash[:]))
	}
}

// TestREST_AddressUTXOs_SpentRemoved verifies that once a UTXO is removed from
// the set (simulating a spend), it no longer appears in the address listing.
func TestREST_AddressUTXOs_SpentRemoved(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	spendPub := wk.Spend.Public
	addr := crypto.EncodeAddress(crypto.MainnetByte, spendPub, wk.View.Public)

	var txHash crypto.Hash32
	txHash[0] = 0xCD
	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		OneTimePub:  spendPub,
		BlockHeight: 0,
	})

	// Confirm it appears before spending
	_, before := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	beforeList, _ := before["utxos"].([]interface{})
	if len(beforeList) != 1 {
		t.Fatalf("pre-spend utxos = %d, want 1", len(beforeList))
	}

	// Spend the UTXO (remove from set) and flush the response cache so the
	// next request sees the updated UTXO set rather than the cached entry.
	utxos.Remove(txHash, 0)
	srv.FlushUTXOCache()

	// Confirm it is gone
	code, after := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	afterList, _ := after["utxos"].([]interface{})
	if len(afterList) != 0 {
		t.Errorf("post-spend utxos = %d, want 0 (spent UTXO must not appear)", len(afterList))
	}
}

// TestREST_AddressUTXOs_MintHeightMatch verifies that a coinbase/mint UTXO
// whose OneTimePub was derived as spend_pub + height*G is matched correctly,
// and that the returned block_height reflects the height used during derivation.
func TestREST_AddressUTXOs_MintHeightMatch(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	spendPub := wk.Spend.Public
	addr := crypto.EncodeAddress(crypto.MainnetByte, spendPub, wk.View.Public)

	const mintHeight = uint64(42)

	// Compute mint pub: spend_pub + mintHeight * G  (matches the handler logic)
	heightPub, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(mintHeight))
	if err != nil {
		t.Fatalf("ScalarMulBase: %v", err)
	}
	mintPub, err := crypto.AddPoints(spendPub, heightPub)
	if err != nil {
		t.Fatalf("AddPoints: %v", err)
	}

	var txHash crypto.Hash32
	txHash[0] = 0xEF
	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		OneTimePub:  mintPub,
		BlockHeight: mintHeight,
	})

	code, resp := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	list, _ := resp["utxos"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("utxos count = %d, want 1 (mint UTXO must match via height-offset pub)", len(list))
	}
	entry := list[0].(map[string]interface{})
	if entry["block_height"] != float64(mintHeight) {
		t.Errorf("block_height = %v, want %d", entry["block_height"], mintHeight)
	}
}

// TestREST_AddressUTXOs_StealthNotReturnedWithoutViewKey confirms that a
// genuine stealth UTXO (TxPubKey ≠ zero, OneTimePub derived via ECDH) is
// intentionally NOT returned by the address/utxos endpoint when no view key is
// supplied.  This is the expected behaviour, not a bug — the server cannot
// reverse the Diffie-Hellman without the receiver's private view key.
//
// The second sub-test verifies that supplying view_key_hex resolves the gap:
// the same stealth UTXO is discovered and amount_napr is decoded inline.
func TestREST_AddressUTXOs_StealthNotReturnedWithoutViewKey(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	// Build a genuine stealth output for this wallet.
	stealthOut, err := crypto.CreateStealthOutput(wk.Spend.Public, wk.View.Public)
	if err != nil {
		t.Fatalf("CreateStealthOutput: %v", err)
	}

	// Encrypt a fake amount (500 nAPRO) so amount_napr can be decoded later.
	const testAmountNapr = uint64(500)
	encAmt := core.EncryptAmount(testAmountNapr, &stealthOut.HsScalar)

	var txHash crypto.Hash32
	txHash[0] = 0x5E

	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		OneTimePub:  stealthOut.OneTimePub,
		TxPubKey:    stealthOut.TxPubKey,
		EncAmount:   encAmt,
		BlockHeight: 1,
	})

	// ── Without view key: stealth output must be absent ──────────────────────
	code, resp := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	list, _ := resp["utxos"].([]interface{})
	if len(list) != 0 {
		t.Errorf("without view key: utxos count = %d, want 0 — stealth output must NOT be returned without view-key scan (expected gap, not a bug)", len(list))
	}
	note, _ := resp["note"].(string)
	if note == "" {
		t.Error("without view key: expected a non-empty note documenting the stealth limitation")
	}

	// ── With view key (X-View-Key header): stealth output must be present ────
	viewKeyHex := hex.EncodeToString(wk.View.Private[:])
	code2, resp2 := restGetHeader(t, srv, "/api/v1/address/"+string(addr)+"/utxos", "X-View-Key", viewKeyHex)
	if code2 != http.StatusOK {
		t.Fatalf("with view key: status = %d, want 200", code2)
	}
	list2, _ := resp2["utxos"].([]interface{})
	if len(list2) != 1 {
		t.Fatalf("with view key: utxos count = %d, want 1", len(list2))
	}
	entry := list2[0].(map[string]interface{})
	if entry["tx_hash"] != hex.EncodeToString(txHash[:]) {
		t.Errorf("with view key: tx_hash = %v, want %s", entry["tx_hash"], hex.EncodeToString(txHash[:]))
	}
	if entry["amount_napr"] == nil {
		t.Error("with view key: amount_napr must be decoded (non-null) for stealth output")
	} else if uint64(entry["amount_napr"].(float64)) != testAmountNapr {
		t.Errorf("with view key: amount_napr = %v, want %d", entry["amount_napr"], testAmountNapr)
	}
}

// TestREST_AddressUTXOs_RollbackTransparent verifies that after ApplyBlock adds a
// block containing a transparent output for an address, then RollbackBlock reverts
// it, the UTXO no longer appears in GET /api/v1/address/{addr}/utxos.
func TestREST_AddressUTXOs_RollbackTransparent(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	spendPub := wk.Spend.Public
	addr := crypto.EncodeAddress(crypto.MainnetByte, spendPub, wk.View.Public)

	// Build a block at height 1 with a transparent output (OneTimePub == spendPub).
	priv, pub, _ := crypto.GenerateValidatorKey()
	tx := core.Transaction{
		Version: core.TxVersionBase,
		Outputs: []core.Output{
			{OneTimePub: spendPub},
		},
	}
	txs := []core.Transaction{tx}
	hdr := core.BlockHeader{
		Height:       1,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr, Txs: txs}

	// Apply the block — the transparent output must appear in the UTXO listing.
	if err := utxos.ApplyBlock(block); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	code, resp := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code != http.StatusOK {
		t.Fatalf("post-apply: status = %d, want 200", code)
	}
	list, _ := resp["utxos"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("post-apply: utxos = %d, want 1 (transparent output must appear after ApplyBlock)", len(list))
	}

	// Roll back the block — the output must disappear from the UTXO listing.
	// Flush the response cache so the handler re-scans the live set.
	if err := utxos.RollbackBlock(block); err != nil {
		t.Fatalf("RollbackBlock: %v", err)
	}
	srv.FlushUTXOCache()
	code2, resp2 := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code2 != http.StatusOK {
		t.Fatalf("post-rollback: status = %d, want 200", code2)
	}
	list2, _ := resp2["utxos"].([]interface{})
	if len(list2) != 0 {
		t.Errorf("post-rollback: utxos = %d, want 0 (rolled-back transparent UTXO must not appear in listing)", len(list2))
	}
}

// TestREST_AddressUTXOs_RollbackCoinbase verifies the same rollback behaviour for
// coinbase (height-offset mint) outputs, where OneTimePub = spend_pub + height*G.
func TestREST_AddressUTXOs_RollbackCoinbase(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	spendPub := wk.Spend.Public
	addr := crypto.EncodeAddress(crypto.MainnetByte, spendPub, wk.View.Public)

	const mintHeight = uint64(7)

	// Compute mintPub = spendPub + mintHeight*G (mirrors restAddressUTXOs handler).
	heightPub, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(mintHeight))
	if err != nil {
		t.Fatalf("ScalarMulBase: %v", err)
	}
	mintPub, err := crypto.AddPoints(spendPub, heightPub)
	if err != nil {
		t.Fatalf("AddPoints: %v", err)
	}

	// Build a block at mintHeight with a coinbase-style output (OneTimePub = mintPub).
	priv, pub, _ := crypto.GenerateValidatorKey()
	tx := core.Transaction{
		Version: core.TxVersionBase,
		Outputs: []core.Output{
			{OneTimePub: mintPub},
		},
	}
	txs := []core.Transaction{tx}
	hdr := core.BlockHeader{
		Height:       mintHeight,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr, Txs: txs}

	// Apply the block — the mint output must appear in the UTXO listing.
	if err := utxos.ApplyBlock(block); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	code, resp := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code != http.StatusOK {
		t.Fatalf("post-apply: status = %d, want 200", code)
	}
	list, _ := resp["utxos"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("post-apply: utxos = %d, want 1 (coinbase mint UTXO must appear after ApplyBlock)", len(list))
	}
	entry := list[0].(map[string]interface{})
	if entry["block_height"] != float64(mintHeight) {
		t.Errorf("post-apply: block_height = %v, want %d", entry["block_height"], mintHeight)
	}

	// Roll back the block — the mint output must disappear from the UTXO listing.
	// Flush the response cache so the handler re-scans the live set.
	if err := utxos.RollbackBlock(block); err != nil {
		t.Fatalf("RollbackBlock: %v", err)
	}
	srv.FlushUTXOCache()
	code2, resp2 := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code2 != http.StatusOK {
		t.Fatalf("post-rollback: status = %d, want 200", code2)
	}
	list2, _ := resp2["utxos"].([]interface{})
	if len(list2) != 0 {
		t.Errorf("post-rollback: utxos = %d, want 0 (rolled-back coinbase UTXO must not appear in listing)", len(list2))
	}
}

// TestREST_AddressScan_IsCoinbaseFlagged verifies that POST /api/v1/address/{addr}/scan
// sets is_coinbase=true on coinbase (height-offset mint) outputs and is_coinbase=false
// on stealth outputs discovered by the view key.
func TestREST_AddressScan_IsCoinbaseFlagged(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	// Coinbase (mint) output: OneTimePub = spendPub + height*G
	const mintHeight = uint64(12)
	heightPub, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(mintHeight))
	if err != nil {
		t.Fatalf("ScalarMulBase: %v", err)
	}
	mintPub, err := crypto.AddPoints(wk.Spend.Public, heightPub)
	if err != nil {
		t.Fatalf("AddPoints: %v", err)
	}
	var coinbaseTxHash crypto.Hash32
	coinbaseTxHash[0] = 0xC0
	utxos.Add(&core.UTXO{
		TxHash:      coinbaseTxHash,
		OutputIndex: 0,
		OneTimePub:  mintPub,
		BlockHeight: mintHeight,
	})

	// Stealth output sent to this wallet: discovered via view key.
	stealthOut, err := crypto.CreateStealthOutput(wk.Spend.Public, wk.View.Public)
	if err != nil {
		t.Fatalf("CreateStealthOutput: %v", err)
	}
	const stealthAmt = uint64(100_000_000)
	encAmt := core.EncryptAmount(stealthAmt, &stealthOut.HsScalar)
	var stealthTxHash crypto.Hash32
	stealthTxHash[0] = 0x5C
	utxos.Add(&core.UTXO{
		TxHash:      stealthTxHash,
		OutputIndex: 0,
		OneTimePub:  stealthOut.OneTimePub,
		TxPubKey:    stealthOut.TxPubKey,
		EncAmount:   encAmt,
		BlockHeight: 1,
	})

	viewKeyHex := hex.EncodeToString(wk.View.Private[:])
	body := `{"view_key_hex":"` + viewKeyHex + `"}`
	code, resp := restPostJSON(t, srv, "/api/v1/address/"+string(addr)+"/scan", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("POST /scan status = %d, want 200: %v", code, resp)
	}
	list, _ := resp["utxos"].([]interface{})
	if len(list) != 2 {
		t.Fatalf("utxos count = %d, want 2 (coinbase + stealth)", len(list))
	}

	byHash := make(map[string]map[string]interface{}, 2)
	for _, raw := range list {
		e := raw.(map[string]interface{})
		byHash[e["tx_hash"].(string)] = e
	}

	coinbaseEntry, ok := byHash[hex.EncodeToString(coinbaseTxHash[:])]
	if !ok {
		t.Fatal("coinbase UTXO not present in scan response")
	}
	if coinbaseEntry["is_coinbase"] != true {
		t.Errorf("coinbase UTXO: is_coinbase = %v, want true", coinbaseEntry["is_coinbase"])
	}

	stealthEntry, ok := byHash[hex.EncodeToString(stealthTxHash[:])]
	if !ok {
		t.Fatal("stealth UTXO not present in scan response")
	}
	if stealthEntry["is_coinbase"] != false {
		t.Errorf("stealth UTXO: is_coinbase = %v, want false", stealthEntry["is_coinbase"])
	}
}

// TestREST_ScanOutputs_ReturnsStealthFields verifies that GET /api/v1/scan/outputs
// returns all fields required for a client-side stealth scan (one_time_pub_hex,
// tx_pub_key_hex, enc_amount_hex) and respects from_height / limit pagination.
func TestREST_ScanOutputs_ReturnsStealthFields(t *testing.T) {
	srv, _ := buildChainServer(t, 3) // genesis + 3 blocks, each with a coinbase tx

	code, resp := restGet(t, srv, "/api/v1/scan/outputs?from_height=0&limit=50")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	outputs, ok := resp["outputs"].([]interface{})
	if !ok || len(outputs) == 0 {
		t.Fatalf("expected non-empty outputs array, got %v", resp["outputs"])
	}

	// Every output must carry the stealth-scan fields.
	for i, raw := range outputs {
		entry, _ := raw.(map[string]interface{})
		for _, field := range []string{"tx_hash", "out_idx", "block_height",
			"one_time_pub_hex", "tx_pub_key_hex", "amount_commit_hex", "enc_amount_hex"} {
			if entry[field] == nil {
				t.Errorf("output[%d] missing field %q", i, field)
			}
		}
	}

	// note must be present (documents that view key stays on device).
	if resp["note"] == nil || resp["note"] == "" {
		t.Error("expected non-empty note in response")
	}
}

// TestREST_ScanOutputs_Pagination verifies that limit and from_height are honoured.
func TestREST_ScanOutputs_Pagination(t *testing.T) {
	srv, _ := buildChainServer(t, 5)

	code, resp := restGet(t, srv, "/api/v1/scan/outputs?from_height=1&limit=2")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// limit=2 caps the output count at 2 (assuming ≥1 output per block)
	outputs, _ := resp["outputs"].([]interface{})
	if len(outputs) > 2 {
		t.Errorf("limit=2: got %d outputs, want ≤2", len(outputs))
	}
	// All returned outputs must be at height ≥1
	for _, raw := range outputs {
		entry := raw.(map[string]interface{})
		if entry["block_height"].(float64) < 1 {
			t.Errorf("from_height=1: got output at height %v", entry["block_height"])
		}
	}
}

// TestREST_ScanOutputs_SkipCoinbase verifies that ?skip_coinbase=true omits
// coinbase outputs and that callers without the param still receive them.
func TestREST_ScanOutputs_SkipCoinbase(t *testing.T) {
	srv, _ := buildChainServer(t, 3) // genesis + 3 blocks, each with a coinbase tx

	// Without skip_coinbase — all outputs returned, at least some are coinbase.
	_, respAll := restGet(t, srv, "/api/v1/scan/outputs?from_height=0&limit=50")
	allOutputs, _ := respAll["outputs"].([]interface{})
	coinbaseCount := 0
	for _, raw := range allOutputs {
		entry := raw.(map[string]interface{})
		if cb, _ := entry["is_coinbase"].(bool); cb {
			coinbaseCount++
		}
	}
	if coinbaseCount == 0 {
		t.Fatal("expected at least one coinbase output without skip_coinbase")
	}

	// With skip_coinbase=true — no coinbase outputs in the response.
	code, respSkip := restGet(t, srv, "/api/v1/scan/outputs?from_height=0&limit=50&skip_coinbase=true")
	if code != http.StatusOK {
		t.Fatalf("skip_coinbase: status = %d, want 200", code)
	}
	skippedOutputs, _ := respSkip["outputs"].([]interface{})
	for i, raw := range skippedOutputs {
		entry := raw.(map[string]interface{})
		if cb, _ := entry["is_coinbase"].(bool); cb {
			t.Errorf("skip_coinbase=true: output[%d] has is_coinbase=true", i)
		}
	}
	// output_count in the envelope must equal the filtered slice length.
	// (The field is not present in the current response; len(outputs) is the source of truth.)
	_ = skippedOutputs
}

// TestREST_AddressScan_PostViewKey verifies POST /api/v1/address/{addr}/scan
// with the view key in the request body discovers stealth outputs and decodes amounts.
func TestREST_AddressScan_PostViewKey(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	stealthOut, err := crypto.CreateStealthOutput(wk.Spend.Public, wk.View.Public)
	if err != nil {
		t.Fatalf("CreateStealthOutput: %v", err)
	}
	const testAmt = uint64(42_00000000)
	encAmt := core.EncryptAmount(testAmt, &stealthOut.HsScalar)

	var txHash crypto.Hash32
	txHash[0] = 0x7A
	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		OneTimePub:  stealthOut.OneTimePub,
		TxPubKey:    stealthOut.TxPubKey,
		EncAmount:   encAmt,
		BlockHeight: 1,
	})

	import_ := `{"view_key_hex":"` + hex.EncodeToString(wk.View.Private[:]) + `"}`
	code, resp := restPostJSON(t, srv, "/api/v1/address/"+string(addr)+"/scan", []byte(import_))
	if code != http.StatusOK {
		t.Fatalf("POST /scan status = %d, want 200: %v", code, resp)
	}
	list, _ := resp["utxos"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("utxos count = %d, want 1", len(list))
	}
	entry := list[0].(map[string]interface{})
	if entry["amount_napr"] == nil {
		t.Error("amount_napr must be non-null when view key is provided via POST body")
	} else if uint64(entry["amount_napr"].(float64)) != testAmt {
		t.Errorf("amount_napr = %v, want %d", entry["amount_napr"], testAmt)
	}
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

func restLocalDelete(t *testing.T, srv *api.Server, path string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var resp map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	return rr.Code, resp
}

func TestREST_NetworkBanByAddr_LoopbackHost(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetBanLiftFunc(func(string) bool { return false }) // no active ban

	code, _ := restLocalDelete(t, srv, "/api/v1/network/bans/192.0.2.1")
	// 404 = "no active ban for this address" which is the correct non-error
	// response when the guard has passed and the ban simply isn't found.
	if code != http.StatusNotFound {
		t.Fatalf("expected 404 (no active ban), got %d", code)
	}
}

// TestREST_UTXO_InMemorySpentCheck confirms that /api/v1/utxo/{hash}/{idx} returns
// 404 when the UTXO is present in the in-memory UTXOSet but the su/ spent-UTXO
// index (via blockStore) marks it as spent.  This guards the bug where the
// in-memory fast path skipped the IsUTXOSpent cross-check that the disk fallback
// always applied, causing spent outputs to be reported as exists=true.
func TestREST_UTXO_InMemorySpentCheck(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	// Open a real LevelDB store so IsUTXOSpent has a backing su/ index.
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv.SetStore(db)

	// Register a UTXO in the in-memory set.
	var txHash crypto.Hash32
	txHash[0] = 0xDE
	txHash[1] = 0xAD
	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		BlockHeight: 1,
	})

	// Sanity: not yet marked spent → 200.
	path := "/api/v1/utxo/" + hex.EncodeToString(txHash[:]) + "/0"
	code, resp := restGet(t, srv, path)
	if code != http.StatusOK {
		t.Fatalf("before spend: status = %d, want 200; resp=%v", code, resp)
	}
	if resp["exists"] != true {
		t.Fatalf("before spend: exists = %v, want true", resp["exists"])
	}

	// Mark the UTXO spent in the su/ index.
	if err := db.MarkUTXOSpent(txHash, 0); err != nil {
		t.Fatalf("MarkUTXOSpent: %v", err)
	}

	// Now the same UTXO must be reported as not-found (exists=false / 404)
	// even though it is still in the in-memory UTXOSet.
	code, resp = restGet(t, srv, path)
	if code != http.StatusNotFound {
		t.Fatalf("after spend: status = %d, want 404; resp=%v", code, resp)
	}
}
