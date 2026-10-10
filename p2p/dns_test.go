package p2p

// dns_test.go — unit tests for resolveBootnode / ResolveBootnode, including
// IPv6 multiaddr handling and malformed-address error cases.

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

func TestResolveBootnode_MalformedMultiaddr_NoTCP(t *testing.T) {
	// A multiaddr that starts with /ip6/ but has no /tcp/<port> component must
	// return an error so the caller gets a clear message instead of passing the
	// raw string to net.Dial (which would fail with "too many colons in address").
	_, err := resolveBootnode("/ip6/badaddr")
	if err == nil {
		t.Fatal("expected an error for multiaddr with missing /tcp/ component, got nil")
	}
}

func TestResolveBootnode_MalformedMultiaddr_IPv4NoTCP(t *testing.T) {
	// Same check for /ip4/ prefix without a /tcp/<port> component.
	_, err := resolveBootnode("/ip4/1.2.3.4")
	if err == nil {
		t.Fatal("expected an error for /ip4/ multiaddr with missing /tcp/ component, got nil")
	}
}

func TestResolveBootnode_MalformedMultiaddr_InvalidPort(t *testing.T) {
	// A multiaddr with a non-numeric port must return an error.
	_, err := resolveBootnode("/ip4/1.2.3.4/tcp/notaport")
	if err == nil {
		t.Fatal("expected an error for multiaddr with invalid port, got nil")
	}
}

func TestResolveBootnode_MalformedHostPort(t *testing.T) {
	// A plain address that is not a valid host:port must return an error.
	_, err := resolveBootnode("not-a-valid-address")
	if err == nil {
		t.Fatal("expected an error for address that is not a valid host:port, got nil")
	}
}

func TestResolveBootnode_ExportedWrapper(t *testing.T) {
	// ResolveBootnode (exported) must behave identically to resolveBootnode.
	addrs, err := ResolveBootnode("/ip4/192.168.1.1/tcp/30303")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != "192.168.1.1:30303" {
		t.Errorf("got %v, want [192.168.1.1:30303]", addrs)
	}
}

// ── NormalizeBootnodeAddr tests ───────────────────────────────────────────────

