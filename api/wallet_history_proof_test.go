package api

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestWalletHistoryRequiresCanonicalTransactionAndExactOutput(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain"))
	if err != nil { t.Fatal(err) }
	defer db.Close()
	out := core.Output{OneTimePub: crypto.Point32{1}, TxPubKey: crypto.Point32{2}, AmountCommit: crypto.Commitment{3}}
	tx := core.Transaction{Version: core.TxVersionBase, Outputs: []core.Output{out}}
	block := core.Block{Header: core.BlockHeader{Height: 1}, Txs: []core.Transaction{tx}}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	raw, err := json.Marshal(block)
	if err != nil { t.Fatal(err) }
	if err := db.PutRawBlock(block.Hash(), 1, raw); err != nil { t.Fatal(err) }
	stored := &store.StoredUTXO{TxHash: tx.Hash(), OutputIndex: 0, BlockHeight: 1,
		OneTimePub: out.OneTimePub, TxPubKey: out.TxPubKey, AmountCommit: out.AmountCommit, EncAmount: out.EncAmount}
	if err := db.PutUTXO(tx.Hash(), 0, stored); err != nil { t.Fatal(err) }
	wrongHash := crypto.HashBytes([]byte("wallet-recorded-but-not-in-block"))
	if err := db.PutUTXO(wrongHash, 0, stored); err != nil { t.Fatal(err) }
	refs := []struct{ Hash string `json:"tx_hash"`; Index uint32 `json:"out_idx"` }{
		{fmt.Sprintf("%x", tx.Hash()), 0}, {fmt.Sprintf("%x", wrongHash), 0},
	}
	snapshot, err := db.NewWalletReadSnapshot()
	if err != nil { t.Fatal(err) }
	defer snapshot.Release()
	proof := walletHistoryOutputProof(snapshot, refs)
	if !proof[refs[0].Hash+":0"] || proof[refs[1].Hash+":0"] {
		t.Fatal("index presence confused with canonical membership")
	}
	stored.AmountCommit[0]++
	if err := db.PutUTXO(tx.Hash(), 0, stored); err != nil { t.Fatal(err) }
	newSnapshot, err := db.NewWalletReadSnapshot()
	if err != nil { t.Fatal(err) }
	defer newSnapshot.Release()
	if walletHistoryOutputProof(newSnapshot, refs)[refs[0].Hash+":0"] {
		t.Fatal("wrong output commitment promoted to confirmed")
	}
	if !walletHistoryOutputProof(snapshot, refs)[refs[0].Hash+":0"] {
		t.Fatal("a pinned proof changed after a later mutation")
	}
}
