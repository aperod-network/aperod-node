package api_test

// Tests for GET /api/v1/blocks/{id}/outputs — the per-block output endpoint
// added for the standalone explorer indexer.

import (
	"fmt"
	"net/http"
	"testing"
)

// TestREST_BlockOutputs_Basic verifies that GET /api/v1/blocks/{height}/outputs
// returns a non-empty outputs list for a block with a coinbase transaction.
func TestREST_BlockOutputs_Basic(t *testing.T) {
	srv, _ := buildChainServer(t, 2) // genesis + 2 blocks

	code, resp := restGet(t, srv, "/api/v1/blocks/0/outputs")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	// Top-level fields must be present
	if resp["block_hash"] == nil || resp["block_hash"] == "" {
		t.Error("expected block_hash in response")
	}
	bh, ok := resp["block_height"].(float64)
	if !ok || bh != 0 {
		t.Errorf("block_height = %v, want 0", resp["block_height"])
	}

	outputs, ok := resp["outputs"].([]interface{})
	if !ok {
		t.Fatalf("expected outputs array, got %T: %v", resp["outputs"], resp["outputs"])
	}
	// Genesis block has one coinbase tx with at least one output.
	if len(outputs) == 0 {
		t.Fatal("expected at least one output in genesis block")
	}

	// Every output must carry the required indexer fields.
	for i, raw := range outputs {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			t.Errorf("output[%d] is not an object", i)
			continue
		}
		for _, field := range []string{"tx_hash", "tx_index", "output_index", "one_time_pub_hex"} {
			if entry[field] == nil {
				t.Errorf("output[%d] missing field %q", i, field)
			}
		}
		// one_time_pub_hex must be 64 hex characters (32 bytes).
		if pub, ok := entry["one_time_pub_hex"].(string); ok {
			if len(pub) != 64 {
				t.Errorf("output[%d] one_time_pub_hex len = %d, want 64", i, len(pub))
			}
		}
	}
}

// TestREST_BlockOutputs_OutputCount verifies that output_count matches
// the length of the outputs array.
func TestREST_BlockOutputs_OutputCount(t *testing.T) {
	srv, _ := buildChainServer(t, 3)

	for _, height := range []string{"0", "1", "2"} {
		code, resp := restGet(t, srv, "/api/v1/blocks/"+height+"/outputs")
		if code != http.StatusOK {
			t.Errorf("height %s: status = %d, want 200", height, code)
			continue
		}
		outputs, _ := resp["outputs"].([]interface{})
		count, _ := resp["output_count"].(float64)
		if int(count) != len(outputs) {
			t.Errorf("height %s: output_count=%d but len(outputs)=%d", height, int(count), len(outputs))
		}
	}
}

// TestREST_BlockOutputs_IsCoinbase verifies that the first output in each
// block has is_coinbase set to true (the first tx in a block is always coinbase).
func TestREST_BlockOutputs_IsCoinbase(t *testing.T) {
	srv, _ := buildChainServer(t, 1)

	code, resp := restGet(t, srv, "/api/v1/blocks/1/outputs")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	outputs, _ := resp["outputs"].([]interface{})
	if len(outputs) == 0 {
		t.Fatal("expected at least one output")
	}
	first := outputs[0].(map[string]interface{})
	isCoinbase, _ := first["is_coinbase"].(bool)
	if !isCoinbase {
		t.Errorf("first output is_coinbase = false, want true (first tx is always coinbase)")
	}
}

// TestREST_BlockOutputs_ByHash verifies that the endpoint accepts a block hash
// as the identifier, not only a height.
func TestREST_BlockOutputs_ByHash(t *testing.T) {
	srv, chain := buildChainServer(t, 1)

	b := chain.GetByHeight(1)
	if b == nil {
		t.Fatal("block at height 1 not found")
	}
	h := b.Hash()
	hashHex := fmt.Sprintf("%x", h[:])

	code, resp := restGet(t, srv, "/api/v1/blocks/"+hashHex+"/outputs")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; hash=%s", code, hashHex)
	}
	outputs, _ := resp["outputs"].([]interface{})
	if len(outputs) == 0 {
		t.Fatal("expected outputs when looking up by hash")
	}
}

// TestREST_BlockOutputs_NotFound verifies that a request for a non-existent
// block height returns 404.
func TestREST_BlockOutputs_NotFound(t *testing.T) {
	srv, _ := buildChainServer(t, 1)

	code, _ := restGet(t, srv, "/api/v1/blocks/99999/outputs")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

// TestREST_BlockOutputs_InvalidID verifies that a malformed block ID returns 400.
func TestREST_BlockOutputs_InvalidID(t *testing.T) {
	srv, _ := buildChainServer(t, 0)

	code, _ := restGet(t, srv, "/api/v1/blocks/not-a-valid-id/outputs")
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}
