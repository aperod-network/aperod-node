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
// Test design — why each Fallback is blocked except Fallback 2:
//
//   Fallback 1 (mempool):   not seeded — mp is empty.
//   Fallback 2 (PutTxIdx):  ACTIVE — db.PutTxIdx written for the mint tx;
//                            this is the path under test.
//   Fallback 3 (GetUTXO):   blocked — db.PutUTXO is never called, so the u/
//                            prefix has no entry for (mintTxHash, 0).
//   Fallback 4 (UTXOSet):   blocked — the UTXOSet is seeded with a *fake*
//                            txHash for the mint UTXO so s.utxos.Get(mintTxHash, 0)
//                            returns nil, while byPubKey[OneTimePub] still
//                            holds the correct AmountCommit for VerifyTx C-0.
//
// If Fallback 2 is nonfunctional the RPC returns an error; the test fails.
// The in-memory txIndex of the same chain instance is used in the server so
// the eviction precondition directly proves chain.GetTransaction would fail.

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
		// mintAmount must be large enough to cover the ring-CT fee.
		// 500_000_000_000 nAPRO (5 000 APRO) leaves ample headroom.
		mintAmount = uint64(500_000_000_000)
		sendAmount = uint64(1_000_000) // 0.01 APRO — the payment

		// Tiny in-memory window so only windowSize+2 blocks are needed to
		// evict the mint tx rather than the production 1 000.
		windowSize = uint64(3)
		mintHeight = uint64(1) // block height at which the mint tx lands
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
	//   TxPubKey   = zero  (transparent mint)
	//   OneTimePub = spendPub + mintHeight*G
	//   AmountCommit = Commit(mintAmount, DeterministicMintBlind(spendPub, mintAmount))
	mintTx, err := core.BuildMintTx(aliceAddr, mintAmount, mintHeight)
	if err != nil {
		t.Fatalf("BuildMintTx: %v", err)
	}
	mintTxHash := mintTx.Hash()

	// ── 4. Helper: sign and persist a block ──────────────────────────────────
	cb := func() core.Transaction { return core.CoinbaseTx(crypto.Point32(valPub), 1_000_000) }

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
	// Use the same chain instance in the server (step 8) so the eviction of the
	// mint tx from txIndex is directly observed by the RPC handler.
	chain := core.NewChain(windowSize)

	genesis := makeAndStore(t, 0, crypto.Hash32{}, []core.Transaction{cb()})
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	// Block at height 1 contains: coinbase (txIdx=0) + mint tx (txIdx=1).
	block1 := makeAndStore(t, 1, genesis.Hash(), []core.Transaction{cb(), *mintTx})
	if err := chain.AddBlock(block1); err != nil {
		t.Fatalf("AddBlock h=1: %v", err)
	}

	// Index the mint tx in LevelDB so getTransactionFromDisk() can find it.
	// txIdx=1 because mintTx is the second transaction in block1.Txs.
	if err := db.PutTxIdx(mintTxHash, mintHeight, 1); err != nil {
		t.Fatalf("PutTxIdx for mint tx: %v", err)
	}

	// Advance windowSize+1 more blocks to evict the mint tx from txIndex.
	prev := block1
	for i := uint64(2); i <= windowSize+2; i++ {
		blk := makeAndStore(t, i, prev.Hash(), []core.Transaction{cb()})
		if err := chain.AddBlock(blk); err != nil {
			t.Fatalf("AddBlock h=%d: %v", i, err)
		}
		prev = blk
	}

	// ── 6. Precondition: confirm the mint tx is evicted from in-memory index ──
	// chain.GetTransaction returning !ok is what triggers the disk fallback.
	_, _, inMemory := chain.GetTransaction(mintTxHash)
	if inMemory {
		t.Fatal("precondition failed: mint tx is still in the in-memory txIndex — " +
			"adjust windowSize or block count so the disk-fallback path is exercised")
	}

	// ── 7. UTXOSet: seed the C-0 check but block Fallback 4 ──────────────────
	//
	// VerifyTx C-0 needs byPubKey[mintPub] → correct AmountCommit to be present.
	// Fallback 4 (rpc_wallet.go) uses s.utxos.Get(txHash, outIdx) which reads
	// byIndex.  If the real mintTxHash is in byIndex, Fallback 4 succeeds even
	// when Fallback 2 (disk PutTxIdx) is broken — making the test a false positive.
	//
	// Fix: register the mint UTXO under a sentinel fake txHash so byPubKey has
	// the correct commitment (C-0 passes) but s.utxos.Get(mintTxHash, 0) returns
	// nil (Fallback 4 is definitively blocked).
	var fakeTxHash crypto.Hash32
	fakeTxHash[0] = 0xde
	fakeTxHash[1] = 0xad // distinct from mintTxHash; Get(mintTxHash, 0) → nil

	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       fakeTxHash,              // blocks Fallback 4
		OutputIndex:  0,
		OneTimePub:   mintTx.Outputs[0].OneTimePub,
		TxPubKey:     mintTx.Outputs[0].TxPubKey,
		AmountCommit: mintTx.Outputs[0].AmountCommit, // satisfies C-0
		BlockHeight:  mintHeight,
	})

	// ── 8. Create the server with the same (evicted) chain + LevelDB store ────
	// NOTE: db.PutUTXO is intentionally NOT called, so blockStore.GetUTXO
	// (Fallback 3) also returns nil.  The only working fallback is Fallback 2.
	mp := core.NewMempool(core.DefaultMempoolConfig())
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	srv.SetStore(db) // enables getTransactionFromDisk() (Fallback 2)

	// ── 9. Call apr_walletSend via JSON-RPC ──────────────────────────────────
	// The UTXO is a BuildMintTx output (TxPubKey=zero).  Passing blind_hex=""
	// tells the server to derive DeterministicMintBlind(spendPub, mintAmount),
	// which is the same blind BuildMintTx used — the pre-flight commitment check
	// will match and the ring-CT build succeeds.
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
			"With Fallback 3 and Fallback 4 both blocked, only Fallback 2 "+
			"(getTransactionFromDisk via PutTxIdx) can supply the output.\n"+
			"A 're-mint required' error means that path is not working.", msg)
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

	t.Logf("apr_walletSend via disk fallback succeeded: tx_hash=%s "+
		"(mint at height %d evicted from in-memory window=%d; disk PutTxIdx path exercised)",
		txHashStr, mintHeight, windowSize)
}
