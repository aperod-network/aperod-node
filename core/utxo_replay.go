package core

import (
"fmt"
"github.com/aperod/aperod/crypto"
)

// WithAnchoredReplayKeyImages is exclusively for single-threaded, pre-readiness
// startup. The callback sees the anchor's spent set, not the durable tip's.
// Persistent callbacks and backends are disconnected until the callback ends.
// UTXO mutations remain in memory on error; the caller MUST abort startup.
func (s *UTXOSet) WithAnchoredReplayKeyImages(images []crypto.KeyImage, replay func() error) error {
recent := make(map[crypto.KeyImage]struct{}, len(images))
for _, image := range images {
canonical, err := crypto.CanonicalKeyImage(image)
if err != nil || canonical != image {
return fmt.Errorf("anchor has invalid/noncanonical key image")
}
if _, exists := recent[image]; exists {
return fmt.Errorf("anchor has duplicate key image")
}
recent[image] = struct{}{}
}
s.mu.Lock()
original := s.keyImages
spent, restored, deleted := s.OnUTXOSpent, s.OnUTXORestored, s.OnUTXODeleted
members := s.ringMembers
s.keyImages = compactKeyImageSet{recent: recent}
s.OnUTXOSpent, s.OnUTXORestored, s.OnUTXODeleted = nil, nil, nil
s.ringMembers = nil
s.mu.Unlock()
defer func() {
s.mu.Lock()
s.keyImages = original
s.OnUTXOSpent, s.OnUTXORestored, s.OnUTXODeleted = spent, restored, deleted
s.ringMembers = members
s.mu.Unlock()
}()
return replay()
}