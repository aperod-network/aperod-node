package main

// TestMemWatchdogSavesSnapshot exercises the production memory-watchdog path
// end-to-end:
//
//  runMemoryWatchdogLoop (goroutine) → memPressureCh (signal loop) →
//  performShutdown → saveShutdownSnapshot → saveStartupSnapshot
//
// The test uses:
//   - a fake ticker channel (send ticks manually, no wall-clock wait)
//   - a fake RSS reader that always returns above-threshold values
//   - a fake consensus engine (stop / engineDone channels)
//
// This mirrors the approach in snapshot_restart_test.go (fake engine, real store).

import (
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
)

// TestMemWatchdogSavesSnapshotBeforeKill verifies that the memory-watchdog
// goroutine delivers a signal through memPressureCh, that the signal loop
// correctly triggers performShutdown, and that a valid snapshot file exists
// on disk after shutdown completes.
func TestMemWatchdogSavesSnapshotBeforeKill(t *testing.T) {
	dir := t.TempDir()

	// ── Step 1: build a minimal chain in a real LevelDB store ────────────────
	// buildChainInStore (snapshot_restart_test.go) creates genesis + 4 extra
	// blocks and sets the DB tip.  tip height = 4.
	db, blocks := buildChainInStore(t, dir, 4)

	tip := blocks[len(blocks)-1]
	tipHash := tip.Hash()
	tipHeight := tip.Header.Height // 4

	// ── Step 2: set up a fake consensus engine ───────────────────────────────
	// performShutdown closes stop and then waits on engineDone.
	// The goroutine below simulates consensus.Engine.Run: it exits immediately
	// when stop is closed, which is the only behaviour the shutdown path needs.
	engineStop := make(chan struct{})
	engineDone := make(chan struct{})
	go func() {
		<-engineStop
		close(engineDone)
	}()

	// ── Step 3: wire the watchdog with injectable fakes ───────────────────────
	// ticks replaces time.NewTicker(20*time.Second).C.  A buffered channel lets
	// us send exactly one tick without blocking.
	ticks := make(chan time.Time, 1)

	// threshold = 65 % of a 1 GiB fake limit (same formula as production).
	const fakeLimit int64 = 1 << 30 // 1 GiB
	threshold := uint64(fakeLimit) * 65 / 100

	// rssAboveThreshold always reports RSS just above the threshold so that
	// every tick triggers the watchdog.
	rssAboveThreshold := func() int64 { return int64(threshold) + 1 }

	// memPressureCh mirrors the production channel: buffered capacity 1.
	memPressureCh := make(chan struct{}, 1)

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	// ── Step 4: run the watchdog goroutine ────────────────────────────────────
	// runMemoryWatchdogLoop (extracted from run() in main.go) blocks until
	// engineStop is closed.  It sends to memPressureCh when RSS ≥ threshold.
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		runMemoryWatchdogLoop(ticks, engineStop, threshold, fakeLimit, rssAboveThreshold, memPressureCh, log)
	}()

	// ── Step 5: send one fake tick → watchdog fires ───────────────────────────
	ticks <- time.Now()

	// ── Step 6: wait for the watchdog to deliver the memory-pressure signal ───
	// This mirrors the signal-loop case arm in run():
	//   case <-memPressureCh:
	//       log.Warn("memory watchdog triggered — ...")
	select {
	case <-memPressureCh:
		// signal received — the watchdog fired as expected
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for memPressureCh signal from watchdog")
	}

	// ── Step 7: call performShutdown (what the signal loop does next) ─────────
	// Use a real UTXOSet and registry so saveShutdownSnapshot does not
	// short-circuit on registry == nil.
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()

	// performShutdown closes engineStop; the fake engine goroutine (step 2)
	// closes engineDone in response, so the wait inside performShutdown returns.
	performShutdown(engineStop, engineDone, db, utxos, registry, dir, log, nil)

	// The watchdog goroutine is still alive (blocked on ticks / engineStop);
	// wait for it to exit now that engineStop is closed.
	select {
	case <-watchdogDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watchdog goroutine to exit")
	}

	// ── Step 8: assert snapshot file exists ───────────────────────────────────
	wantPath := snapshotPath(dir, tipHeight)
	if _, err := os.Stat(wantPath); os.IsNotExist(err) {
		t.Fatalf("watchdog-triggered shutdown did not create snapshot\n  expected: %s\n  log:\n%s",
			wantPath, logBuf.String())
	}

	// ── Step 9: assert the snapshot is valid and carries the correct tip ──────
	tipHashHex := fmt.Sprintf("%x", tipHash[:])
	loaded, err := loadStartupSnapshot(dir, tipHeight, tipHashHex)
	if err != nil {
		t.Fatalf("snapshot written by watchdog shutdown is unreadable: %v\nlog:\n%s",
			err, logBuf.String())
	}
	if loaded.TipHeight != tipHeight {
		t.Errorf("snapshot TipHeight = %d, want %d", loaded.TipHeight, tipHeight)
	}
	if loaded.TipHashHex != tipHashHex {
		t.Errorf("snapshot TipHashHex mismatch\n  got  %s\n  want %s",
			loaded.TipHashHex, tipHashHex)
	}

	// ── Step 10: assert key log messages were emitted ─────────────────────────
	// The watchdog goroutine logs a Warn when it fires.
	if !logContainsMsg(&logBuf, "memory watchdog: RSS approaching GOMEMLIMIT — initiating proactive graceful restart") {
		t.Errorf("expected watchdog Warn log was not emitted\nlog:\n%s", logBuf.String())
	}
	// saveShutdownSnapshot logs Info on success.
	if !logContainsMsg(&logBuf, "shutdown: snapshot saved") {
		t.Errorf("expected \"shutdown: snapshot saved\" log was not emitted\nlog:\n%s", logBuf.String())
	}
}

