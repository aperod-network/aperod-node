package consensus

// This is a two-engine integration network, not an OS-process/P2P benchmark.
// Nodes have independent LevelDB/UTXO/mempool/AVM state. Public submissions use
// production HTTP middleware; private loopback HTTP carries peer txs and blocks.
// The client refuses non-loopback destinations, and all keys/funding are synthetic.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

const (
	httpStressPoolCount = 8
	httpStressPoolBytes = 24 << 10
	httpStressCadence   = 3 * time.Second // normal production target, not an accelerated benchmark
)

type httpStressCase struct {
	name string
	body []byte
}

type httpStressStats struct {
	mu                         sync.Mutex
	attempts, accepted         uint64
	rejected, throttled, other uint64
	latency                    []time.Duration // excludes 429: actual admission path
}

func (s *httpStressStats) record(code int, elapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	switch code {
	case http.StatusAccepted:
		s.accepted++
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		s.rejected++
	case http.StatusTooManyRequests:
		s.throttled++
	default:
		s.other++
	}
	if code != http.StatusTooManyRequests {
		s.latency = append(s.latency, elapsed)
	}
}

func stressRequest(client *http.Client, endpoint, ip string, body []byte) (int, string) {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	if ip != "" {
		// Model a bounded set of origins behind the trusted loopback proxy.
		req.Header.Set("X-Forwarded-For", ip)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	value, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return 0, err.Error()
	}
	return resp.StatusCode, string(value)
}

func stressJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func stressRSS() (uint64, error) {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0, fmt.Errorf("invalid statm")
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	return pages * uint64(os.Getpagesize()), err
}

func stressCPU() (float64, error) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, err
	}
	return float64(usage.Utime.Sec+usage.Stime.Sec) +
		float64(usage.Utime.Usec+usage.Stime.Usec)/1e6, nil
}