func TestNormalizeBootnodeAddr_IPv6Multiaddr(t *testing.T) {
	got, err := NormalizeBootnodeAddr("/ip6/2a0b:4140:4a4e::2/tcp/30303")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "[2a0b:4140:4a4e::2]:30303"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeBootnodeAddr_IPv4Multiaddr(t *testing.T) {
	got, err := NormalizeBootnodeAddr("/ip4/192.168.1.1/tcp/30303")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "192.168.1.1:30303" {
		t.Errorf("got %q, want 192.168.1.1:30303", got)
	}
}

func TestNormalizeBootnodeAddr_DNSHostPassThrough(t *testing.T) {
	// DNS names must be returned unchanged — no resolution performed — so
	// the P2P host can retain the hostname for periodic re-resolution.
	got, err := NormalizeBootnodeAddr("bootnode.aperod.com:30303")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "bootnode.aperod.com:30303" {
		t.Errorf("got %q, want bootnode.aperod.com:30303", got)
	}
}

func TestNormalizeBootnodeAddr_MalformedMultiaddr_NoTCP(t *testing.T) {
	// /ip6/ multiaddr without a /tcp/<port> component must return an error.
	_, err := NormalizeBootnodeAddr("/ip6/badaddr")
	if err == nil {
		t.Fatal("expected error for multiaddr missing /tcp/ component, got nil")
	}
}

func TestNormalizeBootnodeAddr_MalformedMultiaddr_IPv4NoTCP(t *testing.T) {
	_, err := NormalizeBootnodeAddr("/ip4/1.2.3.4")
	if err == nil {
		t.Fatal("expected error for /ip4/ multiaddr missing /tcp/ component, got nil")
	}
}

func TestNormalizeBootnodeAddr_MalformedMultiaddr_InvalidPort(t *testing.T) {
	_, err := NormalizeBootnodeAddr("/ip4/1.2.3.4/tcp/notaport")
	if err == nil {
		t.Fatal("expected error for multiaddr with invalid port, got nil")
	}
}

func TestNormalizeBootnodeAddr_MalformedPlainAddr(t *testing.T) {
	// A plain string that is not a valid host:port must return an error.
	_, err := NormalizeBootnodeAddr("not-a-valid-address")
	if err == nil {
		t.Fatal("expected error for invalid host:port, got nil")
	}
}

func TestNormalizeBootnodeAddr_CrossFamily_IPv4InIPv6(t *testing.T) {
	// /ip6/ prefix with an IPv4 literal must be rejected.
	_, err := NormalizeBootnodeAddr("/ip6/1.2.3.4/tcp/30303")
	if err == nil {
		t.Fatal("expected error for /ip6/ with IPv4 host, got nil")
	}
}

func TestNormalizeBootnodeAddr_CrossFamily_IPv6InIPv4(t *testing.T) {
	// /ip4/ prefix with an IPv6 literal must be rejected.
	_, err := NormalizeBootnodeAddr("/ip4/::1/tcp/30303")
	if err == nil {
		t.Fatal("expected error for /ip4/ with IPv6 host, got nil")
	}
}

func TestNormalizeBootnodeAddr_NonIPHostInMultiaddr(t *testing.T) {
	// /ip4/ prefix with a non-IP hostname must be rejected (only IP literals allowed).
	_, err := NormalizeBootnodeAddr("/ip4/not-an-ip/tcp/30303")
	if err == nil {
		t.Fatal("expected error for /ip4/ with non-IP host, got nil")
	}
}

func TestNormalizeBootnodeAddr_NamedPort(t *testing.T) {
	// Named service ports (e.g. "http") must be rejected; only decimal integers allowed.
	_, err := NormalizeBootnodeAddr("/ip4/192.168.1.1/tcp/http")
	if err == nil {
		t.Fatal("expected error for named port in multiaddr, got nil")
	}
}

func TestNormalizeBootnodeAddr_EmptyHost(t *testing.T) {
	// Empty host in multiaddr must be rejected.
	_, err := NormalizeBootnodeAddr("/ip4//tcp/30303")
	if err == nil {
		t.Fatal("expected error for empty host in multiaddr, got nil")
	}
}

func TestNormalizeBootnodeAddr_PortOutOfRange(t *testing.T) {
	// Port 0 and ports above 65535 must be rejected.
	for _, addr := range []string{
		"/ip4/192.168.1.1/tcp/0",
		"/ip4/192.168.1.1/tcp/65536",
	} {
		_, err := NormalizeBootnodeAddr(addr)
		if err == nil {
			t.Errorf("expected error for out-of-range port in %q, got nil", addr)
		}
	}
}

// ── resolveBootnode strict-validation tests ───────────────────────────────────

func TestResolveBootnode_CrossFamily_IPv4InIPv6(t *testing.T) {
	_, err := resolveBootnode("/ip6/1.2.3.4/tcp/30303")
	if err == nil {
		t.Fatal("expected error for /ip6/ with IPv4 host, got nil")
	}
}

func TestResolveBootnode_NonIPHostInMultiaddr(t *testing.T) {
	// A hostname in a /ip4/ or /ip6/ multiaddr is not valid — only IP literals.
	_, err := resolveBootnode("/ip4/not-an-ip/tcp/30303")
	if err == nil {
		t.Fatal("expected error for /ip4/ with non-IP host, got nil")
	}
}

func TestResolveBootnode_NamedPort(t *testing.T) {
	_, err := resolveBootnode("/ip4/192.168.1.1/tcp/http")
	if err == nil {
		t.Fatal("expected error for named port in multiaddr, got nil")
	}
}
