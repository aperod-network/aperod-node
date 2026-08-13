package api_test

// TestRPC_WalletSend_InMemoryUTXOFallback confirms that apr_walletSend
// completes a transfer when the UTXO's LevelDB u/ record was lost after an
// OOM+repair cycle, but the in-memory UTXOSet still holds the entry.
//
// This exercises Fallback 4 in rpc_wallet.go — the path that calls
// s.utxos.Get(txHash, outIdx) when both the tx-hash index (PutTxIdx / Fallback
// 2) and the UTXO store (GetUTXO / Fallback 3) have no entry for the UTXO.
//
// Production scenario:
//   An OOM kill corrupts the LevelDB UTXO store (u/ prefix entries deleted by
//   --repair-db), but the startup snapshot loaded into s.utxos still carries
//   the UTXO.  Without Fallback 4 the wallet returns
//   "tx not found on chain or mempool — re-mint required after node restart".
//
// Why each Fallback is blocked except Fallback 4:
//
//   Fallback 1a (chain.GetTransaction): blocked — the mint tx is evicted from
//       the in-memory txIndex by adding windowSize+2 filler blocks.
//   Fallback 1b (mempool):              blocked — mempool is empty.
//   Fallback 2  (PutTxIdx disk):        blocked — db.PutTxIdx is never called
//       for the mint tx, so LookupTxIdx returns (0, 0, false).
//   Fallback 3  (GetUTXO):             blocked — db.PutUTXO is never called,
//       so blockStore.GetUTXO returns (nil, nil).
//   Fallback 4  (s.utxos.Get):         ACTIVE — s.utxos is seeded with the
//       real mintTxHash so Get(mintTxHash, 0) returns non-nil.
//
// Fallback 4 then reads the raw block from LevelDB to obtain the authoritative
// AmountCommit before building the ring-CT transaction.  The raw block IS
// stored in LevelDB (via storeBlockForTest) so that disk read succeeds.
//
// If Fallback 4 is nonfunctional (or regressed to return an error), the RPC
// returns "re-mint required" and the test fails.

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
	"github.com/aperod/aperod/store"
)

