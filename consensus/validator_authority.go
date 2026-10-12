package consensus

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func (e *Engine) authority(identity crypto.ValidatorPubKey, height uint64) crypto.ValidatorPubKey {
	return e.cfg.ValidatorAuthorityFork.Authority(identity, height)
}

func (e *Engine) localIdentity(height uint64) crypto.ValidatorPubKey {
	if e.cfg.MyKey == nil {
		return nil
	}
	return e.cfg.ValidatorAuthorityFork.LocalIdentity(e.cfg.MyKey.Public(), height)
}

func (e *Engine) validateAuthorityFork() error {
	f := e.cfg.ValidatorAuthorityFork
	if f == nil {
		return nil
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if e.cfg.Store == nil || e.cfg.Registry == nil {
		return fmt.Errorf("authority fork requires canonical storage and registry")
	}
	genesis, found, err := e.cfg.Store.GetCanonicalHash(0)
	if err != nil || !found || genesis != f.Genesis {
		return fmt.Errorf("authority fork canonical genesis mismatch")
	}
	entry, found := e.cfg.Registry.GetEntry(f.Identity)
	if !found || entry.Seeded || !e.cfg.Registry.IsActive(f.Identity) {
		return fmt.Errorf("authority fork identity lacks active authenticated stake")
	}
	if err := e.cfg.Registry.ConfigureSignatureAuthority(f, e.chain.Height()+1); err != nil {
		return err
	}
	if e.chain.Height() >= f.Height-1 {
		parent, found, err := e.cfg.Store.GetCanonicalHash(f.Height - 1)
		if err != nil || !found || parent != f.Parent {
			return fmt.Errorf("authority fork pinned canonical parent unavailable")
		}
	}
	if e.chain.Height() == f.Height-1 {
		raw, err := json.Marshal(e.cfg.Registry.TakeSnapshot())
		if err != nil || crypto.Hash32(sha256.Sum256(raw)) != f.RegistrySHA256 {
			return fmt.Errorf("authority fork parent registry fingerprint mismatch")
		}
	}
	return nil
}

func (e *Engine) checkAuthorityParent(header core.BlockHeader) error {
	f := e.cfg.ValidatorAuthorityFork
	if f == nil {
		return nil
	}
	if err := f.CheckParent(header); err != nil {
		return err
	}
	if header.Height == f.Height {
		raw, err := json.Marshal(e.cfg.Registry.TakeSnapshot())
		if err != nil || crypto.Hash32(sha256.Sum256(raw)) != f.RegistrySHA256 {
			return fmt.Errorf("authority fork activation registry mismatch")
		}
	}
	return nil
}
