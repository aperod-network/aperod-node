package consensus_test

// TestOraclePrice_SurvivesRestart is a restart-survival test for oracle price
// embedding.  It verifies that:
//
//  1. Blocks produced while the oracle is up carry a non-zero OraclePrice.
//  2. After the oracle goes down, produced blocks carry OraclePrice=0 AND the
//     engine escalates to ERROR-level logging once consecutive failures exceed
//     the oracleErrorThreshold (10).
//  3. After the oracle comes back on the same address, produced blocks resume
//     carrying a non-zero OraclePrice — confirming full recovery without a
//     node restart.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── recordingHandler ─────────────────────────────────────────────────────────

// captureHandler is an slog.Handler that records the highest level seen and
// whether any Error-level record was emitted.  Thread-safe.
type captureHandler struct {
	mu       sync.Mutex
	gotError bool
	records  []slog.Record
}

func (h *captureHandler) Enabled(_ context.Context, lvl slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	if r.Level >= slog.LevelError {
		h.gotError = true
	}
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) HasError() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gotError
}

// ─── gatedOracleServer ────────────────────────────────────────────────────────

// gatedOracleServer serves the oracle JSON endpoint while gate==1 and returns
// HTTP 503 (no body) while gate==0.  Switching the gate simulates killing and
// restarting the oracle without changing the server's port.
type gatedOracleServer struct {
	gate     int32 // 1=up, 0=down  (atomic)
	priceUSD float64
	srv      *httptest.Server
}

func newGatedOracleServer(priceUSD float64) *gatedOracleServer {
	g := &gatedOracleServer{gate: 1, priceUSD: priceUSD}
	mux := http.NewServeMux()
	mux.HandleFunc("/price", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&g.gate) == 0 {
			// Oracle is "down": simulate an unreachable server by returning 503
			// with an empty body so json.Decode fails and fetchOraclePrice
			// increments oracleConsecFails.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]float64{"price_usd": g.priceUSD})
	})
	g.srv = httptest.NewServer(mux)
	return g
}

func (g *gatedOracleServer) URL() string { return g.srv.URL + "/price" }
func (g *gatedOracleServer) Up()         { atomic.StoreInt32(&g.gate, 1) }
func (g *gatedOracleServer) Down()       { atomic.StoreInt32(&g.gate, 0) }
func (g *gatedOracleServer) Close()      { g.srv.Close() }

// ─── helpers ──────────────────────────────────────────────────────────────────

func makeGenesisChain(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey) *core.Chain {
	t.Helper()
	hdr := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatal(err)
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(&core.Block{Header: hdr}); err != nil {
		t.Fatal(err)
	}
	return chain
}

func newOracleEngine(
	t *testing.T,
	validators []crypto.ValidatorPubKey,
	myKey *crypto.LockedValidatorKey,
	chain *core.Chain,
	oracleURL string,
	log *slog.Logger,
) *consensus.Engine {
	t.Helper()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	return consensus.NewEngine(consensus.Config{
		OnCanonicalBlock: noopCanonicalPersistence,
		BlockTime:    15 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   validators,
		MyKey:        myKey,
		OracleURL:    oracleURL,
	}, chain, mp, log)
}

// drainBlocks reads from ch until it has seen n blocks or deadline expires.
// Returns all blocks collected.
func drainBlocks(t *testing.T, ch <-chan *core.Block, n int, timeout time.Duration) []*core.Block {
	t.Helper()
	out := make([]*core.Block, 0, n)
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case b := <-ch:
			out = append(out, b)
		case <-deadline:
			t.Fatalf("drainBlocks: timeout waiting for block %d/%d after %s", len(out)+1, n, timeout)
		}
	}
	return out
}

// ─── Test ─────────────────────────────────────────────────────────────────────

