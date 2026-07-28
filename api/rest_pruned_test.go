package api_test

// Tests for the on-disk block fallback and pruned-block detection in the REST API.
// Covers three scenarios for /api/v1/blocks/{id} and /api/v1/blocks/{id}/transactions:
//   1. In-memory hit       — already covered by the main REST tests.
//   2. Disk fallback hit   — block not in memory, stored in native core.Block JSON format.
//   3. Pruned block        — block rewritten as StoredBlock JSON by prune.go (TxData = nil).

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// buildServerWithStore creates a server backed by an empty in-memory chain plus
// a temporary LevelDB store wired via SetStore.
func buildServerWithStore(t *testing.T) (*api.Server, *core.Chain, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	srv, chain := buildChainServer(t, 0)
	srv.SetStore(db)
	return srv, chain, db
}

// storeNativeBlock marshals b as core.Block JSON and writes it to the store,
// replicating exactly what cmd/node/main.go storeBlock() does.
func storeNativeBlock(t *testing.T, db *store.DB, b *core.Block) {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("json.Marshal block: %v", err)
	}
	h := b.Hash()
	if err := db.PutRawBlock(h, b.Header.Height, data); err != nil {
		t.Fatalf("PutRawBlock: %v", err)
	}
}

// storePrunedBlock writes a pruned StoredBlock (TxData = nil) replicating
// what store/prune.go PruneBlocksOlderThan() writes after stripping TxData.
func storePrunedBlock(t *testing.T, db *store.DB, b *core.Block, txCount int) {
	t.Helper()
	h := b.Hash()
	sb := &store.StoredBlock{
		Height:    b.Header.Height,
		PrevHash:  b.Header.PrevHash,
		Hash:      h,
		Timestamp: b.Header.Timestamp,
		Round:     b.Header.Round,
		TxCount:   txCount,
		TxData:    nil, // stripped by pruner
	}
	if err := db.PutBlock(h, sb); err != nil {
		t.Fatalf("PutBlock (pruned): %v", err)
	}
}

// makeOffChainBlock creates a signed block that is NOT added to any in-memory chain.
func makeOffChainBlock(t *testing.T, height uint64, prevHash crypto.Hash32) *core.Block {
	t.Helper()
	priv, pub, _ := crypto.GenerateValidatorKey()
	cb := core.CoinbaseTx(crypto.Point32(pub), 500_000)
	txs := []core.Transaction{cb}
	hdr := core.BlockHeader{
		Height:       height,
		PrevHash:     prevHash,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return &core.Block{Header: hdr, Txs: txs}
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestREST_BlockTxs_DiskFallback_NativeFormat verifies that a block evicted
// from the in-memory window but present on disk (native JSON) is served
// correctly with full transaction data and pruned absent/false.
func TestREST_BlockTxs_DiskFallback_NativeFormat(t *testing.T) {
	srv, chain, db := buildServerWithStore(t)

	genesis := chain.GetByHeight(0)
	diskBlock := makeOffChainBlock(t, 1, genesis.Hash())
	storeNativeBlock(t, db, diskBlock)

	// Height 1 is not in the in-memory chain — must fall back to disk.
	code, resp := restGet(t, srv, "/api/v1/blocks/1/transactions")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", code, resp)
	}
	if resp["pruned"] == true {
		t.Error("pruned should be absent or false for a native (non-pruned) block")
	}
	txList, _ := resp["transactions"].([]interface{})
	if len(txList) != 1 {
		t.Errorf("transactions count = %d, want 1 (coinbase)", len(txList))
	}
	if resp["tx_count"] != float64(1) {
		t.Errorf("tx_count = %v, want 1", resp["tx_count"])
	}
}

// TestREST_BlockTxs_DiskFallback_Pruned verifies that a pruned block (TxData
// stripped) returns pruned:true with an empty transactions array and the
// correct original tx_count.
func TestREST_BlockTxs_DiskFallback_Pruned(t *testing.T) {
	srv, chain, db := buildServerWithStore(t)

	genesis := chain.GetByHeight(0)
	ghostBlock := makeOffChainBlock(t, 2, genesis.Hash())
	const originalTxCount = 5
	storePrunedBlock(t, db, ghostBlock, originalTxCount)

	code, resp := restGet(t, srv, "/api/v1/blocks/2/transactions")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", code, resp)
	}
	if resp["pruned"] != true {
		t.Errorf("pruned = %v, want true", resp["pruned"])
	}
	if resp["tx_count"] != float64(originalTxCount) {
		t.Errorf("tx_count = %v, want %d", resp["tx_count"], originalTxCount)
	}
	txList, _ := resp["transactions"].([]interface{})
	if len(txList) != 0 {
		t.Errorf("transactions list length = %d, want 0 (data stripped)", len(txList))
	}
}

// TestREST_BlockDetail_DiskFallback_Pruned verifies that the block detail
// endpoint also returns pruned:true for a pruned StoredBlock.
func TestREST_BlockDetail_DiskFallback_Pruned(t *testing.T) {
	srv, chain, db := buildServerWithStore(t)

	genesis := chain.GetByHeight(0)
	ghostBlock := makeOffChainBlock(t, 7, genesis.Hash())
	storePrunedBlock(t, db, ghostBlock, 2)

	code, resp := restGet(t, srv, "/api/v1/blocks/7")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", code, resp)
	}
	if resp["pruned"] != true {
		t.Errorf("pruned = %v, want true", resp["pruned"])
	}
	if resp["height"] != float64(7) {
		t.Errorf("height = %v, want 7", resp["height"])
	}
	if resp["tx_count"] != float64(2) {
		t.Errorf("tx_count = %v, want 2", resp["tx_count"])
	}
}

// TestREST_BlockDetail_DiskFallback_NativeFormat verifies that the block
// detail endpoint serves full metadata for a non-pruned disk block.
func TestREST_BlockDetail_DiskFallback_NativeFormat(t *testing.T) {
	srv, chain, db := buildServerWithStore(t)

	genesis := chain.GetByHeight(0)
	diskBlock := makeOffChainBlock(t, 3, genesis.Hash())
	storeNativeBlock(t, db, diskBlock)

	code, resp := restGet(t, srv, "/api/v1/blocks/3")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", code, resp)
	}
	if resp["pruned"] == true {
		t.Error("pruned should be absent or false for a native (non-pruned) block")
	}
	if resp["height"] != float64(3) {
		t.Errorf("height = %v, want 3", resp["height"])
	}
	// Validator pub and merkle root should be non-empty for full native blocks.
	if resp["validator_pub"] == "" {
		t.Error("validator_pub should not be empty for a native block")
	}
	if resp["merkle_root"] == "" {
		t.Error("merkle_root should not be empty for a native block")
	}
}

// TestREST_BlockTxs_NotFound returns 404 when a height is absent from both
// the in-memory chain and the disk store.
func TestREST_BlockTxs_NotFound_NeitherMemNorDisk(t *testing.T) {
	srv, _, _ := buildServerWithStore(t)
	code, _ := restGet(t, srv, "/api/v1/blocks/9999/transactions")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}
