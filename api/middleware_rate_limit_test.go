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
