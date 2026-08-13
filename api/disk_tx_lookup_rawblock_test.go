package api_test

// TestGetTransactionFromDisk_FindsTxViaRawBlock is the regression guard for
// the GetBlockByHeight vs GetRawBlockByHeight bug.
//
// Bug summary:
//   The original getTransactionFromDisk called GetBlockByHeight, which returns
//   a StoredBlock (pruned JSON shape).  When fed a core.Block JSON payload
//   (written by PutRawBlock), every tx-list field unmarshals as nil, so the
//   method returned "tx not found" for any UTXO confirmed in a block that was
//   evicted from the 1000-block in-memory window.  The fix switches to
//   GetRawBlockByHeight + json.Unmarshal into core.Block.
//
// Test design — why windowSize=4 instead of 1000:
//   Using windowSize=4 lets us evict a tx from the in-memory index by adding
//   only 5 filler blocks instead of 1001.  The underlying disk path exercised
//   is identical; the only difference is scale.  The test is labeled
//   "simulating > 1000 block old UTXO" to document the production invariant.
//
// Critical assertion:
//   The REST endpoint returns the correct block_hash (the full serialised
//   block hash, not a hash of a synthetic stub with only Header.Height set).
//   This assertion would FAIL if GetBlockByHeight were used instead of
//   GetRawBlockByHeight, because a minimal stub serialises differently.

import (
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

// TestGetTransactionFromDisk_FindsTxViaRawBlock confirms that the REST
// /api/v1/transactions/{hash} endpoint correctly resolves a transaction whose
// block has been evicted from the in-memory sliding window (simulating a UTXO
// confirmed in a block > 1000 blocks behind the current chain tip).
//
// Before the fix, GetBlockByHeight was called inside getTransactionFromDisk.
// That returned a StoredBlock, whose TxData is nil when the raw bytes are a
// core.Block JSON payload — causing a silent "tx not found" that surfaced as
// "Balance temporarily unavailable" to the user.
func TestGetTransactionFromDisk_FindsTxViaRawBlock(t *testing.T) {
	// ── Constants ─────────────────────────────────────────────────────────────
	// Use a tiny window to avoid adding 1001 blocks in the test.
	// The disk path exercised is identical to the production 1000-block case.
	const windowSize = uint64(4) // simulating > 1000 block old UTXO (production window = 1000)
	const mintHeight = uint64(1) // height of the block containing the target tx
	const mintAmount = uint64(100_000_000) // 1 APRO

	// ── 1. Keys ───────────────────────────────────────────────────────────────
	aliceKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	aliceAddr := crypto.AddressFromKeys(crypto.MainnetByte, aliceKeys)

	valPriv, valPub, _ := crypto.GenerateValidatorKey()

	// ── 2. Open a temp LevelDB store ─────────────────────────────────────────
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// ── 3. Build the mint tx ──────────────────────────────────────────────────
	mintTx, err := core.BuildMintTx(aliceAddr, mintAmount, mintHeight)
	if err != nil {
		t.Fatalf("BuildMintTx: %v", err)
	}
	mintTxHash := mintTx.Hash()

	// ── 4. Build and persist the chain: genesis + mint block + filler blocks ──
	cb := func() core.Transaction {
		return core.CoinbaseTx(crypto.Point32(valPub), 1_000_000)
	}

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
		storeBlockForTest(t, db, blk)
		if err := db.PutTip(blk.Hash(), height); err != nil {
			t.Fatalf("PutTip h=%d: %v", height, err)
		}
		return blk
	}

	// Use the same chain instance in the server so eviction is directly observed.
	chain := core.NewChain(windowSize)

	genesis := makeAndStore(0, crypto.Hash32{}, []core.Transaction{cb()})
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	// Block 1: coinbase (idx=0) + mint tx (idx=1).
	mintBlock := makeAndStore(mintHeight, genesis.Hash(), []core.Transaction{cb(), *mintTx})
	if err := chain.AddBlock(mintBlock); err != nil {
		t.Fatalf("AddBlock mint block: %v", err)
	}

	// Index the mint tx so getTransactionFromDisk can locate it on disk.
	if err := db.PutTxIdx(mintTxHash, mintHeight, 1); err != nil {
		t.Fatalf("PutTxIdx mint tx: %v", err)
	}

	// Add windowSize+1 filler blocks to evict the mint tx from the in-memory txIndex.
	prev := mintBlock
	for i := mintHeight + 1; i <= mintHeight+windowSize+1; i++ {
		blk := makeAndStore(i, prev.Hash(), []core.Transaction{cb()})
		if err := chain.AddBlock(blk); err != nil {
			t.Fatalf("AddBlock filler h=%d: %v", i, err)
		}
		prev = blk
	}

	// ── 5. Precondition: verify eviction ─────────────────────────────────────
	_, _, inMemory := chain.GetTransaction(mintTxHash)
	if inMemory {
		t.Fatal("precondition failed: mint tx is still in the in-memory txIndex — " +
			"adjust windowSize or filler count so the disk-fallback path is exercised")
	}

	// ── 6. Wire the server with store (activates disk fallback) ───────────────
	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	srv.SetStore(db)

	// ── 7. Call GET /api/v1/transactions/{mintTxHash} ─────────────────────────
	mintTxHashHex := hex.EncodeToString(mintTxHash[:])
	req := httptest.NewRequest(http.MethodGet, "/api/v1/transactions/"+mintTxHashHex, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody: %s\n"+
			"A 404 here means getTransactionFromDisk did not find the tx via "+
			"GetRawBlockByHeight — check that LookupTxIdx and PutRawBlock are "+
			"consistent, and that GetRawBlockByHeight is used (not GetBlockByHeight).",
			rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// ── 8. Assert correct response fields ─────────────────────────────────────

	// block_hash must equal the real hash of the mint block — the full
	// serialised block hash.  Before the fix, GetBlockByHeight returned a
	// StoredBlock with nil TxData; the handler then built a synthetic stub
	// Block{Header: {Height: h}} whose Hash() produced a wrong value.
	wantBlockHash := fmt.Sprintf("%x", mintBlock.Hash())
	if got := resp["block_hash"]; got != wantBlockHash {
		t.Errorf("block_hash = %q\n   want %q\n"+
			"Wrong hash means getTransactionFromDisk used GetBlockByHeight "+
			"(returns StoredBlock with nil TxData) instead of GetRawBlockByHeight "+
			"(returns the raw core.Block JSON payload).",
			got, wantBlockHash)
	}

	if resp["block_height"] != float64(mintHeight) {
		t.Errorf("block_height = %v, want %d", resp["block_height"], mintHeight)
	}

	if resp["hash"] != mintTxHashHex {
		t.Errorf("hash = %v, want %q", resp["hash"], mintTxHashHex)
	}

	// The mint tx has no ring inputs (coinbase-style transparent output).
	if resp["inputs"] != float64(0) {
		t.Errorf("inputs = %v, want 0 (mint tx has no ring inputs)", resp["inputs"])
	}

	if n := resp["outputs"]; n != float64(len(mintTx.Outputs)) {
		t.Errorf("outputs = %v, want %d", n, len(mintTx.Outputs))
	}

	t.Logf("getTransactionFromDisk via GetRawBlockByHeight: tx at height %d found after "+
		"being evicted from in-memory window (windowSize=%d, fillers=%d); "+
		"block_hash=%s (correct)",
		mintHeight, windowSize, windowSize+1, wantBlockHash)
}
