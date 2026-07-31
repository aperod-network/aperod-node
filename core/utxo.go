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
	mu          sync.RWMutex
	utxos       map[UTXOKey]*UTXO
	keyImages   map[crypto.KeyImage]struct{} // spent key images
	byPubKey    map[crypto.Point32]*UTXO     // index by OneTimePub for ring-member lookup (C-0 fix)
	stakedUTXOs map[UTXOKey]struct{}         // UTXOs burned for staking (C-1 fix) — prevents double-use
}

// NewUTXOSet creates an empty in-memory UTXO set.
func NewUTXOSet() *UTXOSet {
	return &UTXOSet{
		utxos:       make(map[UTXOKey]*UTXO),
		keyImages:   make(map[crypto.KeyImage]struct{}),
		byPubKey:    make(map[crypto.Point32]*UTXO),
		stakedUTXOs: make(map[UTXOKey]struct{}),
	}
}

// Add inserts a new UTXO into the set.
func (s *UTXOSet) Add(u *UTXO) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := UTXOKey{TxHash: u.TxHash, OutputIndex: u.OutputIndex}
	s.utxos[key] = u
	s.byPubKey[u.OneTimePub] = u
}

// Get retrieves a UTXO by its key. Returns nil if not found.
func (s *UTXOSet) Get(txHash crypto.Hash32, outIdx uint32) *UTXO {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.utxos[UTXOKey{TxHash: txHash, OutputIndex: outIdx}]
}

// GetByPubKey looks up a UTXO by its stealth one-time public key.
// Used by TxVerifier to bind ring member AmountCommits to actual on-chain
// UTXO commitments (C-0 fix: prevents attacker-supplied fake commitments).
func (s *UTXOSet) GetByPubKey(pub crypto.Point32) *UTXO {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byPubKey[pub]
}

// Remove deletes a UTXO from the set (called when it is spent).
func (s *UTXOSet) Remove(txHash crypto.Hash32, outIdx uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := UTXOKey{TxHash: txHash, OutputIndex: outIdx}
	if u, ok := s.utxos[k]; ok {
		delete(s.byPubKey, u.OneTimePub)
	}
	delete(s.utxos, k)
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
//
// The operation is transactional: a two-pass approach ensures that the UTXO
// set is never partially mutated on rejection.
//
//   Pass 1 — pre-validation (read-only, whole-block lock held):
//     • Check every input key image for historical or within-block double-spend.
//     • If any check fails the function returns an error and the set is unchanged.
//
//   Pass 2 — apply (write, same lock held throughout):
//     • Mark key images spent and add outputs.  Cannot fail after pass 1.
//
// Holding the lock across both passes prevents a concurrent goroutine from
// sneaking in a conflicting key image between validation and application.
func (s *UTXOSet) ApplyBlock(block *Block) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Pass 1: validate all inputs without any state mutation.
	seen := make(map[crypto.KeyImage]int) // ki → first tx index (for error reporting)
	for txIdx, tx := range block.Txs {
		for _, inp := range tx.Inputs {
			if _, spent := s.keyImages[inp.KeyImage]; spent {
				return fmt.Errorf("double-spend detected: key image %x in block %d (historical)",
					inp.KeyImage[:8], block.Header.Height)
			}
			if firstIdx, dup := seen[inp.KeyImage]; dup {
				return fmt.Errorf("double-spend detected: key image %x in block %d (within-block: tx[%d] and tx[%d])",
					inp.KeyImage[:8], block.Header.Height, firstIdx, txIdx)
			}
			seen[inp.KeyImage] = txIdx
		}
	}

	// Pass 2: apply state changes — cannot fail after pass 1 succeeded.
	for _, tx := range block.Txs {
		txHash := tx.Hash()
		for _, inp := range tx.Inputs {
			s.keyImages[inp.KeyImage] = struct{}{}
		}
		for i, out := range tx.Outputs {
			key := UTXOKey{TxHash: txHash, OutputIndex: uint32(i)}
			s.utxos[key] = &UTXO{
				TxHash:       txHash,
				OutputIndex:  uint32(i),
				OneTimePub:   out.OneTimePub,
				TxPubKey:     out.TxPubKey,
				AmountCommit: out.AmountCommit,
				EncAmount:    out.EncAmount,
				BlockHeight:  block.Header.Height,
			}
		}
	}
	return nil
}

// RollbackBlock reverses ApplyBlock for chain reorganization or failed block
// acceptance.  Removes all outputs added by this block and un-marks all key
// images spent by its inputs, restoring the UTXO set to the state before the
// block was applied.
//
// Caution: this is an in-memory rollback only.  The persistent store (LevelDB)
// is not touched here; durable reorg recovery requires the store's revert journal.
func (s *UTXOSet) RollbackBlock(block *Block) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, tx := range block.Txs {
		txHash := tx.Hash()
		// Remove outputs that this block created.
		for i := range tx.Outputs {
			delete(s.utxos, UTXOKey{TxHash: txHash, OutputIndex: uint32(i)})
		}
		// Un-mark key images spent by this block's inputs so they can be
		// re-spent if the block is retried (or another valid block spends them).
		for _, inp := range tx.Inputs {
			delete(s.keyImages, inp.KeyImage)
		}
	}
	return nil
}

// MarkStaked burns a UTXO for staking: removes it from the active set so it
// cannot be spent in a normal RingCT transaction or re-used as a ring decoy.
// The UTXOKey is recorded in stakedUTXOs to prevent double-staking the same
// output.  Returns an error if the UTXO is not in the active set or has
// already been staked.  (C-1 fix)
func (s *UTXOSet) MarkStaked(txHash crypto.Hash32, outIdx uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := UTXOKey{TxHash: txHash, OutputIndex: outIdx}
	if _, already := s.stakedUTXOs[k]; already {
		return fmt.Errorf("UTXO %x:%d already staked", txHash[:8], outIdx)
	}
	u, ok := s.utxos[k]
	if !ok {
		return fmt.Errorf("UTXO %x:%d not found in active set", txHash[:8], outIdx)
	}
	// Remove from both active indices — cannot be spent or used as ring decoy.
	delete(s.byPubKey, u.OneTimePub)
	delete(s.utxos, k)
	s.stakedUTXOs[k] = struct{}{}
	return nil
}

// IsStaked returns true if the UTXO has been burned for staking.
func (s *UTXOSet) IsStaked(txHash crypto.Hash32, outIdx uint32) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.stakedUTXOs[UTXOKey{TxHash: txHash, OutputIndex: outIdx}]
	return ok
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
