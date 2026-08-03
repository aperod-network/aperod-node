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
	kis := make([]crypto.KeyImage, 0, len(s.keyImages))
	for ki := range s.keyImages {
		kis = append(kis, ki)
	}

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

	s.keyImages = make(map[crypto.KeyImage]struct{}, len(snap.KeyImages))
	for _, ki := range snap.KeyImages {
		s.keyImages[ki] = struct{}{}
	}

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
