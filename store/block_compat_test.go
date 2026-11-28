package store

import (
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestDecodeStoredBlockSupportsCanonicalCoreBlock(t *testing.T) {
	var previous, merkle crypto.Hash32
	previous[0] = 0x17
	merkle[0] = 0x29
	header := core.BlockHeader{
		Height:       17,
		PrevHash:     previous,
		MerkleRoot:   merkle,
		Timestamp:    1_800_000_000,
		Round:        9,
		ValidatorPub: make(crypto.ValidatorPubKey, 32),
		OraclePrice:  123_000,
		BaseFee:      50,
	}
	raw, err := json.Marshal(core.Block{Header: header, Txs: []core.Transaction{{}}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := decodeStoredBlock(raw, header.Hash())
	if err != nil {
		t.Fatalf("decode canonical block: %v", err)
	}
	if got.Height != header.Height || got.PrevHash != previous || got.Hash != header.Hash() ||
		got.Timestamp != header.Timestamp || got.Round != header.Round ||
		got.TxCount != 1 || len(got.TxData) != 1 {
		t.Fatalf("canonical header/body was not preserved: %+v", got)
	}
}

func TestDecodeStoredBlockRejectsCanonicalHashKeyMismatch(t *testing.T) {
	header := core.BlockHeader{
		Height:       1,
		ValidatorPub: make(crypto.ValidatorPubKey, 32),
	}
	raw, err := json.Marshal(core.Block{Header: header})
	if err != nil {
		t.Fatal(err)
	}
	var wrongHash crypto.Hash32
	wrongHash[0] = 0x01
	if _, err := decodeStoredBlock(raw, wrongHash); err == nil {
		t.Fatal("canonical block with a mismatched database key was accepted")
	}
}
