package consensus

import "github.com/aperod/aperod/crypto"

// Sales must remain recoverable after restart, when the in-memory per-height
// cache contains only the restored, quorum-verified certificate. A canonical
// ancestor of that finalized block is finalized too. Do not apply this fallback
// to LPoD witness/tier verification, which retains its stricter evidence path.
func (e *Engine) IsSaleFinalizedHash(height uint64, hash crypto.Hash32) bool {
	if height == 0 || hash == (crypto.Hash32{}) || e.cfg.Store == nil {
		return false
	}
	e.mu.Lock()
	var anchorHeight uint64
	var anchorHash crypto.Hash32
	for h, v := range e.lpodFinalizedHashes {
		if h > anchorHeight && v != (crypto.Hash32{}) {
			anchorHeight, anchorHash = h, v
		}
	}
	e.mu.Unlock()
	if anchorHeight < height || anchorHash == (crypto.Hash32{}) {
		return false
	}
	anchor, found, err := e.cfg.Store.GetCanonicalHash(anchorHeight)
	if err != nil || !found || anchor != anchorHash {
		return false
	}
	canonical, found, err := e.cfg.Store.GetCanonicalHash(height)
	return err == nil && found && canonical == hash
}
