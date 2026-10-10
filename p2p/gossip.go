package p2p

import (
	"sync"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

const (
	gossipCacheSize = 4096
	gossipCacheTTL  = 5 * time.Minute
)

// gossipEntry records when a hash was first seen for TTL-based eviction.
type gossipEntry struct {
	at time.Time
}

// GossipFilter is a bounded seen-set that prevents a block or transaction from
// being relayed more than once.  Thread-safe.
type GossipFilter struct {
	mu   sync.Mutex
	seen map[crypto.Hash32]gossipEntry
}

// NewGossipFilter creates an empty GossipFilter.
func NewGossipFilter() *GossipFilter {
	return &GossipFilter{seen: make(map[crypto.Hash32]gossipEntry)}
}

// MarkAndCheck returns true if h was NOT seen before (new message) and marks
// it as seen.  Returns false if h was already present (duplicate — skip relay).
func (gf *GossipFilter) MarkAndCheck(h crypto.Hash32) bool {
	gf.mu.Lock()
	defer gf.mu.Unlock()

	if _, exists := gf.seen[h]; exists {
		return false
	}
	// Evict stale entries before inserting to keep memory bounded.
	if len(gf.seen) >= gossipCacheSize {
		gf.evictLocked()
	}
	gf.seen[h] = gossipEntry{at: time.Now()}
	return true
}

// HasSeen returns true if h is currently in the seen set.
func (gf *GossipFilter) HasSeen(h crypto.Hash32) bool {
	gf.mu.Lock()
	defer gf.mu.Unlock()
	_, ok := gf.seen[h]
	return ok
}

// Size returns the current number of entries in the filter.
func (gf *GossipFilter) Size() int {
	gf.mu.Lock()
	defer gf.mu.Unlock()
	return len(gf.seen)
}

// evictLocked removes all entries older than gossipCacheTTL.
// Caller must hold gf.mu.
func (gf *GossipFilter) evictLocked() {
	cutoff := time.Now().Add(-gossipCacheTTL)
	for h, e := range gf.seen {
		if e.at.Before(cutoff) {
			delete(gf.seen, h)
		}
	}
}

// ─── Gossip ───────────────────────────────────────────────────────────────────

// Gossip implements flood-fill propagation for blocks and transactions.
// When a message arrives from one peer, Gossip relays it to all OTHER connected
// peers — but only once per unique message hash (deduplication via GossipFilter).
type Gossip struct {
	host   *Host
	filter *GossipFilter
}

// NewGossip attaches a gossip controller to host.
func NewGossip(host *Host) *Gossip {
	return &Gossip{
		host:   host,
		filter: NewGossipFilter(),
	}
}

// RelayBlock relays block to all peers except the one at fromAddr.
// Returns true if the block was new and was relayed; false if already seen.
func (g *Gossip) RelayBlock(block *core.Block, fromAddr string) bool {
	hash := block.Hash()
	if !g.filter.MarkAndCheck(hash) {
		return false
	}

	g.host.mu.RLock()
	peers := make([]*Peer, 0, len(g.host.peers))
	for addr, p := range g.host.peers {
		if addr != fromAddr {
			peers = append(peers, p)
		}
	}
	g.host.mu.RUnlock()

	sb := blockToMsg(block)
	for _, p := range peers {
		if err := p.Send(MsgBlock, sb); err != nil {
			g.host.log.Debug("gossip: relay block failed", "peer", p.addr, "err", err)
		}
	}
	return true
}

// RelayTx relays tx to all peers except the one at fromAddr.
// Returns true if the tx was new and was relayed; false if already seen.
func (g *Gossip) RelayTx(tx *core.Transaction, fromAddr string) bool {
	hash := tx.Hash()
	if !g.filter.MarkAndCheck(hash) {
		return false
	}

	g.host.mu.RLock()
	peers := make([]*Peer, 0, len(g.host.peers))
	for addr, p := range g.host.peers {
		if addr != fromAddr {
			peers = append(peers, p)
		}
	}
	g.host.mu.RUnlock()

	for _, p := range peers {
		if err := p.Send(MsgTx, tx); err != nil {
			g.host.log.Debug("gossip: relay tx failed", "peer", p.addr, "err", err)
		}
	}
	return true
}

// MarkBlock marks a block hash as seen without relaying it.
// Use this for blocks produced locally (so they aren't echoed back).
func (g *Gossip) MarkBlock(hash crypto.Hash32) {
	g.filter.MarkAndCheck(hash)
}

// MarkTx marks a tx hash as seen without relaying it.
func (g *Gossip) MarkTx(hash crypto.Hash32) {
	g.filter.MarkAndCheck(hash)
}

// Filter returns the underlying GossipFilter for inspection.
func (g *Gossip) Filter() *GossipFilter { return g.filter }
