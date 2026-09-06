package consensus

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestAVMSpamDoesNotDelayBlockProduction(t *testing.T) {
	if os.Getenv("APEROD_RUN_AVM_STRESS") != "1" {
		t.Skip("set APEROD_RUN_AVM_STRESS=1 to run the isolated AVM stress test")
	}
	duration := 30 * time.Second
	if raw := os.Getenv("APEROD_AVM_STRESS_DURATION"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < time.Second || parsed > 5*time.Minute {
			t.Fatalf("APEROD_AVM_STRESS_DURATION must be between 1s and 5m")
		}
		duration = parsed
	}
	workers := runtime.GOMAXPROCS(0)
	if workers < 4 {
		workers = 4
	}
	if raw := os.Getenv("APEROD_AVM_STRESS_WORKERS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 64 {
			t.Fatalf("APEROD_AVM_STRESS_WORKERS must be between 1 and 64")
		}
		workers = parsed
	}

	keys := []crypto.ValidatorPrivKey{validatorKey(t, 0x61), validatorKey(t, 0x62)}
	pubs := []crypto.ValidatorPubKey{keys[0].Public(), keys[1].Public()}
	root := t.TempDir()
	a := newAVMEngineNode(t, root+"/a", keys, pubs)
	b := newAVMEngineNode(t, root+"/b", keys, pubs)

	owners := make([]*crypto.WalletKeyPair, 4)
	for i := range owners {
		var err error
		owners[i], err = crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatal(err)
		}
	}
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	genesis, blinds := engineGenesis(t, keys[0], owners, 50_000_000)
	a.installGenesis(t, genesis)
	b.installGenesis(t, genesis)
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	valid, _, decoys := buildAVMCLSAG(t, genesis, owners[0], recipient, blinds[0], signer,
		core.AVMDeployContract, [32]byte{}, 0, stateWriteModule())
	_, malformedSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	malformed, _, malformedDecoys := buildAVMCLSAG(t, genesis, owners[1], recipient, blinds[1], malformedSigner,
		core.AVMDeployContract, [32]byte{}, 0, []byte{0, 'a', 's', 'm'})
	_, gasSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	outOfGas, _, gasDecoys := buildAVMCLSAG(t, genesis, owners[2], recipient, blinds[2], gasSigner,
		core.AVMDeployContract, [32]byte{}, 0, stateWriteModule())
	outOfGas.AVM.GasLimit = core.AVMMinGasLimit
	resignAVMPayload(t, outOfGas.AVM, gasSigner)
	_, oversizedSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oversized, _, oversizedDecoys := buildAVMCLSAG(t, genesis, owners[3], recipient, blinds[3], oversizedSigner,
		core.AVMDeployContract, [32]byte{}, 0, stateWriteModule())
	oversized.AVM.Code = make([]byte, core.AVMMaxCodeSize+1)
	oversized.AVM.ContractID = core.DeriveAVMContractID(oversized.AVM.Signer, oversized.AVM.Nonce, oversized.AVM.Code)
	resignAVMPayload(t, oversized.AVM, oversizedSigner)
	decoys = append(decoys, malformedDecoys...)
	decoys = append(decoys, gasDecoys...)
	decoys = append(decoys, oversizedDecoys...)
	for _, decoy := range decoys {
		utxo := &core.UTXO{OneTimePub: decoy.OneTimePub, AmountCommit: decoy.AmountCommit}
		a.utxos.Add(utxo)
		b.utxos.Add(&core.UTXO{OneTimePub: decoy.OneTimePub, AmountCommit: decoy.AmountCommit})
	}

	if err := a.engine.pool.Add(valid); err != nil {
		t.Fatalf("valid deploy admission: %v", err)
	}
	if err := b.engine.pool.Add(valid); err != nil {
		t.Fatalf("valid deploy admission on peer: %v", err)
	}
	block := engineBlock(t, 1, 1, genesis.Hash(), keys[0], []core.Transaction{valid})
	a.accept(t, block)
	b.accept(t, block)
	a.assertSame(t, b, 1, valid)

	stale := valid
	rejected := []core.Transaction{stale, malformed, outOfGas, oversized}
	for _, tx := range rejected {
		if err := a.engine.pool.Add(tx); err == nil {
			t.Fatalf("known-invalid AVM transaction entered mempool")
		}
	}

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var attempts, unexpectedAccepts atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := worker; ; n += workers {
				select {
				case <-stop:
					return
				default:
					attempts.Add(1)
					if a.engine.pool.Add(rejected[n%len(rejected)]) == nil {
						unexpectedAccepts.Add(1)
					}
				}
			}
		}(i)
	}

	targetCadence := 20 * time.Millisecond
	deadline := time.Now().Add(duration)
	previous := block
	height := uint64(1)
	var latencies []time.Duration
	started := time.Now()
	for time.Now().Before(deadline) {
		height++
		signerKey := keys[(height-1)%uint64(len(keys))]
		next := engineBlock(t, height, uint32(height), previous.Hash(), signerKey, nil)
		setBlockBaseFee(t, next, a.engine.expectedBaseFee(), signerKey)
		start := time.Now()
		a.accept(t, next)
		b.accept(t, next)
		latencies = append(latencies, time.Since(start))
		previous = next
		sleep := targetCadence - time.Since(start)
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
	close(stop)
	wg.Wait()
	elapsed := time.Since(started)

	if unexpectedAccepts.Load() != 0 {
		t.Fatalf("%d known-invalid AVM submissions entered mempool", unexpectedAccepts.Load())
	}
	if a.engine.pool.Count() != 0 || a.engine.pool.TotalBytes() != 0 {
		t.Fatalf("spam left mempool residue: count=%d bytes=%d", a.engine.pool.Count(), a.engine.pool.TotalBytes())
	}
	if !reflect.DeepEqual(a.snapshot(t), b.snapshot(t)) {
		t.Fatal("validator state diverged under AVM spam")
	}
	if a.engine.halted.Load() || b.engine.halted.Load() {
		t.Fatal("consensus engine halted under AVM spam")
	}
	// Keep the fail threshold below normal -race instrumentation overhead.
	// The non-race sustained run is the performance signal; this still catches
	// a twofold cadence regression while letting the race detector do its job.
	minBlocks := int(float64(elapsed/targetCadence) * 0.50)
	if len(latencies) < minBlocks {
		t.Fatalf("block cadence degraded: blocks=%d minimum=%d elapsed=%s", len(latencies), minBlocks, elapsed)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50, p95, p99 := percentileDuration(latencies, 50), percentileDuration(latencies, 95), percentileDuration(latencies, 99)
	if p99 > 500*time.Millisecond {
		t.Fatalf("p99 two-validator commit latency %s exceeds 500ms", p99)
	}
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	heapDelta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("AVM_STRESS_SUMMARY duration=%s workers=%d attempts=%d blocks=%d blocks_per_sec=%.2f p50=%s p95=%s p99=%s heap_delta_bytes=%d",
		elapsed.Round(time.Millisecond), workers, attempts.Load(), len(latencies),
		float64(len(latencies))/elapsed.Seconds(), p50, p95, p99, heapDelta)
}

func resignAVMPayload(t *testing.T, payload *core.AVMPayload, private ed25519.PrivateKey) {
	t.Helper()
	hash := payload.SigningHash()
	copy(payload.Signature[:], ed25519.Sign(private, hash[:]))
}

func percentileDuration(values []time.Duration, percentile int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := (len(values)*percentile + 99) / 100
	if index < 1 {
		index = 1
	}
	return values[index-1]
}
