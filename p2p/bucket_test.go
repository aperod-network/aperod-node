package p2p

// Unit tests for peerTokenBucket.  These are in package p2p (not p2p_test)
// so they can access the unexported type directly without an export shim.

import (
	"testing"
	"time"
)

// TestPeerTokenBucket_DisabledWhenZeroRate verifies that a zero-rate bucket
// never sleeps and always returns 0.
func TestPeerTokenBucket_DisabledWhenZeroRate(t *testing.T) {
	b := peerTokenBucket{rate: 0, burst: 0, lastTime: time.Now()}
	for i := 0; i < 200; i++ {
		if d := b.wait(); d != 0 {
			t.Fatalf("zero-rate bucket should never sleep, got %v on call %d", d, i)
		}
	}
}

// TestPeerTokenBucket_DisabledWhenNegativeRate verifies that a negative-rate
// bucket (misconfiguration) is treated identically to disabled (rate = 0).
func TestPeerTokenBucket_DisabledWhenNegativeRate(t *testing.T) {
	b := peerTokenBucket{rate: -10, burst: 10, lastTime: time.Now()}
	for i := 0; i < 50; i++ {
		if d := b.wait(); d != 0 {
			t.Fatalf("negative-rate bucket should never sleep, got %v on call %d", d, i)
		}
	}
}

// TestPeerTokenBucket_BurstIsFree verifies that the first <burst> calls
// return immediately without sleeping when tokens start full.
func TestPeerTokenBucket_BurstIsFree(t *testing.T) {
	const rate = 10.0
	b := peerTokenBucket{
		tokens:   rate, // start full
		lastTime: time.Now(),
		rate:     rate,
		burst:    rate,
	}
	// Exactly <rate> tokens are available; all should be free.
	for i := 0; i < int(rate); i++ {
		if d := b.wait(); d != 0 {
			t.Fatalf("burst call %d should be free, got sleep %v", i, d)
		}
	}
}

// TestPeerTokenBucket_SustainedRate verifies that after the initial burst is
// exhausted, the effective throughput converges to the configured rate.
//
// Key correctness check: the token-bucket must NOT double-count the sleep
// duration as freshly-accrued tokens.  Before the bug fix, each sleeping call
// was immediately followed by a free call (the sleep itself refilled the
// token), producing pairs of blocks at 2× the configured rate.
func TestPeerTokenBucket_SustainedRate(t *testing.T) {
	// Use a high rate so the test completes quickly (< 1 s) while still being
	// sensitive enough to detect the double-credit bug.
	const rate = 200.0 // tokens/sec → one token every 5 ms
	const calls = 30   // calls after burst is spent; should take ≈150 ms

	b := peerTokenBucket{
		tokens:   rate, // start full (burst already spent by first <rate> calls)
		lastTime: time.Now(),
		rate:     rate,
		burst:    rate,
	}

	// Spend the initial burst instantly.
	for i := 0; i < int(rate); i++ {
		b.wait()
	}

	start := time.Now()
	for i := 0; i < calls; i++ {
		b.wait()
	}
	elapsed := time.Since(start)

	// Each post-burst call should take ≈ 1/rate seconds.
	// Allow a generous 50 % margin for scheduler jitter, but the lower bound
	// must still be above zero to confirm the bucket is actually throttling.
	minExpected := time.Duration(float64(calls)/rate*float64(time.Second)) / 2
	if elapsed < minExpected {
		t.Fatalf(
			"sustained rate too high: %d calls took %v, expected at least %v "+
				"(configured rate %v/s) — token bucket may be double-crediting sleep duration",
			calls, elapsed, minExpected, rate,
		)
	}

	// Upper bound: at most 3× the ideal time (guard against a runaway sleep loop).
	maxExpected := time.Duration(float64(calls)/rate*float64(time.Second)) * 3
	if elapsed > maxExpected {
		t.Fatalf(
			"sustained rate too low: %d calls took %v, expected at most %v "+
				"(configured rate %v/s)",
			calls, elapsed, maxExpected, rate,
		)
	}
}

// TestPeerTokenBucket_NoPairedBlocks is the targeted regression test for the
// double-credit bug.  After exhausting the burst the very next two calls must
// each sleep approximately 1/rate seconds — not one sleeping and one free.
func TestPeerTokenBucket_NoPairedBlocks(t *testing.T) {
	const rate = 100.0 // one token every 10 ms

	b := peerTokenBucket{
		tokens:   rate, // start full
		lastTime: time.Now(),
		rate:     rate,
		burst:    rate,
	}

	// Exhaust burst.
	for i := 0; i < int(rate); i++ {
		b.wait()
	}

	// First post-burst call should sleep.
	d1 := b.wait()
	// Second post-burst call must also sleep, not be free.
	d2 := b.wait()

	// 1/rate = 10 ms; allow down to 5 ms for scheduler jitter.
	minSleep := time.Duration(1.0/rate*float64(time.Second)) / 2
	if d1 < minSleep {
		t.Fatalf("first post-burst call did not sleep (got %v, want >= %v)", d1, minSleep)
	}
	if d2 < minSleep {
		t.Fatalf("second post-burst call was free (got %v, want >= %v) — "+
			"double-credit bug: sleep duration was counted as accrued tokens", d2, minSleep)
	}
}
