package p2p

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// banEntry records why and until when a peer is banned.
type banEntry struct {
	reason string
	until  time.Time
}

// dialState tracks consecutive dial failures for exponential back-off.
type dialState struct {
	failures int
	nextDial time.Time
	lastFail time.Time // when the most recent failure was recorded
}

// maxDialStates is the cap on the number of dial-state entries kept in RAM.
// Entries are also pruned when they have not seen a failure for dialStateTTL.
const maxDialStates = 1_000
const dialStateTTL = time.Hour

// backoffDuration returns the delay before the next dial attempt.
// Progression: 5 s, 10 s, 20 s, 40 s, 80 s, … capped at 5 minutes.
func backoffDuration(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	const base = 5 * time.Second
	const cap_ = 5 * time.Minute
	d := base
	for i := 1; i < failures; i++ {
		d *= 2
		if d > cap_ {
			return cap_
		}
	}
	if d > cap_ {
		return cap_
	}
	return d
}

// PeerMgr tracks the ban list, enforces peer limits, and applies
// per-peer exponential back-off on repeated dial failures.
type PeerMgr struct {
	mu         sync.Mutex
	banned     map[string]banEntry  // addr → ban
	dialStates map[string]dialState // addr → backoff state

	// banFile is the path to the JSON persistence file; "" or "-" = disabled.
	banFile string
	// persistMu serializes the snapshot+write+rename sequence so that
	// concurrent Ban/LiftBan calls never interleave their writes.  The mutex
	// is acquired before the snapshot to guarantee the last writer wins with a
	// fully-consistent view of the ban map.
	persistMu sync.Mutex
}

func newPeerMgr() *PeerMgr {
	return &PeerMgr{
		banned:     make(map[string]banEntry),
		dialStates: make(map[string]dialState),
	}
}

// newPeerMgrWithFile creates a PeerMgr that persists its ban list to banFile
// on every change.  An empty banFile or "-" disables persistence.
func newPeerMgrWithFile(banFile string) *PeerMgr {
	return &PeerMgr{
		banned:     make(map[string]banEntry),
		dialStates: make(map[string]dialState),
		banFile:    banFile,
	}
}

// persistedBan is the on-disk representation of one ban entry.
type persistedBan struct {
	Addr   string    `json:"addr"`
	Reason string    `json:"reason"`
	Until  time.Time `json:"until"`
}

// persistBans writes the current active ban list to pm.banFile atomically.
//
// Concurrency contract:
//   - persistMu is acquired BEFORE the snapshot so that the serialized order
//     of writes reflects the serialized order of map mutations.  Two concurrent
//     Ban/LiftBan calls therefore cannot reorder their writes — the second
//     always snapshots after the first has finished its rename.
//   - pm.mu is held only for the duration of the map copy; disk I/O happens
//     outside pm.mu so the ban-check hot path is never blocked by I/O.
//
// A no-op when pm.banFile is empty or "-".
func (pm *PeerMgr) persistBans() {
	if pm.banFile == "" || pm.banFile == "-" {
		return
	}

	// Serialize the complete snapshot+write+rename so concurrent callers
	// see a consistent final file — the last mutation's snapshot wins.
	pm.persistMu.Lock()
	defer pm.persistMu.Unlock()

	// Snapshot the ban map under pm.mu.  Disk I/O happens after unlock.
	pm.mu.Lock()
	now := time.Now()
	out := make([]persistedBan, 0, len(pm.banned))
	for addr, e := range pm.banned {
		if now.Before(e.until) {
			out = append(out, persistedBan{Addr: addr, Reason: e.reason, Until: e.until})
		}
	}
	pm.mu.Unlock()

	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		// Should never happen for this simple struct.
		slog.Default().Error("p2p: ban persistence: marshal failed", "err", err)
		return
	}

	// Write to a unique temp file in the same directory so the final rename
	// is an atomic in-directory move (no cross-device rename).
	dir := filepath.Dir(pm.banFile)
	tmp, err := os.CreateTemp(dir, ".p2p_bans_*.tmp")
	if err != nil {
		slog.Default().Warn("p2p: ban persistence: create temp file failed",
			"dir", dir, "err", err)
		return
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(encoded); err != nil {
		slog.Default().Warn("p2p: ban persistence: write failed", "file", tmpName, "err", err)
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		slog.Default().Warn("p2p: ban persistence: close failed", "file", tmpName, "err", err)
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, pm.banFile); err != nil {
		slog.Default().Warn("p2p: ban persistence: rename failed",
			"tmp", tmpName, "dst", pm.banFile, "err", err)
		os.Remove(tmpName)
	}
}

