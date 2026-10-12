package api

import (
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// SetAuditValidatorAuthorityFork accepts only the config-pinned rule loaded by
// node startup. It is not exposed through HTTP and never accepts a request key.
func (s *Server) SetAuditValidatorAuthorityFork(f *core.ValidatorAuthorityFork) error {
	if f == nil {
		s.validatorAuthorityFork = nil
		return nil
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if s.blockStore == nil {
		return fmt.Errorf("audit authority requires canonical storage")
	}
	genesis, found, err := s.blockStore.GetCanonicalHash(0)
	if err != nil || !found || genesis != f.Genesis {
		return fmt.Errorf("audit authority genesis binding mismatch")
	}
	_, height, err := s.blockStore.GetTip()
	if err != nil {
		return err
	}
	if height >= f.Height-1 {
		parent, found, err := s.blockStore.GetCanonicalHash(f.Height-1)
		if err != nil || !found || parent != f.Parent {
			return fmt.Errorf("audit authority activation-parent binding mismatch")
		}
	}
	s.validatorAuthorityFork = f.Clone()
	return nil
}

// Resolve collateral identity separately from the loaded signing key. A retired
// key must fail even when its public bytes still identify the local collateral.
func (s *Server) localValidatorIdentity(next bool) (crypto.ValidatorPubKey, error) {
	if s.myKey == nil {
		return nil, fmt.Errorf("local validator key unavailable")
	}
	signing := s.myKey.Public()
	if s.validatorAuthorityFork == nil {
		return signing, nil
	}
	var height uint64
	if s.blockStore != nil {
		_, h, err := s.blockStore.GetTip()
		if err != nil { return nil, err }
		height = h
	} else if s.chain != nil && s.chain.Tip() != nil {
		height = s.chain.Tip().Header.Height
	} else {
		return nil, fmt.Errorf("canonical validator authority height unavailable")
	}
	if next {
		if height == ^uint64(0) { return nil, fmt.Errorf("validator authority height overflow") }
		height++
	}
	identity := s.validatorAuthorityFork.LocalIdentity(signing, height)
	if !s.validatorAuthorityFork.Authority(identity, height).Equals(signing) {
		return nil, fmt.Errorf("loaded key is not the authorized collateral signer at this height")
	}
	return identity, nil
}

func (s *Server) verifyAuditHeader(header *core.BlockHeader) bool {
	copy := *header
	if s.validatorAuthorityFork == nil {
		return copy.VerifySignature()
	}
	if err := s.validatorAuthorityFork.CheckParent(copy); err != nil {
		return false
	}
	return copy.VerifySignature(s.validatorAuthorityFork.Authority(copy.ValidatorPub, copy.Height))
}