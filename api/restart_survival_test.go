package api_test

// TestREST_NetworkStats_TxCountSurvivesRestart verifies that total_txs in
// /api/v1/network/stats is correctly initialised after a node restart.
//
// Regression test for the bug where the count was rebuilt during the
// key-image scan in cmd/node/main.go but never wired to the API server,
// causing every restart to report total_txs: 0.
//
// The test mirrors the startup path in main.go:
//   1. Seed a temp store with genesis + N blocks, each carrying K
//      non-coinbase transactions.
//   2. Run the same counting loop that main.go runs during key-image rebuild.
//   3. Call srv.SetTxTotal(count) — the same call main.go makes.
//   4. Assert GET /api/v1/network/stats returns total_txs == N*K.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// storeBlockForTest serialises b and writes it into db, mirroring the
// storeBlock helper in cmd/node/main.go (which lives in package main and
// cannot be imported by tests).
func storeBlockForTest(t *testing.T, db *store.DB, b *core.Block) {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal block h=%d: %v", b.Header.Height, err)
	}
	hash := b.Hash()
	if err := db.PutRawBlock(hash, b.Header.Height, data); err != nil {
		t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, err)
	}
}

// makeNonCoinbaseTx returns a minimal Transaction that IsCoinbase() == false.
// It carries one dummy RingInput so len(Inputs) > 0.
// No cryptographic validity is required — the counting logic in main.go only
// checks IsCoinbase(), not signatures.
func makeNonCoinbaseTx() core.Transaction {
	var ki crypto.KeyImage
	ki[0] = 0xde
	ki[1] = 0xad
	return core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{KeyImage: ki},
		},
	}
}

// buildSeededStore creates a temporary LevelDB store populated with:
//   - genesis block  (height 0, coinbase-only)
//   - extraBlocks blocks (heights 1..extraBlocks), each with
//     coinbase tx at index 0 PLUS txsPerBlock non-coinbase transactions.
//
// Returns the open store and the chain loaded from it.
func buildSeededStore(
	t *testing.T,
	extraBlocks int,
	txsPerBlock int,
) (*store.DB, *core.Chain) {
	t.Helper()

	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	priv, pub, _ := crypto.GenerateValidatorKey()

	makeBlock := func(height uint64, prevHash crypto.Hash32, extraTxs int) *core.Block {
		cb := core.CoinbaseTx(crypto.Point32(pub), 1_000_000)
		txs := []core.Transaction{cb}
		for i := 0; i < extraTxs; i++ {
			txs = append(txs, makeNonCoinbaseTx())
		}
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

	chain := core.NewChain()

	var prev crypto.Hash32
	genesis := makeBlock(0, prev, 0) // genesis: coinbase only
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	storeBlockForTest(t, db, genesis)
	if err := db.PutTip(genesis.Hash(), 0); err != nil {
		t.Fatalf("PutTip genesis: %v", err)
	}

	for i := 1; i <= extraBlocks; i++ {
		parent := chain.GetByHeight(uint64(i - 1))
		prev = parent.Hash()
		blk := makeBlock(uint64(i), prev, txsPerBlock)
		if err := chain.AddBlock(blk); err != nil {
			t.Fatalf("AddBlock h=%d: %v", i, err)
		}
		storeBlockForTest(t, db, blk)
		if err := db.PutTip(blk.Hash(), uint64(i)); err != nil {
			t.Fatalf("PutTip h=%d: %v", i, err)
		}
	}

	return db, chain
}

// simulateRestartScan mirrors the key-image rebuild loop in
// cmd/node/main.go (the loop that populates initialTxTotal).
// It reads every block from height 1..tipHeight out of the store and
// counts non-coinbase transactions exactly as main.go does.
func simulateRestartScan(t *testing.T, db *store.DB, tipHeight uint64) int64 {
	t.Helper()
	var total int64
	for h := uint64(1); h <= tipHeight; h++ {
		raw, err := db.GetRawBlockByHeight(h)
		if err != nil || raw == nil {
			t.Fatalf("GetRawBlockByHeight h=%d: %v", h, err)
		}
		var b core.Block
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("unmarshal block h=%d: %v", h, err)
		}
		for txIdx, tx := range b.Txs {
			if !(txIdx == 0 && tx.IsCoinbase()) {
				total++
			}
		}
	}
	return total
}

// TestREST_NetworkStats_TxCountSurvivesRestart is the primary regression test.
func TestREST_NetworkStats_TxCountSurvivesRestart(t *testing.T) {
	const (
		extraBlocks = 3
		txsPerBlock = 2
		wantTotal   = extraBlocks * txsPerBlock // 6
	)

	db, chain := buildSeededStore(t, extraBlocks, txsPerBlock)

	// ── Simulate node restart: run the counting scan ──────────────────────────
	tipHeight := chain.Height() // == extraBlocks
	counted := simulateRestartScan(t, db, tipHeight)

	if counted != int64(wantTotal) {
		t.Fatalf("restart scan: got %d non-coinbase txs, want %d", counted, wantTotal)
	}

	// ── Wire counted total into the API server (exactly as main.go does) ─────
	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	srv.SetTxTotal(counted) // ← the call that was missing before the fix

	// ── Assert the REST endpoint reports the correct total ────────────────────
	req := httptest.NewRequest(http.MethodGet, "/api/v1/network/stats", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	got, ok := resp["total_txs"]
	if !ok {
		t.Fatal("response missing total_txs field")
	}
	if got != float64(wantTotal) {
		t.Errorf("total_txs = %v, want %d", got, wantTotal)
	}
}

// TestREST_NetworkStats_TxCountZeroWithoutSetTxTotal confirms that total_txs
// is 0 when SetTxTotal is never called — baseline for the above test.
func TestREST_NetworkStats_TxCountZeroWithoutSetTxTotal(t *testing.T) {
	const extraBlocks = 3

	_, chain := buildSeededStore(t, extraBlocks, 2)

	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	// Intentionally do NOT call srv.SetTxTotal(...)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/network/stats", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["total_txs"] != float64(0) {
		t.Errorf("total_txs = %v, want 0 (SetTxTotal never called)", resp["total_txs"])
	}
}

// TestREST_NetworkStats_TxCountIncrements verifies that AddTxCount increments
// the live counter, i.e. new blocks produced after startup are reflected.
func TestREST_NetworkStats_TxCountIncrements(t *testing.T) {
	const (
		initialTotal int64 = 10
		delta        int64 = 3
	)

	_, chain := buildSeededStore(t, 1, 0)

	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	srv.SetTxTotal(initialTotal)
	srv.AddTxCount(delta) // simulates new block produced after startup

	req := httptest.NewRequest(http.MethodGet, "/api/v1/network/stats", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	want := float64(initialTotal + delta)
	if resp["total_txs"] != want {
		t.Errorf("total_txs = %v, want %v", resp["total_txs"], want)
	}
}
