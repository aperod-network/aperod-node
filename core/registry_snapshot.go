package core

// RegistrySnapshot is a serialisable image of the ValidatorRegistry state.
// The utxos pointer cannot be serialised and must be re-wired via
// SetUTXOSet after RestoreFromSnapshot.
type RegistrySnapshot struct {
	Validators     map[string]*ValidatorEntry `json:"validators"`
	DynamicMinNAPR uint64                     `json:"dynamic_min_napr"`
}

// TakeSnapshot captures the current state of the registry.
// Returns a deep copy; the original registry may be updated concurrently.
func (r *ValidatorRegistry) TakeSnapshot() RegistrySnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return RegistrySnapshot{
		Validators:     deepCopyValidators(r.validators),
		DynamicMinNAPR: r.dynamicMinNAPR,
	}
}

// RestoreFromSnapshot replaces the registry content with the provided snapshot.
// Call SetUTXOSet separately after this to re-wire the UTXOSet pointer.
//
// A nil Validators map (e.g. a snapshot written before registry snapshotting
// was added) is treated as an empty registry rather than a nil map, preventing
// a panic in any subsequent InitFromGenesis or ProcessStakeTx call.
func (r *ValidatorRegistry) RestoreFromSnapshot(snap RegistrySnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if snap.Validators != nil {
		r.validators = snap.Validators
	} else {
		r.validators = make(map[string]*ValidatorEntry)
	}
	r.dynamicMinNAPR = snap.DynamicMinNAPR
}
