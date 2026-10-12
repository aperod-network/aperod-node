package core

import (
	"fmt"

	"github.com/aperod/aperod/crypto"
)

// ConfigureSignatureAuthority binds a rule once; it never changes collateral
// identities or accounting. Configure before replay and before admitting txs.
func (r *ValidatorRegistry) ConfigureSignatureAuthority(f *ValidatorAuthorityFork, nextHeight uint64) error {
	if f == nil {
		return nil
	}
	if err := f.Validate(); err != nil {
		return err
	}
	candidate := f.Clone()
	if !r.signatureFork.CompareAndSwap(nil, candidate) {
		if r.signatureFork.Load().Message() != f.Message() {
			return fmt.Errorf("registry: conflicting signature authority fork")
		}
	}
	r.signatureAdmissionHeight.Store(nextHeight)
	return nil
}

func (r *ValidatorRegistry) SetSignatureAdmissionHeight(nextHeight uint64) {
	r.signatureAdmissionHeight.Store(nextHeight)
}

func (r *ValidatorRegistry) SignatureAuthority(identity crypto.ValidatorPubKey, height uint64) crypto.ValidatorPubKey {
	return r.signatureFork.Load().Authority(identity, height)
}

func (r *ValidatorRegistry) verifyValidatorSignature(identity crypto.ValidatorPubKey, height uint64, msg crypto.Hash32, signature []byte) bool {
	return r.SignatureAuthority(identity, height).Verify(msg, signature)
}