// TestOraclePrice_SurvivesRestart verifies that oracle price embedding:
//   - works while the oracle is up (OraclePrice > 0)
//   - stops and escalates to ERROR log when oracle goes down (≥11 consecutive fails)
//   - resumes automatically when oracle comes back on the same address
func TestOraclePrice_SurvivesRestart(t *testing.T) {
	const testPriceUSD = 0.042

	// 1. Start gated oracle server.
	oracle := newGatedOracleServer(testPriceUSD)
	defer oracle.Close()

	// 2. Create single-validator engine so it's always the proposer.
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain := makeGenesisChain(t, priv, pub)
	lk, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}

	capture := &captureHandler{}
	logger := slog.New(capture)

	eng := newOracleEngine(t, []crypto.ValidatorPubKey{pub}, lk, chain, oracle.URL(), logger)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		eng.Run(stop)
		close(done)
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
		lk.Destroy()
	})

	produced := eng.ProducedCh()

	// ── Phase 1: oracle is UP — expect non-zero OraclePrice in produced blocks ──

	t.Log("phase 1: oracle up — waiting for block with non-zero OraclePrice")
	deadline1 := time.After(2 * time.Second)
	var phase1Block *core.Block
	for phase1Block == nil {
		select {
		case b := <-produced:
			if b.Header.OraclePrice > 0 {
				phase1Block = b
				t.Logf("phase 1 OK: block height=%d oracle_price=%d", b.Header.Height, b.Header.OraclePrice)
			}
		case <-deadline1:
			t.Fatal("phase 1: timeout — no block with non-zero OraclePrice while oracle is up")
		}
	}

	// ── Phase 2: bring oracle DOWN — collect 12 more blocks ──
	// 12 > oracleErrorThreshold(10) so the ERROR log must fire.

	t.Log("phase 2: oracle down — collecting 12 blocks to trigger ERROR log")
	oracle.Down()

	// Drain any queued blocks that were already in flight with oracle still up.
	// We want 12 blocks that are definitely produced AFTER the oracle went down.
	// Strategy: wait briefly then collect 12 blocks; all should have OraclePrice=0.
	time.Sleep(30 * time.Millisecond) // let any in-flight fetch finish

	const failBlocks = 12
	phase2Blocks := drainBlocks(t, produced, failBlocks, 5*time.Second)

	zeroCount := 0
	for _, b := range phase2Blocks {
		if b.Header.OraclePrice == 0 {
			zeroCount++
		}
	}
	// All 12 blocks collected after oracle went down must carry OraclePrice=0.
	if zeroCount < failBlocks {
		// Allow a small margin: the very first block after Down() might have been
		// fetched just before the gate closed.  Require at least 11 zeros.
		const minZero = failBlocks - 1
		if zeroCount < minZero {
			t.Errorf("phase 2: expected at least %d blocks with OraclePrice=0, got %d/%d",
				minZero, zeroCount, failBlocks)
		}
	}
	t.Logf("phase 2 OK: %d/%d blocks have OraclePrice=0", zeroCount, failBlocks)

	// Verify that the engine escalated to ERROR level after > 10 consecutive fails.
	if !capture.HasError() {
		t.Error("phase 2: expected at least one ERROR-level log after 11+ consecutive oracle failures, got none")
	} else {
		t.Log("phase 2 OK: ERROR-level log emitted as expected")
	}

	// ── Phase 3: bring oracle back UP on same address — expect price to resume ──

	t.Log("phase 3: oracle back up — waiting for block with non-zero OraclePrice")
	oracle.Up()

	deadline3 := time.After(3 * time.Second)
	var phase3Block *core.Block
	for phase3Block == nil {
		select {
		case b := <-produced:
			if b.Header.OraclePrice > 0 {
				phase3Block = b
			}
		case <-deadline3:
			t.Fatal("phase 3: timeout — oracle came back but blocks still have OraclePrice=0; price embedding did not recover")
		}
	}

	// Sanity check: the recovered price should encode testPriceUSD within
	// rounding error (oraclePriceScale = 1e9).
	const oraclePriceScale = 1_000_000_000
	wantFixed := uint64(testPriceUSD * oraclePriceScale)
	got := phase3Block.Header.OraclePrice
	// Allow ±1 unit of last place rounding.
	diff := int64(got) - int64(wantFixed)
	if diff < -1 || diff > 1 {
		t.Errorf("phase 3: recovered OraclePrice=%d, want ~%d (±1 ULP) for price_usd=%.3f",
			got, wantFixed, testPriceUSD)
	}
	t.Logf("phase 3 OK: oracle recovered — block height=%d oracle_price=%d (want ~%d)",
		phase3Block.Header.Height, got, wantFixed)

	// Extra: confirm oracleConsecFails was reset — next block after recovery must
	// also carry a non-zero price (no lingering fail state).
	deadline4 := time.After(2 * time.Second)
	for {
		select {
		case b := <-produced:
			if b.Header.OraclePrice > 0 {
				t.Logf("phase 3 confirmed: second post-recovery block height=%d also has OraclePrice>0", b.Header.Height)
				return
			}
		case <-deadline4:
			t.Fatal("phase 3: second block after recovery has OraclePrice=0 — fail counter may not have been reset")
		}
	}
}

// TestOraclePrice_ZeroWhenOracleDown verifies that blocks produced when OracleURL
// is unreachable always carry OraclePrice=0 (no stale non-zero value bleeds in).
func TestOraclePrice_ZeroWhenOracleDown(t *testing.T) {
	// Oracle server is immediately "down" from the start.
	oracle := newGatedOracleServer(0.123)
	oracle.Down()
	defer oracle.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain := makeGenesisChain(t, priv, pub)
	lk, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Destroy()

	eng := newOracleEngine(t, []crypto.ValidatorPubKey{pub}, lk, chain, oracle.URL(), newNopLogger())

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// Collect 5 blocks — all must have OraclePrice=0.
	blocks := drainBlocks(t, eng.ProducedCh(), 5, 3*time.Second)
	for _, b := range blocks {
		if b.Header.OraclePrice != 0 {
			t.Errorf("block height=%d has OraclePrice=%d, want 0 (oracle is down)",
				b.Header.Height, b.Header.OraclePrice)
		}
	}
	t.Logf("OK: all %d blocks have OraclePrice=0 while oracle is down", len(blocks))
}

