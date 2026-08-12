package p2p_test

// Test for TASK #1926: banned peers must not appear in outbound MsgPeers.
//
// The MsgGetPeers handler builds a MsgPeers payload from the connected peer
// table.  A peer whose bare IP is currently banned must be filtered out so a
// locally-banned node cannot propagate back into the network through the
// peer-exchange protocol.

import (
	"testing"
	"time"

	"github.com/aperod/aperod/p2p"
)

// TestGetPeers_ExcludesBannedAddress seeds two connected peers into a host,
// bans one of them, and asserts the banned address is absent from the
// peers-list construction while the un-banned one remains.
func TestGetPeers_ExcludesBannedAddress(t *testing.T) {
	t.Parallel()

	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "host",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())

	const goodAddr = "203.0.113.5:7777"
	const bannedAddr = "198.51.100.9:7777"

	p2p.HostSeedConnectedPeer(host, goodAddr)
	p2p.HostSeedConnectedPeer(host, bannedAddr)

	// Ban the bare IP of bannedAddr.  IsBanned resolves "IP:port" to the bare
	// IP, so banning "198.51.100.9" must hide the "198.51.100.9:7777" entry.
	p2p.HostBanPeer(host, "198.51.100.9", "test ban", time.Hour)

	addrs := p2p.HostPeersToAdvertise(host)

	sawGood := false
	for _, a := range addrs {
		if a == bannedAddr {
			t.Errorf("banned address %q must not appear in MsgPeers payload: %v", bannedAddr, addrs)
		}
		if a == goodAddr {
			sawGood = true
		}
	}
	if !sawGood {
		t.Errorf("un-banned address %q missing from MsgPeers payload: %v", goodAddr, addrs)
	}
}
