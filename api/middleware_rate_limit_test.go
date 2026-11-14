package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiter429IncludesRetryAfterAndDoesNotExemptLoopback(t *testing.T) {
	rl := NewRateLimiter()
	bucket := rl.bucket("127.0.0.1")
	bucket.mu.Lock()
	bucket.tokens = 0
	bucket.lastFill = time.Now()
	bucket.mu.Unlock()

	nextCalled := false
	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "http://node.local/api/v1/blocks?limit=1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("loopback status = %d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if nextCalled {
		t.Fatal("rate-limited loopback request reached the handler")
	}
}

func TestHeavyRateLimiter429IncludesRetryAfter(t *testing.T) {
	const ip = "198.51.100.251"
	globalHeavyRL.mu.Lock()
	globalHeavyRL.buckets[ip] = &tokenBucket{tokens: 0, lastFill: time.Now()}
	globalHeavyRL.mu.Unlock()
	t.Cleanup(func() {
		globalHeavyRL.mu.Lock()
		delete(globalHeavyRL.buckets, ip)
		globalHeavyRL.mu.Unlock()
	})

	handler := NewRateLimiter().Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "http://node.local/api/v1/address/aproExample/transactions", nil)
	req.RemoteAddr = ip + ":12345"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("heavy endpoint status = %d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
}

func TestRateLimiterBucketMapsHaveHardBounds(t *testing.T) {
	rlOverflow := &tokenBucket{tokens: rlBurst, lastFill: time.Now()}
	rl := &RateLimiter{
		buckets:    make(map[string]*tokenBucket),
		maxBuckets: 2,
		overflow:   rlOverflow,
	}
	rl.bucket("198.51.100.1")
	rl.bucket("198.51.100.2")
	if got := len(rl.buckets); got != 2 {
		t.Fatalf("general buckets = %d, want 2", got)
	}
	if got := rl.bucket("198.51.100.3"); got != rlOverflow {
		t.Fatal("new IP after the general cap did not use the shared overflow bucket")
	}
	if got := rl.bucket("198.51.100.4"); got != rlOverflow {
		t.Fatal("overflow IPs did not share the bounded fallback bucket")
	}
	if got := len(rl.buckets); got != 2 {
		t.Fatalf("general buckets grew past cap: %d", got)
	}

	heavyOverflow := &tokenBucket{tokens: heavyBurst, lastFill: time.Now()}
	heavy := &heavyRL{
		buckets:    make(map[string]*tokenBucket),
		maxBuckets: 2,
		overflow:   heavyOverflow,
	}
	heavy.bucket("198.51.100.11")
	heavy.bucket("198.51.100.12")
	if got := len(heavy.buckets); got != 2 {
		t.Fatalf("heavy buckets = %d, want 2", got)
	}
	if got := heavy.bucket("198.51.100.13"); got != heavyOverflow {
		t.Fatal("new IP after the heavy cap did not use the shared overflow bucket")
	}
	if got := heavy.bucket("198.51.100.14"); got != heavyOverflow {
		t.Fatal("heavy overflow IPs did not share the bounded fallback bucket")
	}
	if got := len(heavy.buckets); got != 2 {
		t.Fatalf("heavy buckets grew past cap: %d", got)
	}
}
