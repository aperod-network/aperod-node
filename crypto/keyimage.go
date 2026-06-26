package crypto

import (
        "crypto/sha512"

        "filippo.io/edwards25519"
)

// KeyImage is the unique "fingerprint" of a spent UTXO.
// I = x · Hp(P) where x is the one-time private key and P is the one-time public key.
// A node stores all used Key Images; spending the same UTXO twice produces the
// same Key Image, making double-spends immediately detectable even with ring signatures.
type KeyImage [32]byte

// ComputeKeyImage computes I = x · Hp(P).
// x is the one-time private scalar for this UTXO.
// P is the corresponding one-time public key (oneTimePub from StealthAddress).
func ComputeKeyImage(x Scalar32, P Point32) (KeyImage, error) {
        xScalar, err := ScalarFromBytes(x[:])
        if err != nil {
                return KeyImage{}, err
        }

        // Hp(P): hash P to a curve point
        hpP := hashToCurvePoint(P[:])

        // I = x · Hp(P)
        I := (&edwards25519.Point{}).ScalarMult(xScalar, hpP)

        var ki KeyImage
        copy(ki[:], I.Bytes())
        return ki, nil
}

// hashToCurvePoint maps arbitrary bytes to an Edwards25519 curve point using
// the try-and-increment method seeded by SHA-512.
func hashToCurvePoint(data []byte) *edwards25519.Point {
        h := sha512.New()
        h.Write([]byte("Aperod/hash-to-curve/v1"))
        h.Write(data)
        var wide [64]byte
        copy(wide[:], h.Sum(nil))
        s, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
        return (&edwards25519.Point{}).ScalarBaseMult(s)
}

// HashToCurvePoint is exported for use in RingCT.
func HashToCurvePoint(data []byte) *edwards25519.Point {
        return hashToCurvePoint(data)
}

// Equal returns true if two Key Images are identical (detects double-spends).
func (ki KeyImage) Equal(other KeyImage) bool {
        return ki == other
}

// KeyImageSet is a thread-unsafe set of used Key Images.
// The production implementation lives in store/utxo.go with LevelDB persistence.
type KeyImageSet map[KeyImage]struct{}

// NewKeyImageSet creates an empty set.
func NewKeyImageSet() KeyImageSet { return make(KeyImageSet) }

// Add marks a Key Image as spent.
func (s KeyImageSet) Add(ki KeyImage) { s[ki] = struct{}{} }

// Contains returns true if the Key Image has already been spent.
func (s KeyImageSet) Contains(ki KeyImage) bool {
        _, ok := s[ki]
        return ok
}
