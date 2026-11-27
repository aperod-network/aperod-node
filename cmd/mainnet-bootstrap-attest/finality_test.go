package main

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

func TestVerifySingleOperatorVoteUsesPinnedHeightScopedAuthority(t *testing.T) {
	raw, err := hex.DecodeString(mainnetbootstrap.GenesisSignerPublicKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := crypto.ValidatorPubKeyFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	signer, authority, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	fork := &core.ValidatorAuthorityFork{Height: 10, Identity: identity, Replacement: authority}
	for _, tc := range []struct {
		name string
		wire uint8
		want uint8
	}{
		{"implicit v1", 0, crypto.FinalityVoteVersionLegacy},
		{"explicit v1", crypto.FinalityVoteVersionLegacy, crypto.FinalityVoteVersionLegacy},
		{"v2", crypto.FinalityVoteVersionHeightBound, crypto.FinalityVoteVersionHeightBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert := &store.FinalityCertificate{Version: 1, Height: 10, BlockHash: crypto.HashStr("checkpoint")}
			sig, err := signer.Sign(crypto.FinalityVoteMessage(tc.wire, cert.Height, cert.BlockHash))
			if err != nil {
				t.Fatal(err)
			}
			cert.Votes = []store.FinalityVote{{Validator: identity, VoteVersion: tc.wire, Signature: sig}}
			if got, err := verifySingleOperatorVote(cert, fork); err != nil || got != tc.want {
				t.Fatalf("valid vote rejected or misclassified: version %d, %v", got, err)
			}
			cert.Height--
			if _, err := verifySingleOperatorVote(cert, fork); err == nil {
				t.Fatal("accepted replacement key before authority activation")
			}
			cert.Height++
			cert.Votes[0].Signature[0] ^= 1
			if _, err := verifySingleOperatorVote(cert, fork); err == nil {
				t.Fatal("accepted modified signature")
			}
			cert.Votes[0].VoteVersion = 255
			if _, err := verifySingleOperatorVote(cert, fork); err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("unknown vote version was not rejected explicitly: %v", err)
			}
		})
	}
}
