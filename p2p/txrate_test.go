package p2p

// Unit tests for the per-source-IP transaction rate limiter (Task: prevent
// a slow mempool flood from degrading block-production time).

import (
	"fmt"
	"testing"
	"time"
)

// newTestLimiter returns a limiter with a controllable clock.
func newTestLimiter(burst, sustained, banAfter int) (*txRateLimiter, *time.Time) {
	l := newTxRateLimiter(burst, sustained, banAfter)
	now := time.Unix(1_700_000_000, 0)
	l.nowFunc = func() time.Time { return now }
	return l, &now
}

func TestTxRate_DisabledWhenZero(t *testing.T) {
	if l := newTxRateLimiter(0, 10, 100); l != nil {
		t.Errorf("burst=0 should return nil limiter (disabled), got %+v", l)
	}
	if l := newTxRateLimiter(50, 0, 100); l != nil {
		t.Errorf("sustained=0 should return nil limiter (disabled), got %+v", l)
	}
}

func TestTxRate_BurstThenThrottle(t *testing.T) {
	l, _ := newTestLimiter(5, 1, 0)
	for i := 0; i < 5; i++ {
		allowed, _, _ := l.allow("1.2.3.4")
		if !allowed {
			t.Fatalf("tx %d within burst of 5 was throttled", i+1)
		}
	}
	allowed, _, violations := l.allow("1.2.3.4")
	if allowed {
		t.Fatal("6th back-to-back tx should be throttled (burst=5)")
	}
	if violations != 1 {
		t.Errorf("violations = %d, want 1", violations)
	}
}

func TestTxRate_SustainedRefill(t *testing.T) {
	l, now := newTestLimiter(5, 2, 0) // 2 tx/sec sustained
	for i := 0; i < 5; i++ {
		l.allow("1.2.3.4")
	}
	if allowed, _, _ := l.allow("1.2.3.4"); allowed {
		t.Fatal("bucket should be empty after burst")
	}
	// 1 second later → 2 tokens refilled.
	*now = now.Add(time.Second)
	for i := 0; i < 2; i++ {
		if allowed, _, _ := l.allow("1.2.3.4"); !allowed {
			t.Fatalf("refilled tx %d should be allowed (2 tx/sec sustained)", i+1)
		}
	}
	if allowed, _, _ := l.allow("1.2.3.4"); allowed {
		t.Fatal("3rd tx in the same second should be throttled")
	}
	// Refill never exceeds burst capacity.
	*now = now.Add(time.Hour)
	for i := 0; i < 5; i++ {
		if allowed, _, _ := l.allow("1.2.3.4"); !allowed {
			t.Fatalf("tx %d after long idle should be allowed (full burst)", i+1)
		}
	}
	if allowed, _, _ := l.allow("1.2.3.4"); allowed {
		t.Fatal("tokens must be capped at burst — 6th tx should be throttled")
	}
}

func TestTxRate_PerIPIndependence(t *testing.T) {
	l, _ := newTestLimiter(2, 1, 0)
	l.allow("1.1.1.1")
	l.allow("1.1.1.1")
	if allowed, _, _ := l.allow("1.1.1.1"); allowed {
		t.Fatal("1.1.1.1 should be throttled")
	}
	if allowed, _, _ := l.allow("2.2.2.2"); !allowed {
		t.Fatal("2.2.2.2 must not be affected by 1.1.1.1's flood")
	}
}

func TestTxRate_BanThresholdFiresExactlyOnce(t *testing.T) {
	l, _ := newTestLimiter(1, 1, 3)
	l.allow("9.9.9.9") // consume the single token
	for i := 1; i <= 5; i++ {
		_, banNow, violations := l.allow("9.9.9.9")
		if violations != i {
			t.Errorf("violation %d: counter = %d", i, violations)
		}
		if wantBan := i == 3; banNow != wantBan {
			t.Errorf("violation %d: banNow = %v, want %v", i, banNow, wantBan)
		}
	}
}

func TestTxRate_ViolationsResetWhenBackUnderLimit(t *testing.T) {
	l, now := newTestLimiter(1, 1, 5)
	l.allow("9.9.9.9")
	l.allow("9.9.9.9") // violation 1
	l.allow("9.9.9.9") // violation 2

	// Peer slows down: a second later one token is available again.
	*now = now.Add(time.Second)
	if allowed, _, _ := l.allow("9.9.9.9"); !allowed {
		t.Fatal("tx after refill should be allowed")
	}
	// Counter must have been reset — next violation starts from 1.
	_, _, violations := l.allow("9.9.9.9")
	if violations != 1 {
		t.Errorf("violations after back-under-limit reset = %d, want 1", violations)
	}
}

func TestTxRate_StaleEntryStartsFresh(t *testing.T) {
	l, now := newTestLimiter(2, 1, 3)
	l.allow("5.5.5.5")
	l.allow("5.5.5.5")
	l.allow("5.5.5.5") // violation 1
	l.allow("5.5.5.5") // violation 2

	*now = now.Add(txRateEntryTTL + time.Minute)
	// Long-dormant IP: full burst restored, violations wiped.
	for i := 0; i < 2; i++ {
		if allowed, _, _ := l.allow("5.5.5.5"); !allowed {
			t.Fatalf("tx %d after TTL expiry should be allowed (fresh bucket)", i+1)
		}
	}
	_, banNow, violations := l.allow("5.5.5.5")
	if violations != 1 || banNow {
		t.Errorf("after TTL expiry violations = %d banNow = %v, want 1/false", violations, banNow)
	}
}

func TestTxRate_ForgetClearsState(t *testing.T) {
	l, _ := newTestLimiter(1, 1, 2)
	l.allow("7.7.7.7")
	l.allow("7.7.7.7") // violation 1
	l.forget("7.7.7.7")
	if allowed, _, _ := l.allow("7.7.7.7"); !allowed {
		t.Fatal("after forget the IP should start with a fresh full bucket")
	}
}

func TestTxRate_MapCapFailsOpenAndEvictsStale(t *testing.T) {
	l, now := newTestLimiter(1, 1, 0)
	// Fill the map to the cap.
	for i := 0; i < txRateMaxTrackedIPs; i++ {
		l.allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	if len(l.buckets) != txRateMaxTrackedIPs {
		t.Fatalf("tracked = %d, want %d", len(l.buckets), txRateMaxTrackedIPs)
	}
	// A new IP cannot be tracked (all entries fresh) → fail open, not tracked.
	allowed, _, _ := l.allow("99.99.99.99")
	if !allowed {
		t.Fatal("untracked IP at map cap must fail open (allowed)")
	}
	if _, ok := l.buckets["99.99.99.99"]; ok {
		t.Fatal("IP must not be tracked while map is at cap")
	}
	// Once the old entries go stale they are evicted and tracking resumes.
	*now = now.Add(txRateEntryTTL + time.Minute)
	l.allow("99.99.99.99")
	if _, ok := l.buckets["99.99.99.99"]; !ok {
		t.Fatal("stale entries should have been evicted, freeing room to track")
	}
	if len(l.buckets) != 1 {
		t.Errorf("tracked after eviction = %d, want 1", len(l.buckets))
	}
}
