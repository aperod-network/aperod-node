package api

// Middleware for the Aperod API server:
//   - Per-IP rate limiting  (2.5.1): token bucket, 100 req/s, burst 200
//   - API key auth          (2.5.2): X-API-Key header for write operations
//   - CORS                  (2.5.3): configurable allowed origins
//   - WS client cap         (2.2.5): max 1000 concurrent WebSocket connections

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// ─── Rate Limiter ─────────────────────────────────────────────────────────────

const (
	rlRate  = 100 // tokens per second
	rlBurst = 200 // bucket capacity
)

type tokenBucket struct {
	tokens   float64
	lastFill time.Time
	mu       sync.Mutex
}

func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.lastFill).Seconds()
	tb.lastFill = now
	tb.tokens += elapsed * rlRate
	if tb.tokens > rlBurst {
		tb.tokens = rlBurst
	}
	if tb.tokens < 1 {
		return false
	}
	tb.tokens--
	return true
}

// RateLimiter holds per-IP token buckets and prunes stale entries every minute.
type RateLimiter struct {
	mu      sync.RWMutex
	buckets map[string]*tokenBucket
}

func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{buckets: make(map[string]*tokenBucket)}
	go rl.prune()
	return rl
}

func (rl *RateLimiter) prune() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-2 * time.Minute)
		rl.mu.Lock()
		for ip, tb := range rl.buckets {
			tb.mu.Lock()
			stale := tb.lastFill.Before(cutoff)
			tb.mu.Unlock()
			if stale {
				delete(rl.buckets, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) bucket(ip string) *tokenBucket {
	rl.mu.RLock()
	tb, ok := rl.buckets[ip]
	rl.mu.RUnlock()
	if ok {
		return tb
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if tb, ok = rl.buckets[ip]; ok {
		return tb
	}
	tb = &tokenBucket{tokens: rlBurst, lastFill: time.Now()}
	rl.buckets[ip] = tb
	return tb
}

// Middleware returns an http.Handler that enforces the rate limit.
// rateLimitExempt lists paths that bypass the per-IP token bucket entirely.
// Use sparingly — only for internal probes that must never be throttled.
var rateLimitExempt = map[string]bool{
	// Watchdog liveness probe: fired every 60 s from localhost by the
	// aperod-node-watchdog.timer systemd unit.  Exempting it ensures the
	// watchdog never triggers a spurious rate-limit 429, which would cause a
	// false-positive node restart.
	"/api/v1/status": true,
}

func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rateLimitExempt[r.URL.Path] {
			ip := realIP(r)
			if !rl.bucket(ip).allow() {
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// realIP extracts the real client IP.
// X-Forwarded-For is only trusted when the request comes from the loopback
// interface (127.0.0.1 / ::1), i.e. from a local reverse proxy.  Accepting
// XFF unconditionally lets any client spoof its IP and bypass rate-limiting.
func realIP(r *http.Request) string {
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if remoteIP == "" {
		remoteIP = r.RemoteAddr
	}
	// Only honour X-Forwarded-For from a trusted local proxy.
	if remoteIP == "127.0.0.1" || remoteIP == "::1" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Take only the first (leftmost) address.
			for idx := 0; idx < len(xff); idx++ {
				if xff[idx] == ',' {
					xff = xff[:idx]
					break
				}
			}
			xff = trimSpace(xff)
			if xff != "" {
				return xff
			}
		}
	}
	return remoteIP
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// ─── API Key Auth ──────────────────────────────────────────────────────────────

// APIKeyMiddleware requires X-API-Key to match key for the wrapped handler.
// Used for write-operation endpoints (sendRawTransaction).
func APIKeyMiddleware(key string, next http.HandlerFunc) http.HandlerFunc {
	if key == "" {
		// No key configured — allow all (dev mode)
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != key {
			http.Error(w, `{"error":"unauthorized: missing or invalid X-API-Key"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// ─── CORS ─────────────────────────────────────────────────────────────────────

// CORSConfig holds allowed origins. Empty slice means all origins ("*").
type CORSConfig struct {
	AllowedOrigins []string // e.g. ["https://explorer.aperod.io"]
}

// Middleware adds CORS headers and handles preflight OPTIONS requests.
func (c CORSConfig) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := ""
		if len(c.AllowedOrigins) == 0 {
			allowed = "*"
		} else {
			for _, o := range c.AllowedOrigins {
				if o == origin {
					allowed = origin
					break
				}
			}
		}
		if allowed != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowed)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── WS client cap ────────────────────────────────────────────────────────────

const MaxWSClients = 1000

// CanAcceptClient reports whether the hub has capacity for another WS client.
func (h *Hub) CanAcceptClient() bool {
	return h.ClientCount() < MaxWSClients
}
