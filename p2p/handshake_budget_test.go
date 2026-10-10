package p2p

import (
	"sync"
	"testing"
)

func TestInboundHandshakeBudgetBySource(t *testing.T) {
	h := &Host{cfg: Config{MaxPendingHandshakes: 20, MaxPeersPerIP: 3}}
	for i := 0; i < 3; i++ {
		if !h.reserveInboundHandshake("one") {
			t.Fatal("available source reservation denied")
		}
	}
	for i := 0; i < 30; i++ {
		if h.reserveInboundHandshake("one") {
			t.Fatal("source quota exceeded")
		}
	}
	if !h.reserveInboundHandshake("two") {
		t.Fatal("independent source denied")
	}
	h.releaseInboundHandshake("two")
	for i := 0; i < 3; i++ {
		h.releaseInboundHandshake("one")
	}
	h.releaseInboundHandshake("one")
	if h.PendingHandshakes() != 0 || len(h.pendingHandshakeIPs) != 0 {
		t.Fatal("reservation state retained after release")
	}
}

func TestInboundHandshakeBudgetConcurrent(t *testing.T) {
	h := &Host{cfg: Config{MaxPendingHandshakes: 4}}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.reserveInboundHandshake("source")
		}()
	}
	wg.Wait()
	if h.PendingHandshakes() != 2 {
		t.Fatalf("one source used %d reservations", h.PendingHandshakes())
	}
	if !h.reserveInboundHandshake("other") || !h.reserveInboundHandshake("other") {
		t.Fatal("reserved capacity unavailable to independent source")
	}
	if h.reserveInboundHandshake("third") {
		t.Fatal("global quota exceeded")
	}
	for _, ip := range []string{"source", "other"} {
		h.releaseInboundHandshake(ip)
		h.releaseInboundHandshake(ip)
	}
	if h.PendingHandshakes() != 0 {
		t.Fatal("global reservation leak")
	}
}
