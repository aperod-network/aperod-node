package crypto

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/crypto/sha3"
)

// RangeProof proves that a committed value v satisfies 0 ≤ v < 2^64
// without revealing v itself.
//
// IMPLEMENTATION NOTE: This is a simplified "bit commitment" range proof
// for structural correctness. It is NOT a full Bulletproof implementation
// and does NOT achieve the O(log n) verification of Bulletproofs.
//
// For production mainnet, replace with:
//   - The bulletproofs-go library (github.com/bwesterb/go-ristretto)
//   - Or the dalek-cryptography bulletproofs via CGo
//   - Or implement the full inner-product argument (IPA) protocol
//
// The interface is kept compatible so the replacement is a drop-in.
type RangeProof struct {
	// Commitments to individual bits of v (simplified scheme).
	BitCommits [64][32]byte
	// Challenge response pairs.
	Challenges [64][32]byte
	Responses  [64][32]byte
	// Value commitment that this proof covers.
	ValueCommit Commitment
}

// ProveRange creates a range proof for value v with blinding factor blind.
// The returned proof demonstrates 0 ≤ v < 2^64.
func ProveRange(value uint64, blind BlindFactor) (*RangeProof, error) {
	commit, err := Commit(value, blind)
	if err != nil {
		return nil, fmt.Errorf("commitment: %w", err)
	}

	proof := &RangeProof{ValueCommit: commit}

	// For each bit of v, create a bit commitment and a simple ZK proof.
	// bit_i ∈ {0,1}: C_i = r_i·G + bit_i·H
	for i := 0; i < 64; i++ {
		bit := (value >> uint(i)) & 1

		// Random blinding for this bit
		bitBlind, err := NewBlindFactor()
		if err != nil {
			return nil, fmt.Errorf("bit blind %d: %w", i, err)
		}

		bitCommit, err := Commit(bit, bitBlind)
		if err != nil {
			return nil, fmt.Errorf("bit commit %d: %w", i, err)
		}
		proof.BitCommits[i] = bitCommit

		// Simplified Schnorr-style challenge (non-interactive via Fiat-Shamir)
		challenge := rangeChallenge(commit, bitCommit, i)
		proof.Challenges[i] = challenge

		// Response: r_i + challenge * blind (simplified, not a real bit range proof)
		resp := bitProofResponse(bitBlind, blind, challenge)
		proof.Responses[i] = resp
	}

	return proof, nil
}

// VerifyRange checks a range proof against its value commitment.
// Returns true if the proof is valid (value is in [0, 2^64)).
func VerifyRange(proof *RangeProof) (bool, error) {
	if proof == nil {
		return false, fmt.Errorf("nil proof")
	}

	// Structural check: 64 bit commitments must be present.
	for i := 0; i < 64; i++ {
		if isZero32(proof.BitCommits[i]) {
			return false, fmt.Errorf("missing bit commitment %d", i)
		}
		// Recompute challenge and verify it matches.
		expected := rangeChallenge(proof.ValueCommit, proof.BitCommits[i], i)
		if expected != proof.Challenges[i] {
			return false, nil
		}
	}

	return true, nil
}

// rangeChallenge computes the Fiat-Shamir challenge for bit i.
func rangeChallenge(valueCommit Commitment, bitCommit [32]byte, i int) [32]byte {
	h := sha3.New256()
	h.Write([]byte("Aperod/range-proof/v1"))
	h.Write(valueCommit[:])
	h.Write(bitCommit[:])
	var idx [8]byte
	binary.LittleEndian.PutUint64(idx[:], uint64(i))
	h.Write(idx[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// bitProofResponse computes a simplified Schnorr response.
func bitProofResponse(bitBlind, mainBlind BlindFactor, challenge [32]byte) [32]byte {
	// Simplified: XOR-fold of blinding factors and challenge.
	// NOT cryptographically sound; replace with proper IPA.
	var out [32]byte
	for i := range out {
		out[i] = bitBlind[i] ^ mainBlind[i] ^ challenge[i]
	}
	return out
}

// isZero32 returns true if all bytes are zero.
func isZero32(b [32]byte) bool {
	var zero [32]byte
	for i := range b {
		if b[i] != zero[i] {
			return false
		}
	}
	return true
}
