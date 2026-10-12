package api

import (
	"encoding/hex"
	"encoding/json"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
	"path/filepath"
	"testing"
)

func TestWalletHistoryExactBudgetSendHasCanonicalEvidenceWithoutChange(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	image := crypto.KeyImage{7}
	tx := core.Transaction{Version: core.TxVersionCLSAG, Fee: 252750,
		Inputs:  []core.RingInput{{KeyImage: image}},
		Outputs: []core.Output{{OneTimePub: crypto.Point32{8}, TxPubKey: crypto.Point32{9}, AmountCommit: crypto.Commitment{10}}}}
	block := core.Block{Header: core.BlockHeader{Height: 12}, Txs: []core.Transaction{tx}}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.PutRawBlock(block.Hash(), 12, raw); err != nil {
		t.Fatal(err)
	}
	hash := tx.Hash()
	// Exercise the pre-locator fallback as well, not only recent t/ records.
	if err = db.PutUTXO(hash, 0, &store.StoredUTXO{TxHash: hash, BlockHeight: 12,
		OneTimePub: tx.Outputs[0].OneTimePub, TxPubKey: tx.Outputs[0].TxPubKey,
		AmountCommit: tx.Outputs[0].AmountCommit}); err != nil {
		t.Fatal(err)
	}
	view, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer view.Release()
	id := hex.EncodeToString(hash[:])
	ref := walletHistoryTransactionRef{Hash: id, KeyImages: []string{hex.EncodeToString(image[:])}}
	proof := walletHistoryTransactionEvidence(view, []walletHistoryTransactionRef{ref})[id]
	if proof == nil || proof.Height != 12 || proof.Fee != "252750" {
		t.Fatalf("missing exact-budget proof: %+v", proof)
	}
	wrong := crypto.KeyImage{11}
	ref.KeyImages[0] = hex.EncodeToString(wrong[:])
	if walletHistoryTransactionEvidence(view, []walletHistoryTransactionRef{ref})[id] != nil {
		t.Fatal("wrong input images accepted")
	}
}
