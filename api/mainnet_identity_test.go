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
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

func TestMainnetAPIRejectsDatabaseConsistentForeignGenesis(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("open fixture store: %v", err)
	}
	defer db.Close()

	private, public, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("generate fixture validator key: %v", err)
	}
	genesis := &core.Block{Header: core.BlockHeader{
		Height:       0,
		MerkleRoot:   core.MerkleRoot(nil),
		Timestamp:    1_000_000_000,
		ValidatorPub: public,
	}}
	genesisIndexHash := historicalHeaderHash(genesis.Header, false, false)
	genesis.Header.Signature, err = private.Sign(genesisIndexHash)
	if err != nil {
		t.Fatalf("sign fixture genesis: %v", err)
	}
	rawGenesis, err := json.Marshal(genesis)
	if err != nil {
		t.Fatalf("marshal fixture genesis: %v", err)
	}
	if err := db.PutRawBlock(genesisIndexHash, 0, rawGenesis); err != nil {
		t.Fatalf("store fixture genesis: %v", err)
	}
	first := &core.Block{Header: core.BlockHeader{
		Height:       1,
		PrevHash:     genesisIndexHash,
		MerkleRoot:   core.MerkleRoot(nil),
		Timestamp:    4_000_000_000,
		ValidatorPub: public,
	}}
	if err := first.Header.Sign(private); err != nil {
		t.Fatalf("sign fixture first block: %v", err)
	}
	rawFirst, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal fixture first block: %v", err)
	}
	if err := db.PutRawBlock(first.Hash(), 1, rawFirst); err != nil {
		t.Fatalf("store fixture first block: %v", err)
	}

	server := NewServer(":0", core.NewChain(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetStore(db)
	server.mainnetIdentity = &mainnetGenesisIdentity{
		genesisHeaderHash:    crypto.HashBytes([]byte("signed Mainnet header")),
		genesisIndexHash:     crypto.HashBytes([]byte("signed Mainnet index")),
		checkpointHeight:     1,
		checkpointHeaderHash: crypto.HashBytes([]byte("signed Mainnet checkpoint header")),
		checkpointIndexHash:  crypto.HashBytes([]byte("signed Mainnet checkpoint index")),
	}

	for _, id := range []string{"0", hashHex(genesisIndexHash), "1"} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/blocks/"+id, nil))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET /api/v1/blocks/%s status=%d body=%s; want 503", id, response.Code, response.Body.String())
		}
	}
	if _, err := server.canonicalPrunedBlockID(&store.StoredBlock{
		Height: 0,
		Hash:   genesisIndexHash,
	}); err == nil {
		t.Fatal("pruned foreign genesis passed the signed Mainnet identity check")
	}
}

func TestSetMainnetBootstrapIdentityRejectsInvalidSignature(t *testing.T) {
	server := NewServer(":0", core.NewChain(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetStore(&store.DB{})
	if err := server.SetMainnetBootstrapIdentity(mainnetbootstrap.SignedStatement{}); err == nil {
		t.Fatal("empty bootstrap statement was accepted")
	}
	if server.mainnetIdentity != nil {
		t.Fatal("invalid bootstrap statement pinned API identity")
	}
}