// TestMemWatchdogFiresOnlyOnce verifies that the memory watchdog sends at most
// one signal through memPressureCh even when RSS stays above the threshold
// across multiple ticker ticks.
//
// A refactor that accidentally removes the `fired` flag guard would cause
// memPressureCh to receive a second signal, triggering a double-shutdown that
// could corrupt the snapshot or panic on a closed channel.
func TestMemWatchdogFiresOnlyOnce(t *testing.T) {
	// ticks is a buffered channel: we can pre-load two ticks without blocking.
	ticks := make(chan time.Time, 2)

	// threshold = 65 % of a 1 GiB fake limit (same formula as production).
	const fakeLimit int64 = 1 << 30 // 1 GiB
	threshold := uint64(fakeLimit) * 65 / 100

	// rssAboveThreshold always reports RSS just above the threshold so that
	// every tick should (naïvely) trigger the watchdog.
	rssAboveThreshold := func() int64 { return int64(threshold) + 1 }

	// memPressureCh is buffered (capacity 1) — same as production.
	// After the watchdog fires once the channel is full; a non-guarded second
	// send would silently drop into the default branch, so we use a larger
	// buffer (capacity 2) to detect whether a second signal was attempted.
	memPressureCh := make(chan struct{}, 2)

	stop := make(chan struct{})
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	// Pre-load two above-threshold ticks so the goroutine processes both
	// without any wall-clock wait.
	ticks <- time.Now()
	ticks <- time.Now()

	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		runMemoryWatchdogLoop(ticks, stop, threshold, fakeLimit, rssAboveThreshold, memPressureCh, log)
	}()

	// Give the goroutine enough time to drain both ticks.
	// A short sleep is fine here because we pre-loaded the channel and the
	// goroutine does no I/O — it will finish both iterations in microseconds.
	time.Sleep(50 * time.Millisecond)

	// Stop the watchdog.
	close(stop)
	select {
	case <-watchdogDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watchdog goroutine to exit")
	}

	// Assert: exactly one signal was sent, not two.
	got := len(memPressureCh)
	if got != 1 {
		t.Errorf("memPressureCh has %d signal(s) after two above-threshold ticks, want exactly 1\nlog:\n%s",
			got, logBuf.String())
	}

	// The watchdog must have logged the Warn exactly once.
	if !logContainsMsg(&logBuf, "memory watchdog: RSS approaching GOMEMLIMIT — initiating proactive graceful restart") {
		t.Errorf("expected watchdog Warn log was not emitted\nlog:\n%s", logBuf.String())
	}
}
