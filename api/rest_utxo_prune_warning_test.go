package api_test

// Tests for the blocks_until_pruned warning field in GET /api/v1/utxo/{txhash}/{idx}.
//
// The field is emitted only when:
//   - pruning_mode == "light" (archive nodes never emit it)
//   - blocksLeft (= UTXO.BlockHeight + keep_blocks − tipHeight) ≤
//     max(keep_blocks/10, core.PartialUnbondingBlocks)
//
// The effective threshold is therefore at least PartialUnbondingBlocks so the
// CLI always receives the field when the UTXO would be pruned before the
// unbonding period completes, regardless of keep_blocks size.
//
// The tests cover boundary cases around both the 10 % threshold and the
// PartialUnbondingBlocks floor.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// makePruneWarningFixture builds a Server whose chain tip is at tipHeight,
// with a single UTXO at utxoBlockHeight.  keepBlocks and pruningMode are
// applied via the public setters.
func makePruneWarningFixture(
	t *testing.T,
	tipHeight int,
	utxoBlockHeight uint64,
	keepBlocks uint64,
	pruningMode string,
) (*api.Server, crypto.Hash32) {
	t.Helper()

	// Re-use the chain built by buildChainServer (genesis + tipHeight blocks).
	_, chain := buildChainServer(t, tipHeight)

	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()

	var txHash crypto.Hash32
	for i := range txHash {
		txHash[i] = 0xEE
	}
	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		BlockHeight: utxoBlockHeight,
		// AmountCommit is zero — restUTXO only reads it for the hex field,
		// not for the prune-warning calculation.
	})

	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	srv.SetPruningMode(pruningMode)
	srv.SetKeepBlocks(keepBlocks)
	return srv, txHash
}

// pruneGET issues GET /api/v1/utxo/{txhash}/0 and decodes the JSON body.
func pruneGET(t *testing.T, srv *api.Server, txHash crypto.Hash32) (int, map[string]interface{}) {
	t.Helper()
	path := fmt.Sprintf("/api/v1/utxo/%x/0", txHash[:])
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var m map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&m); err != nil {
		t.Fatalf("decode response JSON: %v — body: %s", err, rr.Body.String())
	}
	return rr.Code, m
}

// TestUTXOPruneWarning_FieldPresentWhenClose verifies that blocks_until_pruned
// is included in the response when the UTXO is within the 10 % danger zone.
//
// Setup:
//
//	keepBlocks = 20  →  threshold = 2
//	utxoBlockHeight = 0,  pruneAt = 20
//	tipHeight = 18    →  blocksLeft = 2  (exactly at threshold → present)
func TestUTXOPruneWarning_FieldPresentWhenClose(t *testing.T) {
	const (
		keepBlocks      uint64 = 20
		tipHeight              = 18 // blocksLeft = 20 − 18 = 2 ≤ threshold(2)
		utxoBlockHeight uint64 = 0
		wantBlocksLeft  float64 = 2
	)

	srv, txHash := makePruneWarningFixture(t, tipHeight, utxoBlockHeight, keepBlocks, "light")
	code, body := pruneGET(t, srv, txHash)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", code, body)
	}
	raw, ok := body["blocks_until_pruned"]
	if !ok {
		t.Fatalf("blocks_until_pruned missing from response; body = %v", body)
	}
	// JSON numbers are decoded as float64 by encoding/json.
	if got, _ := raw.(float64); got != wantBlocksLeft {
		t.Errorf("blocks_until_pruned = %v, want %v", got, wantBlocksLeft)
	}
}

