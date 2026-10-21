package api

import (
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestGetTransactionFromDisk_AcceptsPreCLSAGTransactionID(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var input core.RingInput
	input.KeyImage[0] = 0xa1
	input.AmountCommit[0] = 0xa2
	input.Ring = make([]crypto.RingMember, crypto.RingSize)
	input.RingCommitments = make([]crypto.Commitment, crypto.RingSize)
	for i := range input.Ring {
		input.Ring[i][0] = byte(i + 1)
		input.RingCommitments[i][0] = byte(i + 33)
	}
	input.PseudoOut[0] = 0xb1

	var output core.Output
	output.OneTimePub[0] = 0xc1
	output.AmountCommit[0] = 0xc2
	output.TxPubKey[0] = 0xc3
	output.EncAmount[0] = 0xc4

	tx := core.Transaction{
		Version: core.TxVersionCLSAG,
		Fee:     17,
		Inputs:  []core.RingInput{input},
		Outputs: []core.Output{output},
	}
	tx.FeeCommit[0] = 0xd1

	legacyID := preCLSAGIDForTest(&tx)
	if legacyID == tx.Hash() {
		t.Fatal("fixture must distinguish the pre-CLSAG ID from the current transaction hash")
	}

	coinbase := core.CoinbaseTx(crypto.Point32{}, 1)
	txs := []core.Transaction{coinbase, tx}
	block := &core.Block{Header: core.BlockHeader{
		Height:     42,
		MerkleRoot: core.MerkleRoot(txs),
	}, Txs: txs}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	blockHash := block.Hash()
	if err := db.PutRawBlock(blockHash, block.Header.Height, raw); err != nil {
		t.Fatalf("PutRawBlock: %v", err)
	}
	if err := db.PutTip(blockHash, block.Header.Height); err != nil {
		t.Fatalf("PutTip: %v", err)
	}
	if err := db.PutTxIdx(legacyID, block.Header.Height, 1); err != nil {
		t.Fatalf("PutTxIdx: %v", err)
	}

	server := &Server{blockStore: db}
	got, loc, found, err := server.getTransactionFromDisk(legacyID)
	if err != nil {
		t.Fatalf("getTransactionFromDisk: %v", err)
	}
	if !found {
		t.Fatal("historical transaction ID was not resolved")
	}
	if got.Hash() == legacyID || preCLSAGTransactionHash(&got) != legacyID {
		t.Fatal("resolved body does not match the requested historical transaction ID")
	}
	if loc.Block.Header.Height != block.Header.Height || loc.TxIndex != 1 || loc.Block.Hash() != blockHash {
		t.Fatalf("returned location = height %d, tx index %d, block hash %x; want height %d, tx index 1, hash %x",
			loc.Block.Header.Height, loc.TxIndex, loc.Block.Hash(), block.Header.Height, blockHash)
	}
}

func preCLSAGIDForTest(tx *core.Transaction) crypto.Hash32 {
	fee := make([]byte, 8)
	binary.LittleEndian.PutUint64(fee, tx.Fee)
	parts := [][]byte{{byte(tx.Version)}, fee, tx.FeeCommit[:], tx.Extra}
	for _, input := range tx.Inputs {
		parts = append(parts, input.KeyImage[:], input.AmountCommit[:])
		for _, member := range input.Ring {
			parts = append(parts, member[:])
		}
	}
	for _, output := range tx.Outputs {
		parts = append(parts, output.OneTimePub[:], output.AmountCommit[:],
			output.TxPubKey[:], output.EncAmount[:])
	}
	return crypto.HashBytes(parts...)
}
