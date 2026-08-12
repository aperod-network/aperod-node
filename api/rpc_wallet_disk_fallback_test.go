package api_test

// TestRPC_WalletSend_DiskFallbackForOldUTXO confirms that apr_walletSend never
// returns "re-mint required" when the source UTXO lives in a block that has
// been evicted from the in-memory txIndex.
//
// Context:
//   The Chain keeps only the last MaxInMemoryBlocks transactions in its
//   in-memory txIndex.  Any UTXO from an older block causes
//   chain.GetTransaction to return !ok.  Without the disk fallback that
//   triggers getTransactionFromDisk(), the wallet would see:
//     "tx not found on chain or mempool — re-mint required after node restart"
//
//   This test sets the in-memory window to 3 blocks and mints a UTXO at
//   height 1, then advances 4 more blocks to push the mint tx out of the
//   window.  The mint tx IS indexed in LevelDB via PutTxIdx.  A fresh server
//   backed by that LevelDB store must succeed on apr_walletSend — exercising
//   getTransactionFromDisk() to locate the tx.
//
// Pass/fail criteria:
//   - apr_walletSend succeeds (no error, tx_hash in result)
//   - No "re-mint required" in the error path
//   - The full chain.GetTransaction returns !ok before the call (precondition)
//     — proving that only the disk fallback can supply the tx

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

func TestRPC_WalletSend_DiskFallbackForOldUTXO(t *testing.T) {
	// ── Constants ─────────────────────────────────────────────────────────────
	const (
		// mintAmount must be large enough to cover the ring-CT fee.  The fee
		// depends on the serialised tx size; 500_000_000_000 nAPRO (5000 APRO)
		// leaves ample headroom.
		mintAmount = uint64(500_000_000_000)
		sendAmount = uint64(1_000_000) // 0.01 APRO — the payment

		// Use a tiny in-memory window so only 4 extra blocks are needed to
		// evict the mint tx instead of the production 1 000.
		windowSize = uint64(3)
		mintHeight = uint64(1) // height at which the mint tx lands
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
	// BuildMintTx produces:
	//   TxPubKey  = zero   (transparent mint)
	//   OneTimePub = spendPub + mintHeight*G
	//   AmountCommit = Commit(mintAmount, DeterministicMintBlind(spendPub, mintAmount))
	mintTx, err := core.BuildMintTx(aliceAddr, mintAmount, mintHeight)
	if err != nil {
		t.Fatalf("BuildMintTx: %v", err)
	}
	mintTxHash := mintTx.Hash()

	// ── 4. Helper: sign and store a block ─────────────────────────────────────
	makeAndStore := func(t *testing.T, height uint64, prevHash crypto.Hash32, txs []core.Transaction) *core.Block {
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
		storeBlockForTest(t, db, blk)
		if err := db.PutTip(blk.Hash(), height); err != nil {
			t.Fatalf("PutTip h=%d: %v", height, err)
		}
		return blk
	}

	// ── 5. Build a chain: genesis + mint block + windowSize+1 empty blocks ────
	chain := core.NewChain(windowSize)

	cb := func() core.Transaction { return core.CoinbaseTx(crypto.Point32(valPub), 1_000_000) }

	genesis := makeAndStore(t, 0, crypto.Hash32{}, []core.Transaction{cb()})
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	// Block at height 1 contains: coinbase (idx=0) + mint tx (idx=1).
	block1 := makeAndStore(t, 1, genesis.Hash(), []core.Transaction{cb(), *mintTx})
	if err := chain.AddBlock(block1); err != nil {
		t.Fatalf("AddBlock h=1: %v", err)
	}

	// Index the mint tx so getTransactionFromDisk() can find it.
	// mintTx is at position 1 within block1.Txs.
	if err := db.PutTxIdx(mintTxHash, mintHeight, 1); err != nil {
		t.Fatalf("PutTxIdx for mint tx: %v", err)
	}

	// Advance windowSize+1 blocks to evict the mint tx from the in-memory window.
	prev := block1
	for i := uint64(2); i <= windowSize+2; i++ {
		blk := makeAndStore(t, i, prev.Hash(), []core.Transaction{cb()})
		if err := chain.AddBlock(blk); err != nil {
			t.Fatalf("AddBlock h=%d: %v", i, err)
		}
		prev = blk
	}

	// ── 6. Precondition: mint tx must NOT be in the in-memory txIndex ─────────
	_, _, inMemory := chain.GetTransaction(mintTxHash)
	if inMemory {
		t.Fatal("precondition failed: mint tx is still in the in-memory index — " +
			"the disk-fallback path would not be exercised; adjust windowSize or block count")
	}

	// ── 7. UTXOSet: register the mint UTXO so VerifyTx C-0 can validate it ───
	// ApplyBlock adds every output from block1 (coinbase + mint) to byPubKey.
	// The verifier only needs the real spender's AmountCommit to be present.
	utxos := core.NewUTXOSet()
	if err := utxos.ApplyBlock(block1); err != nil {
		t.Fatalf("ApplyBlock mint block: %v", err)
	}

	// ── 8. Create a fresh server backed by the LevelDB store ─────────────────
	// The fresh chain has no genesis set, so its txIndex is completely empty.
	// Every tx lookup MUST go through the disk fallback.
	freshChain := core.NewChain(windowSize)
	mp := core.NewMempool(core.DefaultMempoolConfig())
	srv := api.NewServer(":0", freshChain, mp, utxos, testLogger())
	srv.SetStore(db) // wire LevelDB store → enables getTransactionFromDisk()

	// ── 9. Call apr_walletSend via JSON-RPC ──────────────────────────────────
	// The UTXO is a BuildMintTx output: TxPubKey=zero, blind_hex="" means the
	// server will derive DeterministicMintBlind(spendPub, mintAmount) — the same
	// blind that BuildMintTx used — and the pre-flight commitment check passes.
	params := map[string]interface{}{
		"spend_key_hex":  hex.EncodeToString(aliceKeys.Spend.Private[:]),
		"view_key_hex":   hex.EncodeToString(aliceKeys.View.Private[:]),
		"to_address":     bobAddr.String(),
		"change_address": aliceAddr.String(),
		"amount_napr":    sendAmount,
		"utxos": []map[string]interface{}{
			{
				"tx_hash":     fmt.Sprintf("%x", mintTxHash[:]),
				"out_idx":     uint32(0), // mintTx has one output at index 0
				"amount_napr": mintAmount,
				"blind_hex":   "", // derive DeterministicMintBlind on the server
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

	// ── 10. Assert no error and valid tx_hash ─────────────────────────────────
	if errObj, hasErr := resp["error"].(map[string]interface{}); hasErr {
		msg := fmt.Sprintf("%v", errObj["message"])
		t.Fatalf("apr_walletSend returned error: %s\n"+
			"A 're-mint required' message means the disk fallback (getTransactionFromDisk) "+
			"did not supply the tx even though PutTxIdx was written for it.", msg)
	}

	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("response has no 'result' object: %v", resp)
	}

	txHashStr, _ := result["tx_hash"].(string)
	if len(txHashStr) != 64 {
		t.Errorf("tx_hash = %q, want 64-char hex string", txHashStr)
	}

	// Confirm the payment amount round-trips correctly.
	if result["payment_amount_napr"] != float64(sendAmount) {
		t.Errorf("payment_amount_napr = %v, want %d", result["payment_amount_napr"], sendAmount)
	}

	t.Logf("apr_walletSend via disk fallback succeeded: tx_hash=%s "+
		"(mint was at height %d, in-memory window=%d, chain tip=%d)",
		txHashStr, mintHeight, windowSize, windowSize+2)
}
