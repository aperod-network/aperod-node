package core

import "github.com/aperod/aperod/crypto"

// RollbackJournalEntry is the serialisable form of one rollbackEntry.
// Exported fields allow JSON round-trips in UTXOSnapshot.
// The rollback journal is bounded to maxRollbackDepth distinct heights, so
// the total serialised size is small relative to the full UTXO set.
type RollbackJournalEntry struct {
	Height     uint64         `json:"height"`
	RingMember crypto.Point32 `json:"ring_member"`
	UTXO       *UTXO          `json:"utxo"`
}

// UTXOSnapshot is a point-in-time serialisable image of the entire in-memory
// UTXOSet.  It is used to persist the set after the startup scan so that
// subsequent restarts can skip the expensive block replay.
type UTXOSnapshot struct {
	ActiveUTXOs    []*UTXO                `json:"active_utxos"`
	StakedUTXOs    []*UTXO                `json:"staked_utxos"`
	SpentDecoys    []*UTXO                `json:"spent_decoys"`
	KeyImages      []crypto.KeyImage      `json:"key_images"`
	// RollbackJournal carries the bounded per-block rollback state so that
	// after a snapshot restore the node can roll back a newly applied block
	// even when the spentPubKeys decoy pool is already at capacity.
	// Omitted from old snapshots (treated as empty on restore — safe because
	// post-restore rollback is only ever needed for blocks applied after the
	// restore, which always populate the journal via ApplyBlock).
	RollbackJournal []RollbackJournalEntry `json:"rollback_journal,omitempty"`
}

// TakeSnapshot captures the current in-memory state of the UTXOSet.
// The returned snapshot is a deep copy; the caller may mutate the original
// set concurrently without affecting the snapshot.
func (s *UTXOSet) TakeSnapshot() UTXOSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	active := make([]*UTXO, 0, len(s.utxos))
	for _, u := range s.utxos {
		cp := *u
		active = append(active, &cp)
	}
	staked := make([]*UTXO, 0, len(s.stakedUTXOs))
	for _, u := range s.stakedUTXOs {
		cp := *u
		staked = append(staked, &cp)
	}
	decoys := make([]*UTXO, 0, len(s.spentPubKeys))
	for _, u := range s.spentPubKeys {
		cp := *u
		decoys = append(decoys, &cp)
	}
	// toSlice returns a sorted copy — no additional sort needed on restore.
	kis := s.keyImages.toSlice()

	// Serialise the rollback journal as a flat slice (JSON map keys must be
	// strings; using a slice with an explicit Height field avoids that limit).
	var journal []RollbackJournalEntry
	for height, entries := range s.rollbackJournal {
		for _, e := range entries {
			cp := *e.utxo // deep copy so snapshot is independent of live maps
			journal = append(journal, RollbackJournalEntry{
				Height:     height,
				RingMember: e.ringMember,
				UTXO:       &cp,
			})
		}
	}

	return UTXOSnapshot{
		ActiveUTXOs:     active,
		StakedUTXOs:     staked,
		SpentDecoys:     decoys,
		KeyImages:       kis,
		RollbackJournal: journal,
	}
}

// ReconcileWithStore cross-checks every active and staked UTXO against the
// authoritative persistent store (the u/ LevelDB index, written from raw
// block data at acceptance time) and overwrites in-memory fields that
// diverge.  Snapshots can carry corrupted values indefinitely: a bad write
// into the in-memory set (e.g. a manual restore flow that recomputed
// commitments from incorrect database amounts) is persisted by the next
// snapshot save and reloaded on every subsequent restart.  The disk store is
// the source of truth — consensus verified those outputs when their blocks
// were accepted.
//
// lookup returns the on-disk UTXO for (txHash, outIdx) or ok=false when the
// store has no entry (absent entries are skipped, never treated as
// divergence).  OneTimePub is deliberately not patched: it is the byPubKey
// map key, and a divergence there indicates deeper corruption that must not
// be silently re-keyed; it is only counted and reported.
//
// Returns (checked, fixed, pubKeyMismatches).
func (s *UTXOSet) ReconcileWithStore(
	lookup func(txHash crypto.Hash32, outIdx uint32) (*UTXO, bool),
	onFix func(u *UTXO, disk *UTXO),
) (checked, fixed, pubKeyMismatches int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	reconcile := func(m map[UTXOKey]*UTXO) {
		for k, u := range m {
			disk, ok := lookup(k.TxHash, k.OutputIndex)
			if !ok || disk == nil {
				continue
			}
			checked++
			if u.OneTimePub != disk.OneTimePub {
				pubKeyMismatches++
				continue
			}
			if u.AmountCommit == disk.AmountCommit &&
				u.TxPubKey == disk.TxPubKey &&
				u.EncAmount == disk.EncAmount &&
				u.BlockHeight == disk.BlockHeight {
				continue
			}
			if onFix != nil {
				onFix(u, disk)
			}
			u.AmountCommit = disk.AmountCommit
			u.TxPubKey = disk.TxPubKey
			u.EncAmount = disk.EncAmount
			u.BlockHeight = disk.BlockHeight
			fixed++
		}
	}
	reconcile(s.utxos)
	reconcile(s.stakedUTXOs)
	return checked, fixed, pubKeyMismatches
}

// RestoreFromSnapshot replaces the UTXOSet content with the provided snapshot.
// It rebuilds both primary (utxos) and secondary (byPubKey, spentPubKeys,
// stakedUTXOs, keyImages, rollbackJournal) indexes from the snapshot data.
func (s *UTXOSet) RestoreFromSnapshot(snap UTXOSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.utxos = make(map[UTXOKey]*UTXO, len(snap.ActiveUTXOs))
	s.byPubKey = make(map[crypto.Point32]*UTXO, len(snap.ActiveUTXOs))
	for _, u := range snap.ActiveUTXOs {
		k := UTXOKey{TxHash: u.TxHash, OutputIndex: u.OutputIndex}
		s.utxos[k] = u
		s.byPubKey[u.OneTimePub] = u
	}

	s.stakedUTXOs = make(map[UTXOKey]*UTXO, len(snap.StakedUTXOs))
	for _, u := range snap.StakedUTXOs {
		k := UTXOKey{TxHash: u.TxHash, OutputIndex: u.OutputIndex}
		s.stakedUTXOs[k] = u
	}

	s.spentPubKeys = make(map[crypto.Point32]*UTXO, len(snap.SpentDecoys))
	for _, u := range snap.SpentDecoys {
		s.spentPubKeys[u.OneTimePub] = u
	}

	// restoreFromSlice is a no-op in the LevelDB-backed design: historical
	// key images are read from kiDB on demand rather than pre-loaded into RAM.
	// The call is kept for snapshot-format compatibility; old snapshots' KeyImages
	// field is silently ignored, which also eliminates phantom-KI bugs (mempool
	// entries that used to permanently block valid UTXOs after OOM kills).
	s.keyImages.restoreFromSlice(snap.KeyImages)

	// Rebuild the rollback journal from the snapshot.  Old snapshots that
	// predate this field will have an empty slice here, which is safe: any
	// block applied after the restore will populate the journal via ApplyBlock.
	s.rollbackJournal = make(map[uint64][]rollbackEntry, len(snap.RollbackJournal))
	for _, e := range snap.RollbackJournal {
		s.rollbackJournal[e.Height] = append(s.rollbackJournal[e.Height], rollbackEntry{
			ringMember: e.RingMember,
			utxo:       e.UTXO,
		})
	}
}
