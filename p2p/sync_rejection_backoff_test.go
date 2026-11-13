package p2p

import (
	"testing"
	"time"
)

func TestPeerSyncRejectBackoffIsBoundedAndResets(t *testing.T) {
	peer := &Peer{}
	now := time.Unix(1_000, 0)
	want := []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second, 48 * time.Second, 48 * time.Second}

	for i, expected := range want {
		delay, shouldLog := peer.scheduleSyncRejectRetry(now)
		if !shouldLog {
			t.Fatalf("attempt %d was not recorded as a new rejection", i+1)
		}
		if delay != expected {
			t.Fatalf("attempt %d backoff = %s, want %s", i+1, delay, expected)
		}
		now = now.Add(delay)
	}

	peer.resetSyncRejectRetry()
	delay, shouldLog := peer.scheduleSyncRejectRetry(now)
	if !shouldLog || delay != 3*time.Second {
		t.Fatalf("backoff did not reset: delay=%s shouldLog=%t", delay, shouldLog)
	}
}

func TestPeerSyncRejectBackoffDoesNotExtendOrRelogDuringCooldown(t *testing.T) {
	peer := &Peer{}
	now := time.Unix(2_000, 0)
	if delay, shouldLog := peer.scheduleSyncRejectRetry(now); !shouldLog || delay != 3*time.Second {
		t.Fatalf("first rejection: delay=%s shouldLog=%t", delay, shouldLog)
	}

	delay, shouldLog := peer.scheduleSyncRejectRetry(now.Add(time.Second))
	if shouldLog || delay != 2*time.Second {
		t.Fatalf("rejection during cooldown: delay=%s shouldLog=%t, want 2s and no duplicate log", delay, shouldLog)
	}
}
