package crypto_test

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

func buildV4Ring(t *testing.T) ([]crypto.RingMember, *crypto.WalletKeyPair) {
	t.Helper()
	real, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[5] = real.Spend.Public
	for i := range ring {
		if i == 5 {
			continue
		}
		member, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatal(err)
		}
		ring[i] = member.Spend.Public
	}
	return ring, real
}

func TestMLSAGV4BindsKeyAndCommitmentOpening(t *testing.T) {
	ring, real := buildV4Ring(t)
	blind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	const value = uint64(42_000_000)
	commitment, err := crypto.Commit(value, blind)
	if err != nil {
		t.Fatal(err)
	}
	message := crypto.HashStr("ringct-v4-valid")
	sig, err := crypto.MLSAGSignV4(message, ring, 5, real.Spend.Private, blind, value)
	if err != nil {
		t.Fatalf("MLSAGSignV4: %v", err)
	}
	ok, err := crypto.MLSAGVerifyV4(message, ring, commitment, 5, sig)
	if err != nil || !ok {
		t.Fatalf("valid v4 proof rejected: ok=%v err=%v", ok, err)
	}

	otherBlind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	otherCommitment, err := crypto.Commit(value+1, otherBlind)
	if err != nil {
		t.Fatal(err)
	}
	ok, err = crypto.MLSAGVerifyV4(message, ring, otherCommitment, 5, sig)
	if err == nil && ok {
		t.Fatal("v4 proof accepted a copied commitment with an unknown opening")
	}

	ok, err = crypto.MLSAGVerifyV4(message, ring, commitment, 4, sig)
	if err == nil && ok {
		t.Fatal("v4 proof accepted a different disclosed real ring index")
	}
}

func TestMLSAGV4RejectsTamperedMessageAndLink(t *testing.T) {
	ring, real := buildV4Ring(t)
	blind, _ := crypto.NewBlindFactor()
	commitment, _ := crypto.Commit(7, blind)
	message := crypto.HashStr("ringct-v4-original")
	sig, err := crypto.MLSAGSignV4(message, ring, 5, real.Spend.Private, blind, 7)
	if err != nil {
		t.Fatal(err)
	}

	tampered := crypto.HashStr("ringct-v4-tampered")
	if ok, err := crypto.MLSAGVerifyV4(tampered, ring, commitment, 5, sig); err == nil && ok {
		t.Fatal("v4 proof accepted a tampered transaction message")
	}

	sig.LinkS[0] ^= 1
	if ok, err := crypto.MLSAGVerifyV4(message, ring, commitment, 5, sig); err == nil && ok {
		t.Fatal("v4 proof accepted a tampered direct ownership link")
	}
}