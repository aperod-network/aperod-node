package api_test

// TestRPC_WalletMaxSpendable_Height0Mints — regression for the "send max"
// insufficient-funds bug (Task: exact available balance).
//
// Scenario: an address holds several legacy admin mints (height=0 semantics:
// TxPubKey = zero, OneTimePub = spendPub — no per-height uniqueness).  All of
// them share one key image, so only the LARGEST can ever be spent, yet a
// DB-summed balance counts them all.  apr_walletMaxSpendable must:
//   1. dedup by OneTimePub and quote max = largest − exact fee, and
//   2. the quoted amount must go through apr_walletSend on the FIRST try.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// rpcCall posts a JSON-RPC request to the server and returns the decoded body.
func rpcCallMax(t *testing.T, srv *api.Server, method string, params interface{}) map[string]interface{} {
	t.Helper()
	paramsJSON, _ := json.Marshal(params)
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  json.RawMessage(paramsJSON),
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("%s: decode JSON-RPC response: %v", method, err)
	}
	return resp
}

func TestRPC_WalletMaxSpendable_Height0Mints(t *testing.T) {
	// Amounts intentionally differ so the mint txs get distinct hashes
	// (identical mint txs share one hash).  The middle value reproduces the
	// production mismatch pattern: DB said 148 858 320, chain held 148 858 315.
	mintAmounts := []uint64{
		14_885_831_500_000_000, // 148 858 315 APRO — the largest, only spendable one
		5_000_000_000_000,
		1_000_000_000,
	}

	aliceKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys (alice): %v", err)
	}
	aliceAddr := crypto.AddressFromKeys(crypto.MainnetByte, aliceKeys)
	bobKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys (bob): %v", err)
	}
	bobAddr := crypto.AddressFromKeys(crypto.MainnetByte, bobKeys)

	valPriv, valPub, _ := crypto.GenerateValidatorKey()

	// ── Chain: genesis + one block holding three height-0 style mints ────────
	// BuildMintTx(addr, amount, 0) → OneTimePub = spendPub for every mint,
	// regardless of the block they land in (legacy admin-mint semantics).
	chain := core.NewChain()
	cb := func() core.Transaction { return core.CoinbaseTx(crypto.Point32(valPub), 1_000_000) }

	makeBlock := func(height uint64, prevHash crypto.Hash32, txs []core.Transaction) *core.Block {
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prevHash,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: valPub,
			MerkleRoot:   core.MerkleRoot(txs),
		}
		if err := hdr.Sign(valPriv); err != nil {
			t.Fatalf("Sign block h=%d: %v", height, err)
		}
		return &core.Block{Header: hdr, Txs: txs}
	}

	genesis := makeBlock(0, crypto.Hash32{}, []core.Transaction{cb()})
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	mintTxs := make([]*core.Transaction, len(mintAmounts))
	blockTxs := []core.Transaction{cb()}
	for i, amt := range mintAmounts {
		mt, err := core.BuildMintTx(aliceAddr, amt, 0)
		if err != nil {
			t.Fatalf("BuildMintTx #%d: %v", i, err)
		}
		mintTxs[i] = mt
		blockTxs = append(blockTxs, *mt)
	}
	block1 := makeBlock(1, genesis.Hash(), blockTxs)
	if err := chain.AddBlock(block1); err != nil {
		t.Fatalf("AddBlock h=1: %v", err)
	}

	// ── UTXOSet: register all mint outputs ───────────────────────────────────
	// All three share OneTimePub, so byPubKey keeps the last Add — add the
	// smaller ones first and the largest LAST so the C-0 check sees the
	// commitment of the UTXO that will actually be spent.
	utxos := core.NewUTXOSet()
	for i := len(mintTxs) - 1; i >= 0; i-- {
		mt := mintTxs[i]
		h := mt.Hash()
		utxos.Add(&core.UTXO{
			TxHash:       h,
			OutputIndex:  0,
			OneTimePub:   mt.Outputs[0].OneTimePub,
			TxPubKey:     mt.Outputs[0].TxPubKey,
			AmountCommit: mt.Outputs[0].AmountCommit,
			BlockHeight:  1,
		})
	}

	mp := core.NewMempool(core.DefaultMempoolConfig())
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())

	// The wallet DB view of the same UTXOs (sums to far more than spendable).
	utxoList := make([]map[string]interface{}, len(mintTxs))
	var naiveTotal uint64
	for i, mt := range mintTxs {
		h := mt.Hash()
		utxoList[i] = map[string]interface{}{
			"tx_hash":     fmt.Sprintf("%x", h[:]),
			"out_idx":     uint32(0),
			"amount_napr": mintAmounts[i],
			"blind_hex":   "", // server derives DeterministicMintBlind
		}
		naiveTotal += mintAmounts[i]
	}

	// ── 1. Quote the max spendable amount ────────────────────────────────────
	resp := rpcCallMax(t, srv, "apr_walletMaxSpendable", map[string]interface{}{
		"spend_key_hex": hex.EncodeToString(aliceKeys.Spend.Private[:]),
		"view_key_hex":  hex.EncodeToString(aliceKeys.View.Private[:]),
		"utxos":         utxoList,
	})
	if errObj, hasErr := resp["error"].(map[string]interface{}); hasErr {
		t.Fatalf("apr_walletMaxSpendable returned error: %v", errObj["message"])
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("no result object: %v", resp)
	}

	maxAmount := uint64(result["max_amount_napr"].(float64))
	fee := uint64(result["fee_napr"].(float64))
	utxoCount := int(result["spendable_utxo_count"].(float64))
	spendableTotal := uint64(result["spendable_total_napr"].(float64))

	if utxoCount != 1 {
		t.Fatalf("spendable_utxo_count = %d, want 1 (three mints share one OneTimePub)", utxoCount)
	}
	if spendableTotal != mintAmounts[0] {
		t.Fatalf("spendable_total_napr = %d, want %d (largest mint only)", spendableTotal, mintAmounts[0])
	}
	if spendableTotal >= naiveTotal {
		t.Fatalf("sanity: spendable total %d should be below the naive DB sum %d", spendableTotal, naiveTotal)
	}
	if maxAmount == 0 || maxAmount+fee != mintAmounts[0] {
		t.Fatalf("max_amount_napr = %d, fee = %d; want max+fee == %d", maxAmount, fee, mintAmounts[0])
	}

	// ── 2. Send exactly the quoted amount — must succeed on the FIRST try ────
	sendResp := rpcCallMax(t, srv, "apr_walletSend", map[string]interface{}{
		"spend_key_hex":  hex.EncodeToString(aliceKeys.Spend.Private[:]),
		"view_key_hex":   hex.EncodeToString(aliceKeys.View.Private[:]),
		"to_address":     bobAddr.String(),
		"change_address": aliceAddr.String(),
		"amount_napr":    maxAmount,
		"utxos":          utxoList,
	})
	if errObj, hasErr := sendResp["error"].(map[string]interface{}); hasErr {
		t.Fatalf("send-max failed on first try: %v", errObj["message"])
	}
	sendResult, ok := sendResp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("send: no result object: %v", sendResp)
	}
	if txh, _ := sendResult["tx_hash"].(string); len(txh) != 64 {
		t.Fatalf("send: tx_hash = %q, want 64-char hex", txh)
	}
	if got := uint64(sendResult["total_fee_napr"].(float64)); got != fee {
		t.Errorf("actual fee %d differs from quoted fee %d", got, fee)
	}
	if got := uint64(sendResult["change_amount_napr"].(float64)); got != 0 {
		t.Errorf("change_amount_napr = %d, want 0 for send-max", got)
	}
	t.Logf("send-max OK: max=%d fee=%d (naive DB balance was %d)", maxAmount, fee, naiveTotal)
}
