// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package consensus

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestLPoDFinalityRejectsHeightRebindingBeforeMutation(t *testing.T) {
	e, _, parent, priv := lpodConsensusFixture(t)
	hash := parent.Hash()
	msg := crypto.HashBytes([]byte("aperod/finalize/v1"), hash[:])
	sig, err := priv.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	e.finalized[0] = true
	e.lpodFinalizedHashes = map[uint64]crypto.Hash32{0: e.chain.Genesis().Hash()}
	for _, height := range []uint64{0, 2, 999_999, ^uint64(0)} {
		vote := FinalizeMsg{BlockHash: hash, Height: height, ValidatorPub: priv.Public(), Signature: sig}
		if err := e.handleVote(vote); err == nil {
			t.Fatal("old signature accepted under changed height")
		}
		if !e.finalized[0] || len(e.pendingVoteHeight) != 0 || len(e.votes) != 0 || e.finalized[height] && height != 0 {
			t.Fatal("rebound height poisoned or pruned finality")
		}
	}
	if err := e.handleVote(FinalizeMsg{BlockHash: hash, Height: 1, ValidatorPub: priv.Public(), Signature: sig}); err != nil {
		t.Fatal(err)
	}
	if !e.IsFinalizedHash(1, hash) {
		t.Fatal("genuine canonical block did not finalize")
	}
}
