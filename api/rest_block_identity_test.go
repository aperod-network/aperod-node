package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/internal/testutil"
	"github.com/aperod/aperod/store"
)

func TestRESTUsesCanonicalIndexIDForLegacyGenesis(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("open fixture store: %v", err)
	}
	defer testutil.CleanupClose(t, db)

	legacyPriv, legacyPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("generate legacy validator key: %v", err)
	}
	legacy := &core.Block{Header: core.BlockHeader{
		Height:       0,
		MerkleRoot:   core.MerkleRoot(nil),
		Timestamp:    1_000_000_000,
		ValidatorPub: legacyPub,
	}}
	canonicalHash := historicalHeaderHash(legacy.Header, false, false)
	legacy.Header.Signature, err = legacyPriv.Sign(canonicalHash)
	if err != nil {
		t.Fatalf("sign legacy header: %v", err)
	}
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy genesis: %v", err)
	}
	if err := db.PutRawBlock(canonicalHash, 0, legacyRaw); err != nil {
		t.Fatalf("store legacy genesis: %v", err)
	}

	// Simulate stale in-memory data that has a different genesis body and ID.
	memoryPriv, memoryPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("generate stale validator key: %v", err)
	}
	staleMemoryGenesis := &core.Block{Header: core.BlockHeader{
		Height:       0,
		MerkleRoot:   core.MerkleRoot(nil),
		Timestamp:    9_000_000_000,
		ValidatorPub: memoryPub,
	}}
	if err := staleMemoryGenesis.Header.Sign(memoryPriv); err != nil {
		t.Fatalf("sign stale in-memory genesis: %v", err)
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(staleMemoryGenesis); err != nil {
		t.Fatalf("set stale in-memory genesis: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(":0", chain, nil, nil, logger)
	server.SetStore(db)

	assertCanonicalResponse := func(path string, decode func([]byte) (string, error)) {
		t.Helper()
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
		got, err := decode(response.Body.Bytes())
		if err != nil {
			t.Fatalf("decode GET %s: %v", path, err)
		}
		if got != hashHex(canonicalHash) {
			t.Fatalf("GET %s returned hash %s; want canonical index hash %s", path, got, hashHex(canonicalHash))
		}
	}

	assertCanonicalResponse("/api/v1/blocks/0", func(raw []byte) (string, error) {
		var response BlockResponse
		err := json.Unmarshal(raw, &response)
		return response.Hash, err
	})
	assertCanonicalResponse("/api/v1/blocks?limit=1", func(raw []byte) (string, error) {
		var response struct {
			Blocks []BlockResponse `json:"blocks"`
		}
		err := json.Unmarshal(raw, &response)
		if err != nil {
			return "", err
		}
		if len(response.Blocks) != 1 {
			t.Fatalf("block list returned %d items, want 1", len(response.Blocks))
		}
		return response.Blocks[0].Hash, nil
	})
	assertCanonicalResponse("/api/v1/blocks/0/transactions", func(raw []byte) (string, error) {
		var response struct {
			BlockHash string `json:"block_hash"`
		}
		err := json.Unmarshal(raw, &response)
		return response.BlockHash, err
	})
	assertCanonicalResponse("/api/v1/blocks/0/outputs", func(raw []byte) (string, error) {
		var response struct {
			BlockHash string `json:"block_hash"`
		}
		err := json.Unmarshal(raw, &response)
		return response.BlockHash, err
	})

	staleHash := staleMemoryGenesis.Hash()
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/blocks/"+hashHex(staleHash), nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("stale memory genesis ID status=%d body=%s, want 404", response.Code, response.Body.String())
	}
}
