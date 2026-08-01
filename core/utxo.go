package core

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/aperod/aperod/crypto"
)

// DecoyUTXO is a stripped UTXO descriptor used as a ring decoy in Phase 2.
// Only the public-key and commitment fields are needed to build a ring member.
type DecoyUTXO struct {
	OneTimePub   crypto.Point32
	AmountCommit crypto.Commitment
}

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
	mu           sync.RWMutex
	utxos        map[UTXOKey]*UTXO
	keyImages    map[crypto.KeyImage]struct{} // spent key images
	byPubKey     map[crypto.Point32]*UTXO     // ACTIVE (unspent) UTXOs by OneTimePub for C-0 check
	stakedUTXOs  map[UTXOKey]*UTXO            // UTXOs burned for staking (C-1 fix) — stores data for rollback
	spentPubKeys map[crypto.Point32]*UTXO     // Phase 2: spent UTXOs removed from byPubKey; used as safe ring decoys
}

// NewUTXOSet creates an empty in-memory UTXO set.
func NewUTXOSet() *UTXOSet {
	return &UTXOSet{
		utxos:        make(map[UTXOKey]*UTXO),
		keyImages:    make(map[crypto.KeyImage]struct{}),
		byPubKey:     make(map[crypto.Point32]*UTXO),
		stakedUTXOs:  make(map[UTXOKey]*UTXO),
		spentPubKeys: make(map[crypto.Point32]*UTXO),
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
		// Mark inputs spent and move the real spent UTXO from byPubKey to
		// spentPubKeys.  Phase 2 transactions sample decoys from spentPubKeys
		// (historical spent UTXOs absent from byPubKey); C-0 skips absent members
		// exactly as it skips Phase 1 random keys, while still binding the
		// commitment check to the REAL (unspent) input that IS in byPubKey.
		//
		// Identification: in a validated transaction the C-0 invariant guarantees
		// exactly one ring member in byPubKey has AmountCommit == inp.AmountCommit
		// (the real spent UTXO).  We find it, move it to spentPubKeys, and break.
		for _, inp := range tx.Inputs {
			s.keyImages[inp.KeyImage] = struct{}{}
			for _, member := range inp.Ring {
				if utxo, ok := s.byPubKey[member]; ok {
					if utxo.AmountCommit == inp.AmountCommit {
						delete(s.byPubKey, member)
						s.spentPubKeys[member] = utxo
						break // exactly one match expected (Pedersen commitments are binding)
					}
				}
			}
		}
		// Add outputs to both primary index and the byPubKey ring-member index.
		// byPubKey must be populated here so that TxVerifier.VerifyTx (C-0 full
		// check) can look up ring members created in any historical block after a
		// node restart.  Without this, the startup UTXO replay scan via ApplyBlock
		// would leave byPubKey empty and reject every legitimate RingCT transaction.
		for i, out := range tx.Outputs {
			key := UTXOKey{TxHash: txHash, OutputIndex: uint32(i)}
			u := &UTXO{
				TxHash:       txHash,
				OutputIndex:  uint32(i),
				OneTimePub:   out.OneTimePub,
				TxPubKey:     out.TxPubKey,
				AmountCommit: out.AmountCommit,
				EncAmount:    out.EncAmount,
				BlockHeight:  block.Header.Height,
			}
			s.utxos[key] = u
			s.byPubKey[out.OneTimePub] = u
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
		// Remove outputs that this block created — both from the primary index
		// and from byPubKey.  These UTXOs never made it onto a canonical chain,
		// so they must not remain as usable ring decoys after rollback.
		for i, out := range tx.Outputs {
			delete(s.utxos, UTXOKey{TxHash: txHash, OutputIndex: uint32(i)})
			delete(s.byPubKey, out.OneTimePub)
		}
		// Restore inputs: un-mark key images and move the real spent UTXO
		// from spentPubKeys back to byPubKey (reverting ApplyBlock's move).
		for _, inp := range tx.Inputs {
			delete(s.keyImages, inp.KeyImage)
			for _, member := range inp.Ring {
				if utxo, ok := s.spentPubKeys[member]; ok {
					if utxo.AmountCommit == inp.AmountCommit {
						delete(s.spentPubKeys, member)
						s.byPubKey[member] = utxo
						break
					}
				}
			}
		}
	}
	return nil
}

// MarkStaked burns a UTXO for staking: removes it from the active set so it
// cannot be spent in a normal RingCT transaction or re-used as a ring decoy.
// The UTXO data is preserved in stakedUTXOs (not discarded) so that
// UnmarkStaked can restore it for transactional rollback.
// Returns an error if the UTXO is not in the active set or already staked.  (C-1 fix)
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
	s.stakedUTXOs[k] = u // store data so UnmarkStaked can restore it
	return nil
}

