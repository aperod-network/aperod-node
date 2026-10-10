package crypto

import (
        "crypto/sha512"
        "crypto/subtle"
	"encoding/binary"
        "fmt"

        "filippo.io/edwards25519"
)

// RingSize is the number of public keys in a ring (1 real + N-1 decoys).
// 16 members: 1 real + 15 decoys for a strong anonymity set.
const RingSize = 16

// MLSAGSignature is a Multi-Layered Linkable Spontaneous Anonymous Group signature.
// It proves that the signer holds the private key for one of the ring members
// without revealing which one.
type MLSAGSignature struct {
        // C0 is the initial challenge scalar.
        C0 [32]byte
        // SS[i] is the response scalar for ring member i.
        SS [][32]byte // length = RingSize
        // KeyImage prevents double-spending.
        KeyImage KeyImage
	// BlindSS and ValueSS are the v4 commitment-opening responses. They are
	// empty for legacy signatures and must contain RingSize canonical scalars
	// for v4 signatures.
	BlindSS [][32]byte
	ValueSS [][32]byte
	// LinkR/LinkS are a direct Schnorr proof under the disclosed v4 real ring
	// member. This lets the state transition remove the exact proven UTXO even
	// when multiple outputs share an identical Pedersen commitment.
	LinkR Point32
	LinkS [32]byte
}

// RingMember is a public key in the ring (either the real UTXO or a decoy).
type RingMember = Point32

// MLSAGSign creates a ring signature over message (a hash) using the real key
// at position realIdx within the ring.
//
// Parameters:
//   - message: the transaction hash being signed
//   - ring: slice of RingSize public keys (spend keys of UTXOs)
//   - realIdx: index of the real key in ring (0 ≤ realIdx < RingSize)
//   - privKey: one-time private scalar for the real UTXO
func MLSAGSign(message Hash32, ring []RingMember, realIdx int, privKey Scalar32) (*MLSAGSignature, error) {
        if len(ring) != RingSize {
                return nil, fmt.Errorf("ring size must be %d, got %d", RingSize, len(ring))
        }
        if realIdx < 0 || realIdx >= RingSize {
                return nil, fmt.Errorf("realIdx %d out of range [0, %d)", realIdx, RingSize)
        }
        // C3 fix: reject rings with duplicate members.  A ring where all 16 keys
        // are identical still produces a valid MLSAG challenge chain but collapses
        // the anonymity set to 1, trivially identifying the signer.
        {
                seen := make(map[RingMember]struct{}, RingSize)
                for i, m := range ring {
                        if _, dup := seen[m]; dup {
                                return nil, fmt.Errorf("ring member %d is a duplicate — ring must contain distinct keys", i)
                        }
                        seen[m] = struct{}{}
                }
        }

        x, err := ScalarFromBytes(privKey[:])
        if err != nil {
                return nil, fmt.Errorf("private key: %w", err)
        }

        // Compute key image: I = x · Hp(P_real)
        ki, err := ComputeKeyImage(privKey, ring[realIdx])
        if err != nil {
                return nil, fmt.Errorf("key image: %w", err)
        }

        I, err := PointFromBytes(ki[:])
        if err != nil {
                return nil, fmt.Errorf("key image point: %w", err)
        }

        // --- MLSAG construction ---
        ss := make([]*edwards25519.Scalar, RingSize)
        cc := make([]*edwards25519.Scalar, RingSize+1)

        // Step 1: random alpha for the real index
        alpha, err := randomScalar()
        if err != nil {
                return nil, fmt.Errorf("random alpha: %w", err)
        }

        // Step 2: compute L_real = alpha·G, R_real = alpha·Hp(P_real)
        L_real := (&edwards25519.Point{}).ScalarBaseMult(alpha)
        HpReal := HashToCurvePoint(ring[realIdx][:])
        R_real := (&edwards25519.Point{}).ScalarMult(alpha, HpReal)

        // Step 3: compute challenge c_{real+1}
        cc[nextIdx(realIdx)] = ringChallenge(message, L_real, R_real)

        // Step 4: walk the ring forward from real+1
        for i := 1; i < RingSize; i++ {
                idx := (realIdx + i) % RingSize
                nextI := (realIdx + i + 1) % RingSize

                // Random response for non-real members
                ss[idx], err = randomScalar()
                if err != nil {
                        return nil, fmt.Errorf("random s[%d]: %w", idx, err)
                }

                // L_i = ss[i]·G + cc[i]·P_i
                Pi, err := PointFromBytes(ring[idx][:])
                if err != nil {
                        return nil, fmt.Errorf("ring member %d: %w", idx, err)
                }
                L_i := ringL(ss[idx], cc[(realIdx+i)%RingSize], Pi)

                // R_i = ss[i]·Hp(P_i) + cc[i]·I
                HpI := HashToCurvePoint(ring[idx][:])
                R_i := ringR(ss[idx], cc[(realIdx+i)%RingSize], HpI, I)

                cc[nextI] = ringChallenge(message, L_i, R_i)
        }

        // Step 5: close the ring — compute s_real = alpha - cc[real]·x (mod order)
        cx := edwards25519.NewScalar().Multiply(cc[realIdx], x)
        ss[realIdx] = edwards25519.NewScalar().Subtract(alpha, cx)

        // Encode
        sig := &MLSAGSignature{
                KeyImage: ki,
                SS:       make([][32]byte, RingSize),
        }
        copy(sig.C0[:], cc[0].Bytes())
        for i := range ss {
                copy(sig.SS[i][:], ss[i].Bytes())
        }
        return sig, nil
}

