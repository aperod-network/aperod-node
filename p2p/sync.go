package p2p

import (
        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// HeaderProvider can serve a contiguous range of block headers for chain sync.
// Implemented by node.Node (which has access to the chain store).
type HeaderProvider interface {
        // HeadersFrom returns up to limit headers that come after any of knownHashes.
        // Returns nil if all knownHashes are unknown (force full sync from genesis).
        HeadersFrom(knownHashes []crypto.Hash32, limit int) []core.BlockHeader
}

// syncState tracks in-flight sync requests to a single peer.
type syncState struct {
        inFlight map[crypto.Hash32]bool // hashes requested but not yet received
}

func newSyncState() *syncState {
        return &syncState{inFlight: make(map[crypto.Hash32]bool)}
}

func (s *syncState) markRequested(h crypto.Hash32) { s.inFlight[h] = true }
func (s *syncState) markReceived(h crypto.Hash32)  { delete(s.inFlight, h) }
func (s *syncState) pending() int                   { return len(s.inFlight) }

// ─── Exported wrappers for tests ──────────────────────────────────────────────

// SyncState is an exported type alias for testing.
type SyncState struct{ s *syncState }

// NewSyncState returns an exported SyncState for use in tests.
func NewSyncState() *SyncState { return &SyncState{s: newSyncState()} }

func (ss *SyncState) MarkRequested(h crypto.Hash32) { ss.s.markRequested(h) }
func (ss *SyncState) MarkReceived(h crypto.Hash32)  { ss.s.markReceived(h) }
func (ss *SyncState) Pending() int                  { return ss.s.pending() }
