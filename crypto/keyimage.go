package crypto

import (
	"crypto/sha256"
	"encoding/binary"

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

	// Hp(P): hash P to a curve point whose discrete log is unknown.
	hpP := hashToCurvePoint(P[:])

	// I = x · Hp(P)
	I := (&edwards25519.Point{}).ScalarMult(xScalar, hpP)

	var ki KeyImage
	copy(ki[:], I.Bytes())
	return ki, nil
}

// hashToCurvePoint maps arbitrary bytes to a prime-order Ed25519 point using
// try-and-increment (SHA-256 + counter). No one knows log_G(result).
//
// Security fix (v1 → v2): the previous implementation computed s·G where
// s = SHA-512(domain||data) — a point whose discrete log s is PUBLIC.
// This allowed any observer to compute I_pred = s·P for every on-chain UTXO
// and match it against spent key images, fully breaking ring-signature
// anonymity. v2 uses try-and-increment, producing a point with no known
// discrete log (identical construction to bulletproof.go's hashToPoint).
func hashToCurvePoint(data []byte) *edwards25519.Point {
	h := sha256.New()
	for ctr := uint64(0); ; ctr++ {
		h.Reset()
		h.Write([]byte("Aperod/hash-to-curve/v2"))
		h.Write(data)
		var cb [8]byte
		binary.LittleEndian.PutUint64(cb[:], ctr)
		h.Write(cb[:])
		digest := h.Sum(nil)

		var buf [32]byte
		copy(buf[:], digest)

		// Try positive-x compressed point.
		buf[31] &= 0x7f
		if p, err := new(edwards25519.Point).SetBytes(buf[:]); err == nil {
			r := new(edwards25519.Point).MultByCofactor(p)
			if r.Equal(edwards25519.NewIdentityPoint()) == 0 {
				return r
			}
		}
		// Try negative-x.
		buf[31] = digest[31] | 0x80
		if p, err := new(edwards25519.Point).SetBytes(buf[:]); err == nil {
			r := new(edwards25519.Point).MultByCofactor(p)
			if r.Equal(edwards25519.NewIdentityPoint()) == 0 {
				return r
			}
		}
	}
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
