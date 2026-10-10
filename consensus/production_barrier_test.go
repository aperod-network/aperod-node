package consensus

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func productionBarrierFixture(t *testing.T, stopAt uint64) (*Engine, *crypto.LockedValidatorKey) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(key.Destroy)
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, Timestamp: time.Now().UnixNano()}}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(Config{
		MyKey:                    key,
		Validators:               []crypto.ValidatorPubKey{pub},
		BFTThreshold:             0.667,
		RingCTV4ActivationHeight: ^uint64(0),
		StopProducingAtHeight:    stopAt,
		OnCanonicalBlock: func(*core.Block, *avm.PreparedBlock) error {
			return nil
		},
	}, chain, core.NewMempool(core.DefaultMempoolConfig()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return engine, key
}

func TestProductionBarrierStopsAtHeightAndSurvivesRestart(t *testing.T) {
	engine, _ := productionBarrierFixture(t, 3)
	if err := engine.tick(); err != nil {
		t.Fatal(err)
	}
	if engine.chain.Tip().Header.Height != 1 || !engine.IsFinalized(1) {
		t.Fatal("height 1 did not produce and finalize below barrier")
	}
	if err := engine.tick(); err != nil {
		t.Fatal(err)
	}
	if engine.chain.Tip().Header.Height != 2 || !engine.IsFinalized(2) {
		t.Fatal("height 2 did not produce and finalize below barrier")
	}
	if err := engine.tick(); err != nil {
		t.Fatal(err)
	}
	if engine.chain.Tip().Header.Height != 2 {
		t.Fatalf("barrier height 3 was produced; tip=%d", engine.chain.Tip().Header.Height)
	}

	restarted := NewEngine(engine.cfg, engine.chain, engine.pool, engine.log)
	if err := restarted.tick(); err != nil {
		t.Fatal(err)
	}
	if restarted.chain.Tip().Header.Height != 2 {
		t.Fatal("restart did not retain the configured production barrier")
	}
}

func TestProductionBarrierZeroIsDisabled(t *testing.T) {
	engine, _ := productionBarrierFixture(t, 0)
	for wantHeight := uint64(1); wantHeight <= 3; wantHeight++ {
		if err := engine.tick(); err != nil {
			t.Fatal(err)
		}
		if got := engine.chain.Tip().Header.Height; got != wantHeight {
			t.Fatalf("zero barrier stopped production at height %d; got tip %d", wantHeight, got)
		}
	}
}

func TestProductionBarrierSerializesConcurrentTicks(t *testing.T) {
	engine, _ := productionBarrierFixture(t, 1)
	const concurrentTicks = 16
	var wg sync.WaitGroup
	errs := make(chan error, concurrentTicks)
	wg.Add(concurrentTicks)
	for range concurrentTicks {
		go func() {
			defer wg.Done()
			errs <- engine.tick()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := engine.chain.Tip().Header.Height; got != 0 {
		t.Fatalf("concurrent ticks raced past barrier at height 1; tip=%d", got)
	}
}
