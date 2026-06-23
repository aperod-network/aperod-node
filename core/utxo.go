package core

import (
	"fmt"
	"sync"

	"github.com/aperod/aperod/crypto"
)

// UTXO represents an unspent transaction output.
type UTXO struct {
	// TxHash is the hash of the transaction that created this UTXO.
	TxHash crypto.Hash32
	// OutputIndex is the position of this output within the transaction.
	OutputIndex uint32
	// OneTimePub is the stealth one-time public key for this UTXO.
	OneTimePub crypto.Point32
	// TxPubKey is the ephemeral public key for ECDH (stealth scanning).
	TxPubKey crypto.Point32
	// AmountCommit is the Pedersen commitment hiding the amount.
	AmountCommit crypto.Commitment
	// EncAmount is the 8-byte encrypted amount (for owner's scanning).
	EncAmount [8]byte
	// BlockHeight is where this UTXO was created.
	BlockHeight uint64
}

// UTXOKey uniquely identifies a UTXO in the set.
type UTXOKey struct {
	TxHash      crypto.Hash32
	OutputIndex uint32
}

// UTXOSet is an in-memory UTXO set backed by the persistent store.
// In production, reads/writes go through store.UTXOStore (LevelDB).
type UTXOSet struct {
	mu        sync.RWMutex
	utxos     map[UTXOKey]*UTXO
	keyImages map[crypto.KeyImage]struct{} // spent key images
}

// NewUTXOSet creates an empty in-memory UTXO set.
func NewUTXOSet() *UTXOSet {
	return &UTXOSet{
		utxos:     make(map[UTXOKey]*UTXO),
		keyImages: make(map[crypto.KeyImage]struct{}),
	}
}

// Add inserts a new UTXO into the set.
func (s *UTXOSet) Add(u *UTXO) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := UTXOKey{TxHash: u.TxHash, OutputIndex: u.OutputIndex}
	s.utxos[key] = u
}

// Get retrieves a UTXO by its key. Returns nil if not found.
func (s *UTXOSet) Get(txHash crypto.Hash32, outIdx uint32) *UTXO {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.utxos[UTXOKey{TxHash: txHash, OutputIndex: outIdx}]
}

// Remove deletes a UTXO from the set (called when it is spent).
func (s *UTXOSet) Remove(txHash crypto.Hash32, outIdx uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.utxos, UTXOKey{TxHash: txHash, OutputIndex: outIdx})
}

// MarkSpent records a Key Image as spent.
func (s *UTXOSet) MarkSpent(ki crypto.KeyImage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyImages[ki] = struct{}{}
}

// IsSpent returns true if the Key Image has already been used.
func (s *UTXOSet) IsSpent(ki crypto.KeyImage) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, spent := s.keyImages[ki]
	return spent
}

// ApplyBlock updates the UTXO set by processing all transactions in a block.
// Inputs are removed (key images marked spent), outputs are added.
func (s *UTXOSet) ApplyBlock(block *Block) error {
	for _, tx := range block.Txs {
		txHash := tx.Hash()
		// Mark all input key images as spent
		for _, inp := range tx.Inputs {
			if s.IsSpent(inp.KeyImage) {
				return fmt.Errorf("double-spend detected: key image %x in block %d",
					inp.KeyImage, block.Header.Height)
			}
			s.MarkSpent(inp.KeyImage)
		}
		// Add all new outputs
		for i, out := range tx.Outputs {
			s.Add(&UTXO{
				TxHash:       txHash,
				OutputIndex:  uint32(i),
				OneTimePub:   out.OneTimePub,
				TxPubKey:     out.TxPubKey,
				AmountCommit: out.AmountCommit,
				EncAmount:    out.EncAmount,
				BlockHeight:  block.Header.Height,
			})
		}
	}
	return nil
}

// RollbackBlock reverses ApplyBlock for chain reorganization.
// Note: rolling back spent key images requires a spend journal (not implemented here;
// the persistent store tracks this).
func (s *UTXOSet) RollbackBlock(block *Block) error {
	for _, tx := range block.Txs {
		txHash := tx.Hash()
		// Remove outputs added by this block
		for i := range tx.Outputs {
			s.Remove(txHash, uint32(i))
		}
		// NOTE: restoring spent key images requires the original UTXOs.
		// In production this is handled by the store's revert journal.
	}
	return nil
}

// Count returns the number of UTXOs in the set (for diagnostics).
func (s *UTXOSet) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.utxos)
}

// All returns a snapshot of all UTXOs (for testing / export).
func (s *UTXOSet) All() []*UTXO {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*UTXO, 0, len(s.utxos))
	for _, u := range s.utxos {
		out = append(out, u)
	}
	return out
}
