package core_test

// Tests for the mempool evictions counter (Task: expose mempool size and
// eviction rate in the node metrics endpoint).  EvictionsTotal must count
// every eviction path — capacity-pressure fee-rate eviction, FIFO fallback,
// and TTL expiry — while normal removals (block inclusion) never count.

import (
	"testing"
	"time"

	"github.com/aperod/aperod/core"
)

func TestMempool_EvictionsTotal_StartsAtZero(t *testing.T) {
	pool := core.NewMempool(core.MempoolConfig{
		MaxSize: 10, MaxBytes: 1 << 20, MaxTxSize: 1_000_000, BaseFeePerByte: 1,
	}, silentLogger())
	if got := pool.EvictionsTotal(); got != 0 {
		t.Errorf("EvictionsTotal on fresh pool = %d, want 0", got)
	}
}

func TestMempool_EvictionsTotal_CountsCapacityEvictions(t *testing.T) {
	const cap = 3
	cfg := core.MempoolConfig{
		MaxSize: cap, MaxBytes: 1 << 20, MaxTxSize: 1_000_000, BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	sample := makeTx(0, 999)
	minFee := uint64(sample.Size()) * cfg.BaseFeePerByte
	for i := 0; i < cap; i++ {
		if err := pool.Add(makeTx(minFee, i)); err != nil {
			t.Fatalf("Add tx %d: %v", i, err)
		}
	}
	if got := pool.EvictionsTotal(); got != 0 {
		t.Fatalf("EvictionsTotal before overflow = %d, want 0", got)
	}

	// Two overflowing high-fee txs → two capacity evictions.
	for i := 0; i < 2; i++ {
		if err := pool.Add(makeTx(minFee*10, cap+i)); err != nil {
			t.Fatalf("Add overflow tx %d: %v", i, err)
		}
	}
	if got := pool.EvictionsTotal(); got != 2 {
		t.Errorf("EvictionsTotal after 2 overflows = %d, want 2", got)
	}
	if pool.Count() != cap {
		t.Errorf("Count = %d, want %d", pool.Count(), cap)
	}
}

func TestMempool_EvictionsTotal_CountsTTLEvictions(t *testing.T) {
	cfg := core.MempoolConfig{
		MaxSize: 10, MaxBytes: 1 << 20, MaxTxSize: 1_000_000,
		TTL: time.Nanosecond, BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())
	sample := makeTx(0, 999)
	minFee := uint64(sample.Size()) * cfg.BaseFeePerByte
	for i := 0; i < 3; i++ {
		if err := pool.Add(makeTx(minFee, i)); err != nil {
			t.Fatalf("Add tx %d: %v", i, err)
		}
	}
	time.Sleep(2 * time.Millisecond) // all entries exceed the 1ns TTL
	if removed := pool.Evict(); removed != 3 {
		t.Fatalf("Evict removed %d, want 3", removed)
	}
	if got := pool.EvictionsTotal(); got != 3 {
		t.Errorf("EvictionsTotal after TTL sweep = %d, want 3", got)
	}
}

func TestMempool_EvictionsTotal_NotIncrementedByRemove(t *testing.T) {
	cfg := core.MempoolConfig{
		MaxSize: 10, MaxBytes: 1 << 20, MaxTxSize: 1_000_000, BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())
	sample := makeTx(0, 999)
	minFee := uint64(sample.Size()) * cfg.BaseFeePerByte
	tx := makeTx(minFee, 0)
	if err := pool.Add(tx); err != nil {
		t.Fatalf("Add: %v", err)
	}
	pool.Remove(tx.Hash()) // block inclusion — not an eviction
	if got := pool.EvictionsTotal(); got != 0 {
		t.Errorf("EvictionsTotal after Remove = %d, want 0 (inclusion is not eviction)", got)
	}
}
