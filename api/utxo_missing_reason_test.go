package api_test

// Tests for the utxoMissingReason helper — verifies that the stake deposit
// endpoints correctly distinguish four cases when a burn UTXO is absent from
// the active in-memory set:
//
//  1. No block store wired                            → generic "not found" message
//  2. UTXO never persisted to LevelDB                 → "does not exist" message
//  3. UTXO in LevelDB, block height < prune cursor    → "pruned" message (light mode)
//  4. UTXO in LevelDB, block height >= prune cursor   → "spent or burned" message
//
// The tests spin up real LevelDB instances in t.TempDir() to avoid mocking
// the store interface.

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// openTestStore opens a temporary LevelDB store and registers a cleanup.
func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	dir, err := os.MkdirTemp("", "aperod-api-test-*")
	if err != nil {
		t.Fatalf("TempDir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// putPruneCursor writes the prune_cursor metadata key used by utxoMissingReason.
func putPruneCursor(t *testing.T, db *store.DB, cursor uint64) {
	t.Helper()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], cursor)
	if err := db.PutMeta("prune_cursor", buf[:]); err != nil {
		t.Fatalf("PutMeta prune_cursor: %v", err)
	}
}

// buildMissingUTXOServer creates a server whose active UTXOSet is empty.
// The caller wires blockStore and pruningMode as needed.
func buildMissingUTXOServer(t *testing.T, db *store.DB, pruningMode string) (*api.Server, crypto.Hash32, uint32) {
	t.Helper()
	chain := core.NewChain()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet() // empty — every UTXO lookup will miss

	var txHash crypto.Hash32
	for i := range txHash {
		txHash[i] = 0xCD
	}
	const outIdx uint32 = 0

	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	if db != nil {
		srv.SetStore(db)
	}
	if pruningMode != "" {
		srv.SetPruningMode(pruningMode)
	}
	return srv, txHash, outIdx
}

// restGetStr issues a GET and returns the raw body string for substring checks.
func restGetStr(t *testing.T, srv *api.Server, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// postStakePayload builds a valid-length (173-byte) v2 payload referencing the
// given txHash/outIdx, signs it with a fresh key, and submits it to
// POST /api/v1/stake.  Returns the HTTP status and raw body.
func postStakePayload(t *testing.T, srv *api.Server, txHash crypto.Hash32, outIdx uint32) (int, string) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	const amount uint64 = 10_000_000_000_000
	msg := core.StakeSignMsgV2(core.StakeDeposit, pub, amount, txHash, outIdx)
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
	extra, err := core.EncodeStakeExtraV2(core.StakeDeposit, pub, amount, sig, txHash, outIdx, blind)
	if err != nil {
		t.Fatalf("EncodeStakeExtraV2: %v", err)
	}
	body := fmt.Sprintf(`{"tx_extra_hex":%q}`, hex.EncodeToString(extra))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stake", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// ─── Tests for GET /api/v1/utxo/{txhash}/{idx} ───────────────────────────────

// TestUTXOMissing_NoStore: no block store wired → generic "not found in active set".
func TestUTXOMissing_NoStore(t *testing.T) {
	srv, txHash, outIdx := buildMissingUTXOServer(t, nil, "")

	path := fmt.Sprintf("/api/v1/utxo/%x/%d", txHash[:], outIdx)
	code, body := restGetStr(t, srv, path)

	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", code, body)
	}
	if !strings.Contains(body, "not found in active set") {
		t.Errorf("expected 'not found in active set' in body; got: %s", body)
	}
	// Must NOT claim pruning without evidence.
	if strings.Contains(body, "pruned") {
		t.Errorf("body must not mention pruning when no block store is wired; got: %s", body)
	}
}

// TestUTXOMissing_NeverPersisted: UTXO not in LevelDB at all → "does not exist".
func TestUTXOMissing_NeverPersisted(t *testing.T) {
	db := openTestStore(t)
	srv, txHash, outIdx := buildMissingUTXOServer(t, db, "archive")

	path := fmt.Sprintf("/api/v1/utxo/%x/%d", txHash[:], outIdx)
	code, body := restGetStr(t, srv, path)

	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", code, body)
	}
	if !strings.Contains(body, "does not exist") {
		t.Errorf("expected 'does not exist' in body; got: %s", body)
	}
	// Must NOT claim pruning for a UTXO that was never created.
	if strings.Contains(body, "pruned") {
		t.Errorf("body must not mention pruning for a never-created UTXO; got: %s", body)
	}
}

