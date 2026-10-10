package p2p

import (
	"sync"
	"time"
)

const (
	// txRateMaxTrackedIPs caps the number of source IPs tracked by the
	// transaction rate limiter so that a distributed attacker cannot
	// exhaust node memory by flooding transactions from millions of
	// spoofed-looking source addresses.  When the cap is reached, new
	// IPs are not tracked (fail-open) — established, tracked flooders
	// are still throttled.
	txRateMaxTrackedIPs = 10_000

	// txRateEntryTTL is how long a bucket may sit idle before it is
	// considered stale.  Stale entries are evicted lazily on the next
	// map insertion, and a stale entry's violation counter starts from
	// zero — a long-dormant IP is not banned for last week's burst.
	txRateEntryTTL = 10 * time.Minute
)

// txRateBucket is the per-source-IP token-bucket state.
type txRateBucket struct {
	tokens     float64   // current token balance, capped at burst
	lastRefill time.Time // last time tokens were added
	violations int       // consecutive throttled submissions
	lastSeen   time.Time // last activity, for TTL eviction
}

// txRateLimiter enforces a per-source-IP token bucket on incoming P2P
// transactions.  Each IP may burst up to `burst` transactions
// back-to-back and then sustain `sustained` transactions per second.
// Every throttled transaction increments the IP's violation counter;
// when it reaches `banAfter` the caller is told to ban the peer.
//
// All methods are safe for concurrent use.
type txRateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*txRateBucket
	burst     float64
	sustained float64 // tokens added per second
	banAfter  int     // violations that trigger a ban; 0 = never ban
	nowFunc   func() time.Time
}

// newTxRateLimiter returns a limiter with the given burst capacity,
// sustained rate (tx/sec) and ban threshold.  Returns nil when burst
// or sustained is <= 0, which callers treat as "rate limiting disabled".
func newTxRateLimiter(burst, sustained, banAfter int) *txRateLimiter {
	if burst <= 0 || sustained <= 0 {
		return nil
	}
	return &txRateLimiter{
		buckets:   make(map[string]*txRateBucket),
		burst:     float64(burst),
		sustained: float64(sustained),
		banAfter:  banAfter,
		nowFunc:   time.Now,
	}
}

// allow records one transaction submission from ip and reports whether
// it may be processed.
//
// Returns:
//   - allowed:    true when the tx should be processed and relayed.
//   - banNow:     true exactly once, when the violation counter crosses
//     the ban threshold — the caller should ban the IP.
//   - violations: the IP's current violation count (for logging).
func (l *txRateLimiter) allow(ip string) (allowed, banNow bool, violations int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.nowFunc()
	b, ok := l.buckets[ip]
	if ok && now.Sub(b.lastSeen) > txRateEntryTTL {
		// Stale entry: start fresh (full bucket, zero violations).
		ok = false
	}
	if !ok {
		if len(l.buckets) >= txRateMaxTrackedIPs {
			l.evictStale(now)
		}
		if len(l.buckets) >= txRateMaxTrackedIPs {
			// Map still full after eviction: fail open for this IP
			// rather than letting an attacker evict tracked flooders.
			return true, false, 0
		}
		b = &txRateBucket{tokens: l.burst, lastRefill: now}
		l.buckets[ip] = b
	}

	// Refill.
	if elapsed := now.Sub(b.lastRefill).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.sustained
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.lastRefill = now
	}
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		// A peer that returns below the limit is no longer a
		// "persistent violator": reset the counter so only sustained
		// flooding accumulates toward a ban.
		b.violations = 0
		return true, false, 0
	}

	b.violations++
	banNow = l.banAfter > 0 && b.violations == l.banAfter
	return false, banNow, b.violations
}

// forget removes the entry for ip (called after the IP is banned so a
// post-ban reconnect starts with a clean slate, mirroring the
// badBlockCounts behaviour).
func (l *txRateLimiter) forget(ip string) {
	l.mu.Lock()
	delete(l.buckets, ip)
	l.mu.Unlock()
}

// evictStale removes all entries idle longer than txRateEntryTTL.
// Caller must hold l.mu.
func (l *txRateLimiter) evictStale(now time.Time) {
	for ip, b := range l.buckets {
		if now.Sub(b.lastSeen) > txRateEntryTTL {
			delete(l.buckets, ip)
		}
	}
}
