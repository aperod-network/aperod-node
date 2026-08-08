package main

// Integration test: startup UTXO rebuild stays below a memory ceiling.
//
// Covers the requirement from task #1547:
//   - runStartupScan() is run against a synthetic chain of nScanBlocks blocks,
//     each carrying outputsPerBlock coinbase outputs.  Total outputs cross
//     gcUTXOInterval (50 000) at least once so the periodic GC logic fires.
//   - A background goroutine samples runtime.MemStats.Sys at a fixed interval
//     while the scan is in progress.
//   - The SetSyncProgress callback (fired every 1 000 blocks) takes an
//     additional sample each time.  nScanBlocks > 1 000 guarantees at least
//     one callback invocation, and the test asserts this explicitly.
//   - After the scan the test asserts that the peak Sys value never exceeded
//     scanMemCeilingMiB × 1 MiB.
//
// The ceiling (scanMemCeilingMiB) is set to 512 MiB, which is far below the
// production 5 GB limit.  If that ceiling is ever approached it means
// something in the scan loop is holding large allocations across GC cycles.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// scanMemCeilingMiB is the maximum Sys memory (in MiB) the startup scan may
// use on the synthetic test chain.  Intentionally generous to avoid flakiness
// on loaded CI hosts while still catching large regressions.
const scanMemCeilingMiB = 512

// scanMemSampleInterval controls how often the background goroutine reads
// MemStats.  100 ms gives ~10 samples per second without excessive STW pauses.
const scanMemSampleInterval = 100 * time.Millisecond

// TestStartupScanMemoryCeiling verifies that runStartupScan() keeps its peak
// allocated Sys memory below scanMemCeilingMiB while rebuilding the UTXO set.
//
// Chain shape: nScanBlocks blocks (>1 000, so SetSyncProgress fires at least
// once), each with outputsPerBlock coinbase outputs.  Total outputs exceed
// gcUTXOInterval (50 000) so the GC-per-N-outputs mechanism is exercised.
func TestStartupScanMemoryCeiling(t *testing.T) {
	const (
		nScanBlocks     = 1100 // > 1 000 so syncProgressInterval callback fires ≥ once
		outputsPerBlock = 55   // outputs per block → 60 500 total (> gcUTXOInterval=50 000)
	)

	dir := t.TempDir()

	// ── Build the synthetic chain ─────────────────────────────────────────
	db, tip, pub := buildScanMemChain(t, dir, nScanBlocks, outputsPerBlock)
	tipHashArr := tip.Hash()
	tipHashHex := fmt.Sprintf("%x", tipHashArr[:])

	// ── Prepare UTXOSet + registry ────────────────────────────────────────
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	// ── Memory monitor ────────────────────────────────────────────────────
	// peakSys tracks the highest runtime.MemStats.Sys value observed while
	// the scan is in flight.  Updated atomically by the background goroutine
	// and by the SetSyncProgress callback.
	var peakSys atomic.Uint64

	sampleMem := func() {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		for {
			old := peakSys.Load()
			if ms.Sys <= old || peakSys.CompareAndSwap(old, ms.Sys) {
				break
			}
		}
	}

	// Background sampler: fires every scanMemSampleInterval until the scan
	// completes.
	var monitorWg sync.WaitGroup
	stopMonitor := make(chan struct{})
	monitorWg.Add(1)
	go func() {
		defer monitorWg.Done()
		ticker := time.NewTicker(scanMemSampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sampleMem()
			case <-stopMonitor:
				sampleMem() // one final sample at scan completion
				return
			}
		}
	}()

	// progressCallbacks counts how often SetSyncProgress was invoked.
	// With nScanBlocks=1100 and the internal syncProgressInterval=1000,
	// the callback must fire at least once.
	var progressCallbacks atomic.Int64

	// ── Run the production scan ───────────────────────────────────────────
	var snapWg sync.WaitGroup
	_, err := runStartupScan(startupScanParams{
		DataDir:     dir,
		TipHeight:   tip.Header.Height,
		TipHashHex:  tipHashHex,
		DB:          db,
		UTXOs:       utxos,
		Registry:    registry,
		KiFromIndex: false,
		InitTxTotal: 0,
		Log:         discardLog(),
		SnapshotWg:  &snapWg,
		// SetSyncProgress is called every 1 000 blocks — use it as an
		// additional deterministic memory sample point and count invocations.
		SetSyncProgress: func(_, _ uint64) {
			progressCallbacks.Add(1)
			sampleMem()
		},
	})

	// Stop the background monitor before asserting.
	close(stopMonitor)
	monitorWg.Wait()
	snapWg.Wait()

	if err != nil {
		t.Fatalf("runStartupScan: %v", err)
	}

	// ── Assert SetSyncProgress was actually invoked ───────────────────────
	gotCallbacks := progressCallbacks.Load()
	if gotCallbacks == 0 {
		t.Errorf(
			"SetSyncProgress was never called during the scan (chain has %d blocks; "+
				"the internal syncProgressInterval is 1 000 blocks — "+
				"nScanBlocks must be > 1 000 for at least one invocation)",
			nScanBlocks,
		)
	} else {
		t.Logf("SetSyncProgress invoked %d time(s) during the scan", gotCallbacks)
	}

	// ── Assert memory ceiling ─────────────────────────────────────────────
	peakMiB := peakSys.Load() / (1024 * 1024)
	t.Logf("peak Sys during startup scan: %d MiB  (ceiling: %d MiB, outputs scanned: %d)",
		peakMiB, scanMemCeilingMiB, nScanBlocks*outputsPerBlock)
	if peakMiB > scanMemCeilingMiB {
		t.Errorf(
			"startup scan exceeded memory ceiling: peak Sys = %d MiB, limit = %d MiB\n"+
				"The scan loop may be holding large allocations across GC cycles.\n"+
				"Check for unbounded in-memory indexes added since the last passing run.",
			peakMiB, scanMemCeilingMiB,
		)
	}
}

