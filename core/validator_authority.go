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
//
// Version 1 is a single transition. Version 2 adds Previous, an authenticated
// chain of earlier transitions, so a later rotation can preserve historical
// signing authority instead of rewriting already accepted block signatures.
type ValidatorAuthorityFork struct {
	Version             uint32                  `json:"version"`
	Genesis             crypto.Hash32           `json:"genesis"`
	Height              uint64                  `json:"height"`
	Parent              crypto.Hash32           `json:"parent"`
	Identity            crypto.ValidatorPubKey  `json:"identity"`
	Replacement         crypto.ValidatorPubKey  `json:"replacement"`
	RegistrySHA256      crypto.Hash32           `json:"registry_sha256"`
	LegacyWitnessSHA256 crypto.Hash32           `json:"legacy_witness_sha256"`
	Possession          []byte                  `json:"possession"`
	Previous            *ValidatorAuthorityFork `json:"previous,omitempty"`
}

// Message returns the domain-separated validator-authority fork message.
func (f *ValidatorAuthorityFork) Message() crypto.Hash32 {
	copy := *f
	copy.Possession = nil
	raw, _ := json.Marshal(copy)
	domain := "aperod/validator-authority-fork/v1"
	if f.Version == 2 {
		domain = "aperod/validator-authority-fork/v2"
	}
	return crypto.HashBytes([]byte(domain), raw)
}

// Validate checks the fork's activation and authority configuration.
func (f *ValidatorAuthorityFork) Validate() error {
	return f.validate(0)
}

const maxValidatorAuthorityForkHistory = 16

func (f *ValidatorAuthorityFork) validate(depth int) error {
	if f == nil || depth >= maxValidatorAuthorityForkHistory ||
		(f.Version != 1 && f.Version != 2) ||
		(f.Version == 1 && f.Previous != nil) ||
		(f.Version == 2 && f.Previous == nil) ||
		f.Height < 2 ||
		f.Genesis == (crypto.Hash32{}) || f.Parent == (crypto.Hash32{}) ||
		f.RegistrySHA256 == (crypto.Hash32{}) ||
		len(f.Identity) != 32 || len(f.Replacement) != 32 ||
		f.Identity.Equals(f.Replacement) {
		return fmt.Errorf("validator authority fork: invalid rule")
	}
	if f.Previous != nil {
		if err := f.Previous.validate(depth + 1); err != nil {
			return fmt.Errorf("validator authority fork history: %w", err)
		}
		if f.Genesis != f.Previous.Genesis ||
			!f.Identity.Equals(f.Previous.Identity) ||
			f.Height <= f.Previous.Height ||
			f.Replacement.Equals(f.Previous.Replacement) ||
			f.LegacyWitnessSHA256 != f.Previous.LegacyWitnessSHA256 {
			return fmt.Errorf("validator authority fork: history is not a forward rotation")
		}
	}
	if len(f.Possession) != 64 || !f.Replacement.Verify(f.Message(), f.Possession) {
		return fmt.Errorf("validator authority fork: invalid replacement possession")
	}
	return nil
}

// Clone returns a deep copy of the fork configuration.
func (f *ValidatorAuthorityFork) Clone() *ValidatorAuthorityFork {
	if f == nil {
		return nil
	}
	copy := *f
	copy.Identity = append(crypto.ValidatorPubKey(nil), f.Identity...)
	copy.Replacement = append(crypto.ValidatorPubKey(nil), f.Replacement...)
	copy.Possession = append([]byte(nil), f.Possession...)
	copy.Previous = f.Previous.Clone()
	return &copy
}

// Authority resolves only the designated identity and only in the new era.
// The newest applicable transition wins; earlier transitions remain available
// for verifying their historical height ranges.
func (f *ValidatorAuthorityFork) Authority(identity crypto.ValidatorPubKey, height uint64) crypto.ValidatorPubKey {
	for rule := f; rule != nil; rule = rule.Previous {
		if height >= rule.Height && identity.Equals(rule.Identity) {
			return append(crypto.ValidatorPubKey(nil), rule.Replacement...)
		}
	}
	return append(crypto.ValidatorPubKey(nil), identity...)
}

// LocalIdentity returns the validator identity authorized at height for key.
func (f *ValidatorAuthorityFork) LocalIdentity(key crypto.ValidatorPubKey, height uint64) crypto.ValidatorPubKey {
	for rule := f; rule != nil; rule = rule.Previous {
		if height >= rule.Height {
			if key.Equals(rule.Replacement) {
				return append(crypto.ValidatorPubKey(nil), rule.Identity...)
			}
			return append(crypto.ValidatorPubKey(nil), key...)
		}
	}
	return append(crypto.ValidatorPubKey(nil), key...)
}

// TransitionAt returns the transition activated at height, if any.
func (f *ValidatorAuthorityFork) TransitionAt(height uint64) *ValidatorAuthorityFork {
	for rule := f; rule != nil; rule = rule.Previous {
		if rule.Height == height {
			return rule
		}
	}
	return nil
}

// Transitions returns the configured transitions in activation order.
func (f *ValidatorAuthorityFork) Transitions() []*ValidatorAuthorityFork {
	var reversed []*ValidatorAuthorityFork
	for rule := f; rule != nil; rule = rule.Previous {
		reversed = append(reversed, rule)
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

// CheckParent verifies every configured activation parent.
func (f *ValidatorAuthorityFork) CheckParent(header BlockHeader) error {
	for rule := f; rule != nil; rule = rule.Previous {
		if header.Height == rule.Height && header.PrevHash != rule.Parent {
			return fmt.Errorf("validator authority fork: activation parent mismatch")
		}
	}
	return nil
}
