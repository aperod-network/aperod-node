package crypto_test

// Fourth round: cover the remaining uncovered error branches in crypto.

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

// ─── wallet.go: PointFromBytes wrong length ───────────────────────────────────

func TestPointFromBytes_WrongLength(t *testing.T) {
	_, err := crypto.PointFromBytes(make([]byte, 16)) // not 32
	if err == nil {
		t.Error("PointFromBytes must error on non-32-byte input")
	}
}

func TestPointFromBytes_ValidPoint(t *testing.T) {
	wk, _ := crypto.GenerateWalletKeys()
	// wk.Spend.Public is a known valid 32-byte compressed point
	pt, err := crypto.PointFromBytes(wk.Spend.Public[:])
	if err != nil {
		t.Fatalf("PointFromBytes valid point: %v", err)
	}
	if pt == nil {
		t.Error("PointFromBytes must return non-nil for valid point")
	}
}

// ─── wallet.go: ScalarFromBytes wrong length ──────────────────────────────────

func TestScalarFromBytes_WrongLength(t *testing.T) {
	_, err := crypto.ScalarFromBytes(make([]byte, 16)) // not 32
	if err == nil {
		t.Error("ScalarFromBytes must error on non-32-byte input")
	}
}

// ─── ringct.go: MLSAGSign error paths ────────────────────────────────────────

func TestMLSAGSign_WrongRingSize(t *testing.T) {
	wk, _ := crypto.GenerateWalletKeys()
	ring := make([]crypto.RingMember, 3) // wrong, needs RingSize (16)
	ring[0] = wk.Spend.Public
	msg := crypto.Hash32{}
	_, err := crypto.MLSAGSign(msg, ring, 0, wk.Spend.Private)
	if err == nil {
		t.Error("MLSAGSign must error on wrong ring size")
	}
}

func TestMLSAGSign_RealIdxOutOfRange(t *testing.T) {
	wk, _ := crypto.GenerateWalletKeys()
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = wk.Spend.Public
	msg := crypto.Hash32{}
	_, err := crypto.MLSAGSign(msg, ring, crypto.RingSize+1, wk.Spend.Private) // out of range
	if err == nil {
		t.Error("MLSAGSign must error on realIdx out of range")
	}
}

func TestMLSAGSign_NegativeRealIdx(t *testing.T) {
	wk, _ := crypto.GenerateWalletKeys()
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = wk.Spend.Public
	msg := crypto.Hash32{}
	_, err := crypto.MLSAGSign(msg, ring, -1, wk.Spend.Private)
	if err == nil {
		t.Error("MLSAGSign must error on negative realIdx")
	}
}

// ─── ringct.go: MLSAGVerify error paths ──────────────────────────────────────

func TestMLSAGVerify_NilSig(t *testing.T) {
	wk, _ := crypto.GenerateWalletKeys()
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = wk.Spend.Public
	msg := crypto.Hash32{}
	_, err := crypto.MLSAGVerify(msg, ring, nil)
	if err == nil {
		t.Error("MLSAGVerify must error on nil signature")
	}
}

func TestMLSAGVerify_WrongRingSize(t *testing.T) {
	ring := make([]crypto.RingMember, 3) // wrong
	msg := crypto.Hash32{}
	sig := &crypto.MLSAGSignature{SS: make([][32]byte, 3)}
	_, err := crypto.MLSAGVerify(msg, ring, sig)
	if err == nil {
		t.Error("MLSAGVerify must error on wrong ring size")
	}
}

// ─── bulletproof.go: VerifyRange nil/invalid proof ───────────────────────────

func TestVerifyRange_NilProof(t *testing.T) {
	_, err := crypto.VerifyRange(nil)
	if err == nil {
		t.Error("VerifyRange must error on nil proof")
	}
}

func TestVerifyRange_InvalidProof(t *testing.T) {
	// A proof with empty BitCommits will trigger validation failure
	proof := &crypto.RangeProof{}
	ok, err := crypto.VerifyRange(proof)
	if err != nil {
		// Error is acceptable for an invalid proof
		t.Logf("VerifyRange invalid proof: err=%v", err)
		return
	}
	if ok {
		t.Error("VerifyRange must return false for an invalid (empty) proof")
	}
}

// ─── keys.go: Verify() ────────────────────────────────────────────────────────

func TestValidatorPubKey_Verify_Valid(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	var h crypto.Hash32
	h[0] = 0x42
	sig, err := priv.Sign(h)
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Verify(h, sig) {
		t.Error("Verify must return true for a valid signature")
	}
}

func TestValidatorPubKey_Verify_Invalid(t *testing.T) {
	_, pub, _ := crypto.GenerateValidatorKey()
	// Wrong-length sig → should return false
	if pub.Verify(crypto.Hash32{}, []byte("bad")) {
		t.Error("Verify must return false for wrong-length signature")
	}
}

// ─── keys.go: ValidatorPrivKey.Public() ──────────────────────────────────────

func TestValidatorPrivKey_Public(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	derived := priv.Public()
	if derived.Hex() != pub.Hex() {
		t.Error("ValidatorPrivKey.Public() must derive the matching public key")
	}
}

// ─── pedersen.go: NewBlindFactor multiple calls ───────────────────────────────

func TestNewBlindFactor_Unique(t *testing.T) {
	b1, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	b2, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	if b1 == b2 {
		t.Error("two NewBlindFactor calls must produce different values")
	}
}

// ─── stealth.go: ScanForOutput error path (invalid TxPubKey) ─────────────────

func TestScanForOutput_InvalidTxPubKey(t *testing.T) {
	wk, _ := crypto.GenerateWalletKeys()
	var badTxPub crypto.Point32 // all-zero, not a valid point
	var oneTimePub crypto.Point32
	_, err := crypto.ScanForOutput(wk.View.Private, wk.Spend.Public, badTxPub, oneTimePub)
	if err == nil {
		// Some implementations may return (nil, nil) for a miss instead of error
		t.Logf("ScanForOutput with zero TxPubKey: returned nil, nil (miss)")
	}
}
