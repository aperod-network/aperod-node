package crypto

// f001_deanon_test.go — Regression test for F-001 / F-035 (ring anonymity).
//
// Gh0stAnts finding F-001: the old hashToCurvePoint computed Hp(P) = s·G where
// s = SHA-512("domain"||P) mod l.  Because s was publicly computable, any
// observer could identify the real ring signer:
//
//   For each ring member P_i:
//     s_i = SHA-512("Aperod/hash-to-curve/v1" || P_i) mod l   (public scalar)
//     pred_i = s_i · P_i                                        (= x·s_i·G if P_i=x·G is real)
//     if pred_i == sig.KeyImage → P_i is the real signer
//
// The current Hp (v2, try-and-increment SHA-256) produces points with no known
// discrete log, so the scalar-multiplication trick yields only garbage values
// that never match the key image.
//
// This test runs the full PoC and asserts it identifies ZERO ring members.
// A PASS means anonymity holds; a FAIL means the old broken Hp is back.

import (
	"crypto/sha512"
	"testing"

	"filippo.io/edwards25519"
)

// oldHpScalar replicates the broken F-001 construction:
//
//	s = SHA-512("Aperod/hash-to-curve/v1" || pubBytes) mod l
//
// With the old code, Hp(P) = s·G, meaning I = x·Hp(P_real) = s_real·P_real.
// Any observer who computes s_i·P_i for every ring member and compares to the
// key image trivially identifies the real signer.
func oldHpScalar(pubBytes []byte) (*edwards25519.Scalar, error) {
	h := sha512.New()
	h.Write([]byte("Aperod/hash-to-curve/v1"))
	h.Write(pubBytes)
	var wide [64]byte
	copy(wide[:], h.Sum(nil))
	return edwards25519.NewScalar().SetUniformBytes(wide[:])
}

// TestKeyImageDeanonymization is the researcher PoC (F-001).
//
// It performs the scalar-multiplication attack against a freshly signed ring
// transaction and asserts that NO ring member is identified as the real signer.
// A failure here means Hp has regressed to the broken s·G construction.
func TestKeyImageDeanonymization(t *testing.T) {
	// Generate the real signer's key pair.
	kp, err := GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}

	// Build a ring with the real key at position 7.
	const realIdx = 7
	ring := make([]RingMember, RingSize)
	for i := range ring {
		d, err := GenerateWalletKeys()
		if err != nil {
			t.Fatalf("GenerateWalletKeys decoy %d: %v", i, err)
		}
		ring[i] = d.Spend.Public
	}
	ring[realIdx] = kp.Spend.Public

	msg := HashStr("f001-deanon-poc")

	sig, err := MLSAGSign(msg, ring, realIdx, kp.Spend.Private)
	if err != nil {
		t.Fatalf("MLSAGSign: %v", err)
	}

	// Verify the signature is valid (sanity check).
	ok, err := MLSAGVerify(msg, ring, sig)
	if err != nil || !ok {
		t.Fatalf("MLSAGVerify: ok=%v err=%v", ok, err)
	}

	// ── Deanonymization attack ────────────────────────────────────────────────
	// For each ring member P_i, compute:
	//   s_i = SHA-512("Aperod/hash-to-curve/v1" || P_i) mod l   (old broken domain)
	//   pred_i = s_i · P_i
	// With the old Hp this matched the key image exactly at i=realIdx.
	// With the v2 Hp (try-and-increment) it must NEVER match.

	keyImagePoint, err := PointFromBytes(sig.KeyImage[:])
	if err != nil {
		t.Fatalf("parse key image point: %v", err)
	}

	identified := -1
	for i, member := range ring {
		s, err := oldHpScalar(member[:])
		if err != nil {
			t.Fatalf("oldHpScalar ring[%d]: %v", i, err)
		}
		P, err := PointFromBytes(member[:])
		if err != nil {
			t.Fatalf("PointFromBytes ring[%d]: %v", i, err)
		}
		// pred = s · P  (would equal key image under old broken Hp)
		pred := new(edwards25519.Point).ScalarMult(s, P)
		if pred.Equal(keyImagePoint) == 1 {
			identified = i
			break
		}
	}

	if identified != -1 {
		t.Errorf("F-001 deanonymization PoC SUCCEEDED: ring member %d identified as real signer — hashToCurvePoint has regressed to s·G construction", identified)
	} else {
		t.Logf("F-001 deanonymization PoC correctly failed: Hp(P) has no known discrete log (v2 try-and-increment)")
	}
}
