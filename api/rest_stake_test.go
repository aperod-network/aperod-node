package api_test

// Integration tests for the new stake-registration REST endpoints:
//   GET  /api/v1/utxo/{txhash}/{idx}  — UTXO AmountCommit lookup
//   POST /api/v1/stake                 — broadcast pre-signed v2 stake deposit

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// buildStakeServer creates a minimal server with an empty chain, mempool, and
// a pre-populated UTXOSet containing one known UTXO.
// Returns the server, the UTXO's tx hash, output index, and its AmountCommit.
func buildStakeServer(t *testing.T) (
	srv *api.Server,
	utxoTxHash crypto.Hash32,
	outIdx uint32,
	commit crypto.Commitment,
) {
	t.Helper()

	chain := core.NewChain()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()

	// Synthesise a deterministic UTXO (amount = 100 000 APRO = 10_000_000_000_000 nAPRO).
	const amount uint64 = 10_000_000_000_000
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	_ = priv // kept for the signature helper below

	// Derive the deterministic blind the same way the CLI does.
	var spendPub crypto.Point32
	copy(spendPub[:], []byte(pub))
	blind, err := crypto.DeterministicMintBlind(spendPub, amount)
	if err != nil {
		t.Fatalf("DeterministicMintBlind: %v", err)
	}
	c, err := crypto.Commit(amount, blind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Build a fake UTXO key (txhash = 32-byte sequence of 0xAB, outIdx = 0).
	var txHash crypto.Hash32
	for i := range txHash {
		txHash[i] = 0xAB
	}
	outIdx = 0

	utxo := &core.UTXO{
		TxHash:       txHash,
		OutputIndex:  outIdx,
		OneTimePub:   spendPub,
		AmountCommit: c,
	}
	utxos.Add(utxo)

	srv = api.NewServer(":0", chain, mp, utxos, testLogger())
	return srv, txHash, outIdx, c
}

// makeV2StakeExtra builds a structurally-valid 173-byte v2 stake extra payload
// signed by priv.  The UTXO commit does not need to match anything in the test
// UTXOSet — mempool.Add does not do crypto verification in tests (nil verifier).
func makeV2StakeExtra(t *testing.T) ([]byte, crypto.ValidatorPubKey) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	const amount uint64 = 10_000_000_000_000
	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0xAB
	}
	const burnOutIdx uint32 = 0

	msg := core.StakeSignMsgV2(core.StakeDeposit, pub, amount, burnTxHash, burnOutIdx)
	sig, err := priv.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	var spendPub crypto.Point32
	copy(spendPub[:], []byte(pub))
	blind, err := crypto.DeterministicMintBlind(spendPub, amount)
	if err != nil {
		t.Fatalf("DeterministicMintBlind: %v", err)
	}

	extra, err := core.EncodeStakeExtraV2(
		core.StakeDeposit, pub, amount, sig,
		burnTxHash, burnOutIdx, blind,
	)
	if err != nil {
		t.Fatalf("EncodeStakeExtraV2: %v", err)
	}
	return extra, pub
}

func restPost(t *testing.T, srv *api.Server, path string, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var resp map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	return rr.Code, resp
}

// ─── GET /api/v1/utxo/{txhash}/{idx} ─────────────────────────────────────────

func TestREST_UTXO_Found(t *testing.T) {
	srv, txHash, outIdx, commit := buildStakeServer(t)

	path := fmt.Sprintf("/api/v1/utxo/%x/%d", txHash[:], outIdx)
	code, resp := restGet(t, srv, path)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; resp = %v", code, resp)
	}
	commitHex, _ := resp["amount_commit_hex"].(string)
	want := fmt.Sprintf("%x", commit[:])
	if commitHex != want {
		t.Errorf("amount_commit_hex = %q, want %q", commitHex, want)
	}
	if exists, _ := resp["exists"].(bool); !exists {
		t.Errorf("exists = false, want true")
	}
}

func TestREST_UTXO_NotFound(t *testing.T) {
	srv, _, _, _ := buildStakeServer(t)

	var missing crypto.Hash32
	path := fmt.Sprintf("/api/v1/utxo/%x/0", missing[:])
	code, resp := restGet(t, srv, path)

	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; resp = %v", code, resp)
	}
}

func TestREST_UTXO_BadTxHash(t *testing.T) {
	srv, _, _, _ := buildStakeServer(t)

	code, resp := restGet(t, srv, "/api/v1/utxo/notahex/0")
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; resp = %v", code, resp)
	}
}

func TestREST_UTXO_BadIdx(t *testing.T) {
	srv, txHash, _, _ := buildStakeServer(t)

	path := fmt.Sprintf("/api/v1/utxo/%x/notanumber", txHash[:])
	code, resp := restGet(t, srv, path)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; resp = %v", code, resp)
	}
}

