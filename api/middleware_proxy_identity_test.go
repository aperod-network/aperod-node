package api

import (
	"net/http/httptest"
	"testing"
)

func TestRealIPTrustsOnlyImmediateProxyAppendedAddress(t *testing.T) {
	for _, tc := range []struct {
		name, remote, forwarded, want string
	}{
		{"direct cannot spoof", "198.51.100.10:1234", "203.0.113.1", "198.51.100.10"},
		{"local single address", "127.0.0.1:1234", "198.51.100.10", "198.51.100.10"},
		{"attacker supplied prefix", "127.0.0.1:1234", "203.0.113.1, 198.51.100.10", "198.51.100.10"},
		{"forged loopback prefix", "127.0.0.1:1234", "127.0.0.1, 198.51.100.10", "198.51.100.10"},
		{"local ipv6 proxy", "[::1]:1234", "203.0.113.1, 2001:db8::10", "2001:db8::10"},
		{"canonical ipv6", "[::1]:1234", "2001:0db8:0000::10", "2001:db8::10"},
		{"empty last hop fails closed", "127.0.0.1:1234", "198.51.100.10, ", "127.0.0.1"},
		{"malformed last hop fails closed", "127.0.0.1:1234", "198.51.100.10, invalid", "127.0.0.1"},
		{"hostname is not an IP", "127.0.0.1:1234", "attacker.example", "127.0.0.1"},
		{"no forwarded address", "127.0.0.1:1234", "", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("X-Forwarded-For", tc.forwarded)
			req.Header.Set("CF-Connecting-IP", "203.0.113.1")
			if got := realIP(req); got != tc.want {
				t.Fatalf("realIP = %q, want %q", got, tc.want)
			}
		})
	}
}