// TestUTXOPruneWarning_FieldPresentWithCorrectValue tests a case where blocksLeft
// is strictly less than the threshold to confirm the exact value is correct.
//
// Setup:
//
//	keepBlocks = 20  →  threshold = 2
//	utxoBlockHeight = 0,  pruneAt = 20
//	tipHeight = 19    →  blocksLeft = 1  (< threshold → present, value = 1)
func TestUTXOPruneWarning_FieldPresentWithCorrectValue(t *testing.T) {
	const (
		keepBlocks      uint64 = 20
		tipHeight              = 19
		utxoBlockHeight uint64 = 0
		wantBlocksLeft  float64 = 1
	)

	srv, txHash := makePruneWarningFixture(t, tipHeight, utxoBlockHeight, keepBlocks, "light")
	code, body := pruneGET(t, srv, txHash)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	raw, ok := body["blocks_until_pruned"]
	if !ok {
		t.Fatalf("blocks_until_pruned missing; body = %v", body)
	}
	if got, _ := raw.(float64); got != wantBlocksLeft {
		t.Errorf("blocks_until_pruned = %v, want %v", got, wantBlocksLeft)
	}
}

// TestUTXOPruneWarning_FieldAbsentWhenFar verifies that blocks_until_pruned is
// NOT present when the UTXO is safely far from pruning (blocksLeft well above
// the effective threshold = max(keepBlocks/10, PartialUnbondingBlocks)).
//
// Setup:
//
//	keepBlocks = 100 000  →  10% threshold = 10 000
//	effective threshold   = max(10 000, 43 200) = 43 200
//	utxoBlockHeight = 0,  pruneAt = 100 000
//	tipHeight = 10  →  blocksLeft = 99 990  (> 43 200 → absent)
func TestUTXOPruneWarning_FieldAbsentWhenFar(t *testing.T) {
	const (
		keepBlocks      uint64 = 100_000
		tipHeight              = 10
		utxoBlockHeight uint64 = 0
	)

	srv, txHash := makePruneWarningFixture(t, tipHeight, utxoBlockHeight, keepBlocks, "light")
	code, body := pruneGET(t, srv, txHash)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if v, ok := body["blocks_until_pruned"]; ok {
		t.Errorf("blocks_until_pruned should be absent when far from prune window, got %v", v)
	}
}

// TestUTXOPruneWarning_FieldAbsentInArchiveMode verifies that blocks_until_pruned
// is never emitted on archive nodes, regardless of block distance.
//
// Same heights as TestUTXOPruneWarning_FieldPresentWhenClose — only the mode changes.
func TestUTXOPruneWarning_FieldAbsentInArchiveMode(t *testing.T) {
	const (
		keepBlocks      uint64 = 20
		tipHeight              = 18 // same as "close" test — would trigger in light mode
		utxoBlockHeight uint64 = 0
	)

	srv, txHash := makePruneWarningFixture(t, tipHeight, utxoBlockHeight, keepBlocks, "archive")
	code, body := pruneGET(t, srv, txHash)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if v, ok := body["blocks_until_pruned"]; ok {
		t.Errorf("blocks_until_pruned must not appear in archive mode, got %v", v)
	}
}

// TestUTXOPruneWarning_ZeroAtPruneBoundary verifies that blocks_until_pruned=0
// is reported when tipHeight == pruneAt (the UTXO is exactly at its pruning
// boundary and the source block has been or is about to be stripped).
//
// Setup:
//
//	keepBlocks = 50, utxoBlockHeight = 0  →  pruneAt = 50
//	tipHeight = 50  →  tipHeight == pruneAt  →  blocks_until_pruned = 0
func TestUTXOPruneWarning_ZeroAtPruneBoundary(t *testing.T) {
	const (
		keepBlocks      uint64 = 50
		tipHeight              = 50 // == pruneAt → at boundary
		utxoBlockHeight uint64 = 0
	)

	srv, txHash := makePruneWarningFixture(t, tipHeight, utxoBlockHeight, keepBlocks, "light")
	code, body := pruneGET(t, srv, txHash)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	raw, ok := body["blocks_until_pruned"]
	if !ok {
		t.Fatalf("blocks_until_pruned must be present at prune boundary; body = %v", body)
	}
	if got, _ := raw.(float64); got != 0 {
		t.Errorf("blocks_until_pruned = %v, want 0 at prune boundary", got)
	}
}

