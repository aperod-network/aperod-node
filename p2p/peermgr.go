package p2p

import (
	"net"
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
// It checks both the full "IP:port" address and the bare IP so that a ban
// registered with just the IP (e.g. "1.2.3.4") blocks all connections from
// that host regardless of source port.
func (pm *PeerMgr) IsBanned(addr string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	check := func(key string) bool {
		e, ok := pm.banned[key]
		if !ok {
			return false
		}
		if time.Now().After(e.until) {
			delete(pm.banned, key)
			return false
		}
		return true
	}

	if check(addr) {
		return true
	}
	// Also check a bare-IP ban when addr is "IP:port".
	if ip, _, err := net.SplitHostPort(addr); err == nil && ip != addr {
		return check(ip)
	}
	return false
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