// MLSAGVerify verifies an MLSAG ring signature.
func MLSAGVerify(message Hash32, ring []RingMember, sig *MLSAGSignature) (bool, error) {
        if sig == nil {
                return false, fmt.Errorf("nil signature")
        }
        if len(ring) != RingSize || len(sig.SS) != RingSize {
                return false, fmt.Errorf("ring size mismatch")
        }
        // C3 fix: reject rings with duplicate members at verification time too.
        // Without this check a verifier would accept a ring crafted by a signer
        // that skipped the uniqueness guard.
        {
                seen := make(map[RingMember]struct{}, RingSize)
                for i, m := range ring {
                        if _, dup := seen[m]; dup {
                                return false, fmt.Errorf("ring member %d is a duplicate — ring must contain distinct keys", i)
                        }
                        seen[m] = struct{}{}
                }
        }

        I, err := PointFromBytes(sig.KeyImage[:])
        if err != nil {
                return false, fmt.Errorf("key image: %w", err)
        }
        // Torsion subgroup check (CVE-2017-12424 analogue for APRO RingCT).
        // Ed25519 has an 8-element torsion subgroup.  A forged key image
        // I' = I + T (T small-order) carries the same ring equations but maps
        // to a different raw encoding, letting an attacker spend the same UTXO
        // up to 8 times.  Reject any key image whose canonical form differs
        // from its raw encoding — i.e. any key image with a torsion component.
        canonicalKI, canonErr := CanonicalKeyImage(sig.KeyImage)
        if canonErr != nil {
                return false, fmt.Errorf("key image subgroup check: %w", canonErr)
        }
        if canonicalKI != sig.KeyImage {
                return false, fmt.Errorf("key image is not in the prime-order subgroup (torsion component detected)")
        }

        c0, err := ScalarFromBytes(sig.C0[:])
        if err != nil {
                return false, fmt.Errorf("c0: %w", err)
        }

        cc := make([]*edwards25519.Scalar, RingSize+1)
        cc[0] = c0

        for i := 0; i < RingSize; i++ {
                s_i, err := ScalarFromBytes(sig.SS[i][:])
                if err != nil {
                        return false, fmt.Errorf("ss[%d]: %w", i, err)
                }

                Pi, err := PointFromBytes(ring[i][:])
                if err != nil {
                        return false, fmt.Errorf("ring[%d]: %w", i, err)
                }

                L_i := ringL(s_i, cc[i], Pi)
                HpI := HashToCurvePoint(ring[i][:])
                R_i := ringR(s_i, cc[i], HpI, I)

                cc[i+1] = ringChallenge(message, L_i, R_i)
        }

        // Verify that the ring closes: c_0 == cc[RingSize]
        return subtle.ConstantTimeCompare(cc[0].Bytes(), cc[RingSize].Bytes()) == 1, nil
}

