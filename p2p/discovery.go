package p2p

import (
	"math/rand"
	"time"
)

// Discovery handles peer discovery by periodically requesting peer lists from
// connected peers and re-dialling known addresses when the peer count is low.
// It is a lightweight pull-based "gossip about peers" — each round, we pick up
// to discoverFanout random peers and ask them for their MsgGetPeers list.
type Discovery struct {
	host     *Host
	interval time.Duration
	fanout   int
	done     chan struct{}
}

const discoverFanout = 3

// NewDiscovery creates a peer-discovery controller attached to host.
// interval is how often a discovery round runs (0 defaults to 30s).
func NewDiscovery(host *Host, interval time.Duration) *Discovery {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Discovery{
		host:     host,
		interval: interval,
		fanout:   discoverFanout,
		done:     make(chan struct{}),
	}
}

// Start launches the background discovery loop.
func (d *Discovery) Start() {
	go d.loop()
}

// Stop shuts down the discovery goroutine.
func (d *Discovery) Stop() {
	close(d.done)
}

// loop runs a discovery round immediately, then every d.interval.
func (d *Discovery) loop() {
	d.round() // immediate first round

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			d.round()
		}
	}
}

// round requests peer lists from up to fanout random connected peers.
// If no peers are connected, it re-dials all known bootnodes.
func (d *Discovery) round() {
	h := d.host

	h.mu.RLock()
	peers := make([]*Peer, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.RUnlock()

	if len(peers) == 0 {
		h.log.Debug("discovery: no peers, re-dialling bootnodes")
		for _, addr := range h.cfg.Bootnodes {
			go func(a string) {
				resolved, err := resolveBootnode(a)
				if err != nil {
					h.log.Debug("discovery: bootnode dns resolve failed", "addr", a, "err", err)
					return
				}
				for _, r := range resolved {
					h.dialPeer(r)
				}
			}(addr)
		}
		return
	}

	// Shuffle and pick up to fanout peers to ask
	perm := rand.Perm(len(peers))
	n := d.fanout
	if n > len(perm) {
		n = len(perm)
	}
	for _, i := range perm[:n] {
		if err := peers[i].Send(MsgGetPeers, struct{}{}); err != nil {
			h.log.Debug("discovery: getpeers failed", "peer", peers[i].addr, "err", err)
		}
	}
}

// RequestPeersFrom sends a single MsgGetPeers to the peer at addr (if connected).
// Useful for on-demand discovery immediately after a connection is established.
func (d *Discovery) RequestPeersFrom(addr string) {
	d.host.mu.RLock()
	p, ok := d.host.peers[addr]
	d.host.mu.RUnlock()
	if ok {
		_ = p.Send(MsgGetPeers, struct{}{})
	}
}

// KnownPeerCount returns the number of addresses in the host's known-peer list.
func (d *Discovery) KnownPeerCount() int {
	d.host.mu.RLock()
	defer d.host.mu.RUnlock()
	return len(d.host.peerList)
}