// LoadBansFromFile restores ban entries persisted by a previous run.
// Entries that expired while the node was down are silently discarded.
// A missing file is not an error (first boot) — nil is returned.
//
// A ban file that exists but cannot be read or decoded is a fatal error:
// the caller (Start) must abort rather than continuing with an empty ban
// list, which would allow previously-banned IPs to reconnect immediately
// after a crash-corrupted write.  The operator must repair or remove the
// file to restart the node.
func (pm *PeerMgr) LoadBansFromFile() error {
	if pm.banFile == "" || pm.banFile == "-" {
		return nil
	}
	data, err := os.ReadFile(pm.banFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first boot; no file yet
		}
		return fmt.Errorf("p2p: ban sidecar %q cannot be read: %w — "+
			"repair or remove the file to restart the node", pm.banFile, err)
	}
	var entries []persistedBan
	if jsonErr := json.Unmarshal(data, &entries); jsonErr != nil {
		return fmt.Errorf("p2p: ban sidecar %q is corrupt (JSON parse error): %w — "+
			"repair or remove the file to restart the node", pm.banFile, jsonErr)
	}
	// json.Unmarshal sets entries to nil for a JSON null value.
	// A null sidecar is not a valid empty list: it indicates a truncated or
	// tampered file.  Fail-closed so previously-banned IPs stay blocked.
	if entries == nil {
		return fmt.Errorf("p2p: ban sidecar %q contains JSON null (not a valid "+
			"ban array) — repair or remove the file to restart the node", pm.banFile)
	}
	now := time.Now()
	pm.mu.Lock()
	for _, e := range entries {
		if now.Before(e.Until) {
			pm.banned[e.Addr] = banEntry{reason: e.Reason, until: e.Until}
		}
	}
	pm.mu.Unlock()
	return nil
}

// CanDial returns true when addr is not banned and its back-off window has
// elapsed.  Always returns true for addresses with no recorded failures.
func (pm *PeerMgr) CanDial(addr string) bool {
	if pm.IsBanned(addr) {
		return false
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	st, ok := pm.dialStates[addr]
	if !ok {
		return true
	}
	return time.Now().After(st.nextDial)
}

// OnDialFail records a failed dial attempt and advances the back-off window.
// It also prunes entries that have not seen a failure in dialStateTTL to keep
// the map bounded.  If the map still exceeds maxDialStates after TTL pruning,
// the oldest entries (by lastFail) are removed until it fits.
func (pm *PeerMgr) OnDialFail(addr string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	now := time.Now()
	st := pm.dialStates[addr]
	st.failures++
	st.nextDial = now.Add(backoffDuration(st.failures))
	st.lastFail = now
	pm.dialStates[addr] = st

	// Prune expired entries first.
	for a, s := range pm.dialStates {
		if now.Sub(s.lastFail) > dialStateTTL {
			delete(pm.dialStates, a)
		}
	}
	// If still over the cap, evict the entry with the oldest lastFail.
	for len(pm.dialStates) > maxDialStates {
		var oldest string
		var oldestTime time.Time
		for a, s := range pm.dialStates {
			if oldest == "" || s.lastFail.Before(oldestTime) {
				oldest = a
				oldestTime = s.lastFail
			}
		}
		delete(pm.dialStates, oldest)
	}
}

// OnDialSuccess resets the back-off state for addr after a successful
// connection that has been established (call when handleConn exits cleanly).
func (pm *PeerMgr) OnDialSuccess(addr string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.dialStates, addr)
}

// Ban adds addr to the ban list for duration d with a human-readable reason.
// The updated ban list is persisted to disk (when a ban file is configured)
// after the mutex is released so disk I/O never blocks the caller.
func (pm *PeerMgr) Ban(addr, reason string, d time.Duration) {
	pm.mu.Lock()
	pm.banned[addr] = banEntry{reason: reason, until: time.Now().Add(d)}
	pm.mu.Unlock()
	pm.persistBans()
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
// The updated ban list is persisted to disk (when a ban file is configured)
// after the mutex is released.
func (pm *PeerMgr) LiftBan(addr string) bool {
	pm.mu.Lock()
	_, ok := pm.banned[addr]
	if ok {
		delete(pm.banned, addr)
	}
	pm.mu.Unlock()
	if ok {
		pm.persistBans()
	}
	return ok
}