// MLSAGSignV4 signs a ring while proving that the same hidden ring member
// owns both the one-time private key and the opening of amountCommit.
//
// The additional response vectors form an OR proof of:
//   P_i = x_i*G and amountCommit = r_i*G + value_i*H
// for one common i. The value is supplied as a uint64 because the transaction
// amount domain is uint64; the Pedersen commitment and proof remain hidden.
func MLSAGSignV4(message Hash32, ring []RingMember, realIdx int,
	privKey Scalar32, blind BlindFactor, value uint64,
) (*MLSAGSignature, error) {
	if err := validateRingForV4(ring, realIdx); err != nil {
		return nil, err
	}
	x, err := ScalarFromBytes(privKey[:])
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	r, err := ScalarFromBytes(blind[:])
	if err != nil {
		return nil, fmt.Errorf("blinding factor: %w", err)
	}
	v := scalarFromUint64(value)
	amountCommit := amountCommitForV4(r, v)
	claimed, err := PointFromBytes(amountCommit[:])
	if err != nil {
		return nil, fmt.Errorf("amount commitment: %w", err)
	}

	ki, err := ComputeKeyImage(privKey, ring[realIdx])
	if err != nil {
		return nil, fmt.Errorf("key image: %w", err)
	}
	I, err := PointFromBytes(ki[:])
	if err != nil {
		return nil, fmt.Errorf("key image point: %w", err)
	}

	ssX := make([]*edwards25519.Scalar, RingSize)
	ssR := make([]*edwards25519.Scalar, RingSize)
	ssV := make([]*edwards25519.Scalar, RingSize)
	cc := make([]*edwards25519.Scalar, RingSize+1)

	alphaX, err := randomScalar()
	if err != nil {
		return nil, fmt.Errorf("random alpha x: %w", err)
	}
	alphaR, err := randomScalar()
	if err != nil {
		return nil, fmt.Errorf("random alpha r: %w", err)
	}
	alphaV, err := randomScalar()
	if err != nil {
		return nil, fmt.Errorf("random alpha v: %w", err)
	}

	lReal := (&edwards25519.Point{}).ScalarBaseMult(alphaX)
	rReal := (&edwards25519.Point{}).ScalarMult(alphaX, HashToCurvePoint(ring[realIdx][:]))
	qReal := new(edwards25519.Point).Add(
		(&edwards25519.Point{}).ScalarBaseMult(alphaR),
		(&edwards25519.Point{}).ScalarMult(alphaV, hGenerator),
	)
	cc[nextIdx(realIdx)] = ringChallengeV4(message, uint32(realIdx), lReal, rReal, qReal)

	for i := 1; i < RingSize; i++ {
		idx := (realIdx + i) % RingSize
		nextI := (realIdx + i + 1) % RingSize
		ssX[idx], err = randomScalar()
		if err != nil {
			return nil, fmt.Errorf("random s x[%d]: %w", idx, err)
		}
		ssR[idx], err = randomScalar()
		if err != nil {
			return nil, fmt.Errorf("random s r[%d]: %w", idx, err)
		}
		ssV[idx], err = randomScalar()
		if err != nil {
			return nil, fmt.Errorf("random s v[%d]: %w", idx, err)
		}
		pi, err := PointFromBytes(ring[idx][:])
		if err != nil {
			return nil, fmt.Errorf("ring member %d: %w", idx, err)
		}
		l := ringL(ssX[idx], cc[(realIdx+i)%RingSize], pi)
		hp := HashToCurvePoint(ring[idx][:])
		ri := ringR(ssX[idx], cc[(realIdx+i)%RingSize], hp, I)
		qi := ringCommitmentQ(ssR[idx], ssV[idx], cc[(realIdx+i)%RingSize], claimed)
		cc[nextI] = ringChallengeV4(message, uint32(idx), l, ri, qi)
	}

	cx := edwards25519.NewScalar().Multiply(cc[realIdx], x)
	cr := edwards25519.NewScalar().Multiply(cc[realIdx], r)
	cv := edwards25519.NewScalar().Multiply(cc[realIdx], v)
	ssX[realIdx] = edwards25519.NewScalar().Subtract(alphaX, cx)
	ssR[realIdx] = edwards25519.NewScalar().Subtract(alphaR, cr)
	ssV[realIdx] = edwards25519.NewScalar().Subtract(alphaV, cv)

	sig := &MLSAGSignature{
		KeyImage: ki,
		SS:       make([][32]byte, RingSize),
		BlindSS:  make([][32]byte, RingSize),
		ValueSS:  make([][32]byte, RingSize),
	}
	copy(sig.C0[:], cc[0].Bytes())
	for i := 0; i < RingSize; i++ {
		copy(sig.SS[i][:], ssX[i].Bytes())
		copy(sig.BlindSS[i][:], ssR[i].Bytes())
		copy(sig.ValueSS[i][:], ssV[i].Bytes())
	}
	linkAlpha, err := randomScalar()
	if err != nil {
		return nil, fmt.Errorf("random link alpha: %w", err)
	}
	linkR := (&edwards25519.Point{}).ScalarBaseMult(linkAlpha)
	linkC := ringLinkChallenge(message, uint32(realIdx), ring[realIdx], amountCommit, ki, linkR)
	linkCX := edwards25519.NewScalar().Multiply(linkC, x)
	linkS := edwards25519.NewScalar().Subtract(linkAlpha, linkCX)
	copy(sig.LinkR[:], linkR.Bytes())
	copy(sig.LinkS[:], linkS.Bytes())
	return sig, nil
}

