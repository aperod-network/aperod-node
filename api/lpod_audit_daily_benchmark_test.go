// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// Opt-in full-path synthetic load benchmark. Example:
//
//	cd blockchain
//	LPOD_DAILY_BENCH=1 LPOD_DAILY_BENCH_BLOCKS=100 go test ./api -run '^$' -bench '^BenchmarkLPoDAuditDailySynthetic$' -benchtime=1x
//	/usr/bin/time -v env LPOD_DAILY_BENCH=1 LPOD_DAILY_BENCH_BLOCKS=28800 go test ./api -run '^$' -bench '^BenchmarkLPoDAuditDailySynthetic$' -benchtime=1x
//	LPOD_DAILY_BENCH=1 LPOD_DAILY_BENCH_BLOCKS=28800 LPOD_DAILY_BENCH_VAULTS=16 LPOD_DAILY_BENCH_POSITIONS=64 LPOD_DAILY_BENCH_FILLER_TXS=2 go test ./api -run '^$' -bench '^BenchmarkLPoDAuditDailySynthetic$' -benchtime=1x
//
// For 28,800 blocks the generated timestamps are exactly three seconds apart.
// Smaller calibration sizes are evenly spread over the same Moscow 20:00-to-20:00
// day so the authenticated route still exercises a complete report.
// Vault rows and transaction body bytes are configurable. Position rows are
// configurable up to 64 per block. The position variant uses real signed
// deposit transactions and operator-attested registry snapshots so the route's
// position and APR verifier can complete against the synthetic fixture.
func BenchmarkLPoDAuditDailySynthetic(b *testing.B) {
	if os.Getenv("LPOD_DAILY_BENCH") != "1" {
		b.Skip("set LPOD_DAILY_BENCH=1 to build and run the synthetic database")
	}
	blockCount := benchmarkEnvInt(b, "LPOD_DAILY_BENCH_BLOCKS", 28800)
	if blockCount < 1 || blockCount+2 > int(lpodDailyMaxBlocks) {
		b.Fatalf("LPOD_DAILY_BENCH_BLOCKS must be in [1,%d]", lpodDailyMaxBlocks-2)
	}
	fillerCount := benchmarkEnvInt(b, "LPOD_DAILY_BENCH_FILLER_TXS", 2)
	extraBytes := benchmarkEnvInt(b, "LPOD_DAILY_BENCH_TX_EXTRA_BYTES", 192)
	vaultCount := benchmarkEnvInt(b, "LPOD_DAILY_BENCH_VAULTS", 1)
	positionCount := benchmarkEnvInt(b, "LPOD_DAILY_BENCH_POSITIONS", 0)
	if fillerCount < 0 || fillerCount > 32 || extraBytes < 0 || extraBytes > 255 ||
		vaultCount < 0 || vaultCount > 128 || positionCount < 0 || positionCount > 64 ||
		(positionCount > 0 && vaultCount == 0) {
		b.Fatal("filler txs must be 0..32, tx extra bytes 0..255, vaults 0..128, and positions 0..64 (positions require a vault)")
	}

	// Optional two-stage execution keeps fixture construction outside the timed
	// scan and permits separate bounded processes. Configuration is persisted and
	// checked before reuse; the full verifier still authenticates every block.
	var db *store.DB
	var server *Server
	var path string
	var rawBlockBytes int64
	configuration := []int{blockCount, fillerCount, extraBytes, vaultCount, positionCount}
	if os.Getenv("LPOD_DAILY_BENCH_REUSE_FIXTURE") == "1" {
		path = os.Getenv("LPOD_DAILY_BENCH_FIXTURE_DIR")
		if path == "" {
			b.Fatal("fixture reuse requires LPOD_DAILY_BENCH_FIXTURE_DIR")
		}
		var err error
		db, err = store.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		raw, err := db.GetMeta("benchmark/daily-fixture/v1")
		var saved struct {
			Configuration []int
			RawBlockBytes int64
		}
		if err != nil || json.Unmarshal(raw, &saved) != nil || fmt.Sprint(saved.Configuration) != fmt.Sprint(configuration) {
			db.Close()
			b.Fatal("fixture configuration missing or different")
		}
		rawBlockBytes = saved.RawBlockBytes
		server = NewServer(":0", nil, nil, nil, slog.Default())
		server.SetStore(db)
		server.SetAPIKey(dailyAuditBenchmarkAPIKey)
		server.SetLPoDConfig(nil, func(uint64, crypto.Hash32) bool { return true })
		benchmarkDailySigningKey(b, server)
	} else {
		db, server, path, rawBlockBytes = makeDailyAuditBenchmarkFixture(b, blockCount, fillerCount, extraBytes, vaultCount, 0, positionCount)
		raw, err := json.Marshal(struct {
			Configuration []int
			RawBlockBytes int64
		}{configuration, rawBlockBytes})
		if err != nil {
			b.Fatal(err)
		}
		if err := db.PutMetaSync("benchmark/daily-fixture/v1", raw); err != nil {
			b.Fatal(err)
		}
	}
	defer db.Close()
	diskBytes, err := directoryBytes(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("synthetic fixture: blocks=%d vault-rows/block=%d position-rows/block=%d filler-txs/block=%d raw-block-bytes=%d fixture-db-bytes=%d",
		blockCount, vaultCount, positionCount, fillerCount, rawBlockBytes, diskBytes)
	if os.Getenv("LPOD_DAILY_BENCH_FIXTURE_ONLY") == "1" {
		var usage syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
			b.Fatal(err)
		}
		b.Logf("fixture-only construction complete: peak-rss-kb=%d", usage.Maxrss)
		b.Skip("fixture-only stage; no verification result claimed")
	}
	// These checks use the same authenticated test server/fixture as the
	// benchmark and guard the request cancellation and single-flight behavior.
	cancelled := authenticatedDailyAuditRequest()
	cancelCtx, cancel := context.WithCancel(cancelled.Context())
	cancel()
	cancelled = cancelled.WithContext(cancelCtx)
	cancelRecorder := httptest.NewRecorder()
	server.ServeHTTP(cancelRecorder, cancelled)
	if cancelRecorder.Code != http.StatusOK || !strings.Contains(cancelRecorder.Body.String(), "request was cancelled") {
		b.Fatalf("cancelled request status=%d body=%s", cancelRecorder.Code, cancelRecorder.Body.String())
	}

	var beforeUsage syscall.Rusage
	if err := db.PutMetaSync(dailyJobKey, nil); err != nil {
		b.Fatal(err)
	}
	if os.Getenv("LPOD_DAILY_BENCH_EVICT") == "1" {
		if os.Getenv("LPOD_DAILY_BENCH_REUSE_FIXTURE") != "1" {
			b.Fatal("cache eviction requires a reused synthetic fixture")
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
		files, bytes, err := evictDailyBenchmarkFiles(path)
		if err != nil {
			b.Fatal(err)
		}
		b.Logf("per-file DONTNEED requested: files=%d bytes=%d; advisory, not physical-media proof", files, bytes)
		db, err = store.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		defer db.Close()
		server.SetStore(db)
	}
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &beforeUsage); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recorder := httptest.NewRecorder()
		stopProfile := startDailyAuditProfile(b)
		server.ServeHTTP(recorder, authenticatedDailyAuditRequest())
		var maxBatch time.Duration
		var batches int
		for {
			j, err := server.loadDailyJob()
			if err != nil || j == nil {
				b.Fatalf("load background job: %v", err)
			}
			if j.State != "queued" {
				break
			}
			started := time.Now()
			server.runDailyJobBatch(context.Background())
			elapsed := time.Since(started)
			if elapsed > maxBatch {
				maxBatch = elapsed
			}
			batches++
			if batches > blockCount+4 {
				b.Fatal("background job failed to make progress")
			}
			// The real worker waits for a one-second ticker; this benchmark measures
			// active work, not the scheduler's idle time.
		}
		b.ReportMetric(float64(maxBatch.Microseconds())/1000, "max-batch-ms")
		b.ReportMetric(float64(batches), "batches")
		if stopProfile != nil {
			b.StopTimer()
			stopProfile()
			b.StartTimer()
		}
		recorder = httptest.NewRecorder()
		server.ServeHTTP(recorder, authenticatedDailyAuditRequest())
		if recorder.Code != http.StatusOK {
			b.Fatalf("audit HTTP status=%d: %s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Status    string `json:"status"`
			Reason    string `json:"reason"`
			Positions []struct {
				Accrued string `json:"accrued_napro"`
			} `json:"positions"`
			Coverage struct {
				Blocks    int `json:"blocks"`
				Positions int `json:"positions"`
			} `json:"coverage"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			b.Fatal(err)
		}
		if response.Status != "complete" || response.Coverage.Blocks != blockCount ||
			response.Coverage.Positions != positionCount || len(response.Positions) != positionCount {
			b.Fatalf("daily audit status=%q blocks=%d positions=%d reason=%q (want complete/%d positions/%d)",
				response.Status, response.Coverage.Blocks, response.Coverage.Positions, response.Reason,
				blockCount, positionCount)
		}
		positiveAccrual := false
		for _, position := range response.Positions {
			accrued, parseErr := strconv.ParseUint(position.Accrued, 10, 64)
			if parseErr != nil {
				b.Fatalf("invalid aggregated position accrual %q: %v", position.Accrued, parseErr)
			}
			positiveAccrual = positiveAccrual || accrued > 0
		}
		if positionCount > 0 && !positiveAccrual {
			b.Fatal("position scenario completed without any positive accrued position row")
		}
		if err := db.PutMetaSync(dailyJobKey, nil); err != nil {
			b.Fatal(err)
		}
	}
	var afterUsage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &afterUsage); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(blockCount), "interval-blocks")
	b.ReportMetric(float64(rawBlockBytes), "raw-block-bytes")
	b.ReportMetric(float64(diskBytes), "fixture-db-bytes")
	b.ReportMetric(float64(afterUsage.Maxrss), "peak-rss-kb")
	b.ReportMetric(float64(afterUsage.Inblock-beforeUsage.Inblock), "block-input-ops")
	b.ReportMetric(float64(vaultCount), "vault-rows-per-block")
	b.ReportMetric(float64(fillerCount), "filler-txs-per-block")
	b.ReportMetric(float64(extraBytes), "tx-extra-bytes")
	b.ReportMetric(float64(positionCount), "position-rows-per-block")
}

func startDailyAuditProfile(b *testing.B) func() {
	b.Helper()
	dir := os.Getenv("LPOD_DAILY_BENCH_PROFILE_DIR")
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatalf("create profile directory: %v", err)
	}
	writeAllocs := func(name string) {
		runtime.GC()
		file, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			b.Fatalf("create allocation profile: %v", err)
		}
		if err := pprof.Lookup("allocs").WriteTo(file, 0); err != nil {
			file.Close()
			b.Fatalf("write allocation profile: %v", err)
		}
		if err := file.Close(); err != nil {
			b.Fatalf("close allocation profile: %v", err)
		}
	}
	writeAllocs("handler-allocs-before.pprof")
	cpuFile, err := os.Create(filepath.Join(dir, "handler-cpu.pprof"))
	if err != nil {
		b.Fatalf("create CPU profile: %v", err)
	}
	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		cpuFile.Close()
		b.Fatalf("start CPU profile: %v", err)
	}
	b.Logf("profiling the real ServeHTTP request; profiles are in %s", dir)
	return func() {
		pprof.StopCPUProfile()
		if err := cpuFile.Close(); err != nil {
			b.Fatalf("close CPU profile: %v", err)
		}
		writeAllocs("handler-allocs-after.pprof")
	}
}

const dailyAuditBenchmarkAPIKey = "synthetic-daily-audit-benchmark-key"

func TestDailyAuditBoundarySearchDoesNotReadPrunedPreIntervalBody(t *testing.T) {
	db, server, path, _ := makeDailyAuditBenchmarkFixture(t, 2, 0, 0, 1, 8, 0)
	prunedHash, found, err := db.GetCanonicalHash(2)
	if err != nil || !found {
		t.Fatalf("pre-interval canonical hash: found=%v err=%v", found, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rawDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Put(append([]byte("b/"), prunedHash[:]...), []byte(`{"pruned":true}`), nil); err != nil {
		rawDB.Close()
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	server.SetStore(db)

	recorder := httptest.NewRecorder()
	server.restLPoDAuditDaily(recorder, authenticatedDailyAuditRequest())
	var response struct {
		Status   string `json:"status"`
		Reason   string `json:"reason"`
		Coverage struct {
			Blocks int `json:"blocks"`
		} `json:"coverage"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode daily audit response: %v (%s)", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusOK || response.Status != "complete" || response.Coverage.Blocks != 2 {
		t.Fatalf("pruned pre-interval body affected audit: HTTP %d status=%q blocks=%d reason=%q",
			recorder.Code, response.Status, response.Coverage.Blocks, response.Reason)
	}
}

func TestDailyAuditTimestampMisorderInCertifiedRangeFailsClosed(t *testing.T) {
	db, server, _, _ := makeDailyAuditBenchmarkFixture(t, 2, 0, 0, 1, 8, 0, true)
	t.Cleanup(func() { db.Close() })
	recorder := httptest.NewRecorder()
	server.restLPoDAuditDaily(recorder, authenticatedDailyAuditRequest())
	var response struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode daily audit response: %v (%s)", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusOK || response.Status != "incomplete" ||
		!strings.Contains(response.Reason, "discontinuous") {
		t.Fatalf("timestamp misorder was not rejected: HTTP %d status=%q reason=%q",
			recorder.Code, response.Status, response.Reason)
	}
}

func authenticatedDailyAuditRequest() *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/lpod/audit/daily?date=2024-01-02", nil)
	request.Host = "127.0.0.1"
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-API-Key", dailyAuditBenchmarkAPIKey)
	return request
}

