package crypto

import (
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"fmt"

	"filippo.io/edwards25519"
)

// CLSAGSignature is a compact linkable spontaneous anonymous group signature
// which additionally proves that the input commitment and pseudo output differ
// by the same blinding-factor difference used in D.
type CLSAGSignature struct {
	C1       [32]byte
	S        [][32]byte
	KeyImage KeyImage
	D        Point32
}

// CLSAGSign creates a CLSAG signature for realIdx. ringKeys and
// ringCommitments describe the same ordered set of input outputs. pseudoOut
// must commit to the input amount with pseudoBlind.
func CLSAGSign(message Hash32, ringKeys []Point32, ringCommitments []Commitment,
	pseudoOut Commitment, realIdx int, privateKey Scalar32, inputBlind, pseudoBlind BlindFactor,
) (*CLSAGSignature, error) {
	keys, commitments, pseudo, err := validateCLSAGInputs(ringKeys, ringCommitments, pseudoOut, realIdx)
	if err != nil {
		return nil, err
	}
	x, err := ScalarFromBytes(privateKey[:])
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	inBlind, err := ScalarFromBytes(inputBlind[:])
	if err != nil {
		return nil, fmt.Errorf("input blind: %w", err)
	}
	outBlind, err := ScalarFromBytes(pseudoBlind[:])
	if err != nil {
		return nil, fmt.Errorf("pseudo blind: %w", err)
	}
	if (&edwards25519.Point{}).ScalarBaseMult(x).Equal(keys[realIdx]) != 1 {
		return nil, fmt.Errorf("private key does not match ring key %d", realIdx)
	}

	// z is the commitment blinding difference: C_real - pseudoOut = zG.
	z := edwards25519.NewScalar().Subtract(inBlind, outBlind)
	expectedDiff := (&edwards25519.Point{}).ScalarBaseMult(z)
	actualDiff := (&edwards25519.Point{}).Subtract(commitments[realIdx], pseudo)
	if expectedDiff.Equal(actualDiff) != 1 {
		return nil, fmt.Errorf("real commitment is not consistent with input and pseudo blinding factors")
	}

	ki, err := ComputeKeyImage(privateKey, ringKeys[realIdx])
	if err != nil {
		return nil, fmt.Errorf("key image: %w", err)
	}
	I, err := checkedCLSAGPrimePoint(Point32(ki), "key image")
	if err != nil {
		return nil, err
	}
	D := (&edwards25519.Point{}).ScalarMult(z, HashToCurvePoint(ringKeys[realIdx][:]))
	var dBytes Point32
	copy(dBytes[:], D.Bytes())
	if _, err := checkedCLSAGDPoint(dBytes); err != nil {
		return nil, err
	}

	muP, muC := clsagMus(message, ringKeys, ringCommitments, pseudoOut, ki, dBytes)
	witness := edwards25519.NewScalar().Multiply(muP, x)
	witness.MultiplyAdd(muC, z, witness)

	w := clsagAggregateW(muP, muC, keys, commitments, pseudo)
	J := new(edwards25519.Point).Add(
		new(edwards25519.Point).ScalarMult(muP, I),
		new(edwards25519.Point).ScalarMult(muC, D),
	)
	alpha, err := randomScalar()
	if err != nil {
		return nil, fmt.Errorf("random alpha: %w", err)
	}
	s := make([]*edwards25519.Scalar, RingSize)
	c := make([]*edwards25519.Scalar, RingSize+1)
	l := (&edwards25519.Point{}).ScalarBaseMult(alpha)
	r := (&edwards25519.Point{}).ScalarMult(alpha, HashToCurvePoint(ringKeys[realIdx][:]))
	c[nextIdx(realIdx)] = clsagChallenge(message, ringKeys, ringCommitments, pseudoOut, ki, dBytes, realIdx, l, r)

	for offset := 1; offset < RingSize; offset++ {
		i := (realIdx + offset) % RingSize
		s[i], err = randomScalar()
		if err != nil {
			return nil, fmt.Errorf("random response %d: %w", i, err)
		}
		l = ringL(s[i], c[i], w[i])
		r = ringR(s[i], c[i], HashToCurvePoint(ringKeys[i][:]), J)
		c[nextIdx(i)] = clsagChallenge(message, ringKeys, ringCommitments, pseudoOut, ki, dBytes, i, l, r)
	}
	s[realIdx] = edwards25519.NewScalar().Subtract(alpha, edwards25519.NewScalar().Multiply(c[realIdx], witness))

	sig := &CLSAGSignature{S: make([][32]byte, RingSize), KeyImage: ki, D: dBytes}
	copy(sig.C1[:], c[0].Bytes())
	for i := range s {
		copy(sig.S[i][:], s[i].Bytes())
	}
	return sig, nil
}