// MLSAGVerifyV4 verifies the commitment-binding v4 ring proof.
func MLSAGVerifyV4(message Hash32, ring []RingMember, amountCommit Commitment,
	realIdx int, sig *MLSAGSignature,
) (bool, error) {
	if sig == nil {
		return false, fmt.Errorf("nil signature")
	}
	if len(sig.SS) != RingSize || len(sig.BlindSS) != RingSize || len(sig.ValueSS) != RingSize {
		return false, fmt.Errorf("v4 signature response length mismatch")
	}
	if err := validateRingForV4(ring, 0); err != nil {
		return false, err
	}
	if realIdx < 0 || realIdx >= RingSize {
		return false, fmt.Errorf("realIdx %d out of range [0, %d)", realIdx, RingSize)
	}
	I, err := PointFromBytes(sig.KeyImage[:])
	if err != nil {
		return false, fmt.Errorf("key image: %w", err)
	}
	canonicalKI, err := CanonicalKeyImage(sig.KeyImage)
	if err != nil {
		return false, fmt.Errorf("key image subgroup check: %w", err)
	}
	if canonicalKI != sig.KeyImage {
		return false, fmt.Errorf("key image is not in the prime-order subgroup")
	}
	c0, err := ScalarFromBytes(sig.C0[:])
	if err != nil {
		return false, fmt.Errorf("c0: %w", err)
	}
	claimed, err := PointFromBytes(amountCommit[:])
	if err != nil {
		return false, fmt.Errorf("amount commitment: %w", err)
	}
	cc := make([]*edwards25519.Scalar, RingSize+1)
	cc[0] = c0
	for i := 0; i < RingSize; i++ {
		sx, err := ScalarFromBytes(sig.SS[i][:])
		if err != nil {
			return false, fmt.Errorf("ss[%d]: %w", i, err)
		}
		sr, err := ScalarFromBytes(sig.BlindSS[i][:])
		if err != nil {
			return false, fmt.Errorf("blind ss[%d]: %w", i, err)
		}
		sv, err := ScalarFromBytes(sig.ValueSS[i][:])
		if err != nil {
			return false, fmt.Errorf("value ss[%d]: %w", i, err)
		}
		pi, err := PointFromBytes(ring[i][:])
		if err != nil {
			return false, fmt.Errorf("ring[%d]: %w", i, err)
		}
		l := ringL(sx, cc[i], pi)
		ri := ringR(sx, cc[i], HashToCurvePoint(ring[i][:]), I)
		qi := ringCommitmentQ(sr, sv, cc[i], claimed)
		cc[i+1] = ringChallengeV4(message, uint32(i), l, ri, qi)
	}
	if subtle.ConstantTimeCompare(cc[0].Bytes(), cc[RingSize].Bytes()) != 1 {
		return false, nil
	}

	linkR, err := PointFromBytes(sig.LinkR[:])
	if err != nil {
		return false, fmt.Errorf("link R: %w", err)
	}
	linkS, err := ScalarFromBytes(sig.LinkS[:])
	if err != nil {
		return false, fmt.Errorf("link S: %w", err)
	}
	realPub, err := PointFromBytes(ring[realIdx][:])
	if err != nil {
		return false, fmt.Errorf("real ring member: %w", err)
	}
	linkC := ringLinkChallenge(message, uint32(realIdx), ring[realIdx], amountCommit, sig.KeyImage, linkR)
	expectedR := ringL(linkS, linkC, realPub)
	return subtle.ConstantTimeCompare(expectedR.Bytes(), linkR.Bytes()) == 1, nil
}

