package p2p

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// parseMultiaddr parses a /ip4/<host>/tcp/<port> or /ip6/<host>/tcp/<port>
// multiaddr string and returns the validated host and decimal port as strings.
//
// Validation rules enforced:
//   - /ip4/ prefix requires a non-empty IPv4 literal (net.ParseIP, must not be v6)
//   - /ip6/ prefix requires a non-empty IPv6 literal (net.ParseIP, must not be v4)
//   - /tcp/<port> must be a decimal integer in [1, 65535] — named services (e.g.
//     "http") are rejected
func parseMultiaddr(addr string) (host, port string, err error) {
	isIPv6 := strings.HasPrefix(addr, "/ip6/")
	stripped := addr[5:] // remove "/ip4/" or "/ip6/"
	parts := strings.SplitN(stripped, "/tcp/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid bootnode multiaddr %q: missing /tcp/<port> component", addr)
	}
	host = parts[0]
	port = parts[1]

	if host == "" {
		return "", "", fmt.Errorf("invalid bootnode multiaddr %q: host is empty", addr)
	}

	// Validate that the host is an IP literal of the declared address family.
	ip := net.ParseIP(host)
	if ip == nil {
		return "", "", fmt.Errorf("invalid bootnode multiaddr %q: host %q is not a valid IP literal", addr, host)
	}
	if isIPv6 {
		if ip.To4() != nil {
			return "", "", fmt.Errorf("invalid bootnode multiaddr %q: /ip6/ prefix requires an IPv6 address, got IPv4 %q", addr, host)
		}
	} else {
		if ip.To4() == nil {
			return "", "", fmt.Errorf("invalid bootnode multiaddr %q: /ip4/ prefix requires an IPv4 address, got %q", addr, host)
		}
	}

	// Validate port is a strict decimal integer in [1, 65535].
	// net.LookupPort also accepts named services (e.g. "http") — reject those.
	portNum, convErr := strconv.Atoi(port)
	if convErr != nil || portNum < 1 || portNum > 65535 {
		return "", "", fmt.Errorf("invalid bootnode multiaddr %q: port %q must be a decimal integer in [1, 65535]", addr, port)
	}

	return host, port, nil
}

// ResolveBootnode is the exported form of resolveBootnode; callers outside the
// package can use it for dial-time resolution (DNS included).
func ResolveBootnode(addr string) ([]string, error) {
	return resolveBootnode(addr)
}

// NormalizeBootnodeAddr validates the syntax of a bootnode address and
// normalises multiaddr format (/ip4/ or /ip6/) to a plain host:port string
// without performing any DNS lookup.
//
// Returns an error for malformed multiaddrs — missing /tcp/<port> component,
// a host that is not a valid IP literal of the declared family, an empty host,
// or a port that is not a decimal integer in [1, 65535]. Plain host:port
// addresses — including DNS hostnames like "bootnode.aperod.com:30303" — are
// validated for structure and returned unchanged so the P2P host can retain
// the hostname for periodic re-resolution.
//
// Use this at startup to surface configuration mistakes early with a clear,
// actionable log message. Use resolveBootnode (or ResolveBootnode) at dial
// time to obtain the actual IP:port for each attempt.
func NormalizeBootnodeAddr(addr string) (string, error) {
	if strings.HasPrefix(addr, "/ip4/") || strings.HasPrefix(addr, "/ip6/") {
		host, port, err := parseMultiaddr(addr)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(host, port), nil
	}
	// Plain host:port (IP literal or DNS name) — validate structure only.
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", fmt.Errorf("invalid bootnode address %q: %w", addr, err)
	}
	return addr, nil
}

// resolveBootnode resolves a bootnode address that may contain a DNS hostname
// instead of a raw IP literal.  Returns all resolved "ip:port" strings so the
// caller can dial each one independently.
//
// If addr is already an IP literal (v4 or v6) it is returned as-is without
// any DNS lookup.
//
// Multiaddr format (e.g. "/ip4/1.2.3.4/tcp/30303" or "/ip6/::1/tcp/30303") is
// automatically normalised to "host:port" so operators can use either syntax in
// node.yaml without the dial failing with an "invalid address" error.
//
// Examples:
//
//	"bootnode.aperod.com:30303"    → ["1.2.3.4:30303", "5.6.7.8:30303"]
//	"192.168.1.1:30303"            → ["192.168.1.1:30303"]
//	"[::1]:30303"                  → ["[::1]:30303"]
//	"/ip4/192.168.1.1/tcp/30303"   → ["192.168.1.1:30303"]
//	"/ip6/::1/tcp/30303"           → ["[::1]:30303"]
func resolveBootnode(addr string) ([]string, error) {
	// Normalise multiaddr format: /ip4/<host>/tcp/<port> or /ip6/<host>/tcp/<port>
	if strings.HasPrefix(addr, "/ip4/") || strings.HasPrefix(addr, "/ip6/") {
		host, port, err := parseMultiaddr(addr)
		if err != nil {
			return nil, err
		}
		addr = net.JoinHostPort(host, port)
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid bootnode address %q: %w", addr, err)
	}
	if net.ParseIP(host) != nil {
		// Already an IP literal — no DNS lookup required.
		return []string{addr}, nil
	}
	// Domain name — look up all A/AAAA records.
	ips, err := net.LookupHost(host)
	if err != nil {
		return nil, fmt.Errorf("dns lookup %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("dns lookup %s: no records returned", host)
	}
	addrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, net.JoinHostPort(ip, port))
	}
	return addrs, nil
}
