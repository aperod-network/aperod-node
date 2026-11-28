package consensus

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

func requirePoolPanic(t *testing.T, fragment string, run func()) {
	t.Helper()
	defer func() {
		cause := recover()
		if cause == nil || !strings.Contains(fmt.Sprint(cause), fragment) {
			t.Fatalf("expected fail-closed %q panic, got %v", fragment, cause)
		}
	}()
	run()
}

func TestPoolBalance_RestartRestoreAndInvalidStateFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if db != nil {
			if err := db.Close(); err != nil {
				t.Errorf("close pool fixture: %v", err)
			}
		}
	})
	newEngine := func(database *store.DB) *Engine {
		return NewEngine(Config{StakingPoolNAPR: 1000, Store: database}, core.NewChain(),
			core.NewMempool(core.DefaultMempoolConfig()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	if got := atomic.LoadInt64(&newEngine(db).stakingPoolRemaining); got != 1000 {
		t.Fatalf("positive initial pool control: %d", got)
	}
	if err := db.StoreStakingPoolRemaining(700); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&newEngine(db).stakingPoolRemaining); got != 700 {
		t.Fatalf("restart reset durable pool: %d", got)
	}
	if err := db.StoreStakingPoolRemaining(math.MaxUint64); err != nil {
		t.Fatal(err)
	}
	requirePoolPanic(t, "exceeds signed engine range", func() { newEngine(db) })
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	closed := db
	db = nil
	requirePoolPanic(t, "cannot safely restore persisted balance", func() { newEngine(closed) })
	requirePoolPanic(t, "exceeds signed engine range", func() {
		NewEngine(Config{StakingPoolNAPR: math.MaxUint64}, nil, nil, nil)
	})
	if checkedPoolBalance(0) != 0 || checkedPoolBalance(math.MaxInt64) != math.MaxInt64 {
		t.Fatal("valid signed pool boundaries were rejected")
	}
}