func benchmarkEnvInt(b *testing.B, name string, fallback int) int {
	b.Helper()
	text := os.Getenv(name)
	if text == "" {
		return fallback
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		b.Fatalf("%s must be an integer: %v", name, err)
	}
	return value
}

func makeDailyAuditBenchmarkFixture(
	b testing.TB,
	includedBlocks, fillerCount, extraBytes, vaultCount int,
	preludeBlocks, positionCount int,
	misorderFirstIncluded ...bool,
) (*store.DB, *Server, string, int64) {
	return makeDailyAuditBenchmarkFixtureWithTail(b, includedBlocks, fillerCount, extraBytes, vaultCount,
		preludeBlocks, positionCount, 0, misorderFirstIncluded...)
}

// A certified tail lets tests advance the oracle while the interval verifier
// is still processing its persisted prefix, including across oracle eviction.
func makeDailyAuditBenchmarkFixtureWithTail(
	b testing.TB,
	includedBlocks, fillerCount, extraBytes, vaultCount int,
	preludeBlocks, positionCount, tailBlocks int,
	misorderFirstIncluded ...bool,
) (*store.DB, *Server, string, int64) {
	b.Helper()
	if preludeBlocks < 0 || tailBlocks < 0 {
		b.Fatal("prelude block count cannot be negative")
	}
	path := filepath.Join(b.TempDir(), "synthetic-lpod-audit")
	if directory := os.Getenv("LPOD_DAILY_BENCH_FIXTURE_DIR"); directory != "" {
		if _, err := os.Stat(directory); !os.IsNotExist(err) {
			b.Fatal("explicit fixture directory must not already exist")
		}
		path = directory
	}
	// Bulk fixture construction is not a production DB workload. Keep the batch
	// and memtable bounded, but avoid repeatedly compacting tiny 4 MiB tables
	// across gigabytes of randomly hash-keyed checkpoint/audit data.
	rawDB, err := leveldb.OpenFile(path, &opt.Options{
		WriteBuffer: 32 << 20, BlockCacheCapacity: 8 << 20,
		CompactionTableSize: 8 << 20, CompactionTotalSize: 64 << 20,
	})
	if err != nil {
		b.Fatal(err)
	}
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		b.Fatal(err)
	}
	walletKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		rawDB.Close()
		b.Fatal(err)
	}
	leader := crypto.AddressFromKeys(crypto.MainnetByte, walletKeys)
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		b.Fatal(err)
	}
	date, _ := time.Parse("2006-01-02", "2024-01-02")
	end := time.Date(date.Year(), date.Month(), date.Day(), 20, 0, 0, 0, location)
	startDate := date.AddDate(0, 0, -1)
	start := time.Date(startDate.Year(), startDate.Month(), startDate.Day(), 20, 0, 0, 0, location)
	dayNanos := int64(end.Sub(start))

	vaultRows := make([]store.LPoDAuditVault, 0, vaultCount)
	vaultPubs := make([]crypto.ValidatorPubKey, 0, vaultCount)
	vaultAPRByID := make(map[string]uint64, vaultCount)
	registryBefore := core.RegistrySnapshot{Validators: make(map[string]*core.ValidatorEntry, vaultCount)}
	registryAfter := core.RegistrySnapshot{Validators: make(map[string]*core.ValidatorEntry, vaultCount)}
	previousStake := make(map[string]store.LPoDValidatorStake, vaultCount)
	stake := make(map[string]store.LPoDValidatorStake, vaultCount)
	const validatorStake = uint64(100_000_000) * lpod.Unit
	const guardianStake = uint64(0)
	totalStake := validatorStake + guardianStake
	if vaultCount > 0 {
		tier, err := lpod.TierFor(totalStake)
		if err != nil {
			rawDB.Close()
			b.Fatalf("benchmark vault stake is not a valid LPoD tier: %v", err)
		}
		for i := 0; i < vaultCount; i++ {
			validatorPub := pub
			if i > 0 {
				_, validatorPub, err = crypto.GenerateValidatorKey()
				if err != nil {
					rawDB.Close()
					b.Fatalf("generate benchmark vault validator: %v", err)
				}
			}
			vaultPubs = append(vaultPubs, validatorPub)
			id := validatorPub.Hex()
			entry := &core.ValidatorEntry{
				PubKey: validatorPub, StakeNAPR: validatorStake, Status: core.ValidatorActive,
			}
			registryBefore.Validators[id] = entry
			registryAfter.Validators[id] = entry
			previousStake[id] = store.LPoDValidatorStake{Amount: validatorStake, Active: true}
			stake[id] = store.LPoDValidatorStake{Amount: validatorStake, Active: true}
			vaultAPRByID[id] = tier.APRPercent
			vaultRows = append(vaultRows, store.LPoDAuditVault{
				ID: id, ValidatorStake: validatorStake,
				GuardianStake: guardianStake, TotalStake: totalStake, TierStake: totalStake,
				APRPercent: tier.APRPercent, LeaderPercent: tier.LeaderPercent,
			})
		}
	}
	if positionCount > 0 && vaultCount == 0 {
		rawDB.Close()
		b.Fatal("benchmark positions require at least one operator-attested vault")
	}
	zero := uint64(0)
	batch := new(leveldb.Batch)
	var blockBodyBytes int64
	var parentHash, genesisHash crypto.Hash32
	var beforeDigest crypto.Hash32
	allocationRoot := crypto.HashBytes([]byte("LPoD synthetic benchmark allocation root"))
	fundingHash := crypto.Hash32{}
	positionState := make(map[string]store.LPoDPosition, positionCount)
	previousPositions := make(map[string]store.LPoDPosition, positionCount)
	positionVaultIDs := make(map[string]string, positionCount)
	positionPrincipal := uint64(0)
	positionDue := uint64(0)
	previousTimestamp := int64(0)
	flush := func() {
		b.Helper()
		if batch.Len() == 0 {
			return
		}
		if err := rawDB.Write(batch, nil); err != nil {
			rawDB.Close()
			b.Fatal(err)
		}
		batch.Reset()
		// Fixture construction streams large checkpoint and audit records into
		// LevelDB. Reclaim the JSON/batch working set at each bounded write so
		// the opt-in 28,800-block case does not retain setup garbage until the
		// runtime's default heap-growth trigger.
		if positionCount > 0 {
			runtime.GC()
		}
	}

	// Height zero is the signed opening anchor. Heights 1..includedBlocks are
	// the interval; the final height is the signed 20:00 closing witness.
	firstIncludedHeight := uint64(preludeBlocks + 1)
	closingHeight := firstIncludedHeight + uint64(includedBlocks)
	lastHeight := closingHeight + uint64(tailBlocks)
	for height := uint64(0); height <= lastHeight; height++ {
		timestamp := start.Add(-time.Second)
		if height <= uint64(preludeBlocks) {
			timestamp = start.Add(time.Duration(int64(height)-int64(preludeBlocks)-1) * 3 * time.Second)
		} else {
			includedOffset := height - firstIncludedHeight
			if includedOffset > uint64(includedBlocks) {
				includedOffset = uint64(includedBlocks)
			}
			if height >= closingHeight {
				timestamp = end.Add(time.Duration(height-closingHeight) * 3 * time.Second)
			} else {
				offset := dayNanos * int64(includedOffset) / int64(includedBlocks)
				timestamp = start.Add(time.Duration(offset))
			}
		}
		if height == 0 && preludeBlocks == 0 {
			// Height zero is the immediately preceding opening anchor in the
			// common benchmark scenario.
			timestamp = start.Add(-3 * time.Second)
		}
		if len(misorderFirstIncluded) > 0 && misorderFirstIncluded[0] {
			if height == firstIncludedHeight {
				timestamp = start.Add(2 * time.Hour)
			} else if height == firstIncludedHeight+1 {
				timestamp = start.Add(time.Hour)
			}
		}
		positionTxs := make([]core.Transaction, 0, positionCount)
		if positionCount > 0 && height == firstIncludedHeight {
			for n := 0; n < positionCount; n++ {
				sourceKeys, keyErr := crypto.GenerateWalletKeys()
				if keyErr != nil {
					rawDB.Close()
					b.Fatalf("generate benchmark position source keys: %v", keyErr)
				}
				blind, blindErr := crypto.NewBlindFactor()
				if blindErr != nil {
					rawDB.Close()
					b.Fatalf("generate benchmark position commitment blind: %v", blindErr)
				}
				sourceTx := crypto.HashBytes([]byte(fmt.Sprintf("synthetic-position-source-%04d", n)))
				action := core.LPoDPositionAction{
					Action: core.LPoDDeposit, Genesis: genesisHash,
					PositionID: core.LPoDPositionID(genesisHash, sourceTx, uint32(n)),
					Vault:      crypto.Point32(vaultPubs[n%len(vaultPubs)]), Beneficiary: leader,
					Owner: walletKeys.Spend.Public, SourceTx: sourceTx, SourceIndex: uint32(n),
					SourcePub: sourceKeys.Spend.Public, Amount: lpod.Unit, Blind: blind,
				}
				tx, txErr := core.BuildLPoDPositionTx(action, walletKeys.Spend.Private, sourceKeys.Spend.Private)
				if txErr != nil {
					rawDB.Close()
					b.Fatalf("build benchmark position deposit %d: %v", n, txErr)
				}
				positionID := fmt.Sprintf("%x", action.PositionID[:])
				positionState[positionID] = store.LPoDPosition{Deposit: action}
				positionVaultIDs[positionID] = hex.EncodeToString(action.Vault[:])
				positionTxs = append(positionTxs, *tx)
				if ^uint64(0)-positionPrincipal < action.Amount {
					rawDB.Close()
					b.Fatal("benchmark position principal overflow")
				}
				positionPrincipal += action.Amount
			}
		}
		blockAccruedByVault := make(map[string]uint64, vaultCount)
		blockAccruedByPosition := make(map[string]uint64, positionCount)
		var blockAccrued uint64
		if positionCount > 0 && height > firstIncludedHeight {
			elapsed := uint64(timestamp.UnixNano() - previousTimestamp)
			if elapsed > 15_000_000_000 {
				elapsed = 15_000_000_000
			}
			for positionID, position := range positionState {
				vaultID := positionVaultIDs[positionID]
				apr := vaultAPRByID[vaultID]
				// These fixture-only bounds (1 APRO position, <=10% APR,
				// <=15 seconds) keep the exact integer numerator in uint64.
				numerator := position.Deposit.Amount*apr*elapsed + position.APRCarry
				const denominator = uint64(100) * lpod.YearSeconds * 1_000_000_000
				accrued, carry := numerator/denominator, numerator%denominator
				position.Due += accrued
				position.APRCarry = carry
				positionState[positionID] = position
				blockAccruedByPosition[positionID] = accrued
				blockAccruedByVault[vaultID] += accrued
				blockAccrued += accrued
			}
			positionDue += blockAccrued
		}
		positionCopy := make(map[string]store.LPoDPosition, len(positionState))
		for id, position := range positionState {
			positionCopy[id] = position
		}
		checkpoint := &store.LPoDCheckpoint{
			State: lpod.State{
				FundingDebit: lpod.InitialNAPRO, Balance: lpod.InitialNAPRO, LastHeight: height,
				AccruedLiability: positionDue, UnfundedLiability: positionDue,
			},
			Carries: map[string]lpod.Carry{}, Positions: positionCopy,
			PrincipalDeposited: positionPrincipal, PrincipalLocked: positionPrincipal,
		}
		if height > 0 {
			checkpoint.Allocation = &store.LPoDAllocation{
				Version: 1, PositionLifecycleVersion: 1, FundingHeight: 1, Genesis: genesisHash,
				ReconciliationRoot:        allocationRoot,
				InitialValidatorRemaining: 2_000_000_000 * lpod.Unit,
				ValidatorRemaining:        2_000_000_000 * lpod.Unit,
				Remaining:                 7_000_000_000*lpod.Unit - lpod.InitialNAPRO,
			}
			if fundingHash != (crypto.Hash32{}) {
				checkpoint.Allocation.FundingBlock = fundingHash
			}
		}
		checkpointDigest := checkpoint.Digest()
		txs := make([]core.Transaction, 0, 2+fillerCount+len(positionTxs))
		if height > 0 {
			// An empty-output payout remains a valid zero-settlement commitment.
			auth, marshalErr := json.Marshal(core.LPoDPayoutAuthorization{Leader: leader, Digest: checkpointDigest})
			if marshalErr != nil {
				rawDB.Close()
				b.Fatal(marshalErr)
			}
			txs = append(txs,
				core.Transaction{Version: core.TxVersionLPoDPayout, Extra: auth},
				core.LPoDCheckpointTx(checkpointDigest))
			txs = append(txs, positionTxs...)
			for n := 0; n < fillerCount; n++ {
				txs = append(txs, core.Transaction{Version: core.TxVersionBase, Extra: make([]byte, extraBytes)})
			}
		}
		block := &core.Block{Header: core.BlockHeader{
			Height: height, PrevHash: parentHash, Timestamp: timestamp.UnixNano(),
			ValidatorPub: pub, MerkleRoot: core.MerkleRoot(txs),
		}, Txs: txs}
		if err := block.Header.Sign(priv); err != nil {
			rawDB.Close()
			b.Fatal(err)
		}
		hash := block.Hash()
		if height == 0 {
			genesisHash = hash
		}
		if height == 1 {
			fundingHash = hash
			checkpoint.Allocation.FundingBlock = fundingHash
		}
		raw, err := json.Marshal(block)
		if err != nil {
			rawDB.Close()
			b.Fatal(err)
		}
		blockBodyBytes += int64(len(raw))
		batch.Put(append([]byte("b/"), hash[:]...), raw)
		heightKey := make([]byte, 10)
		copy(heightKey, "h/")
		binary.BigEndian.PutUint64(heightKey[2:], height)
		batch.Put(heightKey, hash[:])

		checkpointRaw, err := json.Marshal(checkpoint)
		if err != nil {
			rawDB.Close()
			b.Fatal(err)
		}
		batch.Put(append([]byte("lpod/checkpoint/v1/"), hash[:]...), checkpointRaw)
		if height >= firstIncludedHeight && height < closingHeight {
			recordVaults := append([]store.LPoDAuditVault(nil), vaultRows...)
			for i := range recordVaults {
				recordVaults[i].Accrued = blockAccruedByVault[recordVaults[i].ID]
				recordVaults[i].Unfunded = recordVaults[i].Accrued
			}
			positionIDs := make([]string, 0, len(checkpoint.Positions))
			for id := range checkpoint.Positions {
				positionIDs = append(positionIDs, id)
			}
			sort.Strings(positionIDs)
			positionRows := make([]store.LPoDAuditPosition, 0, len(positionIDs))
			for _, id := range positionIDs {
				position := checkpoint.Positions[id]
				prior, hadPrior := previousPositions[id]
				dueBefore, carryBefore := uint64(0), uint64(0)
				if hadPrior {
					dueBefore, carryBefore = prior.Due, prior.APRCarry
				}
				vaultID := positionVaultIDs[id]
				positionRows = append(positionRows, store.LPoDAuditPosition{
					ID: id, Beneficiary: string(position.Deposit.Beneficiary),
					SignedVault: vaultID, EffectiveVault: vaultID, EffectiveVaultAfter: vaultID,
					DueBefore: dueBefore, Accrued: blockAccruedByPosition[id], DueAfter: position.Due,
					APRCarryBefore: carryBefore, APRCarryAfter: position.APRCarry,
				})
			}
			record := store.LPoDBlockAudit{
				Version: 3, Available: true, Height: height, BlockHash: hash,
				ParentHash: parentHash, HeaderTimestamp: block.Header.Timestamp,
				FundingHeight: 1, FundingGenesis: genesisHash, FundingRoot: allocationRoot,
				BeforeCheckpointDigest: beforeDigest, AfterCheckpointDigest: checkpointDigest,
				RegistryBefore: &registryBefore, RegistryAfter: &registryAfter,
				PreviousStake: previousStake, Stake: stake,
				PayoutTransactionHash: txs[0].Hash(), Vaults: recordVaults, Positions: positionRows,
				ProtocolBaseFeeBurnNAPRO: &zero, SignedIntentionalBurnNAPRO: &zero,
				AVMGasBurnNAPRO: &zero, TotalBurnNAPRO: &zero,
			}
			recordRaw, err := json.Marshal(record)
			if err != nil {
				rawDB.Close()
				b.Fatal(err)
			}
			batch.Put(append([]byte("lpod/block-audit/v3/"), hash[:]...), recordRaw)
		}
		if height == firstIncludedHeight {
			marker := make([]byte, 8+32+32+8+32+32)
			binary.BigEndian.PutUint64(marker[:8], height)
			offset := 8
			copy(marker[offset:offset+32], hash[:])
			offset += 32
			copy(marker[offset:offset+32], parentHash[:])
			offset += 32
			binary.BigEndian.PutUint64(marker[offset:offset+8], 1)
			offset += 8
			copy(marker[offset:offset+32], genesisHash[:])
			offset += 32
			copy(marker[offset:offset+32], allocationRoot[:])
			batch.Put([]byte("lpod/block-audit-start/v3"), marker)
		}
		beforeDigest = checkpointDigest
		parentHash = hash
		previousTimestamp = timestamp.UnixNano()
		previousPositions = make(map[string]store.LPoDPosition, len(positionState))
		for id, position := range positionState {
			previousPositions[id] = position
		}
		if os.Getenv("LPOD_DAILY_BENCH_PROGRESS") == "1" && height%1000 == 0 {
			var usage syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
				b.Fatal(err)
			}
			fmt.Printf("fixture-progress height=%d/%d peak-rss-kb=%d\n", height, lastHeight, usage.Maxrss)
		}
		// Len counts records, not bytes. Bound encoded batch bytes instead.
		if len(batch.Dump()) >= 8<<20 {
			flush()
		}
	}
	var tipHeightBytes [8]byte
	binary.LittleEndian.PutUint64(tipHeightBytes[:], lastHeight)
	batch.Put([]byte("m/tip/hash"), parentHash[:])
	batch.Put([]byte("m/tip/height"), tipHeightBytes[:])
	flush()
	if err := rawDB.Close(); err != nil {
		b.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	closingHash, found, err := db.GetCanonicalHash(closingHeight)
	if err != nil || !found {
		db.Close()
		b.Fatalf("fixture closing hash unavailable: %v", err)
	}
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{
		Version: 1, Height: closingHeight, BlockHash: closingHash,
		Votes: []store.FinalityVote{{Signature: []byte{1}}},
	}); err != nil {
		db.Close()
		b.Fatal(err)
	}
	server := NewServer(":0", nil, nil, nil, slog.Default())
	server.SetStore(db)
	server.SetAPIKey(dailyAuditBenchmarkAPIKey)
	server.SetLPoDConfig(nil, func(uint64, crypto.Hash32) bool { return true })
	benchmarkDailySigningKey(b, server)
	return db, server, path, blockBodyBytes
}

func benchmarkDailySigningKey(t testing.TB, s *Server) {
	t.Helper()
	priv, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(key.Destroy)
	s.SetValidatorKey(key)
}

func directoryBytes(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
