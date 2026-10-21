package crypto

import (
	"crypto/subtle"
	"encoding/binary"
	"fmt"

	"filippo.io/edwards25519"
)

// KeyImageDLEQProof proves knowledge of x such that P = x*G and
// I = x*Hp(P), without disclosing x. The context must bind the proof to the
// protocol action that consumes the key image.
type KeyImageDLEQProof struct {
	CommitmentG  Point32
	CommitmentHp Point32
	Response     Scalar32
}

// ProveKeyImage creates the canonical key image and a Chaum-Pedersen
// equality-of-discrete-logs proof. It refuses a secret that does not match P.
func ProveKeyImage(secret Scalar32, P Point32, context []byte) (KeyImage, KeyImageDLEQProof, error) {
	var proof KeyImageDLEQProof
	x, err := ScalarFromBytes(secret[:])
	if err != nil {
		return KeyImage{}, proof, fmt.Errorf("key-image secret: %w", err)
	}
	if _, err := checkedDLEQPoint("one-time public key", P); err != nil {
		return KeyImage{}, proof, err
	}
	expectedP := new(edwards25519.Point).ScalarBaseMult(x)
	if subtle.ConstantTimeCompare(expectedP.Bytes(), P[:]) != 1 {
		return KeyImage{}, proof, fmt.Errorf("key-image secret does not match one-time public key")
	}

	ki, err := ComputeKeyImage(secret, P)
	if err != nil {
		return KeyImage{}, proof, fmt.Errorf("compute key image: %w", err)
	}
	hp := hashToCurvePoint(P[:])
	w, err := randomScalar()
	if err != nil {
		return KeyImage{}, proof, fmt.Errorf("key-image proof nonce: %w", err)
	}
	// A zero nonce would produce identity commitments, which are forbidden by
	// the verifier. Reroll rather than emit a non-canonical proof.
	if w.Equal(edwards25519.NewScalar()) == 1 {
		w, err = randomScalar()
		if err != nil {
			return KeyImage{}, proof, fmt.Errorf("key-image proof nonce: %w", err)
		}
		if w.Equal(edwards25519.NewScalar()) == 1 {
			return KeyImage{}, proof, fmt.Errorf("key-image proof nonce is zero")
		}
	}

	A := new(edwards25519.Point).ScalarBaseMult(w)
	B := new(edwards25519.Point).ScalarMult(w, hp)
	copy(proof.CommitmentG[:], A.Bytes())
	copy(proof.CommitmentHp[:], B.Bytes())

	c := keyImageDLEQChallenge(P, ki, proof.CommitmentG, proof.CommitmentHp, context)
	z := new(edwards25519.Scalar).MultiplyAdd(c, x, w)
	copy(proof.Response[:], z.Bytes())
	return ki, proof, nil
}

// VerifyKeyImageProof checks that P and I use the same secret scalar and that
// the proof is bound to context. Invalid encodings and non-prime-subgroup
// points fail closed.
func VerifyKeyImageProof(P Point32, ki KeyImage, context []byte, proof KeyImageDLEQProof) error {
	p, err := checkedDLEQPoint("one-time public key", P)
	if err != nil {
		return err
	}
	imagePoint, err := checkedDLEQPoint("key image", Point32(ki))
	if err != nil {
		return err
	}
	A, err := checkedDLEQPoint("proof commitment G", proof.CommitmentG)
	if err != nil {
		return err
	}
	B, err := checkedDLEQPoint("proof commitment Hp", proof.CommitmentHp)
	if err != nil {
		return err
	}
	z, err := ScalarFromBytes(proof.Response[:])
	if err != nil {
		return fmt.Errorf("key-image proof response: %w", err)
	}

	hp := hashToCurvePoint(P[:])
	c := keyImageDLEQChallenge(P, ki, proof.CommitmentG, proof.CommitmentHp, context)
	leftG := new(edwards25519.Point).ScalarBaseMult(z)
	rightG := new(edwards25519.Point).Add(A, new(edwards25519.Point).ScalarMult(c, p))
	leftHp := new(edwards25519.Point).ScalarMult(z, hp)
	rightHp := new(edwards25519.Point).Add(B, new(edwards25519.Point).ScalarMult(c, imagePoint))
	if leftG.Equal(rightG) != 1 || leftHp.Equal(rightHp) != 1 {
		return fmt.Errorf("key-image DLEQ proof verification failed")
	}
	return nil
}

func checkedDLEQPoint(label string, encoded Point32) (*edwards25519.Point, error) {
	canonical, err := CanonicalKeyImage(KeyImage(encoded))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if subtle.ConstantTimeCompare(canonical[:], encoded[:]) != 1 {
		return nil, fmt.Errorf("%s is not in the prime-order subgroup", label)
	}
	point, err := PointFromBytes(encoded[:])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return point, nil
}

func keyImageDLEQChallenge(P Point32, ki KeyImage, A, B Point32, context []byte) *edwards25519.Scalar {
	transcript := make([]byte, 0, 32*4+8+len(context))
	transcript = append(transcript, []byte("aperod/stake/key-image-dleq/v1")...)
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(context)))
	transcript = append(transcript, size[:]...)
	transcript = append(transcript, context...)
	transcript = append(transcript, P[:]...)
	transcript = append(transcript, ki[:]...)
	transcript = append(transcript, A[:]...)
	transcript = append(transcript, B[:]...)
	return hashToScalar(transcript, "aperod/stake/key-image-dleq/challenge/v1")
}
