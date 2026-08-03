package core

import "github.com/aperod/aperod/crypto"

// UTXOSnapshot is a point-in-time serialisable image of the entire in-memory
// UTXOSet.  It is used to persist the set after the startup scan so that
// subsequent restarts can skip the expensive block replay.
type UTXOSnapshot struct {
	ActiveUTXOs []*UTXO           // utxos map — byPubKey is rebuilt on restore
	StakedUTXOs []*UTXO           // stakedUTXOs map
	SpentDecoys []*UTXO           // spentPubKeys (Phase-2 decoy pool)
	KeyImages   []crypto.KeyImage // keyImages set
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
	return UTXOSnapshot{
		ActiveUTXOs: active,
		StakedUTXOs: staked,
		SpentDecoys: decoys,
		KeyImages:   kis,
	}
}

// RestoreFromSnapshot replaces the UTXOSet content with the provided snapshot.
// It rebuilds both primary (utxos) and secondary (byPubKey, spentPubKeys,
// stakedUTXOs, keyImages) indexes from the snapshot data.
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
}