// TestUTXOPruneWarning_ZeroPastPruneBoundary verifies that blocks_until_pruned=0
// is reported when tipHeight > pruneAt (the source block has already been
// pruned from the chain, but the UTXO entry still exists in the UTXO set).
//
// Setup:
//
//	keepBlocks = 50, utxoBlockHeight = 0  →  pruneAt = 50
//	tipHeight = 51  →  tipHeight > pruneAt  →  blocks_until_pruned = 0
func TestUTXOPruneWarning_ZeroPastPruneBoundary(t *testing.T) {
	const (
		keepBlocks      uint64 = 50
		tipHeight              = 51 // > pruneAt → past boundary
		utxoBlockHeight uint64 = 0
	)

	srv, txHash := makePruneWarningFixture(t, tipHeight, utxoBlockHeight, keepBlocks, "light")
	code, body := pruneGET(t, srv, txHash)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	raw, ok := body["blocks_until_pruned"]
	if !ok {
		t.Fatalf("blocks_until_pruned must be present past prune boundary; body = %v", body)
	}
	if got, _ := raw.(float64); got != 0 {
		t.Errorf("blocks_until_pruned = %v, want 0 past prune boundary", got)
	}
}

// TestUTXOPruneWarning_FieldAbsentWhenKeepBlocksZero verifies that the field is
// absent when keepBlocks is not configured (zero), even in light mode.
func TestUTXOPruneWarning_FieldAbsentWhenKeepBlocksZero(t *testing.T) {
	srv, txHash := makePruneWarningFixture(t, 5, 0, 0 /* keepBlocks=0 */, "light")
	code, body := pruneGET(t, srv, txHash)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if v, ok := body["blocks_until_pruned"]; ok {
		t.Errorf("blocks_until_pruned must not appear when keepBlocks=0, got %v", v)
	}
}

// TestUTXOPruneWarning_ExactlyAtThreshold verifies the boundary where
// blocksLeft == threshold — the last value that triggers the warning.
//
// Setup:
//
//	keepBlocks=100  →  threshold=10
//	utxoBlockHeight=0,  pruneAt=100
//	tipHeight=90    →  blocksLeft=10 == threshold → present
func TestUTXOPruneWarning_ExactlyAtThreshold(t *testing.T) {
	const (
		keepBlocks      uint64 = 100
		tipHeight              = 90
		utxoBlockHeight uint64 = 0
		wantBlocksLeft  float64 = 10
	)

	srv, txHash := makePruneWarningFixture(t, tipHeight, utxoBlockHeight, keepBlocks, "light")
	code, body := pruneGET(t, srv, txHash)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	raw, ok := body["blocks_until_pruned"]
	if !ok {
		t.Fatalf("blocks_until_pruned missing at exact threshold; body = %v", body)
	}
	if got, _ := raw.(float64); got != wantBlocksLeft {
		t.Errorf("blocks_until_pruned = %v, want %v", got, wantBlocksLeft)
	}
}

// TestUTXOPruneWarning_OneAboveThreshold verifies the boundary where
// blocksLeft == PartialUnbondingBlocks+1 — should NOT trigger the field
// (the effective threshold is max(keepBlocks/10, PartialUnbondingBlocks)).
//
// Setup:
//
//	keepBlocks = 43 211  →  10% threshold = 4 321
//	effective threshold  = max(4 321, 43 200) = 43 200
//	utxoBlockHeight = 0,  pruneAt = 43 211
//	tipHeight = 10  →  blocksLeft = 43 201  (> 43 200 → absent)
func TestUTXOPruneWarning_OneAboveThreshold(t *testing.T) {
	const (
		keepBlocks      uint64 = 43_211
		tipHeight              = 10
		utxoBlockHeight uint64 = 0
	)

	srv, txHash := makePruneWarningFixture(t, tipHeight, utxoBlockHeight, keepBlocks, "light")
	code, body := pruneGET(t, srv, txHash)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if v, ok := body["blocks_until_pruned"]; ok {
		t.Errorf("blocks_until_pruned must be absent when blocksLeft > threshold; got %v", v)
	}
}
