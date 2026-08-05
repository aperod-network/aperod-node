package p2p

// dns_test.go — unit tests for resolveBootnode, including IPv6 multiaddr handling.
// Lives in package p2p (not p2p_test) because resolveBootnode is unexported.

import (
	"testing"
)

func TestResolveBootnode_IPv6Multiaddr(t *testing.T) {
	// The production bootnode uses /ip6/<addr>/tcp/<port> format.
	// net.Dial rejects the raw multiaddr with "too many colons in address";
	// resolveBootnode must strip it to "[addr]:port" before the caller dials.
	addr, err := resolveBootnode("/ip6/2a0b:4140:4a4e::2/tcp/30303")
	if err != nil {
		t.Fatalf("resolveBootnode returned unexpected error: %v", err)
	}
	if len(addr) != 1 {
		t.Fatalf("expected 1 result, got %d: %v", len(addr), addr)
	}
	want := "[2a0b:4140:4a4e::2]:30303"
	if addr[0] != want {
		t.Errorf("got %q, want %q", addr[0], want)
	}
}

func TestResolveBootnode_IPv6Multiaddr_Loopback(t *testing.T) {
	// ::1 is the canonical IPv6 loopback address; same parsing rules apply.
	addrs, err := resolveBootnode("/ip6/::1/tcp/9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(addrs) != 1 {
		t.Fatalf("expected 1 result, got %d: %v", len(addrs), addrs)
	}
	want := "[::1]:9000"
	if addrs[0] != want {
		t.Errorf("got %q, want %q", addrs[0], want)
	}
}

func TestResolveBootnode_IPv4Multiaddr(t *testing.T) {
	// /ip4/<addr>/tcp/<port> should still normalise correctly.
	addrs, err := resolveBootnode("/ip4/192.168.1.1/tcp/30303")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(addrs) != 1 {
		t.Fatalf("expected 1 result, got %d: %v", len(addrs), addrs)
	}
	want := "192.168.1.1:30303"
	if addrs[0] != want {
		t.Errorf("got %q, want %q", addrs[0], want)
	}
}

func TestResolveBootnode_IPv4Literal(t *testing.T) {
	addrs, err := resolveBootnode("192.168.1.1:30303")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != "192.168.1.1:30303" {
		t.Errorf("got %v, want [192.168.1.1:30303]", addrs)
	}
}

func TestResolveBootnode_IPv6BracketedLiteral(t *testing.T) {
	// Already in standard host:port form; should pass through unchanged.
	addrs, err := resolveBootnode("[2a0b:4140:4a4e::2]:30303")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != "[2a0b:4140:4a4e::2]:30303" {
		t.Errorf("got %v, want [[2a0b:4140:4a4e::2]:30303]", addrs)
	}
}

func TestResolveBootnode_RawMultiaddrNotPassedToDialer(t *testing.T) {
	// Regression: the raw multiaddr string must NEVER be returned as an
	// address for the caller to dial — net.Dial rejects it with
	// "too many colons in address".
	addrs, err := resolveBootnode("/ip6/2a0b:4140:4a4e::2/tcp/30303")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range addrs {
		if len(a) > 0 && a[0] == '/' {
			t.Errorf("resolveBootnode returned raw multiaddr %q; net.Dial would reject this", a)
		}
	}
}