func TestRPC_WalletSend_InMemoryUTXOFallback(t *testing.T) {
	// ── Constants ─────────────────────────────────────────────────────────────
	const (
		// mintAmount must be large enough to cover the ring-CT fee.
		mintAmount = uint64(500_000_000_000) // 5 000 APRO in nAPRO
		sendAmount = uint64(1_000_000)       // 0.01 APRO — the payment

		// Tiny window so eviction only requires a handful of filler blocks.
		windowSize = uint64(3)
		mintHeight = uint64(1)
	)

	// ── 1. Keys ───────────────────────────────────────────────────────────────
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

	// ── 2. Open a temp LevelDB store ─────────────────────────────────────────
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// ── 3. Build the mint tx for Alice at height=mintHeight ──────────────────
	// BuildMintTx produces a transparent mint output:
	//   TxPubKey   = zero (transparent)
	//   OneTimePub = spendPub + mintHeight*G
	//   AmountCommit = Commit(mintAmount, DeterministicMintBlind(spendPub, mintAmount))
	mintTx, err := core.BuildMintTx(aliceAddr, mintAmount, mintHeight)
	if err != nil {
		t.Fatalf("BuildMintTx: %v", err)
	}
	mintTxHash := mintTx.Hash()

	// ── 4. Build and store the chain: genesis + mint block + filler blocks ───
	cb := func() core.Transaction { return core.CoinbaseTx(crypto.Point32(valPub), 1_000_000) }

	makeAndStore := func(height uint64, prevHash crypto.Hash32, txs []core.Transaction) *core.Block {
		t.Helper()
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
		blk := &core.Block{Header: hdr, Txs: txs}
		// Store the raw block so Fallback 4's GetRawBlockByHeight succeeds.
		storeBlockForTest(t, db, blk)
		if err := db.PutTip(blk.Hash(), height); err != nil {
			t.Fatalf("PutTip h=%d: %v", height, err)
		}
		return blk
	}

	chain := core.NewChain(windowSize)

	genesis := makeAndStore(0, crypto.Hash32{}, []core.Transaction{cb()})
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	// Block 1: coinbase (idx=0) + mint tx (idx=1).
	mintBlock := makeAndStore(mintHeight, genesis.Hash(), []core.Transaction{cb(), *mintTx})
	if err := chain.AddBlock(mintBlock); err != nil {
		t.Fatalf("AddBlock h=1: %v", err)
	}

	// Intentionally skip db.PutTxIdx(mintTxHash, ...) — Fallback 2 must fail.
	// Intentionally skip db.PutUTXO(...) — Fallback 3 must return (nil, nil).

	// Advance windowSize+1 more blocks so the mint tx is evicted from txIndex.
	prev := mintBlock
	for i := mintHeight + 1; i <= mintHeight+windowSize+1; i++ {
		blk := makeAndStore(i, prev.Hash(), []core.Transaction{cb()})
		if err := chain.AddBlock(blk); err != nil {
			t.Fatalf("AddBlock h=%d: %v", i, err)
		}
		prev = blk
	}

	// ── 5. Precondition: confirm the mint tx is evicted from in-memory index ──
	_, _, inMemory := chain.GetTransaction(mintTxHash)
	if inMemory {
		t.Fatal("precondition failed: mint tx still in in-memory txIndex — " +
			"adjust windowSize or filler count so the Fallback 4 path is exercised")
	}

	// ── 6. Seed UTXOSet with the real mintTxHash — enabling Fallback 4 ───────
	//
	// s.utxos.Get(mintTxHash, 0) must return non-nil for Fallback 4 to fire.
	// We register the UTXO under the real mintTxHash so Get() finds it.
	//
	// Note: byPubKey[OneTimePub] is populated by Add() and is also used by
	// VerifyTx C-0 to confirm the AmountCommit.  We set the correct AmountCommit
	// from the actual mint output so both checks pass.
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       mintTxHash,                        // real hash — Get() returns this entry
		OutputIndex:  0,
		OneTimePub:   mintTx.Outputs[0].OneTimePub,
		TxPubKey:     mintTx.Outputs[0].TxPubKey,
		AmountCommit: mintTx.Outputs[0].AmountCommit,   // correct — satisfies VerifyTx C-0
		BlockHeight:  mintHeight,                        // used by Fallback 4 to call GetRawBlockByHeight
	})

	// ── 7. Create the server with the chain + LevelDB + seeded UTXOSet ───────
	// SetStore wires blockStore so Fallback 3 and Fallback 4 are reachable.
	// Fallback 3 returns (nil, nil) because PutUTXO was never called.
	// Fallback 4 then fires because utxos.Get(mintTxHash, 0) returns non-nil.
	mp := core.NewMempool(core.DefaultMempoolConfig())
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	srv.SetStore(db)

	// ── 8. Call apr_walletSend via JSON-RPC ──────────────────────────────────
	// blind_hex="" tells the server to derive DeterministicMintBlind(spendPub, mintAmount),
	// matching what BuildMintTx used; the pre-flight commitment check will pass.
	params := map[string]interface{}{
		"spend_key_hex":  hex.EncodeToString(aliceKeys.Spend.Private[:]),
		"view_key_hex":   hex.EncodeToString(aliceKeys.View.Private[:]),
		"to_address":     bobAddr.String(),
		"change_address": aliceAddr.String(),
		"amount_napr":    sendAmount,
		"utxos": []map[string]interface{}{
			{
				"tx_hash":     fmt.Sprintf("%x", mintTxHash[:]),
				"out_idx":     uint32(0),
				"amount_napr": mintAmount,
				"blind_hex":   "", // server derives DeterministicMintBlind
			},
		},
	}

	paramsJSON, _ := json.Marshal(params)
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "apr_walletSend",
		"params":  json.RawMessage(paramsJSON),
	})

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode JSON-RPC response: %v", err)
	}

	// ── 9. Assert no error and valid tx_hash ──────────────────────────────────
	if errObj, hasErr := resp["error"].(map[string]interface{}); hasErr {
		msg := fmt.Sprintf("%v", errObj["message"])
		t.Fatalf("apr_walletSend returned error: %s\n"+
			"With Fallbacks 1-3 all blocked, only Fallback 4 (s.utxos.Get) can "+
			"supply the output.  A 're-mint required' error means Fallback 4 is "+
			"not firing or GetRawBlockByHeight failed to find the block.", msg)
	}

	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("response has no 'result' object: %v", resp)
	}

	txHashStr, _ := result["tx_hash"].(string)
	if len(txHashStr) != 64 {
		t.Errorf("tx_hash = %q, want 64-char hex string", txHashStr)
	}

	if result["payment_amount_napr"] != float64(sendAmount) {
		t.Errorf("payment_amount_napr = %v, want %d", result["payment_amount_napr"], sendAmount)
	}

	t.Logf("apr_walletSend via in-memory UTXOSet fallback (Fallback 4) succeeded: "+
		"tx_hash=%s (mint at height %d; u/ entry absent from LevelDB; "+
		"t/ index absent; resolved from s.utxos + raw block disk read)",
		txHashStr, mintHeight)
}
