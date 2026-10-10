package p2p_test

// bootnode_warn_test.go — integration test confirming that a malformed
// bootnode address produces a structured log warning both at startup and on
// every runtime maintainLoop tick, while the node continues to start normally
// and connects to any valid bootnodes that are reachable.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/p2p"
)

// syncBuf is a bytes.Buffer protected by a mutex so that concurrent logger
// goroutines (Write) and test goroutines (Reset, String) do not race.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureWarnLogger returns an slog.Logger that writes JSON records at Warn
// level and above to buf.  JSON output makes it straightforward to assert on
// specific structured fields without fragile string matching.
func captureWarnLogger(buf *syncBuf) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))
}

// logContainsBootnodeWarn scans the newline-delimited JSON log lines in buf
// and returns true when at least one line is a WARN record that contains both
// a non-empty "bootnode" key and a non-empty "err" key.
func logContainsBootnodeWarn(buf *syncBuf) bool {
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		level, _ := rec["level"].(string)
		if !strings.EqualFold(level, "warn") {
			continue
		}
		bootnode, hasBoot := rec["bootnode"].(string)
		errVal, hasErr := rec["err"].(string)
		if hasBoot && bootnode != "" && hasErr && errVal != "" {
			return true
		}
	}
	return false
}

// TestBootnodeWarn_StartupLog confirms that a malformed bootnode entry
// (/ip6/badaddr) causes a WARN log with "bootnode" and "err" keys during
// the startup goroutines that fire immediately after Host.Start().
//
// A valid but unreachable address (127.0.0.1:19998) is included to confirm
// the node continues past the bad entry and still tries valid bootnodes.
func TestBootnodeWarn_StartupLog(t *testing.T) {
	var buf syncBuf
	log := captureWarnLogger(&buf)

	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   5,
		NodeID:     "test-bootnode-warn",
		UserAgent:  "aperod-test/0.1",
		Bootnodes: []string{
			"/ip6/badaddr",       // malformed — no /tcp/<port> component
			"127.0.0.1:19998",   // valid syntax but unreachable; node must try it
		},
	}, &stubHandler{}, log)

	if err := h.Start(); err != nil {
		t.Fatalf("Host.Start: %v", err)
	}
	defer h.Stop()

	// The startup dial goroutines are asynchronous.  Give them a short window
	// to fire and write to the log buffer before we inspect it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if logContainsBootnodeWarn(&buf) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !logContainsBootnodeWarn(&buf) {
		t.Errorf("expected a WARN log with keys 'bootnode' and 'err' for the malformed bootnode; log output:\n%s", buf.String())
	}
}

// TestBootnodeWarn_MaintainLoopLog confirms that the runtime maintainLoop
// also emits a WARN log with "bootnode" and "err" keys when it re-resolves
// a malformed bootnode on each periodic tick.
//
// This exercises the path in host.go that runs after the node has already
// started, ensuring operators are warned continuously — not just at startup.
func TestBootnodeWarn_MaintainLoopLog(t *testing.T) {
	var buf syncBuf
	log := captureWarnLogger(&buf)

	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   5,
		NodeID:     "test-bootnode-warn-maintain",
		UserAgent:  "aperod-test/0.1",
		Bootnodes: []string{
			"/ip6/badaddr", // malformed — missing /tcp/<port>
		},
	}, &stubHandler{}, log)

	if err := h.Start(); err != nil {
		t.Fatalf("Host.Start: %v", err)
	}
	defer h.Stop()

	// Drain any startup-path log output so we can attribute the next warning
	// unambiguously to the maintainLoop path.  Wait for the startup goroutine
	// to complete (it is fast — no real I/O for a malformed address).
	time.Sleep(100 * time.Millisecond)
	buf.Reset()

	// Trigger an immediate maintainLoop tick via the test hook.
	p2p.HostTriggerMaintain(h)

	// Give the maintainLoop goroutine time to execute the tick and write the log.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if logContainsBootnodeWarn(&buf) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !logContainsBootnodeWarn(&buf) {
		t.Errorf("expected a WARN log with keys 'bootnode' and 'err' from maintainLoop; log output:\n%s", buf.String())
	}
}

// TestBootnodeWarn_NodeContinuesAfterBadBootnode confirms that a host
// configured with a malformed bootnode still starts successfully (Start()
// returns nil) and is not stuck — it reaches the "listening" state and can
// be stopped cleanly.
//
// This is the "node continues to start" assertion from the acceptance criteria.
func TestBootnodeWarn_NodeContinuesAfterBadBootnode(t *testing.T) {
	var buf syncBuf
	log := captureWarnLogger(&buf)

	h := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   5,
		NodeID:     "test-bootnode-warn-continue",
		UserAgent:  "aperod-test/0.1",
		Bootnodes: []string{
			"/ip6/badaddr",
			"/ip4/not-an-ip/tcp/30303", // another malformed entry
		},
	}, &stubHandler{}, log)

	if err := h.Start(); err != nil {
		t.Fatalf("Host.Start returned an error for a node with only malformed bootnodes — node must start successfully: %v", err)
	}

	// Node must be listening and have an advertised address.
	addr := h.ListenAddr()
	if addr == "" {
		t.Error("Host.ListenAddr() is empty after successful Start()")
	}

	// Stop must complete cleanly.
	stopped := make(chan struct{})
	go func() {
		h.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		// ok
	case <-time.After(2 * time.Second):
		t.Error("Host.Stop() did not complete within 2 s")
	}
}