// buildScanMemChain creates a genesis block followed by nBlocks blocks in a
// fresh LevelDB store.  Each block contains one synthetic coinbase transaction
// with outputsPerBlock outputs.  All outputs have distinct OneTimePub values
// so ApplyBlock does not skip them as duplicates.
//
// Returns the open store, the tip block, and the validator public key.
func buildScanMemChain(
	t *testing.T,
	dir string,
	nBlocks int,
	outputsPerBlock int,
) (*store.DB, *core.Block, crypto.ValidatorPubKey) {
	t.Helper()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	putBlk := func(b *core.Block) {
		raw, merr := json.Marshal(b)
		if merr != nil {
			t.Fatalf("marshal h=%d: %v", b.Header.Height, merr)
		}
		h := b.Hash()
		if perr := db.PutRawBlock(h, b.Header.Height, raw); perr != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, perr)
		}
	}

	makeBlk := func(height uint64, prev crypto.Hash32) *core.Block {
		outs := make([]core.Output, outputsPerBlock)
		for i := range outs {
			// Unique OneTimePub per output: encode (height, outputIndex) into
			// the first 8 bytes so no two outputs share the same key.
			var otp crypto.Point32
			otp[0] = byte(height)
			otp[1] = byte(height >> 8)
			otp[2] = byte(height >> 16)
			otp[3] = byte(height >> 24)
			otp[4] = byte(i)
			otp[5] = byte(i >> 8)
			otp[6] = 0x01 // non-zero marker distinguishes from zero value
			outs[i] = core.Output{
				OneTimePub: otp,
				TxPubKey:   otp,
			}
		}

		coinbase := core.Transaction{
			Version: core.TxVersionBase,
			Outputs: outs,
		}
		txs := []core.Transaction{coinbase}
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prev,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(txs),
		}
		if serr := hdr.Sign(priv); serr != nil {
			t.Fatalf("Sign h=%d: %v", height, serr)
		}
		return &core.Block{Header: hdr, Txs: txs}
	}

	// Genesis: height 0, no transactions.
	genesis := &core.Block{
		Header: core.BlockHeader{
			Height:       0,
			PrevHash:     crypto.Hash32{},
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		},
	}
	if serr := genesis.Header.Sign(priv); serr != nil {
		t.Fatalf("Sign genesis: %v", serr)
	}
	putBlk(genesis)

	parent := genesis
	var tip *core.Block
	for i := 1; i <= nBlocks; i++ {
		blk := makeBlk(uint64(i), parent.Hash())
		putBlk(blk)
		parent = blk
		tip = blk
	}

	if perr := db.PutTip(tip.Hash(), tip.Header.Height); perr != nil {
		t.Fatalf("PutTip: %v", perr)
	}

	return db, tip, pub
}
