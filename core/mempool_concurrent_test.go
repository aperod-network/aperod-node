package core_test

// Concurrency stress tests for the mempool (run with `go test -race`).
//
// The eviction paths (evictLowestFeeRate / evictOldest / TTL sweep) all mutate
// totalBytes and evictionsTotal under the write lock; these tests hammer the
// pool from many goroutines at once so the race detector can surface any
// unsynchronised access, and then assert exact accounting invariants:
//   - pool.Count() never exceeds MaxSize
//   - TotalBytes() equals Count() × txSize exactly (all txs same size)
//   - adds accepted == remaining + evicted + explicitly removed
//
// Three pressure regimes are covered: count-cap eviction, byte-cap eviction,
// and TTL-sweep eviction, plus an eviction-free Add/Remove churn test where
// explicit removals can be reconciled exactly.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
)

// TestMempool_ConcurrentAdd floods a small count-capped pool from many
// goroutines. Every add succeeds (all txs are evictable), so the accounting
// must balance exactly: accepted == remaining + evicted.
func TestMempool_ConcurrentAdd(t *testing.T) {
	const (
		goroutines = 8
		perG       = 200 // 1600 txs total against a 50-slot pool
		maxSize    = 50
	)
	cfg := core.MempoolConfig{
		MaxSize:        maxSize,
		MaxBytes:       256 * 1024 * 1024, // byte cap deliberately out of reach — count pressure only
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	sample := makeTx(0, 60000)
	minFee := uint64(sample.Size()) * cfg.BaseFeePerByte

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				// Unique key image per tx; vary the fee so eviction has
				// a real fee-rate ordering to chew on.
				kiIdx := g*perG + i + 1
				fee := minFee + uint64(kiIdx%7)*minFee
				if err := pool.Add(makeTx(fee, kiIdx)); err == nil {
					accepted.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()

	if pool.Count() > maxSize {
		t.Errorf("Count = %d, exceeds MaxSize %d", pool.Count(), maxSize)
	}
	// Accounting must balance exactly: every accepted tx is either still in
	// the pool or was evicted to make room for a later one.
	got := int64(pool.Count()) + int64(pool.EvictionsTotal())
	if got != accepted.Load() {
		t.Errorf("accounting mismatch: count(%d) + evictions(%d) = %d, want accepted %d",
			pool.Count(), pool.EvictionsTotal(), got, accepted.Load())
	}
	// All txs here vary only in Fee (fixed-width field) and key image
	// (fixed 32 bytes), so every tx has identical size and totalBytes must
	// reconcile exactly with the entry count.
	sized := makeTx(minFee, 1)
	txSize := sized.Size()
	if want := pool.Count() * txSize; pool.TotalBytes() != want {
		t.Errorf("TotalBytes = %d, want count(%d) × txSize(%d) = %d",
			pool.TotalBytes(), pool.Count(), txSize, want)
	}
}

// TestMempool_ConcurrentAdd_BytePressure makes the BYTE cap (not the count
// cap) the binding constraint: MaxSize is huge, MaxBytes only fits ~10 txs.
func TestMempool_ConcurrentAdd_BytePressure(t *testing.T) {
	const (
		goroutines = 8
		perG       = 100
	)
	cfg := core.MempoolConfig{
		MaxSize:        10_000, // never reached — byte pressure must trigger first
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 1,
	}
	// All txs are identical in size; cap the pool at 10½ transactions so the
	// byte check (totalBytes+size > MaxBytes) evicts on every add past 10.
	sample := makeTx(0, 60000)
	txSize := sample.Size()
	cfg.MaxBytes = txSize*10 + txSize/2
	pool := core.NewMempool(cfg, silentLogger())

	minFee := uint64(txSize) * cfg.BaseFeePerByte
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				kiIdx := g*perG + i + 1
				// Identical fee for all txs so every tx has identical size
				// and byte accounting can be reconciled exactly.
				if err := pool.Add(makeTx(minFee, kiIdx)); err == nil {
					accepted.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()

	if accepted.Load() != goroutines*perG {
		t.Fatalf("accepted = %d, want all %d (all txs evictable, none should be rejected)",
			accepted.Load(), goroutines*perG)
	}
	if pool.Count() != 10 {
		t.Errorf("Count = %d, want exactly 10 (MaxBytes fits 10.5 txs)", pool.Count())
	}
	if tb := pool.TotalBytes(); tb > cfg.MaxBytes {
		t.Errorf("TotalBytes = %d, exceeds MaxBytes %d", tb, cfg.MaxBytes)
	}
	if want := pool.Count() * txSize; pool.TotalBytes() != want {
		t.Errorf("TotalBytes = %d, want count(%d) × txSize(%d) = %d",
			pool.TotalBytes(), pool.Count(), txSize, want)
	}
	got := int64(pool.Count()) + int64(pool.EvictionsTotal())
	if got != accepted.Load() {
		t.Errorf("accounting mismatch: count(%d) + evictions(%d) = %d, want accepted %d",
			pool.Count(), pool.EvictionsTotal(), got, accepted.Load())
	}
}

// TestMempool_ConcurrentAddRemove_ExactReconciliation churns Add + Remove +
// readers + Evict sweeps concurrently in a pool large enough that NO eviction
// can occur (MaxSize > total adds, TTL = 1h so the sweep is a true no-op).
// With eviction ruled out, every explicit Remove of a just-added tx must
// succeed, so the final state reconciles exactly.
func TestMempool_ConcurrentAddRemove_ExactReconciliation(t *testing.T) {
	const (
		goroutines = 6
		perG       = 150
	)
	cfg := core.MempoolConfig{
		MaxSize:        goroutines*perG + 1, // eviction can never trigger
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		TTL:            time.Hour, // positive TTL: sweep runs concurrently but removes nothing
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	sample := makeTx(0, 60000)
	txSize := sample.Size()
	minFee := uint64(txSize) * cfg.BaseFeePerByte

	var accepted, removed, evictReturned atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				kiIdx := g*perG + i + 1
				tx := makeTx(minFee, kiIdx) // identical fee → identical size
				if err := pool.Add(tx); err != nil {
					continue
				}
				accepted.Add(1)
				if i%3 == 0 {
					// No eviction and no TTL expiry can touch this entry,
					// and its hash is unique to this goroutine — the
					// removal is guaranteed to delete exactly this tx.
					pool.Remove(tx.Hash())
					removed.Add(1)
				}
			}
		}(g)
	}
	// Readers + TTL sweepers hammer the read paths and the sweep lock path
	// while writers churn the pool.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 3; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = pool.Count()
					_ = pool.TotalBytes()
					_ = pool.EvictionsTotal()
					_ = pool.Hashes()
					evictReturned.Add(int64(pool.Evict()))
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()

	if accepted.Load() != goroutines*perG {
		t.Fatalf("accepted = %d, want all %d (pool can never be full)",
			accepted.Load(), goroutines*perG)
	}
	if evictReturned.Load() != 0 {
		t.Errorf("Evict() removed %d entries, want 0 (TTL is 1h)", evictReturned.Load())
	}
	if pool.EvictionsTotal() != 0 {
		t.Errorf("EvictionsTotal = %d, want 0 (no capacity or TTL pressure)", pool.EvictionsTotal())
	}
	// Exact reconciliation: remaining == accepted − removed.
	if want := accepted.Load() - removed.Load(); int64(pool.Count()) != want {
		t.Errorf("Count = %d, want accepted(%d) − removed(%d) = %d",
			pool.Count(), accepted.Load(), removed.Load(), want)
	}
	if want := pool.Count() * txSize; pool.TotalBytes() != want {
		t.Errorf("TotalBytes = %d, want count(%d) × txSize(%d) = %d",
			pool.TotalBytes(), pool.Count(), txSize, want)
	}
}

// TestMempool_ConcurrentTTLEviction runs adders against concurrent TTL sweeps
// with an immediately-expiring TTL. After a final sweep the pool must be
// empty and every accepted tx must be accounted for as a TTL eviction.
func TestMempool_ConcurrentTTLEviction(t *testing.T) {
	const (
		goroutines = 6
		perG       = 100
	)
	cfg := core.MempoolConfig{
		MaxSize:        goroutines*perG + 1, // only TTL pressure, never capacity
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		TTL:            time.Nanosecond, // everything is expired as soon as it lands
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	sample := makeTx(0, 60000)
	minFee := uint64(sample.Size()) * cfg.BaseFeePerByte

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				kiIdx := g*perG + i + 1
				if err := pool.Add(makeTx(minFee, kiIdx)); err == nil {
					accepted.Add(1)
				}
			}
		}(g)
	}
	// Concurrent sweepers race the adders.
	stop := make(chan struct{})
	var sweepers sync.WaitGroup
	for r := 0; r < 3; r++ {
		sweepers.Add(1)
		go func() {
			defer sweepers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = pool.Evict()
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	sweepers.Wait()
	_ = pool.Evict() // final sweep drains any survivors

	if accepted.Load() != goroutines*perG {
		t.Fatalf("accepted = %d, want all %d", accepted.Load(), goroutines*perG)
	}
	if pool.Count() != 0 {
		t.Errorf("Count = %d, want 0 after final TTL sweep", pool.Count())
	}
	if pool.TotalBytes() != 0 {
		t.Errorf("TotalBytes = %d, want 0 on empty pool", pool.TotalBytes())
	}
	if got := int64(pool.EvictionsTotal()); got != accepted.Load() {
		t.Errorf("EvictionsTotal = %d, want accepted %d (every tx TTL-evicted)",
			got, accepted.Load())
	}
}
