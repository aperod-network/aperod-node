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