// MarkStakedKnown marks a UTXO as staked during startup replay of historical
// committed blocks without requiring it to be present in the active set (it was
// already removed by MarkStaked in a previous run).  Unlike MarkStaked, it never
// returns an error for an already-absent UTXO.  If the UTXO is in the active set
// (populated by the startup UTXO rebuild scan), it is removed there first exactly
// as MarkStaked would do.  The existing UTXO data is preferred over the provided
// u descriptor (which may have a zero OneTimePub) so that UnmarkStaked can restore
// the correct entry on rollback.
// Idempotent: a second call for the same key is a no-op.
func (s *UTXOSet) MarkStakedKnown(txHash crypto.Hash32, outIdx uint32, u *UTXO) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := UTXOKey{TxHash: txHash, OutputIndex: outIdx}
	if _, already := s.stakedUTXOs[k]; already {
		return // idempotent
	}
	// If the UTXO is currently in the active set (e.g. from the startup rebuild
	// scan), use its data for the staked record and remove it from the active
	// indices — exactly matching MarkStaked behaviour.
	if existing, ok := s.utxos[k]; ok {
		delete(s.byPubKey, existing.OneTimePub)
		delete(s.utxos, k)
		s.stakedUTXOs[k] = existing // correct data including OneTimePub
		return
	}
	// UTXO absent from active set (e.g. node upgraded mid-chain without a rebuild
	// or UTXO was already removed): store the provided descriptor so IsStaked()
	// returns true and prevents collateral reuse.
	s.stakedUTXOs[k] = u
}

// UnmarkStaked reverses MarkStaked for transactional rollback: moves the UTXO
// back into the active set and removes it from the staked set.  No-op if the
// UTXO is not in the staked set.
func (s *UTXOSet) UnmarkStaked(txHash crypto.Hash32, outIdx uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := UTXOKey{TxHash: txHash, OutputIndex: outIdx}
	u, ok := s.stakedUTXOs[k]
	if !ok {
		return // already gone (no-op)
	}
	delete(s.stakedUTXOs, k)
	s.utxos[k] = u
	s.byPubKey[u.OneTimePub] = u
}

// IsStaked returns true if the UTXO has been burned for staking.
func (s *UTXOSet) IsStaked(txHash crypto.Hash32, outIdx uint32) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.stakedUTXOs[UTXOKey{TxHash: txHash, OutputIndex: outIdx}]
	return ok
}

// SampleDecoys returns up to count Phase 2 ring decoys from the spentPubKeys
// pool — UTXOs that have already been spent and are therefore absent from
// byPubKey.  Because they are absent, C-0 skips them exactly as it skips Phase
// 1 random keys, so adding them to a ring does NOT trigger a commitment-mismatch
// error for the real (unspent) ring member.
//
// Security invariant: the real spending key belongs to an UNSPENT UTXO that IS
// in byPubKey; C-0 checks its commitment against inp.AmountCommit and rejects
// forgery.  Spent decoys are transparent to C-0 because they are absent.
//
// Any UTXO whose OneTimePub appears in exclude is omitted; callers use this to
// prevent the real input from appearing as its own decoy.
//
// Selection is randomised with a time-seeded PRNG (decoys are public knowledge;
// randomness serves privacy, not security).  If fewer than count candidates
// remain after exclusions, all available candidates are returned — txBuildRing
// fills the remaining ring slots with Phase 1 random keys.
func (s *UTXOSet) SampleDecoys(count int, exclude map[crypto.Point32]bool) []DecoyUTXO {
	s.mu.RLock()
	defer s.mu.RUnlock()

	candidates := make([]*UTXO, 0, len(s.spentPubKeys))
	for pub, u := range s.spentPubKeys {
		if !exclude[pub] {
			candidates = append(candidates, u)
		}
	}

	n := len(candidates)
	want := count
	if want > n {
		want = n
	}
	if want == 0 {
		return nil
	}

	// Fisher-Yates partial shuffle to pick `want` items in random order.
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // privacy, not security
	for i := 0; i < want; i++ {
		j := i + rng.Intn(n-i)
		candidates[i], candidates[j] = candidates[j], candidates[i]
	}

	out := make([]DecoyUTXO, want)
	for i := 0; i < want; i++ {
		out[i] = DecoyUTXO{
			OneTimePub:   candidates[i].OneTimePub,
			AmountCommit: candidates[i].AmountCommit,
		}
	}
	return out
}

// ApplyBlockForSpentDecoys rebuilds the spentPubKeys pool during a node restart.
// It must be called after the active UTXO set has been fully restored from the
// persistent store (IterUTXOs → Add).  For each spending input in the block it
// finds the ring member in byPubKey whose AmountCommit equals inp.AmountCommit
// and moves it to spentPubKeys — exactly the transition that ApplyBlock performs
// at runtime when a new block is committed.
//
// The method is idempotent: if a member was already moved (spent by an earlier
// block in the replay sequence) it will be absent from byPubKey and is silently
// skipped.  Call this for every canonical block from genesis to chain tip in
// ascending height order.
func (s *UTXOSet) ApplyBlockForSpentDecoys(block *Block) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tx := range block.Txs {
		for _, inp := range tx.Inputs {
			for _, member := range inp.Ring {
				if utxo, ok := s.byPubKey[member]; ok {
					if utxo.AmountCommit == inp.AmountCommit {
						delete(s.byPubKey, member)
						s.spentPubKeys[member] = utxo
						break
					}
				}
			}
		}
	}
}

// SpentDecoyCount returns the number of spent UTXOs in the decoy pool.
// Used for logging and diagnostics; not a security-critical value.
func (s *UTXOSet) SpentDecoyCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.spentPubKeys)
}

// AddSpentDecoyForTest directly inserts a UTXO into the spentPubKeys pool.
// For use in unit tests only; production code populates spentPubKeys through
// ApplyBlock when a block containing a spending transaction is committed.
func (s *UTXOSet) AddSpentDecoyForTest(u *UTXO) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spentPubKeys[u.OneTimePub] = u
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
