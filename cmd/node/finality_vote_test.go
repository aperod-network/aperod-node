package main

import (
	"bytes"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/p2p"
)

func TestFinalityMsgFromP2PPreservesValidatorKey(t *testing.T) {
	pub := bytes.Repeat([]byte{0x42}, 32)
	vote := p2p.VoteMsg{
		BlockHash:    crypto.Hash32{1},
		Height:       42,
		ValidatorPub: pub,
		Signature:    []byte{2, 3},
	}
	got, err := finalityMsgFromP2P(vote)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.ValidatorPub, pub) || got.ValidatorPub.ID() == "" {
		t.Fatal("wire vote lost its validator public key")
	}
	if got.BlockHash != vote.BlockHash || got.Height != vote.Height ||
		!bytes.Equal(got.Signature, vote.Signature) {
		t.Fatal("wire vote fields changed during conversion")
	}
}

func TestFinalityMsgFromP2PRejectsMalformedValidatorKey(t *testing.T) {
	for _, pub := range [][]byte{nil, {1, 2, 3}} {
		if _, err := finalityMsgFromP2P(p2p.VoteMsg{ValidatorPub: pub}); err == nil {
			t.Fatalf("accepted validator public key of length %d", len(pub))
		}
	}
}