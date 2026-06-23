package p2p

import (
	"sync"
	"time"
)

// banEntry records why and until when a peer is banned.
type banEntry struct {
	reason  string
	until   time.Time
}

// PeerMgr tracks the ban list and enforces peer limits.
// It is embedded in Host to separate concern from connection logic.
type PeerMgr struct {
	mu      sync.Mutex
	banned  map[string]banEntry // addr → ban
}

func newPeerMgr() *PeerMgr {
	return &PeerMgr{banned: make(map[string]banEntry)}
}

// Ban adds addr to the ban list for duration d with a human-readable reason.
func (pm *PeerMgr) Ban(addr, reason string, d time.Duration) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.banned[addr] = banEntry{reason: reason, until: time.Now().Add(d)}
}

// IsBanned returns true if addr is currently banned.
func (pm *PeerMgr) IsBanned(addr string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	e, ok := pm.banned[addr]
	if !ok {
		return false
	}
	if time.Now().After(e.until) {
		delete(pm.banned, addr)
		return false
	}
	return true
}

// Prune removes expired ban entries.
func (pm *PeerMgr) Prune() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	now := time.Now()
	for addr, e := range pm.banned {
		if now.After(e.until) {
			delete(pm.banned, addr)
		}
	}
}

// BannedCount returns the number of active bans.
func (pm *PeerMgr) BannedCount() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	count := 0
	now := time.Now()
	for _, e := range pm.banned {
		if now.Before(e.until) {
			count++
		}
	}
	return count
}