// TestUTXOMissing_PrunedBlock: UTXO in LevelDB at height < prune cursor in
// light mode → descriptive "pruned" message with block height.
func TestUTXOMissing_PrunedBlock(t *testing.T) {
	db := openTestStore(t)
	srv, txHash, outIdx := buildMissingUTXOServer(t, db, "light")

	// Persist the UTXO record at block height 100.
	const utxoHeight uint64 = 100
	su := &store.StoredUTXO{
		TxHash:      txHash,
		OutputIndex: outIdx,
		BlockHeight: utxoHeight,
	}
	if err := db.PutUTXO(txHash, outIdx, su); err != nil {
		t.Fatalf("PutUTXO: %v", err)
	}
	// Set prune cursor above the UTXO's block height.
	putPruneCursor(t, db, 500)

	path := fmt.Sprintf("/api/v1/utxo/%x/%d", txHash[:], outIdx)
	code, body := restGetStr(t, srv, path)

	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", code, body)
	}
	if !strings.Contains(body, "pruned") {
		t.Errorf("expected 'pruned' in body; got: %s", body)
	}
	if !strings.Contains(body, "light-pruning mode") {
		t.Errorf("expected 'light-pruning mode' in body; got: %s", body)
	}
	// The specific block heights should appear so operators can cross-check.
	if !strings.Contains(body, fmt.Sprintf("%d", utxoHeight)) {
		t.Errorf("expected UTXO block height %d in body; got: %s", utxoHeight, body)
	}
}

// TestUTXOMissing_PrunedBlock_ArchiveNode: same LevelDB state but pruningMode
// is "archive" → must NOT claim pruning (archive nodes don't prune).
func TestUTXOMissing_PrunedBlock_ArchiveNode(t *testing.T) {
	db := openTestStore(t)
	srv, txHash, outIdx := buildMissingUTXOServer(t, db, "archive")

	su := &store.StoredUTXO{TxHash: txHash, OutputIndex: outIdx, BlockHeight: 10}
	if err := db.PutUTXO(txHash, outIdx, su); err != nil {
		t.Fatalf("PutUTXO: %v", err)
	}
	putPruneCursor(t, db, 500)

	path := fmt.Sprintf("/api/v1/utxo/%x/%d", txHash[:], outIdx)
	code, body := restGetStr(t, srv, path)

	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", code, body)
	}
	if strings.Contains(body, "pruned") {
		t.Errorf("archive node must not claim pruning; body: %s", body)
	}
	// Should say spent/burned because the node is archive mode.
	if !strings.Contains(body, "spent or burned") {
		t.Errorf("expected 'spent or burned' in body; got: %s", body)
	}
}

// TestUTXOMissing_SpentNotPruned: UTXO in LevelDB at height >= prune cursor →
// "already spent or burned" (no pruning hint).
func TestUTXOMissing_SpentNotPruned(t *testing.T) {
	db := openTestStore(t)
	srv, txHash, outIdx := buildMissingUTXOServer(t, db, "light")

	su := &store.StoredUTXO{TxHash: txHash, OutputIndex: outIdx, BlockHeight: 900}
	if err := db.PutUTXO(txHash, outIdx, su); err != nil {
		t.Fatalf("PutUTXO: %v", err)
	}
	// Prune cursor is below the UTXO's block height → block not pruned.
	putPruneCursor(t, db, 500)

	path := fmt.Sprintf("/api/v1/utxo/%x/%d", txHash[:], outIdx)
	code, body := restGetStr(t, srv, path)

	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", code, body)
	}
	if !strings.Contains(body, "spent or burned") {
		t.Errorf("expected 'spent or burned' in body; got: %s", body)
	}
	if strings.Contains(body, "pruned") {
		t.Errorf("must not claim pruning when block is above prune cursor; body: %s", body)
	}
}

// ─── Tests for POST /api/v1/admin/stake-deposit (UTXO absent from active set) ──