func validateRingForV4(ring []RingMember, realIdx int) error {
	if len(ring) != RingSize {
		return fmt.Errorf("ring size must be %d, got %d", RingSize, len(ring))
	}
	if realIdx < 0 || realIdx >= RingSize {
		return fmt.Errorf("realIdx %d out of range [0, %d)", realIdx, RingSize)
	}
	seen := make(map[RingMember]struct{}, RingSize)
	for i, member := range ring {
		if _, exists := seen[member]; exists {
			return fmt.Errorf("ring member %d is a duplicate — ring must contain distinct keys", i)
		}
		seen[member] = struct{}{}
	}
	return nil
}

func ringCommitmentQ(sr, sv, c *edwards25519.Scalar, commitment *edwards25519.Point) *edwards25519.Point {
	q := (&edwards25519.Point{}).ScalarBaseMult(sr)
	q.Add(q, (&edwards25519.Point{}).ScalarMult(sv, hGenerator))
	q.Add(q, (&edwards25519.Point{}).ScalarMult(c, commitment))
	return q
}

func ringChallengeV4(msg Hash32, index uint32, l, r, q *edwards25519.Point) *edwards25519.Scalar {
	h := sha512.New()
	h.Write([]byte("Aperod/MLSAG/v4/commitment-binding"))
	h.Write(msg[:])
	var indexBytes [4]byte
	binary.LittleEndian.PutUint32(indexBytes[:], index)
	h.Write(indexBytes[:])
	h.Write(l.Bytes())
	h.Write(r.Bytes())
	h.Write(q.Bytes())
	var wide [64]byte
	copy(wide[:], h.Sum(nil))
	s, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
	return s
}

func ringLinkChallenge(msg Hash32, index uint32, pub RingMember, commitment Commitment,
	keyImage KeyImage, linkR *edwards25519.Point,
) *edwards25519.Scalar {
	h := sha512.New()
	h.Write([]byte("Aperod/RingCT/v4/real-input-link"))
	h.Write(msg[:])
	var indexBytes [4]byte
	binary.LittleEndian.PutUint32(indexBytes[:], index)
	h.Write(indexBytes[:])
	h.Write(pub[:])
	h.Write(commitment[:])
	h.Write(keyImage[:])
	h.Write(linkR.Bytes())
	var wide [64]byte
	copy(wide[:], h.Sum(nil))
	s, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
	return s
}

// amountCommitForV4 computes the commitment used by the v4 signing helper.
// It is kept private so callers cannot accidentally construct a commitment
// with a different generator than the rest of RingCT.
func amountCommitForV4(r, v *edwards25519.Scalar) Commitment {
	point := (&edwards25519.Point{}).ScalarBaseMult(r)
	point.Add(point, (&edwards25519.Point{}).ScalarMult(v, hGenerator))
	var commitment Commitment
	copy(commitment[:], point.Bytes())
	return commitment
}

// ringL computes L = s·G + c·P
func ringL(s, c *edwards25519.Scalar, P *edwards25519.Point) *edwards25519.Point {
        sG := (&edwards25519.Point{}).ScalarBaseMult(s)
        cP := (&edwards25519.Point{}).ScalarMult(c, P)
        return (&edwards25519.Point{}).Add(sG, cP)
}

// ringR computes R = s·Hp + c·I
func ringR(s, c *edwards25519.Scalar, Hp, I *edwards25519.Point) *edwards25519.Point {
        sHp := (&edwards25519.Point{}).ScalarMult(s, Hp)
        cI := (&edwards25519.Point{}).ScalarMult(c, I)
        return (&edwards25519.Point{}).Add(sHp, cI)
}

// ringChallenge computes SHA-512(message || L || R) as a scalar.
func ringChallenge(msg Hash32, L, R *edwards25519.Point) *edwards25519.Scalar {
        h := sha512.New()
        h.Write([]byte("Aperod/MLSAG/v1"))
        h.Write(msg[:])
        h.Write(L.Bytes())
        h.Write(R.Bytes())
        var wide [64]byte
        copy(wide[:], h.Sum(nil))
        s, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
        return s
}

// nextIdx wraps index within RingSize.
func nextIdx(i int) int { return (i + 1) % RingSize }