func TestREST_UTXO_MissingIdx(t *testing.T) {
	srv, txHash, _, _ := buildStakeServer(t)

	// Path without /idx component.
	path := fmt.Sprintf("/api/v1/utxo/%x", txHash[:])
	code, _ := restGet(t, srv, path)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

func TestREST_UTXO_MethodNotAllowed(t *testing.T) {
	srv, txHash, outIdx, _ := buildStakeServer(t)

	path := fmt.Sprintf("/api/v1/utxo/%x/%d", txHash[:], outIdx)
	req := httptest.NewRequest(http.MethodPost, path, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

// ─── POST /api/v1/stake ───────────────────────────────────────────────────────

func TestREST_StakeBroadcast_ValidPayload(t *testing.T) {
	srv, _, _, _ := buildStakeServer(t)

	extra, _ := makeV2StakeExtra(t)
	body := fmt.Sprintf(`{"tx_extra_hex":%q}`, hex.EncodeToString(extra))

	code, resp := restPost(t, srv, "/api/v1/stake", body)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; resp = %v", code, resp)
	}
	txHashHex, _ := resp["tx_hash"].(string)
	if len(txHashHex) != 64 {
		t.Errorf("tx_hash = %q, want 64 hex chars", txHashHex)
	}
	if status, _ := resp["status"].(string); status != "pending" {
		t.Errorf("status = %q, want \"pending\"", status)
	}
}

func TestREST_StakeBroadcast_DuplicateRejected(t *testing.T) {
	srv, _, _, _ := buildStakeServer(t)

	extra, _ := makeV2StakeExtra(t)
	body := fmt.Sprintf(`{"tx_extra_hex":%q}`, hex.EncodeToString(extra))

	// First submission should succeed.
	code, _ := restPost(t, srv, "/api/v1/stake", body)
	if code != http.StatusCreated {
		t.Fatalf("first POST status = %d, want 201", code)
	}
	// Second identical submission must fail (duplicate or same sender pending).
	code2, resp2 := restPost(t, srv, "/api/v1/stake", body)
	if code2 == http.StatusCreated {
		t.Errorf("duplicate stake tx was accepted; resp = %v", resp2)
	}
}

func TestREST_StakeBroadcast_NotHex(t *testing.T) {
	srv, _, _, _ := buildStakeServer(t)

	code, resp := restPost(t, srv, "/api/v1/stake", `{"tx_extra_hex":"not-hex"}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; resp = %v", code, resp)
	}
}

func TestREST_StakeBroadcast_WrongSize(t *testing.T) {
	srv, _, _, _ := buildStakeServer(t)

	// 105-byte v1 payload should be rejected (broadcast only accepts 173-byte v2).
	v1payload := bytes.Repeat([]byte{0x01}, core.StakePayloadSize)
	body := fmt.Sprintf(`{"tx_extra_hex":%q}`, hex.EncodeToString(v1payload))

	code, resp := restPost(t, srv, "/api/v1/stake", body)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; resp = %v", code, resp)
	}
}

func TestREST_StakeBroadcast_MethodNotAllowed(t *testing.T) {
	srv, _, _, _ := buildStakeServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stake", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestREST_StakeBroadcast_InvalidJSON(t *testing.T) {
	srv, _, _, _ := buildStakeServer(t)

	code, resp := restPost(t, srv, "/api/v1/stake", `{bad json}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; resp = %v", code, resp)
	}
}

// ─── Mempool accepts v2 stake tx (zero-input exemption) ──────────────────────

// TestMempool_StakeV2_Accepted verifies that the mempool coinbase-rejection
// guard does not fire for TxVersionStake (zero-input) transactions.
func TestMempool_StakeV2_Accepted(t *testing.T) {
	extra, _ := makeV2StakeExtra(t)
	tx := core.Transaction{
		Version: core.TxVersionStake,
		Extra:   extra,
	}
	mp := core.NewMempool(core.DefaultMempoolConfig())
	if err := mp.Add(tx); err != nil {
		t.Fatalf("mempool.Add v2 stake tx: %v", err)
	}
	if mp.Count() != 1 {
		t.Errorf("mempool.Count() = %d, want 1", mp.Count())
	}
}

// TestMempool_StakeV2_SenderTracked verifies that the per-pubkey rate-limit
// correctly rejects a second v2 deposit from the same validator.
func TestMempool_StakeV2_SenderTracked(t *testing.T) {
	extra, _ := makeV2StakeExtra(t)
	tx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	mp := core.NewMempool(core.DefaultMempoolConfig())
	if err := mp.Add(tx); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Build a second tx with a slightly different blind so the hash differs
	// but the pubkey is the same — this should be rejected by the rate limiter.
	extra2 := make([]byte, len(extra))
	copy(extra2, extra)
	extra2[140] ^= 0xFF // flip a byte in the blind section
	tx2 := core.Transaction{Version: core.TxVersionStake, Extra: extra2}

	if err := mp.Add(tx2); err == nil {
		t.Error("expected second stake tx from same pubkey to be rejected, but it was accepted")
	}
}
