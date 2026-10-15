package p2p

import (
	"container/list"
	"sync"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

const (
	gossipCacheSize = 4096
	gossipCacheTTL  = 5 * time.Minute
)

// gossipEntry records a hash and its insertion time in FIFO order.
type gossipEntry struct {
	hash crypto.Hash32
	at time.Time
}

// GossipFilter is a bounded seen-set that prevents a block or transaction from
// being relayed more than once.  Thread-safe.
type GossipFilter struct {
	mu    sync.Mutex
	seen  map[crypto.Hash32]*list.Element
	order list.List
}

// NewGossipFilter creates an empty GossipFilter.
func NewGossipFilter() *GossipFilter {
	return &GossipFilter{seen: make(map[crypto.Hash32]*list.Element)}
}

// MarkAndCheck returns true if h was NOT seen before (new message) and marks
// it as seen.  Returns false if h was already present (duplicate — skip relay).
func (gf *GossipFilter) MarkAndCheck(h crypto.Hash32) bool {
	gf.mu.Lock()
	defer gf.mu.Unlock()

	now := time.Now()
	gf.evictExpiredLocked(now)
	if _, exists := gf.seen[h]; exists {
		return false
	}
	if len(gf.seen) >= gossipCacheSize {
		gf.evictOldestLocked()
	}
	element := gf.order.PushBack(gossipEntry{hash: h, at: now})
	gf.seen[h] = element
	return true
}

// HasSeen returns true if h is currently in the seen set.
func (gf *GossipFilter) HasSeen(h crypto.Hash32) bool {
	gf.mu.Lock()
	defer gf.mu.Unlock()
	gf.evictExpiredLocked(time.Now())
	_, ok := gf.seen[h]
	return ok
}

// Size returns the current number of entries in the filter.
func (gf *GossipFilter) Size() int {
	gf.mu.Lock()
	defer gf.mu.Unlock()
	return len(gf.seen)
}

// evictExpiredLocked removes expired entries from the FIFO head.
// Caller must hold gf.mu.
func (gf *GossipFilter) evictExpiredLocked(now time.Time) {
	cutoff := now.Add(-gossipCacheTTL)
	for front := gf.order.Front(); front != nil; front = gf.order.Front() {
		entry := front.Value.(gossipEntry)
		if !entry.at.Before(cutoff) {
			return
		}
		delete(gf.seen, entry.hash)
		gf.order.Remove(front)
	}
}

// evictOldestLocked removes exactly one oldest entry.
// Caller must hold gf.mu.
func (gf *GossipFilter) evictOldestLocked() {
	front := gf.order.Front()
	if front == nil {
		return
	}
	entry := front.Value.(gossipEntry)
	delete(gf.seen, entry.hash)
	gf.order.Remove(front)
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
