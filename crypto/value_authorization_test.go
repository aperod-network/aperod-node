package crypto

import "testing"

func TestValueAuthorizationBindsActualDebitWithoutRevealingBlind(t *testing.T) {
	blind, err := NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	commit, proof, err := ProveValueAuthorization(10000000000, blind)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyValueAuthorization(10000000000, commit, proof); err != nil {
		t.Fatal(err)
	}
	if VerifyValueAuthorization(9999999999, commit, proof) == nil {
		t.Fatal("under-accounted issuance accepted")
	}
	mutated := proof
	mutated[32] ^= 1
	if VerifyValueAuthorization(10000000000, commit, mutated) == nil {
		t.Fatal("altered opening accepted")
	}
	other, _ := Commit(10000000001, blind)
	if VerifyValueAuthorization(10000000000, other, proof) == nil {
		t.Fatal("other commitment accepted")
	}
	var malformed Commitment
	for i := range malformed {
		malformed[i] = 255
	}
	if VerifyValueAuthorization(10000000000, malformed, proof) == nil {
		t.Fatal("noncanonical commitment accepted")
	}
}

func TestValidateAuthorizedOutputPoints(t *testing.T) {
	var spendScalar, ephemeralScalar Scalar32
	spendScalar[0] = 1
	ephemeralScalar[0] = 2

	publicKey, err := PublicKeyFromPrivate(spendScalar)
	if err != nil {
		t.Fatal(err)
	}
	ephemeralKey, err := PublicKeyFromPrivate(ephemeralScalar)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAuthorizedOutputPoints(publicKey, ephemeralKey); err != nil {
		t.Fatalf("valid output points rejected: %v", err)
	}

	var smallOrder Point32
	if err := ValidateAuthorizedOutputPoints(smallOrder, ephemeralKey); err == nil {
		t.Fatal("small-order recipient output accepted")
	}
	if err := ValidateAuthorizedOutputPoints(publicKey, smallOrder); err == nil {
		t.Fatal("small-order ephemeral output accepted")
	}
}
