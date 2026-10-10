package crypto

import (
	"testing"

	"filippo.io/edwards25519"
)

func clsagFixture(t *testing.T) (Hash32, []Point32, []Commitment, Commitment, int, Scalar32, BlindFactor, BlindFactor, *CLSAGSignature) {
	t.Helper()
	const real = 7
	keys := make([]Point32, RingSize)
	commitments := make([]Commitment, RingSize)
	var private Scalar32
	var inputBlind BlindFactor
	for i := 0; i < RingSize; i++ {
		x, err := randomScalar()
		if err != nil {
			t.Fatal(err)
		}
		copy(keys[i][:], (&edwards25519.Point{}).ScalarBaseMult(x).Bytes())
		blind, err := NewBlindFactor()
		if err != nil {
			t.Fatal(err)
		}
		commitment, err := Commit(42, blind)
		if err != nil {
			t.Fatal(err)
		}
		commitments[i] = commitment
		if i == real {
			copy(private[:], x.Bytes())
			inputBlind = blind
		}
	}
	pseudoBlind, err := NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	pseudo, err := Commit(42, pseudoBlind)
	if err != nil {
		t.Fatal(err)
	}
	var message Hash32
	message[0] = 1
	sig, err := CLSAGSign(message, keys, commitments, pseudo, real, private, inputBlind, pseudoBlind)
	if err != nil {
		t.Fatal(err)
	}
	return message, keys, commitments, pseudo, real, private, inputBlind, pseudoBlind, sig
}

func TestCLSAGSignVerifyAndBinding(t *testing.T) {
	message, keys, commitments, pseudo, _, _, _, _, sig := clsagFixture(t)
	if ok, err := CLSAGVerify(message, keys, commitments, pseudo, sig); err != nil || !ok {
		t.Fatalf("valid signature rejected: ok=%v err=%v", ok, err)
	}
	wrongMessage := message
	wrongMessage[0] ^= 1
	if ok, err := CLSAGVerify(wrongMessage, keys, commitments, pseudo, sig); err != nil || ok {
		t.Fatalf("wrong message accepted: ok=%v err=%v", ok, err)
	}
	replacement, err := randomScalar()
	if err != nil {
		t.Fatal(err)
	}
	badKey := append([]Point32(nil), keys...)
	copy(badKey[0][:], (&edwards25519.Point{}).ScalarBaseMult(replacement).Bytes())
	if ok, _ := CLSAGVerify(message, badKey, commitments, pseudo, sig); ok {
		t.Fatal("altered ring key accepted")
	}
	badKeys := append([]Point32(nil), keys...)
	badKeys[0] = keys[1]
	if ok, err := CLSAGVerify(message, badKeys, commitments, pseudo, sig); err == nil || ok {
		t.Fatalf("duplicate ring accepted: ok=%v err=%v", ok, err)
	}
	badCommitments := append([]Commitment(nil), commitments...)
	badCommitments[0][0] ^= 1
	if ok, _ := CLSAGVerify(message, keys, badCommitments, pseudo, sig); ok {
		t.Fatal("altered commitment accepted")
	}
	badPseudo := pseudo
	badPseudo[0] ^= 1
	if ok, _ := CLSAGVerify(message, keys, commitments, badPseudo, sig); ok {
		t.Fatal("altered pseudo output accepted")
	}
}

func TestCLSAGRejectsMalformedSignature(t *testing.T) {
	message, keys, commitments, pseudo, _, _, _, _, sig := clsagFixture(t)
	if len(sig.C1) != 32 || len(sig.D) != 32 || len(sig.KeyImage) != 32 || len(sig.S) != RingSize {
		t.Fatal("unexpected CLSAG signature field size")
	}
	short := *sig
	short.S = short.S[:RingSize-1]
	if ok, err := CLSAGVerify(message, keys, commitments, pseudo, &short); err == nil || ok {
		t.Fatalf("short responses accepted: ok=%v err=%v", ok, err)
	}
	nonCanonical := *sig
	nonCanonical.S = append([][32]byte(nil), sig.S...)
	for i := range nonCanonical.S[0] {
		nonCanonical.S[0][i] = 0xff
	}
	if ok, err := CLSAGVerify(message, keys, commitments, pseudo, &nonCanonical); err == nil || ok {
		t.Fatalf("non-canonical response accepted: ok=%v err=%v", ok, err)
	}
	badD := *sig
	badD.D = Point32{}
	if ok, err := CLSAGVerify(message, keys, commitments, pseudo, &badD); err == nil || ok {
		t.Fatalf("small-order D accepted: ok=%v err=%v", ok, err)
	}
	badImage := *sig
	badImage.KeyImage = KeyImage{}
	if ok, err := CLSAGVerify(message, keys, commitments, pseudo, &badImage); err == nil || ok {
		t.Fatalf("small-order key image accepted: ok=%v err=%v", ok, err)
	}
}

func TestCLSAGRejectsTorsionPublicInputs(t *testing.T) {
	message, keys, commitments, pseudo, _, _, _, _, sig := clsagFixture(t)

	// The all-zero compressed Edwards encoding is a valid small-order point.
	var torsion Point32

	badKeys := append([]Point32(nil), keys...)
	badKeys[0] = torsion
	if ok, err := CLSAGVerify(message, badKeys, commitments, pseudo, sig); err == nil || ok {
		t.Fatal("torsion ring key accepted")
	}

	badCommitments := append([]Commitment(nil), commitments...)
	badCommitments[0] = Commitment(torsion)
	if ok, err := CLSAGVerify(message, keys, badCommitments, pseudo, sig); err == nil || ok {
		t.Fatal("torsion ring commitment accepted")
	}

	if ok, err := CLSAGVerify(message, keys, commitments, Commitment(torsion), sig); err == nil || ok {
		t.Fatal("torsion pseudo output accepted")
	}
}