// buildAdminStakeServer creates a server with an empty active UTXOSet and a
// configured validator key so POST /api/v1/admin/stake-deposit reaches the
// UTXO-existence check.  It returns the server, the matching pub key hex, a
// fake tx hash, and the output index.
func buildAdminStakeServer(t *testing.T, db *store.DB, pruningMode string) (
	srv *api.Server, pubHex string, txHash crypto.Hash32, outIdx uint32,
) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	lockedKey, err := crypto.NewLockedValidatorKey([]byte(priv), func(e error) {
		t.Logf("mlock warn: %v", e)
	})
	if err != nil {
		t.Fatalf("NewLockedValidatorKey: %v", err)
	}
	t.Cleanup(func() { lockedKey.Destroy() })

	chain := core.NewChain()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet() // empty — every UTXO lookup will miss

	var fakeHash crypto.Hash32
	for i := range fakeHash {
		fakeHash[i] = 0xEF
	}
	const fakeOutIdx uint32 = 0

	srv = api.NewServer(":0", chain, mp, utxos, testLogger())
	srv.SetValidatorKey(lockedKey)
	if db != nil {
		srv.SetStore(db)
	}
	if pruningMode != "" {
		srv.SetPruningMode(pruningMode)
	}
	return srv, fmt.Sprintf("%x", []byte(pub)), fakeHash, fakeOutIdx
}

// postAdminStakePayload issues POST /api/v1/admin/stake-deposit with a
// syntactically-valid body referencing txHash/outIdx.  The blinding factor is
// an arbitrary non-zero value; the commitment check only runs after the UTXO
// existence check, so any blind is fine for testing the pruning hint path.
func postAdminStakePayload(t *testing.T, srv *api.Server, pubHex string, txHash crypto.Hash32, outIdx uint32) (int, string) {
	t.Helper()
	const amount uint64 = 10_000_000_000_000
	// Use a fixed non-zero blinding factor; commitment check is after UTXO lookup.
	blindHex := strings.Repeat("ab", 32)
	body := fmt.Sprintf(
		`{"pub_key":%q,"amount_napr":%d,"utxo_txhash":%q,"utxo_out_idx":%d,"blind_hex":%q}`,
		pubHex, amount, fmt.Sprintf("%x", txHash[:]), outIdx, blindHex,
	)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/stake-deposit", strings.NewReader(body))
	req.Host = "127.0.0.1" // localOnly DNS-rebinding guard requires a loopback Host
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// TestAdminStakeDeposit_PrunedUTXO: UTXO in LevelDB at pruned height → the
// descriptive pruning hint surfaces via POST /api/v1/admin/stake-deposit.
func TestAdminStakeDeposit_PrunedUTXO(t *testing.T) {
	db := openTestStore(t)
	srv, pubHex, txHash, outIdx := buildAdminStakeServer(t, db, "light")

	su := &store.StoredUTXO{TxHash: txHash, OutputIndex: outIdx, BlockHeight: 50}
	if err := db.PutUTXO(txHash, outIdx, su); err != nil {
		t.Fatalf("PutUTXO: %v", err)
	}
	putPruneCursor(t, db, 1000)

	code, body := postAdminStakePayload(t, srv, pubHex, txHash, outIdx)

	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", code, body)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(body), &resp); err == nil {
		errMsg, _ := resp["error"].(string)
		if !strings.Contains(errMsg, "pruning mode") {
			t.Errorf("expected 'pruning mode' in error field; got: %q", errMsg)
		}
		// The pruned-up-to height (1000) must appear so operators can cross-check.
		if !strings.Contains(errMsg, "1000") {
			t.Errorf("expected pruned-up-to height 1000 in error field; got: %q", errMsg)
		}
		if !strings.Contains(errMsg, "archive node") {
			t.Errorf("expected 'archive node' hint in error field; got: %q", errMsg)
		}
	} else {
		if !strings.Contains(body, "pruning mode") {
			t.Errorf("expected 'pruning mode' in body; got: %s", body)
		}
		if !strings.Contains(body, "1000") {
			t.Errorf("expected pruned-up-to height 1000 in body; got: %s", body)
		}
	}
}

// TestAdminStakeDeposit_MissingUTXO_NoPruning: UTXO never persisted to LevelDB
// → "does not exist" (no pruning hint) via POST /api/v1/admin/stake-deposit.
func TestAdminStakeDeposit_MissingUTXO_NoPruning(t *testing.T) {
	db := openTestStore(t)
	srv, pubHex, txHash, outIdx := buildAdminStakeServer(t, db, "archive")

	code, body := postAdminStakePayload(t, srv, pubHex, txHash, outIdx)

	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", code, body)
	}
	if !strings.Contains(body, "does not exist") {
		t.Errorf("expected 'does not exist' in body; got: %s", body)
	}
	if strings.Contains(body, "pruned") {
		t.Errorf("must not mention pruning for a never-created UTXO; body: %s", body)
	}
}

