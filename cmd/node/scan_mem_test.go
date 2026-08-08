package main

// Integration test: startup UTXO rebuild stays below a memory ceiling.
//
// Covers the requirement from task #1547:
//   - runStartupScan() is run against a synthetic chain where each block
//     carries multiple coinbase outputs so that the cumulative output counter
//     crosses gcUTXOInterval (50 000) at least once, triggering the periodic
//     runtime.GC() call inside the scan loop.
//   - A background goroutine samples runtime.MemStats.Sys at a fixed interval
//     while the scan is in progress.
//   - The SetSyncProgress callback takes an additional sample at every 1 000
//     blocks so there is always at least one sample per progress tick.
//   - After the scan the test asserts that the peak Sys value never exceeded
//     scanMemCeilingMiB × 1 MiB, failing loudly with the peak value if so.
//
// The ceiling (scanMemCeilingMiB) is set to 512 MiB, which is far below the
// production 5 GB limit and still leaves plenty of room for in-process Go
// runtime overhead on any reasonable CI host.  If that ceiling is ever
// approached it means something in the scan loop is holding large allocations
// across GC cycles and must be investigated.

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
// Chain shape: nScanBlocks blocks, each with outputsPerBlock coinbase outputs.
// Total outputs = nScanBlocks × outputsPerBlock, chosen to cross
// gcUTXOInterval (50 000) at least once so the GC-per-N-outputs mechanism is
// exercised.
func TestStartupScanMemoryCeiling(t *testing.T) {
	const (
		nScanBlocks     = 200 // blocks written to the store
		outputsPerBlock = 300 // outputs per block → 60 000 total (> gcUTXOInterval=50 000)
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
		// additional deterministic memory sample point.
		SetSyncProgress: func(_, _ uint64) { sampleMem() },
	})

	// Stop the background monitor before asserting.
	close(stopMonitor)
	monitorWg.Wait()
	snapWg.Wait()

	if err != nil {
		t.Fatalf("runStartupScan: %v", err)
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
			// Unique OneTimePub per output: encode (height, i) into the first
			// 8 bytes so no two outputs share the same key.
			var otp crypto.Point32
			otp[0] = byte(height)
			otp[1] = byte(height >> 8)
			otp[2] = byte(height >> 16)
			otp[3] = byte(height >> 24)
			otp[4] = byte(i)
			otp[5] = byte(i >> 8)
			otp[6] = 0x01 // non-zero marker distinguishes from the zero value
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
