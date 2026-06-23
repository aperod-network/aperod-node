package core

import (
	"github.com/aperod/aperod/crypto"
)

// MerkleRoot computes the Merkle root of a list of transactions.
// Uses SHA3-256 pairwise. If the list is empty, returns the zero hash.
// If the list is odd, the last element is duplicated.
func MerkleRoot(txs []Transaction) crypto.Hash32 {
	if len(txs) == 0 {
		return crypto.Hash32{}
	}

	// Build leaf hashes
	hashes := make([]crypto.Hash32, len(txs))
	for i, tx := range txs {
		hashes[i] = tx.Hash()
	}

	return merkleReduce(hashes)
}

// merkleReduce reduces a slice of hashes to a single Merkle root.
func merkleReduce(hashes []crypto.Hash32) crypto.Hash32 {
	if len(hashes) == 1 {
		return hashes[0]
	}

	// Pad to even length
	if len(hashes)%2 == 1 {
		hashes = append(hashes, hashes[len(hashes)-1])
	}

	next := make([]crypto.Hash32, len(hashes)/2)
	for i := 0; i < len(hashes); i += 2 {
		next[i/2] = crypto.HashBytes(hashes[i][:], hashes[i+1][:])
	}

	return merkleReduce(next)
}

// MerkleProof is a Merkle inclusion proof for a single transaction.
type MerkleProof struct {
	// Leaf is the hash of the transaction being proven.
	Leaf crypto.Hash32
	// Path is the list of sibling hashes from leaf to root.
	Path []MerkleNode
}

// MerkleNode is one element in a Merkle proof path.
type MerkleNode struct {
	Hash  crypto.Hash32
	Right bool // true if this sibling is on the right side
}

// GenerateMerkleProof generates an inclusion proof for the transaction at index idx.
func GenerateMerkleProof(txs []Transaction, idx int) *MerkleProof {
	if idx < 0 || idx >= len(txs) {
		return nil
	}

	hashes := make([]crypto.Hash32, len(txs))
	for i, tx := range txs {
		hashes[i] = tx.Hash()
	}

	proof := &MerkleProof{Leaf: hashes[idx]}
	pos := idx

	for len(hashes) > 1 {
		if len(hashes)%2 == 1 {
			hashes = append(hashes, hashes[len(hashes)-1])
		}
		var sibling MerkleNode
		if pos%2 == 0 {
			sibling = MerkleNode{Hash: hashes[pos+1], Right: true}
		} else {
			sibling = MerkleNode{Hash: hashes[pos-1], Right: false}
		}
		proof.Path = append(proof.Path, sibling)

		next := make([]crypto.Hash32, len(hashes)/2)
		for i := 0; i < len(hashes); i += 2 {
			next[i/2] = crypto.HashBytes(hashes[i][:], hashes[i+1][:])
		}
		hashes = next
		pos /= 2
	}

	return proof
}

// Verify checks that the Merkle proof is valid for the given root.
func (p *MerkleProof) Verify(root crypto.Hash32) bool {
	cur := p.Leaf
	for _, node := range p.Path {
		if node.Right {
			cur = crypto.HashBytes(cur[:], node.Hash[:])
		} else {
			cur = crypto.HashBytes(node.Hash[:], cur[:])
		}
	}
	return cur == root
}
