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

// BanInfo is a snapshot of one active ban entry.
type BanInfo struct {
	Addr      string    `json:"addr"`
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ListBans returns a snapshot of all currently active bans (expired entries
// are pruned before the snapshot is taken).
func (pm *PeerMgr) ListBans() []BanInfo {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	now := time.Now()
	out := make([]BanInfo, 0, len(pm.banned))
	for addr, e := range pm.banned {
		if now.After(e.until) {
			delete(pm.banned, addr)
			continue
		}
		out = append(out, BanInfo{Addr: addr, Reason: e.reason, ExpiresAt: e.until})
	}
	return out
}

// LiftBan removes the ban for addr (exact match only).
// Returns true when a ban was found and removed; false when addr was not banned.
func (pm *PeerMgr) LiftBan(addr string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if _, ok := pm.banned[addr]; ok {
		delete(pm.banned, addr)
		return true
	}
	return false
}
