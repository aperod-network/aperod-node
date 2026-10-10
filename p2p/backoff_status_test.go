// backoff_status_test.go — internal tests for Host.ReconnectBackoffActive,
// the health flag that lets external monitoring detect the "relay silently
// stuck: only known peer unreachable, dial back-off active" state.
package p2p

import (
	"testing"
	"time"
)

// newBackoffTestHost builds a minimal Host with just the fields the
// bootnode back-off bookkeeping touches.
func newBackoffTestHost(maxBackoff time.Duration) *Host {
	return &Host{
		cfg:               Config{MaxDialBackoff: maxBackoff},
		bootnodeFailState: make(map[string]bootnodeFailEntry),
	}
}

func TestReconnectBackoffActive_FalseWhenNoFailures(t *testing.T) {
	h := newBackoffTestHost(5 * time.Minute)
	if h.ReconnectBackoffActive() {
		t.Error("ReconnectBackoffActive() = true with no recorded failures, want false")
	}
}

func TestReconnectBackoffActive_TrueAfterDialFailure(t *testing.T) {
	h := newBackoffTestHost(5 * time.Minute)
	h.recordBootnodeFail("10.0.0.1:30303")
	if !h.ReconnectBackoffActive() {
		t.Error("ReconnectBackoffActive() = false right after a dial failure, want true (5s back-off window)")
	}
}

func TestReconnectBackoffActive_FalseAfterClear(t *testing.T) {
	h := newBackoffTestHost(5 * time.Minute)
	h.recordBootnodeFail("10.0.0.1:30303")
	h.clearBootnodeFail("10.0.0.1:30303")
	if h.ReconnectBackoffActive() {
		t.Error("ReconnectBackoffActive() = true after clearBootnodeFail, want false")
	}
}

func TestReconnectBackoffActive_FalseAfterWindowExpires(t *testing.T) {
	h := newBackoffTestHost(5 * time.Minute)
	// Simulate an expired back-off window directly.
	h.bootnodeMu.Lock()
	h.bootnodeFailState["10.0.0.1:30303"] = bootnodeFailEntry{
		failures: 3,
		nextDial: time.Now().Add(-time.Second),
	}
	h.bootnodeMu.Unlock()
	if h.ReconnectBackoffActive() {
		t.Error("ReconnectBackoffActive() = true after nextDial passed, want false")
	}
}
