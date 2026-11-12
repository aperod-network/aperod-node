package store

import (
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestPruneBlocksOlderThanStripsCanonicalCoreBlockBody(t *testing.T) {
	db, err := Open(t.TempDir() + "/chain")
	if err != nil {
		t.Fatal(err)
	}
	defer closeStoreTestResource(t, db)

	genesis := &core.Block{Header: core.BlockHeader{Height: 0, Timestamp: 1, MerkleRoot: core.MerkleRoot(nil)}}
	raw, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitRawBlockWithAVM(genesis.Hash(), 0, raw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}

	txs := []core.Transaction{{}}
	block := &core.Block{Header: core.BlockHeader{
		Height: 1, PrevHash: genesis.Hash(), Timestamp: 2, MerkleRoot: core.MerkleRoot(txs),
	}, Txs: txs}
	raw, err = json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitRawBlockWithAVM(block.Hash(), block.Header.Height, raw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}

	pruned, err := db.PruneBlocksOlderThan(2)
	if err != nil || pruned != 1 {
		t.Fatalf("current canonical block body was not pruned: count=%d err=%v", pruned, err)
	}
	raw, err = db.GetRawBlockByHeight(1)
	if err != nil {
		t.Fatal(err)
	}
	var headerOnly core.Block
	if err := json.Unmarshal(raw, &headerOnly); err != nil {
		t.Fatal(err)
	}
	if headerOnly.Header.Height != block.Header.Height || headerOnly.Hash() != block.Hash() ||
		len(headerOnly.Txs) != 0 || headerOnly.Header.MerkleRoot != block.Header.MerkleRoot {
		t.Fatal("pruning changed canonical header or retained transaction payload")
	}
	if pruned, err := db.PruneBlocksOlderThan(2); err != nil || pruned != 0 {
		t.Fatalf("repeat pruning was not idempotent: count=%d err=%v", pruned, err)
	}
}
