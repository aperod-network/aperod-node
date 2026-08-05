package p2p

import (
	"fmt"
	"net"
	"strings"
)

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
		// Strip the leading "/ip4/" or "/ip6/" prefix and split on "/tcp/"
		stripped := addr[5:] // remove "/ip4/" or "/ip6/"
		parts := strings.SplitN(stripped, "/tcp/", 2)
		if len(parts) == 2 {
			host := parts[0]
			port := parts[1]
			// Validate port is numeric
			if _, err := net.LookupPort("tcp", port); err == nil {
				addr = net.JoinHostPort(host, port)
			}
		}
		// If parsing failed, fall through and let SplitHostPort handle/reject it.
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not a host:port pair — pass through and let the dialer handle it.
		return []string{addr}, nil
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
