package core_test

// Concurrency stress tests for the mempool (run with `go test -race`).
//
// The eviction paths (evictLowestFeeRate / evictOldest / TTL sweep) all mutate
// totalBytes and evictionsTotal under the write lock; these tests hammer the
// pool from many goroutines at once so the race detector can surface any
// unsynchronised access, and then assert the core invariants:
//   - pool.Count() never exceeds MaxSize
//   - TotalBytes() is never negative and never exceeds MaxBytes
//   - accounting balances: adds accepted == remaining + evicted (+ removed)

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aperod/aperod/core"
)

func TestMempool_ConcurrentAdd(t *testing.T) {
	const (
		goroutines = 8
		perG       = 200 // 1600 txs total against a 50-slot pool
		maxSize    = 50
	)
	cfg := core.MempoolConfig{
		MaxSize:        maxSize,
		MaxBytes:       256 * 1024 * 1024,
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
	if tb := pool.TotalBytes(); tb < 0 {
		t.Errorf("TotalBytes = %d, must never be negative", tb)
	} else if tb > cfg.MaxBytes {
		t.Errorf("TotalBytes = %d, exceeds MaxBytes %d", tb, cfg.MaxBytes)
	}

	// Accounting must balance exactly: every accepted tx is either still in
	// the pool or was evicted to make room for a later one.
	got := int64(pool.Count()) + int64(pool.EvictionsTotal())
	if got != accepted.Load() {
		t.Errorf("accounting mismatch: count(%d) + evictions(%d) = %d, want accepted %d",
			pool.Count(), pool.EvictionsTotal(), got, accepted.Load())
	}
}

// TestMempool_ConcurrentAddRemoveEvict mixes writers, removers and readers so
// -race can observe every lock interaction path at once.
func TestMempool_ConcurrentAddRemoveEvict(t *testing.T) {
	const (
		goroutines = 6
		perG       = 150
		maxSize    = 40
	)
	cfg := core.MempoolConfig{
		MaxSize:        maxSize,
		MaxBytes:       1 << 20, // 1 MiB — small enough for the byte cap to bite
		MaxTxSize:      1_000_000,
		TTL:            0, // TTL sweep in this test relies on Evict() no-op path
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	sample := makeTx(0, 60000)
	minFee := uint64(sample.Size()) * cfg.BaseFeePerByte

	var wg sync.WaitGroup
	// Writers: concurrent Add with occasional Remove of their own tx.
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				kiIdx := g*perG + i + 1
				tx := makeTx(minFee+uint64(kiIdx%5)*minFee, kiIdx)
				if err := pool.Add(tx); err == nil && i%3 == 0 {
					pool.Remove(tx.Hash())
				}
			}
		}(g)
	}
	// Readers + sweepers: hammer the read paths and the TTL sweep while
	// writers churn the pool.
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
					_ = pool.Evict()
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()

	if pool.Count() > maxSize {
		t.Errorf("Count = %d, exceeds MaxSize %d", pool.Count(), maxSize)
	}
	if tb := pool.TotalBytes(); tb < 0 {
		t.Errorf("TotalBytes = %d, must never be negative", tb)
	} else if tb > cfg.MaxBytes {
		t.Errorf("TotalBytes = %d, exceeds MaxBytes %d", tb, cfg.MaxBytes)
	}
}
