package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMempoolEvictionDurationRejectsOverflowBeforeStartup(t *testing.T) {
	for _, seconds := range []uint64{maxMempoolEvictIntervalSec + 1, math.MaxUint64} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.yaml")
			body := fmt.Sprintf("network: testnet\ndata_dir: ./data\nconsensus:\n  block_time: 1s\nmempool_evict_interval_sec: %d\n", seconds)
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.MempoolEvictIntervalSec = 300
			if err := cfg.Validate(); err != nil {
				t.Fatalf("positive configuration control is invalid: %v", err)
			}
			cfg.MempoolEvictIntervalSec = seconds
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mempool_evict_interval_sec") {
				t.Fatalf("invalid duration configuration was not rejected: %v", err)
			}
		})
	}
}

func TestMempoolEvictionDurationRepresentableBoundary(t *testing.T) {
	for _, seconds := range []uint64{1, 300, maxMempoolEvictIntervalSec} {
		want := time.Duration(seconds) * time.Second
		if got := ResolveMempoolEvictInterval(seconds); got != want || got <= 0 {
			t.Fatalf("duration(%d)=%v, want %v", seconds, got, want)
		}
	}
	if ResolveMempoolEvictInterval(0) != 5*time.Minute {
		t.Fatal("the zero-value default changed")
	}
}

func TestMempoolEvictionDurationInvalidDirectCallFailsExplicitly(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Error("out-of-range direct input silently wrapped")
		}
	}()
	ResolveMempoolEvictInterval(maxMempoolEvictIntervalSec + 1)
}