// CLSAGVerify verifies a CLSAG signature and all canonical encoding,
// uniqueness, and prime-subgroup requirements on its public inputs.
func CLSAGVerify(message Hash32, ringKeys []Point32, ringCommitments []Commitment,
	pseudoOut Commitment, sig *CLSAGSignature,
) (bool, error) {
	if sig == nil {
		return false, fmt.Errorf("nil signature")
	}
	if len(sig.S) != RingSize {
		return false, fmt.Errorf("CLSAG response length must be %d, got %d", RingSize, len(sig.S))
	}
	keys, commitments, pseudo, err := validateCLSAGInputs(ringKeys, ringCommitments, pseudoOut, 0)
	if err != nil {
		return false, err
	}
	I, err := checkedCLSAGPrimePoint(Point32(sig.KeyImage), "key image")
	if err != nil {
		return false, err
	}
	D, err := checkedCLSAGDPoint(sig.D)
	if err != nil {
		return false, err
	}
	c0, err := ScalarFromBytes(sig.C1[:])
	if err != nil {
		return false, fmt.Errorf("C1: %w", err)
	}
	muP, muC := clsagMus(message, ringKeys, ringCommitments, pseudoOut, sig.KeyImage, sig.D)
	w := clsagAggregateW(muP, muC, keys, commitments, pseudo)
	J := new(edwards25519.Point).Add(
		new(edwards25519.Point).ScalarMult(muP, I),
		new(edwards25519.Point).ScalarMult(muC, D),
	)

	c := c0
	for i := 0; i < RingSize; i++ {
		s, err := ScalarFromBytes(sig.S[i][:])
		if err != nil {
			return false, fmt.Errorf("response %d: %w", i, err)
		}
		l := ringL(s, c, w[i])
		r := ringR(s, c, HashToCurvePoint(ringKeys[i][:]), J)
		c = clsagChallenge(message, ringKeys, ringCommitments, pseudoOut, sig.KeyImage, sig.D, i, l, r)
	}
	return subtle.ConstantTimeCompare(c0.Bytes(), c.Bytes()) == 1, nil
}

func validateCLSAGInputs(ringKeys []Point32, ringCommitments []Commitment, pseudoOut Commitment, realIdx int) ([]*edwards25519.Point, []*edwards25519.Point, *edwards25519.Point, error) {
	if len(ringKeys) != RingSize || len(ringCommitments) != RingSize {
		return nil, nil, nil, fmt.Errorf("CLSAG requires exactly %d ring keys and commitments", RingSize)
	}
	if realIdx < 0 || realIdx >= RingSize {
		return nil, nil, nil, fmt.Errorf("realIdx %d out of range", realIdx)
	}
	keys := make([]*edwards25519.Point, RingSize)
	commitments := make([]*edwards25519.Point, RingSize)
	seen := make(map[Point32]struct{}, RingSize)
	for i := 0; i < RingSize; i++ {
		if _, exists := seen[ringKeys[i]]; exists {
			return nil, nil, nil, fmt.Errorf("ring key %d is a duplicate", i)
		}
		seen[ringKeys[i]] = struct{}{}
		var err error
		if keys[i], err = checkedCLSAGSubgroupPoint(ringKeys[i], fmt.Sprintf("ring key %d", i), false); err != nil {
			return nil, nil, nil, err
		}
		if commitments[i], err = checkedCLSAGSubgroupPoint(Point32(ringCommitments[i]), fmt.Sprintf("ring commitment %d", i), true); err != nil {
			return nil, nil, nil, err
		}
	}
	pseudo, err := checkedCLSAGSubgroupPoint(Point32(pseudoOut), "pseudo output", true)
	if err != nil {
		return nil, nil, nil, err
	}
	return keys, commitments, pseudo, nil
}