func TestAVMMixedHTTPStress(t *testing.T) {
	if os.Getenv("APEROD_RUN_AVM_STRESS") != "1" {
		t.Skip("isolated stress test requires APEROD_RUN_AVM_STRESS=1")
	}
	duration := 2 * time.Minute
	if raw := os.Getenv("APEROD_AVM_STRESS_DURATION"); raw != "" {
		var err error
		duration, err = time.ParseDuration(raw)
		if err != nil || duration < 2*time.Second || duration > 5*time.Minute {
			t.Fatal("APEROD_AVM_STRESS_DURATION must be between 2s and 5m")
		}
	}
	workers := 12
	if raw := os.Getenv("APEROD_AVM_STRESS_WORKERS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 6 || n > 32 {
			t.Fatal("HTTP stress workers must be between 6 and 32")
		}
		workers = n
	}
	keys := []crypto.ValidatorPrivKey{validatorKey(t, 0x71), validatorKey(t, 0x72)}
	pubs := []crypto.ValidatorPubKey{keys[0].Public(), keys[1].Public()}
	root := t.TempDir()
	nodes := []*avmEngineNode{
		newAVMEngineNode(t, root+"/a", keys, pubs),
		newAVMEngineNode(t, root+"/b", keys, pubs),
	}
	for i, node := range nodes {
		locked, err := crypto.NewLockedValidatorKey(keys[i].Bytes(), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(locked.Destroy)
		node.engine.cfg.MyKey = locked
		cfg := core.DefaultMempoolConfig()
		cfg.MaxSize, cfg.MaxBytes = httpStressPoolCount, httpStressPoolBytes
		cfg.CurrentHeight = node.chain.Height
		cfg.RingCTV4ActivationHeight, cfg.RingCTCLSAGActivationHeight, cfg.AVMActivationHeight = 1, 1, 1
		cfg.Verifier = core.NewTxVerifier(node.utxos)
		state := avm.LevelStore{DB: node.db}
		cfg.AVMNonceLookup = func(signer [32]byte) (uint64, error) { return avm.SignerNonce(state, signer) }
		cfg.AVMAdmissionCheck = func(p *core.AVMPayload) error { return avm.ValidateMempoolAdmission(state, p) }
		node.engine.pool = core.NewMempool(cfg)
	}

	// Enough independent, genuinely funded transactions to submit fresh valid
	// work across the entire run; do not count duplicate replays as valid load.
	const positiveCount = 64
	owners := make([]*crypto.WalletKeyPair, positiveCount+5)
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
	genesis, blinds := engineGenesis(t, keys[0], owners, 2_000_000_000)
	for _, node := range nodes {
		node.installGenesis(t, genesis)
	}
	build := func(owner int, signer ed25519.PrivateKey, action core.AVMAction, contract [32]byte, nonce uint64, code []byte, gas uint64) core.Transaction {
		tx, _, decoys := buildAVMCLSAG(t, genesis, owners[owner], recipient, blinds[owner],
			signer, action, contract, nonce, code, func(p *core.AVMPayload) { p.GasLimit = gas })
		for _, d := range decoys {
			for _, node := range nodes {
				node.utxos.Add(&core.UTXO{OneTimePub: d.OneTimePub, AmountCommit: d.AmountCommit})
			}
		}
		// Controls must have valid wallet signatures, even when execution/nonce
		// will fail. Mutating AVM fields after CLSAG signing is not such a control.
		if err := core.NewTxVerifier(nodes[0].utxos).VerifyTxAtHeight(&tx, 1); err != nil {
			t.Fatalf("invalid cryptographic fixture: %v", err)
		}
		return tx
	}
	newSigner := func() ed25519.PrivateKey {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	seedSigner := newSigner()
	seed := build(0, seedSigner, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(), 1000)
	positives := make([]core.Transaction, positiveCount)
	for i := range positives {
		positives[i] = build(i+5, newSigner(), core.AVMDeployContract, [32]byte{}, 0,
			stateWriteModule(), core.AVMMaxBlockGas/2+1)
	}
	stale := build(1, seedSigner, core.AVMExecuteContract, seed.AVM.ContractID, 0, nil, 1000)
	future := build(2, newSigner(), core.AVMDeployContract, [32]byte{}, 2, stateWriteModule(), 1000)
	malformed := build(3, newSigner(), core.AVMDeployContract, [32]byte{}, 0, []byte{0, 'a', 's', 'm'}, 1000)
	outOfGas := build(4, newSigner(), core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(), core.AVMMinGasLimit)
	var oversized core.Transaction
	if err := json.Unmarshal(stressJSON(t, positives[0]), &oversized); err != nil {
		t.Fatal(err)
	}
	oversized.AVM.Code = make([]byte, core.AVMMaxCodeSize+1) // structural rejection, before crypto
	cases := []httpStressCase{
		{"stale_nonce", stressJSON(t, map[string]any{"tx": stale})},
		{"future_nonce", stressJSON(t, map[string]any{"tx": future})},
		{"malformed_wasm", stressJSON(t, map[string]any{"tx": malformed})},
		{"out_of_gas", stressJSON(t, map[string]any{"tx": outOfGas})},
		{"oversized_code", stressJSON(t, map[string]any{"tx": oversized})},
		{"http_body_limit", bytes.Repeat([]byte(" "), (4<<20)+1)},
	}

	dialer := &net.Dialer{Timeout: time.Second}
	transport := &http.Transport{MaxConnsPerHost: workers + 8, MaxIdleConnsPerHost: workers + 8,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(address)
			if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
				return nil, fmt.Errorf("stress client refuses non-loopback destination")
			}
			return dialer.DialContext(ctx, network, address)
		}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	servers := make([]*httptest.Server, 2)
	for i, node := range nodes {
		public := api.NewServer("", node.chain, node.engine.pool, node.utxos, testNopLogger())
		public.SetAVMStore(1, avm.LevelStore{DB: node.db})
		public.SetReady()
		mux := http.NewServeMux()
		mux.Handle("/api/", api.CORSConfig{}.Middleware(api.NewRateLimiter().Middleware(public)))
		mux.HandleFunc("/peer/tx", func(w http.ResponseWriter, r *http.Request) {
			var tx core.Transaction
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&tx); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if err := node.engine.pool.Add(tx); err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			w.WriteHeader(202)
		})
		mux.HandleFunc("/peer/block", func(w http.ResponseWriter, r *http.Request) {
			var block core.Block
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&block); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if err := node.engine.handleIncomingBlock(&block); err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			w.WriteHeader(204)
		})
		servers[i] = httptest.NewServer(mux)
		t.Cleanup(servers[i].Close)
		peerIndex := 1 - i
		public.SetTransactionBroadcaster(func(tx *core.Transaction) {
			// No production P2P host or external peer is configured here.
			raw, _ := json.Marshal(tx)
			stressRequest(client, servers[peerIndex].URL+"/peer/tx", "", raw)
		})
	}
	var blockLatency, blockGaps []time.Duration
	var maxBlockGas uint64
	gasSelectionExercised := false
	produce := func(measure bool) {
		t.Helper()
		start := time.Now()
		height := nodes[0].chain.Height() + 1
		expected := nodes[0].engine.proposerAt(uint32(height))
		producer := nodes[0]
		if expected.Equals(keys[1].Public()) {
			producer = nodes[1]
		}
		backlog := producer.engine.pool.Count()
		block, err := producer.engine.produceBlock(height, height, nodes[0].chain.Tip())
		if err != nil {
			t.Fatal(err)
		}
		var gas uint64
		for _, tx := range block.Txs {
			if tx.AVM != nil {
				gas += tx.AVM.GasLimit
			}
		}
		if gas > core.AVMMaxBlockGas {
			t.Fatal("producer exceeded protocol block-gas limit")
		}
		if gas > maxBlockGas {
			maxBlockGas = gas
		}
		if backlog > 1 && gas == core.AVMMaxBlockGas/2+1 {
			gasSelectionExercised = true
		}
		for _, server := range servers {
			if status, body := stressRequest(client, server.URL+"/peer/block", "", stressJSON(t, block)); status != 204 {
				t.Fatalf("peer block rejected: %d %s", status, body)
			}
		}
		if measure {
			blockLatency = append(blockLatency, time.Since(start))
		}
	}
	if status, body := stressRequest(client, servers[0].URL+"/api/v1/avm/transactions", "192.0.2.1",
		stressJSON(t, map[string]any{"tx": seed})); status != 202 {
		t.Fatalf("valid seed rejected: %d %s", status, body)
	}
	produce(false)
	for n := range nodes {
		for i, c := range cases {
			status, body := stressRequest(client, servers[n].URL+"/api/v1/avm/transactions",
				fmt.Sprintf("198.51.100.%d", n*len(cases)+i+1), c.body)
			expected := 422
			if c.name == "http_body_limit" {
				expected = 400
			}
			if status != expected {
				t.Fatalf("%s control on node %d: status=%d body=%s", c.name, n, status, body)
			}
			if (c.name == "malformed_wasm" || c.name == "out_of_gas") && !strings.Contains(body, "AVM admission rejected") {
				t.Fatalf("%s did not reach dynamic AVM validation: %s", c.name, body)
			}
			if strings.Contains(c.name, "nonce") && !strings.Contains(body, "nonce") {
				t.Fatalf("nonce control rejected for wrong reason: %s", body)
			}
		}
	}
	// Baseline uses exactly the same producer + peer validation/commit path.
	var baseline []time.Duration
	for i := 0; i < 10; i++ {
		start := time.Now()
		produce(false)
		baseline = append(baseline, time.Since(start))
		time.Sleep(httpStressCadence)
	}
	stats := make([]*httpStressStats, len(cases))
	for i := range stats {
		stats[i] = &httpStressStats{}
	}
	positiveStats := &httpStressStats{}
	positiveBodies := make([][]byte, len(positives))
	for i, tx := range positives {
		positiveBodies[i] = stressJSON(t, map[string]any{"tx": tx})
	}
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	startRSS, err := stressRSS()
	if err != nil {
		t.Fatal(err)
	}
	cpuBefore, err := stressCPU()
	if err != nil {
		t.Fatal(err)
	}
	peakRSS, peakHeap, peakCount, peakBytes := startRSS, before.HeapAlloc, 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			c := cases[worker%len(cases)]
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					start := time.Now()
					code, _ := stressRequest(client, servers[(worker/len(cases))%2].URL+"/api/v1/avm/transactions",
						fmt.Sprintf("203.0.113.%d", worker+1), c.body)
					stats[worker%len(cases)].record(code, time.Since(start))
				}
			}
		}(i)
	}
	defer func() { cancel(); wg.Wait() }()
	resourceError := make(chan string, 1)
	sample := func() error {
		rss, err := stressRSS()
		if err != nil {
			return err
		}
		if rss > peakRSS {
			peakRSS = rss
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > peakHeap {
			peakHeap = m.HeapAlloc
		}
		for _, n := range nodes {
			count, bytes := n.engine.pool.Count(), n.engine.pool.TotalBytes()
			if count > peakCount {
				peakCount = count
			}
			if bytes > peakBytes {
				peakBytes = bytes
			}
			if count > httpStressPoolCount || bytes > httpStressPoolBytes {
				return fmt.Errorf("mempool cap exceeded count=%d bytes=%d", count, bytes)
			}
		}
		return nil
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := sample(); err != nil {
					resourceError <- err.Error()
					return
				}
			}
		}
	}()
	started := time.Now()
	// Deliberately saturate the lowered test pool limits, then continue fresh
	// valid traffic throughout the soak. These are fixture caps, not a claim
	// to have filled the production 256-MiB pool in this 8-GiB workspace.
	// Keep client-side signing/submission off the block-production scheduler;
	// otherwise time waiting for an HTTP response would fake a consensus stall.
	positiveError := make(chan string, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(duration / (positiveCount - 12))
		defer tick.Stop()
		for index, raw := range positiveBodies {
			if index >= 16 {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			start := time.Now()
			code, body := stressRequest(client, servers[index%2].URL+"/api/v1/avm/transactions",
				fmt.Sprintf("192.0.2.%d", index+2), raw)
			positiveStats.record(code, time.Since(start))
			if code != 202 {
				positiveError <- fmt.Sprintf("valid submission failed: %d %s", code, body)
				return
			}
		}
	}()
	lastBlock := time.Now()
	for time.Since(started) < duration {
		slot := time.Now()
		produce(true)
		now := time.Now()
		blockGaps = append(blockGaps, now.Sub(lastBlock))
		lastBlock = now
		if sleep := httpStressCadence - time.Since(slot); sleep > 0 {
			time.Sleep(sleep)
		}
	}
	cancel()
	wg.Wait()
	select {
	case err := <-positiveError:
		t.Fatal(err)
	default:
	}
	select {
	case err := <-resourceError:
		t.Fatal(err)
	default:
	}
	elapsed := time.Since(started)
	cpuAfter, err := stressCPU()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < httpStressPoolCount+1 && (nodes[0].engine.pool.Count() > 0 || nodes[1].engine.pool.Count() > 0); i++ {
		produce(false)
	}
	if !reflect.DeepEqual(nodes[0].snapshot(t), nodes[1].snapshot(t)) {
		t.Fatal("independent validator states diverged")
	}
	for _, n := range nodes {
		if n.engine.halted.Load() || n.engine.pool.Count() != 0 || n.engine.pool.TotalBytes() != 0 {
			t.Fatal("node halted or retained mempool residue after drain")
		}
	}
	confirmed := 0
	for _, tx := range positives {
		h := tx.Hash()
		key := append([]byte{0xff, 'a', 'v', 'm', '/', 'r', '/'}, h[:]...)
		_, found, err := (avm.LevelStore{DB: nodes[0].db}).Get(key)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			confirmed++
		}
	}
	var total uint64
	for i, stat := range stats {
		if stat.attempts == 0 || stat.rejected == 0 || stat.throttled == 0 || stat.accepted != 0 || stat.other != 0 {
			t.Fatalf("invalid class %s not safely exercised: %+v", cases[i].name, stat)
		}
		sort.Slice(stat.latency, func(i, j int) bool { return stat.latency[i] < stat.latency[j] })
		p95, p99 := percentileDuration(stat.latency, 95), percentileDuration(stat.latency, 99)
		if p99 > time.Second {
			t.Fatalf("%s admission p99=%s exceeds 1s", cases[i].name, p99)
		}
		total += stat.attempts
		t.Logf("AVM_HTTP_CLASS name=%s attempts=%d rejected=%d throttled=%d admission_p95=%s admission_p99=%s",
			cases[i].name, stat.attempts, stat.rejected, stat.throttled, p95, p99)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	sort.Slice(blockLatency, func(i, j int) bool { return blockLatency[i] < blockLatency[j] })
	sort.Slice(blockGaps, func(i, j int) bool { return blockGaps[i] < blockGaps[j] })
	sort.Slice(baseline, func(i, j int) bool { return baseline[i] < baseline[j] })
	sort.Slice(positiveStats.latency, func(i, j int) bool { return positiveStats.latency[i] < positiveStats.latency[j] })
	blockP99 := percentileDuration(blockLatency, 99)
	gapP99 := percentileDuration(blockGaps, 99)
	cpuCores := (cpuAfter - cpuBefore) / elapsed.Seconds()
	if confirmed < 5 || !gasSelectionExercised || peakCount < 4 {
		t.Fatalf("insufficient positive/capacity controls: confirmed=%d gas_selection=%v peak_count=%d", confirmed, gasSelectionExercised, peakCount)
	}
	if len(blockLatency) < int(elapsed/httpStressCadence)*95/100 || blockP99 > 500*time.Millisecond || gapP99 > httpStressCadence+500*time.Millisecond || blockGaps[len(blockGaps)-1] > 2*httpStressCadence {
		t.Fatalf("block cadence failed: blocks=%d elapsed=%s p99_commit=%s p99_gap=%s", len(blockLatency), elapsed, blockP99, gapP99)
	}
	if positiveStats.other != 0 || percentileDuration(positiveStats.latency, 99) > time.Second {
		t.Fatal("valid admission failed latency/error bound")
	}
	if peakRSS > startRSS+(256<<20) || int64(after.HeapAlloc)-int64(before.HeapAlloc) > 64<<20 || cpuCores > float64(runtime.GOMAXPROCS(0))+0.5 {
		t.Fatalf("process resource bound exceeded: rss_start=%d rss_peak=%d retained_heap_delta=%d cpu_cores=%.2f",
			startRSS, peakRSS, int64(after.HeapAlloc)-int64(before.HeapAlloc), cpuCores)
	}
	t.Logf("AVM_HTTP_STRESS_SUMMARY duration=%s nodes=2 transport=loopback_http target_cadence=%s workers=%d attempts=%d requests_per_sec=%.2f valid_accepted=%d valid_confirmed=%d valid_p95=%s valid_p99=%s blocks=%d blocks_per_sec=%.2f baseline_commit_p99=%s commit_p95=%s commit_p99=%s gap_p99=%s gap_max=%s cpu_seconds=%.3f avg_cpu_cores=%.3f rss_start=%d rss_peak=%d heap_peak=%d retained_heap_delta=%d mempool_peak_count=%d/%d mempool_peak_bytes=%d/%d block_reserved_gas_peak=%d/%d",
		elapsed.Round(time.Millisecond), httpStressCadence, workers, total+positiveStats.attempts, float64(total+positiveStats.attempts)/elapsed.Seconds(),
		positiveStats.accepted, confirmed, percentileDuration(positiveStats.latency, 95), percentileDuration(positiveStats.latency, 99),
		len(blockLatency), float64(len(blockLatency))/elapsed.Seconds(), percentileDuration(baseline, 99),
		percentileDuration(blockLatency, 95), blockP99, gapP99, blockGaps[len(blockGaps)-1], cpuAfter-cpuBefore, cpuCores, startRSS, peakRSS, peakHeap,
		int64(after.HeapAlloc)-int64(before.HeapAlloc), peakCount, httpStressPoolCount, peakBytes, httpStressPoolBytes, maxBlockGas, core.AVMMaxBlockGas)
}
