package p2p_test

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/p2p"
)

// TestHost_InboundAdmissionRecheckAfterHandshake releases several application
// handshakes together.  They all pass acceptLoop's early empty-table snapshot,
// so the post-handshake admission check must serialize their registration.
func TestHost_InboundAdmissionRecheckAfterHandshake(t *testing.T) {
	const (
		maxPeers    = 2
		minOutbound = 1
		maxPerIP    = 1
		attackers   = 6
	)

	host := p2p.NewHost(p2p.Config{
		ListenAddr:    "127.0.0.1:0",
		MaxPeers:      maxPeers,
		MinOutbound:   minOutbound,
		MaxPeersPerIP: maxPerIP,
		NodeID:        "admission-race-host",
		UserAgent:     "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	t.Cleanup(host.Stop)

	conns := make([]net.Conn, 0, attackers)
	for i := 0; i < attackers; i++ {
		conn, err := net.DialTimeout("tcp", host.ListenAddr(), 2*time.Second)
		if err != nil {
			t.Fatalf("dial attacker %d: %v", i, err)
		}
		conns = append(conns, conn)
	}
	t.Cleanup(func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	// Let acceptLoop start all the pre-handshake handlers, then release their
	// Pings concurrently.  This specifically exercises the window between its
	// early optimization and post-handshake peer registration.
	time.Sleep(50 * time.Millisecond)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i, conn := range conns {
		wg.Add(1)
		go func(i int, conn net.Conn) {
			defer wg.Done()
			<-release
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
				NodeID:    fmt.Sprintf("attacker-%d", i),
				UserAgent: "aperod/test",
				Timestamp: time.Now().UnixNano(),
			}); err == nil {
				// A rejected peer may still receive its Pong because admission
				// occurs after the asymmetric application handshake.
				_, _, _ = p2p.ReadMsg(conn)
			}
		}(i, conn)
	}
	close(release)
	wg.Wait()

	// All attackers use 127.0.0.1.  The strictest inbound limit is one:
	// MaxPeers-MinOutbound == 1 and MaxPeersPerIP == 1.  It is necessarily
	// also below MaxPeers.
	time.Sleep(100 * time.Millisecond)
	if got := host.PeerCount(); got > maxPeers {
		t.Errorf("PeerCount = %d, exceeds MaxPeers = %d", got, maxPeers)
	}
	if got := host.PeerCount(); got > maxPeers-minOutbound {
		t.Errorf("inbound PeerCount = %d, exceeds MaxPeers-MinOutbound = %d", got, maxPeers-minOutbound)
	}
	if got := host.PeerCount(); got > maxPerIP {
		t.Errorf("same-IP PeerCount = %d, exceeds MaxPeersPerIP = %d", got, maxPerIP)
	}

	for _, conn := range conns {
		_ = conn.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for host.PeerCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := host.PeerCount(); got != 0 {
		t.Fatalf("attacker peer did not disconnect: PeerCount = %d", got)
	}

	// A later honest peer must still be admitted once the attacker releases
	// its slot; rejected post-handshake attempts must not leak a peer entry.
	honest, err := net.DialTimeout("tcp", host.ListenAddr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial honest peer: %v", err)
	}
	defer honest.Close()
	_ = honest.SetDeadline(time.Now().Add(2 * time.Second))
	if err := p2p.WriteMsg(honest, p2p.MsgPing, p2p.PingMsg{
		NodeID: "honest-peer", UserAgent: "aperod/test", Timestamp: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("write honest ping: %v", err)
	}
	if msgType, _, err := p2p.ReadMsg(honest); err != nil || msgType != p2p.MsgPong {
		t.Fatalf("read honest pong: type=%v err=%v", msgType, err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for host.PeerCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := host.PeerCount(); got != 1 {
		t.Fatalf("honest peer was not admitted: PeerCount = %d, want 1", got)
	}
}
