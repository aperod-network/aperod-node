package p2p

import (
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
)

func gossipTestHash(value uint64) crypto.Hash32 {
	var hash crypto.Hash32
	for i := range 8 {
		hash[i] = byte(value >> (8 * i))
	}
	return hash
}

func TestGossipFilterHardBoundEvictsOldest(t *testing.T) {
	filter := NewGossipFilter()
	oldest := gossipTestHash(0)

	for i := uint64(0); i < gossipCacheSize+256; i++ {
		if !filter.MarkAndCheck(gossipTestHash(i)) {
			t.Fatalf("new hash %d was marked as a duplicate", i)
		}
		if got := filter.Size(); got > gossipCacheSize {
			t.Fatalf("cache grew to %d entries, hard limit is %d", got, gossipCacheSize)
		}
	}
	if got := filter.Size(); got != gossipCacheSize {
		t.Fatalf("cache size = %d, want %d", got, gossipCacheSize)
	}
	if filter.HasSeen(oldest) {
		t.Fatal("oldest entry should have been evicted")
	}
	if !filter.HasSeen(gossipTestHash(gossipCacheSize + 255)) {
		t.Fatal("newest entry should still be retained")
	}
	if !filter.MarkAndCheck(oldest) {
		t.Fatal("evicted hash should be accepted as new")
	}
	if got := filter.Size(); got != gossipCacheSize {
		t.Fatalf("cache size after replacement = %d, want %d", got, gossipCacheSize)
	}
}

func TestGossipFilterExpiresEntriesFromOldestFirst(t *testing.T) {
	filter := NewGossipFilter()
	expired := gossipTestHash(1)
	live := gossipTestHash(2)
	filter.MarkAndCheck(expired)
	filter.MarkAndCheck(live)

	filter.mu.Lock()
	filter.order.Front().Value = gossipEntry{
		hash: expired,
		at:   time.Now().Add(-gossipCacheTTL - time.Second),
	}
	filter.mu.Unlock()

	if filter.HasSeen(expired) {
		t.Fatal("expired entry should be removed when checking the filter")
	}
	if !filter.HasSeen(live) {
		t.Fatal("live entry should remain after expiration cleanup")
	}
	if !filter.MarkAndCheck(expired) {
		t.Fatal("expired hash should be accepted again")
	}
	if got := filter.Size(); got != 2 {
		t.Fatalf("cache size = %d, want 2", got)
	}
}
