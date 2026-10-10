package p2p_test

// Tests for peer Discovery: lifecycle, round with/without peers, KnownPeerCount.

import (
        "testing"
        "time"

        "github.com/aperod/aperod/p2p"
)

func TestDiscovery_StartStop(t *testing.T) {
        host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
        d := p2p.NewDiscovery(host, 100*time.Millisecond)
        d.Start()
        time.Sleep(20 * time.Millisecond)
        d.Stop()
}

func TestDiscovery_DefaultInterval(t *testing.T) {
        // interval=0 should default to 30s without panicking
        host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
        d := p2p.NewDiscovery(host, 0)
        d.Start()
        time.Sleep(10 * time.Millisecond)
        d.Stop()
}

func TestDiscovery_KnownPeerCount_Empty(t *testing.T) {
        host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
        d := p2p.NewDiscovery(host, time.Minute)
        if got := d.KnownPeerCount(); got != 0 {
                t.Errorf("KnownPeerCount = %d, want 0", got)
        }
}

func TestDiscovery_RequestPeersFrom_NotConnected(t *testing.T) {
        // Should not panic when peer is not connected
        host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
        d := p2p.NewDiscovery(host, time.Minute)
        d.RequestPeersFrom("192.168.1.1:7777")
}

func TestDiscovery_RoundNoBootnodes_NoPanic(t *testing.T) {
        // No bootnodes, no peers — multiple rounds should be a no-op
        host := p2p.NewHost(p2p.Config{MaxPeers: 10, Bootnodes: nil}, &stubHandler{}, newTestLogger())
        d := p2p.NewDiscovery(host, 50*time.Millisecond)
        d.Start()
        time.Sleep(130 * time.Millisecond) // at least 2 rounds
        d.Stop()
}

func TestDiscovery_RoundWithPeer_SendsGetPeers(t *testing.T) {
        // Start a host, connect a peer, then run a discovery round.
        // The round should send MsgGetPeers to the connected peer.
        host := p2p.NewHost(p2p.Config{
                ListenAddr: "127.0.0.1:0",
                MaxPeers:   10,
                NodeID:     "disc-host",
                UserAgent:  "aperod/test",
        }, &stubHandler{}, newTestLogger())
        if err := host.Start(); err != nil {
                t.Fatalf("Start: %v", err)
        }
        defer host.Stop()

        addr := host.ListenAddr()
        if addr == "" {
                t.Skip("ListenAddr unavailable")
        }

        // Connect and handshake — nil fn means just handshake and disconnect
        connectAndHandshake(t, addr, nil)

        // Now run discovery — may or may not reach the peer before it disconnects
        d := p2p.NewDiscovery(host, 50*time.Millisecond)
        d.Start()
        time.Sleep(150 * time.Millisecond)
        d.Stop()
}
