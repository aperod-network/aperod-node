package api_test

// TestREST_Transaction_CoinbaseDiskFallback verifies that a coinbase/admin-mint
// transaction stored in LevelDB (evicted from the in-memory sliding window after
// a node restart) is served correctly by GET /api/v1/transactions/{hash}.
//
// This is the regression test for the block_hash bug: the original
// getTransactionFromDisk built a synthetic TxLocation.Block with only
// Header.Height set.  loc.Block.Hash() then produced a wrong hash because
// Block.Hash() serialises the full block — a minimal stub has different bytes.
// The fix uses the full deserialized block (&b) so Hash() is correct.
//
// The test also confirms is_coinbase, inputs, outputs, fee, and version are
// all correctly populated from the disk payload.

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

// buildCoinbaseBlockInStore creates a temporary LevelDB store, writes one
// genesis block that contains a coinbase transaction, and returns the store,
// the block, and the open store.  The caller owns closing the store.
func buildCoinbaseBlockInStore(t *testing.T) (*store.DB, *core.Block) {
	t.Helper()

	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	priv, pub, _ := crypto.GenerateValidatorKey()
	spendPt := crypto.Point32(pub)

	cb := core.CoinbaseTx(spendPt, 5_000_000) // coinbase, fee=0, 1 output
	txs := []core.Transaction{cb}
	hdr := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	_ = hdr.Sign(priv)
	genesis := &core.Block{Header: hdr, Txs: txs}

	// Write the block to disk exactly as main.go does.
	storeBlockForTest(t, db, genesis)
	if err := db.PutTip(genesis.Hash(), 0); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	// Index the coinbase tx hash so getTransactionFromDisk can find it.
	cbHash := cb.Hash()
	if err := db.PutTxIdx(cbHash, 0, 0); err != nil {
		t.Fatalf("PutTxIdx: %v", err)
	}

	return db, genesis
}

// TestREST_Transaction_CoinbaseDiskFallback is the primary regression test.
func TestREST_Transaction_CoinbaseDiskFallback(t *testing.T) {
	db, genesis := buildCoinbaseBlockInStore(t)

	// ── Simulate a restart: create a fresh chain with no transactions in its
	//    in-memory txIndex.  Only the genesis block is in the chain; the coinbase
	//    tx hash is NOT in the chain's txIndex map.  This forces the disk fallback.
	freshChain := core.NewChain()
	// SetGenesis adds genesis but keeps MaxInMemoryBlocks=1000; for the test we
	// want an empty chain (no txIndex) to force the disk path unconditionally.
	// We skip chain.SetGenesis so txIndex stays empty.

	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()
	srv := api.NewServer(":0", freshChain, mp, utxos, testLogger())
	srv.SetStore(db) // wire the LevelDB store so the disk fallback is active

	// ── Compute expected values from the genesis block ────────────────────────
	expectedBlockHash := fmt.Sprintf("%x", genesis.Hash())
	cbTx := genesis.Txs[0]
	cbHash := cbTx.Hash()
	cbHashHex := hex.EncodeToString(cbHash[:])

	// ── Call the transaction endpoint ─────────────────────────────────────────
	req := httptest.NewRequest(http.MethodGet, "/api/v1/transactions/"+cbHashHex, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// ── Assert all required fields ────────────────────────────────────────────

	// block_hash must equal the real hash of the persisted block, not a hash
	// of a synthetic stub (the root cause of the original bug).
	if got := resp["block_hash"]; got != expectedBlockHash {
		t.Errorf("block_hash = %q, want %q", got, expectedBlockHash)
	}

	if resp["block_height"] != float64(0) {
		t.Errorf("block_height = %v, want 0", resp["block_height"])
	}

	if resp["is_coinbase"] != true {
		t.Errorf("is_coinbase = %v, want true", resp["is_coinbase"])
	}

	if resp["inputs"] != float64(0) {
		t.Errorf("inputs = %v, want 0 (coinbase has no ring inputs)", resp["inputs"])
	}

	if resp["outputs"] != float64(len(cbTx.Outputs)) {
		t.Errorf("outputs = %v, want %d", resp["outputs"], len(cbTx.Outputs))
	}

	if resp["fee"] != float64(cbTx.Fee) {
		t.Errorf("fee = %v, want %d", resp["fee"], cbTx.Fee)
	}

	if _, ok := resp["version"]; !ok {
		t.Error("version field missing from disk-fallback response")
	}

	if resp["hash"] != cbHashHex {
		t.Errorf("hash = %v, want %q", resp["hash"], cbHashHex)
	}
}
