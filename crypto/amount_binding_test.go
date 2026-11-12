package crypto_test

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestAmountBindingRoundTripAndTamperRejection(t *testing.T) {
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	blind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	const amount = uint64(0x1122334455667788)
	commitment, err := crypto.Commit(amount, blind)
	if err != nil {
		t.Fatal(err)
	}
	binding, combined, err := crypto.CreateAmountBinding(amount, blind, recipient.Spend.Public)
	if err != nil {
		t.Fatalf("CreateAmountBinding: %v", err)
	}
	if combined != commitment {
		t.Fatal("weighted chunk commitment differs from the transaction output commitment")
	}
	ok, err := crypto.VerifyAmountBinding(commitment, recipient.Spend.Public, binding)
	if err != nil || !ok {
		t.Fatalf("VerifyAmountBinding = %v, %v", ok, err)
	}
	decoded, err := crypto.DecryptAmountBinding(binding, recipient.Spend.Private)
	if err != nil || decoded != amount {
		t.Fatalf("DecryptAmountBinding = %d, %v; want %d", decoded, err, amount)
	}

	tampered := *binding
	tampered.Chunks[2].E[0] ^= 1
	if ok, err := crypto.VerifyAmountBinding(commitment, recipient.Spend.Public, &tampered); err == nil && ok {
		t.Fatal("tampered amount ciphertext accepted")
	}

	tampered = *binding
	tampered.Chunks[0].RangeProof.Bits = 64
	if ok, err := crypto.VerifyAmountBinding(commitment, recipient.Spend.Public, &tampered); err == nil && ok {
		t.Fatal("tampered range width accepted")
	}
}

func TestProveRangeBitsRejectsUnsupportedWidthsAndOverflow(t *testing.T) {
	blind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crypto.ProveRangeBits(1, blind, 8); err == nil {
		t.Fatal("unsupported range width accepted")
	}
	if _, err := crypto.ProveRangeBits(1<<16, blind, 16); err == nil {
		t.Fatal("16-bit overflow accepted")
	}
}