// TestAdminStakeDeposit_SpentUTXO: UTXO in LevelDB but block above prune
// cursor → "already spent or burned" (no pruning hint).
func TestAdminStakeDeposit_SpentUTXO(t *testing.T) {
	db := openTestStore(t)
	srv, pubHex, txHash, outIdx := buildAdminStakeServer(t, db, "light")

	su := &store.StoredUTXO{TxHash: txHash, OutputIndex: outIdx, BlockHeight: 800}
	if err := db.PutUTXO(txHash, outIdx, su); err != nil {
		t.Fatalf("PutUTXO: %v", err)
	}
	putPruneCursor(t, db, 500) // cursor < UTXO block height → block not pruned

	code, body := postAdminStakePayload(t, srv, pubHex, txHash, outIdx)

	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", code, body)
	}
	if !strings.Contains(body, "spent or burned") {
		t.Errorf("expected 'spent or burned' in body; got: %s", body)
	}
	if strings.Contains(body, "pruned") {
		t.Errorf("must not claim pruning when block is above prune cursor; body: %s", body)
	}
}

// ─── Tests for POST /api/v1/stake (UTXO absent from active set) ───────────────

// TestStakeBroadcast_MissingUTXO_NoPruning: active set is empty but store has
// no record → "does not exist" error on stake broadcast.
func TestStakeBroadcast_MissingUTXO_NoPruning(t *testing.T) {
	db := openTestStore(t)
	srv, txHash, outIdx := buildMissingUTXOServer(t, db, "archive")

	code, body := postStakePayload(t, srv, txHash, outIdx)

	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", code, body)
	}
	if !strings.Contains(body, "does not exist") {
		t.Errorf("expected 'does not exist' in body; got: %s", body)
	}
}

// TestStakeBroadcast_PrunedUTXO: UTXO in LevelDB at pruned height → descriptive
// pruning error surfaced by POST /api/v1/stake.
func TestStakeBroadcast_PrunedUTXO(t *testing.T) {
	db := openTestStore(t)
	srv, txHash, outIdx := buildMissingUTXOServer(t, db, "light")

	su := &store.StoredUTXO{TxHash: txHash, OutputIndex: outIdx, BlockHeight: 50}
	if err := db.PutUTXO(txHash, outIdx, su); err != nil {
		t.Fatalf("PutUTXO: %v", err)
	}
	putPruneCursor(t, db, 1000)

	code, body := postStakePayload(t, srv, txHash, outIdx)

	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", code, body)
	}

	// Verify the response body (JSON string field) contains the key phrases.
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(body), &resp); err == nil {
		errMsg, _ := resp["error"].(string)
		if !strings.Contains(errMsg, "pruned") {
			t.Errorf("expected 'pruned' in error field; got: %q", errMsg)
		}
		if !strings.Contains(errMsg, "light-pruning mode") {
			t.Errorf("expected 'light-pruning mode' in error field; got: %q", errMsg)
		}
		if !strings.Contains(errMsg, "archive node") {
			t.Errorf("expected 'archive node' hint in error field; got: %q", errMsg)
		}
	} else {
		// Raw body fallback.
		if !strings.Contains(body, "pruned") {
			t.Errorf("expected 'pruned' in body; got: %s", body)
		}
	}
}

// TestStakeBroadcast_SpentUTXO: UTXO in LevelDB but block not pruned →
// "already spent or burned" (no pruning hint).
func TestStakeBroadcast_SpentUTXO(t *testing.T) {
	db := openTestStore(t)
	srv, txHash, outIdx := buildMissingUTXOServer(t, db, "light")

	su := &store.StoredUTXO{TxHash: txHash, OutputIndex: outIdx, BlockHeight: 800}
	if err := db.PutUTXO(txHash, outIdx, su); err != nil {
		t.Fatalf("PutUTXO: %v", err)
	}
	putPruneCursor(t, db, 500) // cursor < UTXO block height → not pruned

	code, body := postStakePayload(t, srv, txHash, outIdx)

	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", code, body)
	}
	if !strings.Contains(body, "spent or burned") {
		t.Errorf("expected 'spent or burned' in body; got: %s", body)
	}
	if strings.Contains(body, "pruned") {
		t.Errorf("must not claim pruning when block is above prune cursor; body: %s", body)
	}
}
