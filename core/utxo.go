package core

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/aperod/aperod/crypto"
)

// compactKeyImageSet stores spent key images in a sorted []crypto.KeyImage
// slice rather than a Go map.  A Go map[KeyImage]struct{} consumes ~150 bytes
// per entry (bucket overhead, hash, alignment padding) for a 32-byte key;
// the sorted slice needs exactly 32 bytes per entry.  On a chain with 1 M
// spent key images this cuts key-image memory from ~150 MB to ~32 MB — a
// ~5× reduction that is the dominant contributor to the post-snapshot RSS gap.
//
// Lookups use binary search (O(log n)).  Insertions maintain sorted order via
// an in-place insert that copies the tail of the slice; on a PoA chain that
// adds 1–10 key images per block this costs ~3 ms at 1 M entries — well within
// a 1-second block time.
//
// The zero value (nil slice) is valid and represents an empty set.
type compactKeyImageSet struct {
	sorted []crypto.KeyImage
}

// kiLess returns true when a < b in lexicographic byte order.
func kiLess(a, b crypto.KeyImage) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// kiSearch returns the index of the first element ≥ ki (sort.Search semantics).
func (c *compactKeyImageSet) kiSearch(ki crypto.KeyImage) int {
	return sort.Search(len(c.sorted), func(i int) bool {
		return !kiLess(c.sorted[i], ki)
	})
}

// contains reports whether ki is present (O(log n) binary search).
func (c *compactKeyImageSet) contains(ki crypto.KeyImage) bool {
	idx := c.kiSearch(ki)
	return idx < len(c.sorted) && c.sorted[idx] == ki
}

// insert adds ki maintaining sorted order.  No-op if already present.
// O(n) due to the slice copy required to shift the tail.
func (c *compactKeyImageSet) insert(ki crypto.KeyImage) {
	idx := c.kiSearch(ki)
	if idx < len(c.sorted) && c.sorted[idx] == ki {
		return // already present
	}
	c.sorted = append(c.sorted, crypto.KeyImage{})
	copy(c.sorted[idx+1:], c.sorted[idx:])
	c.sorted[idx] = ki
}

// remove deletes ki if present.  No-op if absent.
// O(n) due to the slice copy required to compact the tail.
func (c *compactKeyImageSet) remove(ki crypto.KeyImage) {
	idx := c.kiSearch(ki)
	if idx >= len(c.sorted) || c.sorted[idx] != ki {
		return // not found
	}
	c.sorted = append(c.sorted[:idx], c.sorted[idx+1:]...)
}

// length returns the number of stored key images.
func (c *compactKeyImageSet) length() int { return len(c.sorted) }

// toSlice returns a sorted copy of all key images (for snapshot serialisation).
func (c *compactKeyImageSet) toSlice() []crypto.KeyImage {
	if len(c.sorted) == 0 {
		return nil
	}
	out := make([]crypto.KeyImage, len(c.sorted))
	copy(out, c.sorted)
	return out
}

// restoreFromSlice replaces the set's contents with a sorted copy of kis.
// Called once during snapshot restore; pays the one-time O(n log n) sort cost
// instead of O(n²) sequential insertions.
func (c *compactKeyImageSet) restoreFromSlice(kis []crypto.KeyImage) {
	c.sorted = make([]crypto.KeyImage, len(kis))
	copy(c.sorted, kis)
	sort.Slice(c.sorted, func(i, j int) bool {
		return kiLess(c.sorted[i], c.sorted[j])
	})
}

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

// maxSpentDecoys is the maximum number of entries kept in the spentPubKeys
// ring-decoy pool.  Once this limit is reached, new entries are silently
// dropped (the pool already has enough decoys; dropping older ones has no
// security impact because spent outputs cannot be double-spent regardless).
// This cap prevents unbounded memory growth on long-running chains.
//
// Lowered from 50 000 → 10 000 to reduce post-snapshot RSS: each entry holds
// a full *UTXO struct (~148 B) plus ~150 B of Go map bucket overhead, so
// 10 000 entries ≈ 3 MB vs 15 MB for 50 000.  10 000 decoys already provides
// more than enough ring privacy on a PoA chain.
//
// Exposed as a var (not const) so that regression tests can temporarily lower
// the cap without needing to pre-fill 10 000 entries.
var maxSpentDecoys = 10_000

// maxRollbackDepth is the number of recent block heights kept in the rollback
// journal.  Any chain reorganisation deeper than this cannot be reversed in
// memory.  On a PoA chain such deep reorgs are operationally impossible.
const maxRollbackDepth = 256