func checkedCLSAGPrimePoint(encoded Point32, name string) (*edwards25519.Point, error) {
	return checkedCLSAGSubgroupPoint(encoded, name, false)
}

// checkedCLSAGSubgroupPoint rejects non-canonical encodings and points with a
// torsion component. Commitments may be the canonical identity (for a zero
// value and zero blind); spend keys and key images may not.
func checkedCLSAGSubgroupPoint(encoded Point32, name string, allowIdentity bool) (*edwards25519.Point, error) {
	p, err := PointFromBytes(encoded[:])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if p.Equal(edwards25519.NewIdentityPoint()) == 1 {
		if !allowIdentity {
			return nil, fmt.Errorf("%s is the identity point", name)
		}
		var identity Point32
		copy(identity[:], edwards25519.NewIdentityPoint().Bytes())
		if encoded != identity {
			return nil, fmt.Errorf("%s identity is not canonically encoded", name)
		}
		return p, nil
	}
	canonical, err := CanonicalKeyImage(KeyImage(encoded))
	if err != nil {
		return nil, fmt.Errorf("%s subgroup check: %w", name, err)
	}
	if Point32(canonical) != encoded {
		return nil, fmt.Errorf("%s is not in the prime-order subgroup", name)
	}
	return p, nil
}

// checkedCLSAGDPoint accepts the canonical identity for D.  A one-input
// balance can legitimately have equal input and pseudo-output blinds (z=0),
// yielding D=0; unlike a key image this does not weaken linkability.
func checkedCLSAGDPoint(encoded Point32) (*edwards25519.Point, error) {
	return checkedCLSAGSubgroupPoint(encoded, "D", true)
}

func clsagAggregateW(muP, muC *edwards25519.Scalar, keys, commitments []*edwards25519.Point, pseudo *edwards25519.Point) []*edwards25519.Point {
	w := make([]*edwards25519.Point, RingSize)
	for i := range w {
		diff := (&edwards25519.Point{}).Subtract(commitments[i], pseudo)
		w[i] = new(edwards25519.Point).Add(
			new(edwards25519.Point).ScalarMult(muP, keys[i]),
			new(edwards25519.Point).ScalarMult(muC, diff),
		)
	}
	return w
}

func clsagMus(message Hash32, keys []Point32, commitments []Commitment, pseudo Commitment, image KeyImage, d Point32) (*edwards25519.Scalar, *edwards25519.Scalar) {
	return clsagHash("Aperod/CLSAG/v1/muP", message, keys, commitments, pseudo, image, d, -1, nil, nil),
		clsagHash("Aperod/CLSAG/v1/muC", message, keys, commitments, pseudo, image, d, -1, nil, nil)
}

func clsagChallenge(message Hash32, keys []Point32, commitments []Commitment, pseudo Commitment, image KeyImage, d Point32, index int, l, r *edwards25519.Point) *edwards25519.Scalar {
	return clsagHash("Aperod/CLSAG/v1/challenge", message, keys, commitments, pseudo, image, d, index, l, r)
}

func clsagHash(domain string, message Hash32, keys []Point32, commitments []Commitment, pseudo Commitment, image KeyImage, d Point32, index int, l, r *edwards25519.Point) *edwards25519.Scalar {
	h := sha512.New()
	h.Write([]byte(domain))
	h.Write(message[:])
	for i := 0; i < RingSize; i++ {
		h.Write(keys[i][:])
		h.Write(commitments[i][:])
	}
	h.Write(pseudo[:])
	h.Write(image[:])
	h.Write(d[:])
	if index >= 0 {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(index))
		h.Write(b[:])
		h.Write(l.Bytes())
		h.Write(r.Bytes())
	}
	var wide [64]byte
	copy(wide[:], h.Sum(nil))
	s, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
	return s
}
