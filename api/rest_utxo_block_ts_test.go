package api_test

// Tests for block_height / block_timestamp fields on GET /api/v1/utxo/:hash/:idx.
//
//  1. In-memory block — timestamp comes from chain.GetByHeight.
//  2. Block on disk (native core.Block JSON) — timestamp comes from blockStore.
//  3. Pruned StoredBlock on disk — timestamp comes from StoredBlock.Timestamp.
//  4. No block available — block_height present, block_timestamp absent.

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

// addUTXOAtHeight adds a minimal UTXO to the set at the given block height and
// returns the tx hash and the hex string of the commit for request building.
func addUTXOAtHeight(utxos *core.UTXOSet, txHashByte byte, height uint64) (crypto.Hash32, string) {
	var txHash crypto.Hash32
	txHash[0] = txHashByte
	var commit [32]byte
	commit[0] = 0xAB
	utxos.Add(&core.UTXO{
		TxHash:        txHash,
		OutputIndex:   0,
		AmountCommit:  commit,
		BlockHeight:   height,
	})
	return txHash, hex.EncodeToString(commit[:])
}

// getUTXOResp hits GET /api/v1/utxo/<hash>/<idx> and returns the parsed body.
func getUTXOResp(t *testing.T, srv *api.Server, txHash crypto.Hash32) (int, map[string]interface{}) {
	t.Helper()
	path := fmt.Sprintf("/api/v1/utxo/%x/0", txHash)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	srv.ServeHTTP(rec, req)
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// makeBlockAtHeight creates a signed block at the given height with a known
// timestamp (Unix nanoseconds) and returns the block.
func makeBlockAtHeight(t *testing.T, height uint64, ts int64) *core.Block {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	hdr := core.BlockHeader{
		Height:       height,
		Timestamp:    ts,
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return &core.Block{Header: hdr}
}

// ── Test 1: block is in the in-memory chain ───────────────────────────────────

func TestUTXO_BlockTimestamp_FromMemory(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	const height = uint64(0)
	txHash, _ := addUTXOAtHeight(utxos, 0x01, height)

	// Height 0 = genesis block, already in chain (buildUTXOServer sets genesis).
	code, body := getUTXOResp(t, srv, txHash)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, body)
	}
	if bh, ok := body["block_height"]; !ok || bh == nil {
		t.Fatalf("block_height missing from response: %v", body)
	}
	if _, ok := body["block_timestamp"]; !ok {
		t.Fatalf("block_timestamp missing for in-memory block: %v", body)
	}
}

// ── Test 2: block is on disk as native core.Block JSON ───────────────────────

func TestUTXO_BlockTimestamp_FromDiskNative(t *testing.T) {
	db := openTestStore(t)

	chain := core.NewChain()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	srv.SetStore(db)

	const height = uint64(42)
	wantTs := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC).UnixNano()

	// Persist a native core.Block to disk (mimicking what the node stores).
	blk := makeBlockAtHeight(t, height, wantTs)
	blkHash := blk.Hash()
	raw, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	if err := db.PutRawBlock(blkHash, height, raw); err != nil {
		t.Fatalf("PutRawBlock: %v", err)
	}

	txHash, _ := addUTXOAtHeight(utxos, 0x02, height)
	code, body := getUTXOResp(t, srv, txHash)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, body)
	}

	ts, ok := body["block_timestamp"].(string)
	if !ok || ts == "" {
		t.Fatalf("block_timestamp missing or empty in native-disk response: %v", body)
	}
	got, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("block_timestamp not RFC3339: %q", ts)
	}
	if got.UTC().UnixNano() != wantTs {
		t.Fatalf("block_timestamp mismatch: want %v, got %v", time.Unix(0, wantTs).UTC(), got.UTC())
	}

	bh, ok := body["block_height"].(float64)
	if !ok || uint64(bh) != height {
		t.Fatalf("block_height mismatch: want %d, got %v", height, body["block_height"])
	}
}

// ── Test 3: block is on disk as pruned StoredBlock ────────────────────────────

func TestUTXO_BlockTimestamp_FromDiskPruned(t *testing.T) {
	db := openTestStore(t)

	chain := core.NewChain()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	srv.SetStore(db)

	const height = uint64(77)
	wantTs := time.Date(2024, 1, 15, 8, 30, 0, 0, time.UTC).UnixNano()

	// Persist a pruned StoredBlock (mimicking what the pruner stores).
	var blkHash crypto.Hash32
	blkHash[0] = 0xBB
	sb := &store.StoredBlock{
		Hash:      blkHash,
		Height:    height,
		Timestamp: wantTs,
		TxCount:   3,
	}
	if err := db.PutBlock(blkHash, sb); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	// Also write the height→hash index so GetRawBlockByHeight resolves.
	raw, _ := json.Marshal(sb)
	if err := db.PutRawBlock(blkHash, height, raw); err != nil {
		t.Fatalf("PutRawBlock (StoredBlock JSON): %v", err)
	}

	txHash, _ := addUTXOAtHeight(utxos, 0x03, height)
	code, body := getUTXOResp(t, srv, txHash)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, body)
	}

	ts, ok := body["block_timestamp"].(string)
	if !ok || ts == "" {
		t.Fatalf("block_timestamp missing or empty in pruned-disk response: %v", body)
	}
	got, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("block_timestamp not RFC3339: %q", ts)
	}
	if got.UTC().UnixNano() != wantTs {
		t.Fatalf("block_timestamp mismatch: want %v, got %v", time.Unix(0, wantTs).UTC(), got.UTC())
	}
}

// ── Test 4: block is unavailable (no store, block not in memory) ──────────────

func TestUTXO_BlockTimestamp_AbsentWhenNoBlock(t *testing.T) {
	// Server with no blockStore; UTXO points to height 999 which isn't in chain.
	chain := core.NewChain()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())

	txHash, _ := addUTXOAtHeight(utxos, 0x04, 999)
	code, body := getUTXOResp(t, srv, txHash)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, body)
	}
	if _, ok := body["block_timestamp"]; ok {
		t.Fatalf("expected block_timestamp to be absent when block unavailable, got: %v", body)
	}
	if bh, ok := body["block_height"]; !ok || bh == nil {
		t.Fatalf("block_height should always be present: %v", body)
	}
}
