// Package crypto provides cryptographic primitives for the Aperod blockchain.
// All hashing uses SHA3-256. Block hashes, transaction hashes, and key images
// are all 32-byte arrays to avoid aliasing bugs.
package crypto

import (
	"golang.org/x/crypto/sha3"
)

// Hash32 is a 32-byte hash.
type Hash32 [32]byte

// Zero returns true if the hash is all zeros.
func (h Hash32) Zero() bool { return h == (Hash32{}) }

// Bytes returns a slice copy.
func (h Hash32) Bytes() []byte {
	out := make([]byte, 32)
	copy(out, h[:])
	return out
}

// HashBytes hashes arbitrary data with SHA3-256.
func HashBytes(data ...[]byte) Hash32 {
	h := sha3.New256()
	for _, d := range data {
		h.Write(d)
	}
	var out Hash32
	copy(out[:], h.Sum(nil))
	return out
}

// HashStr is a convenience wrapper for string inputs.
func HashStr(s string) Hash32 {
	return HashBytes([]byte(s))
}

// HashConcat hashes the concatenation of all byte slices.
func HashConcat(parts ...[]byte) Hash32 {
	return HashBytes(parts...)
}

// DoubleSHA3 applies SHA3-256 twice (used for checksums).
func DoubleSHA3(data []byte) Hash32 {
	first := HashBytes(data)
	return HashBytes(first[:])
}
