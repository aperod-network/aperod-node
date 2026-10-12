package core

import (
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/crypto"
)

// ValidatorAuthorityFork is an operator-approved, future-only consensus fork.
// Identity remains the collateral/accounting identity: it is NOT a current
// signing key after Height. This avoids rewriting stake references, LPoD
// allocation IDs, generation, withdrawal nonces, or historical block hashes.
// Possession proves control of the replacement, not governance authorization.
// Every participating node must independently pin the same approved rule.
type ValidatorAuthorityFork struct {
	Version             uint32                 `json:"version"`
	Genesis             crypto.Hash32          `json:"genesis"`
	Height              uint64                 `json:"height"`
	Parent              crypto.Hash32          `json:"parent"`
	Identity            crypto.ValidatorPubKey `json:"identity"`
	Replacement         crypto.ValidatorPubKey `json:"replacement"`
	RegistrySHA256      crypto.Hash32          `json:"registry_sha256"`
	LegacyWitnessSHA256 crypto.Hash32          `json:"legacy_witness_sha256"`
	Possession          []byte                 `json:"possession"`
}

func (f *ValidatorAuthorityFork) Message() crypto.Hash32 {
	copy := *f
	copy.Possession = nil
	raw, _ := json.Marshal(copy)
	return crypto.HashBytes([]byte("aperod/validator-authority-fork/v1"), raw)
}

func (f *ValidatorAuthorityFork) Validate() error {
	if f == nil || f.Version != 1 || f.Height < 2 ||
		f.Genesis == (crypto.Hash32{}) || f.Parent == (crypto.Hash32{}) ||
		f.RegistrySHA256 == (crypto.Hash32{}) ||
		len(f.Identity) != 32 || len(f.Replacement) != 32 ||
		f.Identity.Equals(f.Replacement) {
		return fmt.Errorf("validator authority fork: invalid rule")
	}
	if len(f.Possession) != 64 || !f.Replacement.Verify(f.Message(), f.Possession) {
		return fmt.Errorf("validator authority fork: invalid replacement possession")
	}
	return nil
}

func (f *ValidatorAuthorityFork) Clone() *ValidatorAuthorityFork {
	if f == nil {
		return nil
	}
	copy := *f
	copy.Identity = append(crypto.ValidatorPubKey(nil), f.Identity...)
	copy.Replacement = append(crypto.ValidatorPubKey(nil), f.Replacement...)
	copy.Possession = append([]byte(nil), f.Possession...)
	return &copy
}

// Authority resolves only the designated identity and only in the new era.
// Historical signatures and unrelated validators retain their original keys.
func (f *ValidatorAuthorityFork) Authority(identity crypto.ValidatorPubKey, height uint64) crypto.ValidatorPubKey {
	if f != nil && height >= f.Height && identity.Equals(f.Identity) {
		return append(crypto.ValidatorPubKey(nil), f.Replacement...)
	}
	return append(crypto.ValidatorPubKey(nil), identity...)
}

func (f *ValidatorAuthorityFork) LocalIdentity(key crypto.ValidatorPubKey, height uint64) crypto.ValidatorPubKey {
	if f != nil && height >= f.Height && key.Equals(f.Replacement) {
		return append(crypto.ValidatorPubKey(nil), f.Identity...)
	}
	return append(crypto.ValidatorPubKey(nil), key...)
}

func (f *ValidatorAuthorityFork) CheckParent(header BlockHeader) error {
	if f != nil && header.Height == f.Height && header.PrevHash != f.Parent {
		return fmt.Errorf("validator authority fork: activation parent mismatch")
	}
	return nil
}
