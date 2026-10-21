package api

import (
	"fmt"
	"testing"
	"time"
)

func TestBoundedUTXOAddrCacheEvictsOldEntriesAtCapacity(t *testing.T) {
	var cache boundedUTXOAddrCache
	now := time.Now()
	for i := 0; i <= maxUTXOAddrCacheEntries; i++ {
		cache.store(fmt.Sprintf("key-%d", i), &utxoCacheEntry{
			body:      []byte("response"),
			expiresAt: now.Add(time.Minute + time.Duration(i)*time.Second),
		}, now)
	}

	if got := len(cache.entries); got != maxUTXOAddrCacheEntries {
		t.Fatalf("cached entries = %d, want at most %d", got, maxUTXOAddrCacheEntries)
	}
	if _, ok := cache.load("key-0", now); ok {
		t.Fatal("oldest cache entry was not evicted")
	}
	if _, ok := cache.load(fmt.Sprintf("key-%d", maxUTXOAddrCacheEntries), now); !ok {
		t.Fatal("newest cache entry was evicted")
	}
}

func TestBoundedUTXOAddrCacheDeletesExpiredEntries(t *testing.T) {
	var cache boundedUTXOAddrCache
	now := time.Now()
	cache.store("expired", &utxoCacheEntry{
		body:      []byte("response"),
		expiresAt: now.Add(-time.Second),
	}, now.Add(-2*time.Second))

	// A cache entry that is no longer valid is never returned; expired entries
	// are also removed while a later write sweeps the map.
	cache.store("live", &utxoCacheEntry{
		body:      []byte("response"),
		expiresAt: now.Add(time.Minute),
	}, now)
	if _, ok := cache.load("expired", now); ok {
		t.Fatal("expired entry was returned")
	}
	if _, ok := cache.load("live", now); !ok {
		t.Fatal("live entry was not retained")
	}
}