// rollbackEntry records one spent UTXO for the per-block rollback journal.
// It is kept independent of the capped spentPubKeys decoy pool so that
// RollbackBlock can always restore a UTXO even when the decoy pool was
// already at capacity when ApplyBlock ran.
type rollbackEntry struct {
	ringMember crypto.Point32 // key that was deleted from byPubKey
	utxo       *UTXO          // full data needed to restore both indexes
}

// UTXOSet is an in-memory UTXO set backed by the persistent store.
// In production, reads/writes go through store.UTXOStore (LevelDB).
type UTXOSet struct {
	mu              sync.RWMutex
	utxos           map[UTXOKey]*UTXO
	keyImages       compactKeyImageSet           // spent key images — compact sorted slice (32 B/entry vs ~150 B map)
	byPubKey        map[crypto.Point32]*UTXO     // ACTIVE (unspent) UTXOs by OneTimePub for C-0 check
	stakedUTXOs     map[UTXOKey]*UTXO            // UTXOs burned for staking (C-1 fix) — stores data for rollback
	spentPubKeys    map[crypto.Point32]*UTXO     // Phase 2: spent UTXOs removed from byPubKey; used as safe ring decoys
	rollbackJournal map[uint64][]rollbackEntry   // height → UTXOs spent at that height (for RollbackBlock)
}