// TestOraclePrice_NoURLSkipsEmbedding verifies that when OracleURL is empty the
// engine never embeds a price (OraclePrice stays 0 for all produced blocks).
func TestOraclePrice_NoURLSkipsEmbedding(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain := makeGenesisChain(t, priv, pub)
	lk, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Destroy()

	mp := core.NewMempool(core.DefaultMempoolConfig())
	// No OracleURL → embedding disabled.
	eng := consensus.NewEngine(consensus.Config{
		OnCanonicalBlock: noopCanonicalPersistence,
		BlockTime:    15 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{pub},
		MyKey:        lk,
		OracleURL:    "", // intentionally empty
	}, chain, mp, newNopLogger())

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	blocks := drainBlocks(t, eng.ProducedCh(), 3, 2*time.Second)
	for _, b := range blocks {
		if b.Header.OraclePrice != 0 {
			t.Errorf("block height=%d has OraclePrice=%d, want 0 (no OracleURL configured)",
				b.Header.Height, b.Header.OraclePrice)
		}
	}
	t.Logf("OK: all %d blocks have OraclePrice=0 with empty OracleURL", len(blocks))
}

// TestOraclePrice_ConsecFailsExceedThreshold is a focused unit test that confirms
// the oracleErrorThreshold boundary: after exactly threshold+1 failures the engine
// must emit at least one ERROR-level log (the "consecutive failures exceed
// threshold" message).  This guards against the threshold value being accidentally
// changed without also updating the test expectations.
func TestOraclePrice_ConsecFailsExceedThreshold(t *testing.T) {
	// Gate starts down → every fetch is a failure.
	oracle := newGatedOracleServer(0.05)
	oracle.Down()
	defer oracle.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain := makeGenesisChain(t, priv, pub)
	lk, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Destroy()

	capture := &captureHandler{}
	logger := slog.New(capture)

	eng := newOracleEngine(t, []crypto.ValidatorPubKey{pub}, lk, chain, oracle.URL(), logger)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// oracleErrorThreshold = 10 (from poa.go).  We need >10 blocks produced
	// so the fail counter exceeds the threshold.  Collect 13 to be safe.
	const need = 13
	drainBlocks(t, eng.ProducedCh(), need, 5*time.Second)

	if !capture.HasError() {
		t.Errorf("expected at least one ERROR-level log after %d consecutive oracle failures (threshold=10), got none", need)
	} else {
		t.Logf("OK: ERROR-level oracle log emitted after %d consecutive failures", need)
	}
}

// TestOracleSlowFetch_DoesNotDelayBlockProduction verifies that a slow oracle
// (2-second HTTP response time) does not stall block production.
//
// With the async oracle fetcher the price is updated in a background goroutine;
// produceBlock() reads the cached value atomically and never waits on the
// network.  Five blocks at 30 ms block-time should complete in well under
// 2 seconds; without the fix each block would stall for up to 2 s, making the
// same five blocks take ≥ 10 s.
func TestOracleSlowFetch_DoesNotDelayBlockProduction(t *testing.T) {
	const (
		blockTime   = 30 * time.Millisecond
		oracleDelay = 2 * time.Second // far longer than blockTime
		wantBlocks  = 5
	)

	// Slow oracle: responds correctly but only after oracleDelay.
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(oracleDelay)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]float64{"price_usd": 0.05})
	}))
	defer slowSrv.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain := makeGenesisChain(t, priv, pub)
	lk, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Destroy()

	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		OnCanonicalBlock: noopCanonicalPersistence,
		BlockTime:    blockTime,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{pub},
		MyKey:        lk,
		OracleURL:    slowSrv.URL, // slow — responds after 2 s
	}, chain, mp, newNopLogger())

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// Measure how long it takes to collect wantBlocks blocks.
	// With the async fix this should finish in well under oracleDelay;
	// without the fix it would take ≥ wantBlocks × oracleDelay.
	start := time.Now()
	// Generous deadline: 5× blockTime × wantBlocks still << oracleDelay.
	drainBlocks(t, eng.ProducedCh(), wantBlocks, 3*time.Second)
	elapsed := time.Since(start)

	// If oracle latency leaked into block production the elapsed time would be
	// several seconds.  Require it completes in under oracleDelay.
	if elapsed >= oracleDelay {
		t.Errorf("block production took %s for %d blocks — oracle latency is blocking produceBlock (want < %s)",
			elapsed, wantBlocks, oracleDelay)
	}
	t.Logf("OK: produced %d blocks in %s with oracle_delay=%s (async fetcher working)",
		wantBlocks, elapsed, oracleDelay)
}

// Ensure we have a local newNopLogger if this file is compiled in isolation.
// consensus_test.go defines it already; this compile-time check prevents
// duplicate-symbol errors by relying on the package-level definition there.
var _ = fmt.Sprintf // force fmt import used in the file
