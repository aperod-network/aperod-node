package p2p

import (
	"net/netip"
	"testing"
)

func TestIsPublicPeerIPRejectsNonPublicAndEmbeddedTargets(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{name: "public IPv4", addr: "8.8.8.8", want: true},
		{name: "public IPv6", addr: "2606:4700:4700::1111", want: true},
		{name: "private IPv4", addr: "10.0.0.1", want: false},
		{name: "NAT64 private IPv4", addr: "64:ff9b::a00:1", want: false},
		{name: "NAT64 loopback IPv4", addr: "64:ff9b::7f00:1", want: false},
		{name: "NAT64 public IPv4", addr: "64:ff9b::808:808", want: true},
		{name: "NAT64 local-use prefix", addr: "64:ff9b:1::a00:1", want: false},
		{name: "Teredo", addr: "2001::1", want: false},
		{name: "IPv6 discard-only", addr: "100::1", want: false},
		{name: "6to4", addr: "2002:a00:1::1", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := netip.MustParseAddr(tt.addr)
			if got := isPublicPeerIP(ip); got != tt.want {
				t.Fatalf("isPublicPeerIP(%s) = %t, want %t", ip, got, tt.want)
			}
		})
	}
}

func TestNormalizeGossipPeerAddressChecksNAT64EmbeddedIPv4(t *testing.T) {
	if got := normalizeGossipPeerAddress("[64:ff9b::a00:1]:30303"); got != "" {
		t.Fatalf("private NAT64 peer address was accepted as %q", got)
	}
	if got := normalizeGossipPeerAddress("[64:ff9b::808:808]:30303"); got == "" {
		t.Fatal("public NAT64 peer address was rejected")
	}
}