// NewUTXOSet creates an empty in-memory UTXO set.
func NewUTXOSet() *UTXOSet {
	return &UTXOSet{
		utxos:           make(map[UTXOKey]*UTXO),
		byPubKey:        make(map[crypto.Point32]*UTXO),
		stakedUTXOs:     make(map[UTXOKey]*UTXO),
		spentPubKeys:    make(map[crypto.Point32]*UTXO),
		rollbackJournal: make(map[uint64][]rollbackEntry),
		// keyImages zero value (nil slice) is a valid empty compactKeyImageSet.
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
// The key image is normalised to its prime-order subgroup representative before
// storage so that torsion variants (ki + T) map to the same entry.
func (s *UTXOSet) MarkSpent(ki crypto.KeyImage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	canonical, err := crypto.CanonicalKeyImage(ki)
	if err != nil {
		// Malformed / non-prime-order point: store raw as a best-effort guard.
		// ApplyBlock uses the same fallback for validated inputs; keeping the
		// two paths consistent prevents lookup misses for test-injected images.
		s.keyImages.insert(ki)
		return
	}
	s.keyImages.insert(canonical)
}

// IsSpent returns true if the Key Image has already been used.
// Normalises to the canonical representative before lookup so that any
// torsion variant of a spent key image is correctly detected as spent.
func (s *UTXOSet) IsSpent(ki crypto.KeyImage) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	canonical, err := crypto.CanonicalKeyImage(ki)
	if err != nil {
		// Mirror the MarkSpent fallback: look up the raw key image so that
		// test-injected (non-canonical) images are still detected as spent.
		return s.keyImages.contains(ki)
	}
	return s.keyImages.contains(canonical)
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
			canonical, cerr := crypto.CanonicalKeyImage(inp.KeyImage)
			if cerr != nil {
				return fmt.Errorf("block %d tx[%d]: invalid key image: %w",
					block.Header.Height, txIdx, cerr)
			}
			if s.keyImages.contains(canonical) {
				return fmt.Errorf("double-spend detected: key image %x in block %d (historical)",
					inp.KeyImage[:8], block.Header.Height)
			}
			if firstIdx, dup := seen[canonical]; dup {
				return fmt.Errorf("double-spend detected: key image %x in block %d (within-block: tx[%d] and tx[%d])",
					inp.KeyImage[:8], block.Header.Height, firstIdx, txIdx)
			}
			seen[canonical] = txIdx
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
		// (the real spent UTXO).  We find it, remove it from the primary utxos
		// index (critical for memory: prevents unbounded growth of s.utxos),
		// move it to spentPubKeys (capped ring-decoy pool), and break.
		for _, inp := range tx.Inputs {
			if canonical, cerr := crypto.CanonicalKeyImage(inp.KeyImage); cerr == nil {
				s.keyImages.insert(canonical)
			} else {
				s.keyImages.insert(inp.KeyImage) // fallback: store raw (already validated)
			}
			for _, member := range inp.Ring {
				if utxo, ok := s.byPubKey[member]; ok {
					if utxo.AmountCommit == inp.AmountCommit {
						delete(s.byPubKey, member)
						// Remove from the primary utxos index so that spent UTXOs do not
						// accumulate in memory indefinitely.  This is the critical fix for
						// OOM kills during normal operation: without this delete, s.utxos
						// grows proportionally to the total number of outputs ever created
						// rather than only the currently-unspent set.
						delete(s.utxos, UTXOKey{TxHash: utxo.TxHash, OutputIndex: utxo.OutputIndex})
						// Always record in the rollback journal, independent of the decoy
						// pool cap.  RollbackBlock reads from here, not from spentPubKeys,
						// so that a UTXO spent when the decoy pool is full can still be
						// restored on a chain reorganisation.
						s.rollbackJournal[block.Header.Height] = append(
							s.rollbackJournal[block.Header.Height],
							rollbackEntry{ringMember: member, utxo: utxo},
						)
						// Prune the journal once it exceeds maxRollbackDepth distinct
						// heights, evicting the oldest entry to keep memory bounded.
						if len(s.rollbackJournal) > maxRollbackDepth {
							var oldest uint64 = ^uint64(0)
							for h := range s.rollbackJournal {
								if h < oldest {
									oldest = h
								}
							}
							delete(s.rollbackJournal, oldest)
						}
						// Conditionally add to the ring-decoy pool (capped).
						if len(s.spentPubKeys) < maxSpentDecoys {
							s.spentPubKeys[member] = utxo
						}
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
// Spent UTXO data is sourced from the rollback journal (populated by ApplyBlock
// independently of the capped spentPubKeys decoy pool) so that restoration
// works correctly even when the decoy pool was full at apply time.  As a safety
// net, any input whose journal entry is missing (e.g. journal pruned for a
// deep reorg beyond maxRollbackDepth) is also looked up in spentPubKeys.
//
// Caution: this is an in-memory rollback only.  The persistent store (LevelDB)
// is not touched here; durable reorg recovery requires the store's revert journal.
func (s *UTXOSet) RollbackBlock(block *Block) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	height := block.Header.Height

	// ── Step 1: restore spent inputs from the rollback journal ───────────────
	// Build a set of ring members restored from the journal so we can skip
	// the spentPubKeys fallback for those that were already handled.
	journalRestored := make(map[crypto.Point32]struct{})
	for _, entry := range s.rollbackJournal[height] {
		s.utxos[UTXOKey{TxHash: entry.utxo.TxHash, OutputIndex: entry.utxo.OutputIndex}] = entry.utxo
		s.byPubKey[entry.ringMember] = entry.utxo
		// Remove from decoy pool — the UTXO is unspent again after rollback.
		delete(s.spentPubKeys, entry.ringMember)
		journalRestored[entry.ringMember] = struct{}{}
	}
	delete(s.rollbackJournal, height)

	// ── Step 2: per-transaction cleanup ──────────────────────────────────────
	for _, tx := range block.Txs {
		txHash := tx.Hash()
		// Remove outputs that this block created — both from the primary index
		// and from byPubKey.  These UTXOs never made it onto a canonical chain,
		// so they must not remain as usable ring decoys after rollback.
		for i, out := range tx.Outputs {
			delete(s.utxos, UTXOKey{TxHash: txHash, OutputIndex: uint32(i)})
			delete(s.byPubKey, out.OneTimePub)
		}
		for _, inp := range tx.Inputs {
			// Un-mark key images using the canonical form that ApplyBlock stored.
			if canonical, cerr := crypto.CanonicalKeyImage(inp.KeyImage); cerr == nil {
				s.keyImages.remove(canonical)
			} else {
				s.keyImages.remove(inp.KeyImage) // fallback: remove raw
			}
			// Safety-net fallback: if the journal was pruned (reorg deeper than
			// maxRollbackDepth) try spentPubKeys as a last resort.
			for _, member := range inp.Ring {
				if _, alreadyRestored := journalRestored[member]; alreadyRestored {
					break
				}
				if utxo, ok := s.spentPubKeys[member]; ok {
					if utxo.AmountCommit == inp.AmountCommit {
						delete(s.spentPubKeys, member)
						s.byPubKey[member] = utxo
						s.utxos[UTXOKey{TxHash: utxo.TxHash, OutputIndex: utxo.OutputIndex}] = utxo
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
						// Mirror the primary-index delete from ApplyBlock so that the
						// startup spent-decoy rebuild does not leave s.utxos inflated
						// with spent outputs when called during a full block scan.
						delete(s.utxos, UTXOKey{TxHash: utxo.TxHash, OutputIndex: utxo.OutputIndex})
						if len(s.spentPubKeys) < maxSpentDecoys {
							s.spentPubKeys[member] = utxo
						}
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
