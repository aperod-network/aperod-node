//go:build linux

package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// Synthetic storage identities, not wallet keys or spendable curve points.
// The sampler is being measured as a storage operation, not a signing proof.
func diskFixtureOutput(id uint64, sparse bool) *StoredUTXO {
	var input [9]byte
	binary.BigEndian.PutUint64(input[1:], id)
	input[0] = 1
	pub := crypto.Point32(sha256.Sum256(input[:]))
	binary.BigEndian.PutUint64(pub[24:], id)
	input[0] = 2
	commit := crypto.Commitment(sha256.Sum256(input[:]))
	var hash crypto.Hash32
	binary.BigEndian.PutUint64(hash[:8], id)
	return &StoredUTXO{TxHash: hash, OneTimePub: pub, AmountCommit: commit,
		BlockHeight: id, ProtocolLocked: id%100 == 0 || (sparse && id > 10)}
}

func seedDiskFixture(db *DB, population uint64, rewriteSparse bool) error {
	batch := new(leveldb.Batch)
	for id := uint64(1); id <= population; id++ {
		u := diskFixtureOutput(id, rewriteSparse)
		data, err := json.Marshal(u)
		if err != nil {
			return err
		}
		batch.Put(utxoKey(u.TxHash, 0), data)
		if rewriteSparse {
			batch.Put(ringMemberKey(u.OneTimePub), data)
		}
		if id%2 == 0 && !rewriteSparse {
			batch.Put(spentUTXOKey(u.TxHash, 0), []byte{1})
		}
		if id%1000 == 0 {
			if err := db.db.Write(batch, nil); err != nil {
				return err
			}
			batch.Reset()
		}
	}
	return db.db.Write(batch, &opt.WriteOptions{Sync: true})
}

func auditDiskFixture(db *DB, population uint64, sparse bool) error {
	count, ready, err := ringSamplingCount(db.db)
	expected := population - population/100
	if sparse {
		expected = 10
	}
	if err != nil || !ready || count != expected {
		return fmt.Errorf("count=%d expected=%d ready=%t: %v", count, expected, ready, err)
	}
	iter := db.db.NewIterator(util.BytesPrefix(prefixRingMember), nil)
	var canonical, eligible, spent uint64
	for iter.Next() {
		u, err := decodeRingRecord(iter.Key(), iter.Value())
		if err != nil {
			iter.Release()
			return err
		}
		id := binary.BigEndian.Uint64(u.OneTimePub[24:])
		expectedU := diskFixtureOutput(id, sparse)
		if id == 0 || id > population || *u != *expectedU {
			iter.Release()
			return fmt.Errorf("canonical fixture mismatch %d", id)
		}
		original, err := db.db.Get(utxoKey(u.TxHash, 0), nil)
		if err != nil || !bytes.Equal(original, iter.Value()) {
			iter.Release()
			return fmt.Errorf("u/rm mismatch %d: %v", id, err)
		}
		marker, err := db.db.Get(spentUTXOKey(u.TxHash, 0), nil)
		if id%2 == 0 {
			if err != nil || !bytes.Equal(marker, []byte{1}) {
				iter.Release()
				return fmt.Errorf("spent record lost %d: %v", id, err)
			}
			spent++
		} else if !errors.Is(err, leveldb.ErrNotFound) {
			iter.Release()
			return fmt.Errorf("unexpected spent marker %d: %v", id, err)
		}
		reverse, err := db.db.Get(ringSamplingReverseKey(u.OneTimePub), nil)
		if u.ProtocolLocked {
			if !errors.Is(err, leveldb.ErrNotFound) {
				iter.Release()
				return fmt.Errorf("locked inverse %d: %v", id, err)
			}
		} else {
			if err != nil || len(reverse) != 8 {
				iter.Release()
				return fmt.Errorf("missing inverse %d: %v", id, err)
			}
			ordinal := binary.BigEndian.Uint64(reverse)
			slot, err := db.db.Get(ringSamplingSlotKey(ordinal), nil)
			if err != nil || ordinal >= count || !bytes.Equal(slot, u.OneTimePub[:]) {
				iter.Release()
				return fmt.Errorf("bijection mismatch %d: %v", id, err)
			}
			eligible++
		}
		canonical++
	}
	err = iter.Error()
	iter.Release()
	if err != nil {
		return err
	}
	if canonical != population || eligible != expected || spent != population/2 {
		return fmt.Errorf("audit counts canonical=%d eligible=%d spent=%d", canonical, eligible, spent)
	}
	for _, prefix := range [][]byte{ringSamplingSlotPrefix, ringSamplingReversePrefix} {
		it := db.db.NewIterator(util.BytesPrefix(prefix), nil)
		var n uint64
		for it.Next() {
			if bytes.Equal(prefix, ringSamplingSlotPrefix) &&
				!bytes.Equal(it.Key(), ringSamplingSlotKey(n)) {
				it.Release()
				return fmt.Errorf("non-dense slot %d", n)
			}
			n++
		}
		err := it.Error()
		it.Release()
		if err != nil || n != expected {
			return fmt.Errorf("derived namespace count %d expected %d: %v", n, expected, err)
		}
	}
	return nil
}

func diskSampleCalls(db *DB, calls, count, expected int, pubs map[crypto.Point32]bool,
	outpoints map[core.UTXOKey]bool, sparse bool) ([]time.Duration, error) {
	latencies := make([]time.Duration, 0, calls)
	for i := 0; i < calls; i++ {
		begin := time.Now()
		sample, err := db.SampleRingMembersExcluding(count, pubs, outpoints)
		latencies = append(latencies, time.Since(begin))
		if err != nil || len(sample) != expected {
			return latencies, fmt.Errorf("sample size %d expected %d: %v", len(sample), expected, err)
		}
		seen := make(map[crypto.Point32]bool, len(sample))
		for _, pair := range sample {
			id := binary.BigEndian.Uint64(pair.OneTimePub[24:])
			u := diskFixtureOutput(id, sparse)
			if seen[pair.OneTimePub] || u.ProtocolLocked || pair.OneTimePub != u.OneTimePub ||
				pair.AmountCommit != u.AmountCommit || pubs[pair.OneTimePub] ||
				outpoints[core.UTXOKey{TxHash: u.TxHash, OutputIndex: 0}] {
				return latencies, fmt.Errorf("noncanonical/duplicate/excluded sample %d", id)
			}
			seen[pair.OneTimePub] = true
		}
	}
	return latencies, nil
}

func crashDuringDiskBackfill(path, proof string, oldTail, crashAt uint64) error {
	db, err := Open(path)
	if err != nil {
		return err
	}
	if oldTail <= crashAt {
		return fmt.Errorf("crash threshold must precede old tail")
	}
	if _, err := db.db.Get(ringSamplingSlotKey(oldTail), nil); err != nil {
		return fmt.Errorf("initial old tail: %w", err)
	}
	go func() {
		cleanupObserved := false
		for {
			// A count invalidation leaves OLD slots present. Do not mistake
			// them for a new partial build: first witness cleanup of its tail.
			if !cleanupObserved {
				if _, err := db.db.Get(ringSamplingSlotKey(oldTail), nil); errors.Is(err, leveldb.ErrNotFound) {
					cleanupObserved = true
				}
				time.Sleep(time.Millisecond)
				continue
			}
			if data, err := db.db.Get(ringSamplingSlotKey(crashAt), nil); err == nil && len(data) == 32 {
				if _, ready, err := ringSamplingCount(db.db); err != nil || ready {
					os.Exit(74)
				}
				// This isolated-fixture metadata write fsyncs the journal,
				// ensuring preceding non-Sync index batches survive exit.
				if err := db.db.Put([]byte("m/rehearsal-partial-fsync"), []byte{1}, &opt.WriteOptions{Sync: true}); err != nil {
					os.Exit(76)
				}
				resources, err := diskResourceSnapshot()
				evidence := struct {
					Resources       diskResources `json:"resources"`
					OldTail         uint64        `json:"old_tail"`
					CrashAt         uint64        `json:"new_partial_slot"`
					CleanupObserved bool          `json:"old_tail_cleanup_observed"`
					JournalSynced   bool          `json:"partial_journal_synced"`
				}{resources, oldTail, crashAt, cleanupObserved, true}
				if err != nil || writeDiskReport(proof, evidence) != nil {
					os.Exit(75)
				}
				os.Exit(73)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if err := db.EnsureRingMemberIndex(); err != nil {
		return err
	}
	return fmt.Errorf("partial-build crash point was missed")
}

func TestRingSamplingDiskRehearsal(t *testing.T) {
	if os.Getenv("APEROD_RING_DISK_REHEARSAL") != "1" {
		t.Skip("opt-in physical multi-million LevelDB rehearsal")
	}
	root := os.Getenv("APEROD_RING_DISK_ROOT")
	if root == "" || !filepath.IsAbs(root) {
		t.Fatal("explicit isolated output directory required")
	}
	path := filepath.Join(root, "synthetic.leveldb")
	reportRoot := os.Getenv("APEROD_RING_DISK_REPORT_ROOT")
	if reportRoot == "" {
		reportRoot = root
	}
	if !filepath.IsAbs(reportRoot) {
		t.Fatal("report directory must be absolute")
	}
	if os.Getenv("APEROD_RING_DISK_PHASE") == "crash" {
		for _, dir := range []string{root, reportRoot} {
			marker, err := os.ReadFile(filepath.Join(dir, "synthetic-only"))
			if err != nil || string(marker) != "ring-disk-rehearsal-synthetic-only\n" {
				t.Fatal("crash worker refuses a directory without synthetic fixture marker")
			}
		}
		tail, tailErr := strconv.ParseUint(os.Getenv("APEROD_RING_DISK_OLD_TAIL"), 10, 64)
		crashAt := uint64(100_000)
		if raw := os.Getenv("APEROD_RING_DISK_CRASH_AT"); raw != "" {
			var err error
			crashAt, err = strconv.ParseUint(raw, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
		}
		if tailErr != nil {
			t.Fatal("crash worker needs explicit old tail", tailErr)
		}
		if err := crashDuringDiskBackfill(path, filepath.Join(reportRoot, "crash-proof.json"), tail, crashAt); err != nil {
			t.Fatal(err)
		}
		return
	}
	population := uint64(2_000_000)
	if raw := os.Getenv("APEROD_RING_DISK_POPULATION"); raw != "" {
		var err error
		population, err = strconv.ParseUint(raw, 10, 64)
		if err != nil || population < 1_000_000 || population > 10_000_000 {
			t.Fatal("population must be 1M..10M")
		}
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal("refusing existing/nonisolated rehearsal directory:", err)
	}
	if err := os.WriteFile(filepath.Join(root, "synthetic-only"), []byte("ring-disk-rehearsal-synthetic-only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if reportRoot != root {
		if err := os.Mkdir(reportRoot, 0700); err != nil {
			t.Fatal("refusing existing report directory:", err)
		}
		if err := os.WriteFile(filepath.Join(reportRoot, "synthetic-only"), []byte("ring-disk-rehearsal-synthetic-only\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	report := struct {
		Population uint64      `json:"population"`
		Go         string      `json:"go_version"`
		CPU        int         `json:"logical_cpu"`
		GoMaxProcs int         `json:"gomaxprocs"`
		GoMemLimit string      `json:"gomemlimit"`
		Cache      string      `json:"backend_cache"`
		ColdMethod string      `json:"cold_method"`
		IOMethod   string      `json:"io_method"`
		Scope      string      `json:"scope"`
		Phases     []diskPhase `json:"phases"`
		Passed     bool        `json:"passed"`
		Failure    string      `json:"failure,omitempty"`
	}{Population: population, Go: runtime.Version(), CPU: runtime.NumCPU(),
		GoMaxProcs: runtime.GOMAXPROCS(0), GoMemLimit: os.Getenv("GOMEMLIMIT"),
		Cache:      "store.Open: 32MiB LevelDB block cache, 16MiB write buffer; no population memory backend",
		ColdMethod: "close, FADV_DONTNEED on isolated DB files, reopen; sample timing excludes reopen; advisory eviction, not guaranteed globally cold",
		IOMethod:   "LevelDB DBStats IORead/IOWrite file-storage bytes, not physical-media IO. /proc/self/io unavailable. Phase IO excludes crashed child; cold phase reopens reset counters, so sample-only bytes are reported separately.",
		Scope:      "synthetic storage-only identities; no real keys, funds, production changes or host E2E"}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	defer func() {
		if err := writeDiskReport(filepath.Join(reportRoot, "report.json"), report); err != nil {
			t.Error(err)
		}
	}()
	phase := func(name string, work func() ([]time.Duration, error)) {
		t.Helper()
		t.Log("phase:", name)
		result, err := measureDiskPhase(name, path, func() *DB { return db }, work)
		report.Phases = append(report.Phases, result)
		if err != nil {
			report.Failure = err.Error()
		}
		if saveErr := writeDiskReport(filepath.Join(reportRoot, "report.json"), report); saveErr != nil {
			t.Fatal("save phase checkpoint:", saveErr)
		}
		t.Logf("%s: %.3fs p50=%.3f p95=%.3f p99=%.3fms RSS=%d", name, result.Seconds, result.P50MS, result.P95MS, result.P99MS, result.ObservedRSSMax)
		if err != nil {
			t.Fatal(name, err)
		}
	}
	phase("seed_legacy_u_only", func() ([]time.Duration, error) { return nil, seedDiskFixture(db, population, false) })
	phase("streaming_legacy_u_and_ordinal_backfill", func() ([]time.Duration, error) { return nil, db.EnsureRingMemberIndex() })
	phase("full_canonical_bijection_and_spent_audit", func() ([]time.Duration, error) { return nil, auditDiskFixture(db, population, false) })
	if err := db.InvalidateRingSamplingIndex(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	cmd := exec.Command(os.Args[0], "-test.run=^TestRingSamplingDiskRehearsal$", "-test.v")
	cmd.Env = append(os.Environ(), "APEROD_RING_DISK_PHASE=crash",
		fmt.Sprintf("APEROD_RING_DISK_OLD_TAIL=%d", population-population/100-1),
		"APEROD_RING_DISK_CRASH_AT=100000")
	crashOutput, crashErr := cmd.CombinedOutput()
	t.Logf("partial-build child: %s", crashOutput)
	var exited *exec.ExitError
	if !errors.As(crashErr, &exited) || exited.ExitCode() != 73 {
		t.Fatalf("expected abrupt partial build exit 73: %v", crashErr)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ready, err := ringSamplingCount(db.db); err != nil || ready {
		t.Fatal("partial build published readiness", err)
	}
	if data, err := db.db.Get(ringSamplingSlotKey(100_000), nil); err != nil || len(data) != 32 {
		t.Fatal("partial persisted batch missing", err)
	}
	if sample, err := db.SampleRingMembers(15, nil); err == nil || sample != nil {
		t.Fatal("partial index did not fail closed")
	}
	phase("restart_rm_backfill_with_partial_cleanup", func() ([]time.Duration, error) { return nil, db.EnsureRingMemberIndex() })
	phase("full_post_restart_audit", func() ([]time.Duration, error) { return nil, auditDiskFixture(db, population, false) })
	phase("compact_before_latency", func() ([]time.Duration, error) { return nil, db.db.CompactRange(util.Range{}) })
	phase("warm_15_members_1000_calls", func() ([]time.Duration, error) { return diskSampleCalls(db, 1000, 15, 15, nil, nil, false) })
	var coldSampleReadBytes uint64
	var coldPhysicalCalls int
	phase("advisory_cold_15_members_200_reopens", func() ([]time.Duration, error) {
		latencies := make([]time.Duration, 0, 200)
		for i := 0; i < 200; i++ {
			if err := db.Close(); err != nil {
				return latencies, err
			}
			db = nil
			if err := adviseDiskFilesCold(path); err != nil {
				return latencies, err
			}
			db, err = Open(path)
			if err != nil {
				return latencies, err
			}
			var before leveldb.DBStats
			if err := db.db.Stats(&before); err != nil {
				return latencies, err
			}
			next, err := diskSampleCalls(db, 1, 15, 15, nil, nil, false)
			if err != nil {
				return latencies, err
			}
			var after leveldb.DBStats
			if err := db.db.Stats(&after); err != nil {
				return latencies, err
			}
			delta := after.IORead - before.IORead
			coldSampleReadBytes += delta
			if delta > 0 {
				coldPhysicalCalls++
			}
			latencies = append(latencies, next...)
		}
		return latencies, nil
	})
	report.Phases[len(report.Phases)-1].SampleReadBytes = coldSampleReadBytes
	report.Phases[len(report.Phases)-1].SampleReadCalls = coldPhysicalCalls
	if err := writeDiskReport(filepath.Join(reportRoot, "report.json"), report); err != nil {
		t.Fatal(err)
	}
	pubs := make(map[crypto.Point32]bool, 4096)
	outpoints := make(map[core.UTXOKey]bool, 100_000)
	for i := uint64(1); i <= 100_000; i++ {
		u := diskFixtureOutput(i, false)
		outpoints[core.UTXOKey{TxHash: u.TxHash, OutputIndex: 0}] = true
		if i <= 4096 {
			pubs[u.OneTimePub] = true
		}
	}
	phase("excluded_4096_keys_100k_outpoints_1000_calls", func() ([]time.Duration, error) {
		return diskSampleCalls(db, 1000, 15, 15, pubs, outpoints, false)
	})
	phase("max_request_4096_members_30_calls", func() ([]time.Duration, error) {
		return diskSampleCalls(db, 30, 4096, 4096, pubs, outpoints, false)
	})
	phase("half_population_outpoint_exclusions_up_to_1m_1000_calls", func() ([]time.Duration, error) {
		size := population / 2
		if size > 1_000_000 {
			size = 1_000_000
		}
		large := make(map[core.UTXOKey]bool, int(size))
		for id := uint64(1); id <= size; id++ {
			u := diskFixtureOutput(id, false)
			large[core.UTXOKey{TxHash: u.TxHash, OutputIndex: 0}] = true
		}
		return diskSampleCalls(db, 1000, 15, 15, nil, large, false)
	})
	runtime.GC()
	phase("1000_lock_and_unlock_transitions", func() ([]time.Duration, error) {
		times := make([]time.Duration, 0, 2000)
		for _, locked := range []bool{true, false} {
			for id := uint64(1); id <= 1010; id++ {
				if id%100 == 0 {
					continue
				}
				u := diskFixtureOutput(id, false)
				u.ProtocolLocked = locked
				begin := time.Now()
				err := db.PutUTXO(u.TxHash, 0, u)
				times = append(times, time.Since(begin))
				if err != nil {
					return times, err
				}
				if locked {
					if _, err := db.db.Get(ringSamplingReverseKey(u.OneTimePub), nil); !errors.Is(err, leveldb.ErrNotFound) {
						return times, fmt.Errorf("locked output indexed: %v", err)
					}
				}
			}
		}
		return times, nil
	})
	if err := db.InvalidateRingSamplingIndex(); err != nil {
		t.Fatal(err)
	}
	phase("rewrite_sparse_locked_fixture", func() ([]time.Duration, error) { return nil, seedDiskFixture(db, population, true) })
	phase("sparse_locked_streaming_backfill", func() ([]time.Duration, error) { return nil, db.EnsureRingMemberIndex() })
	phase("shortfall_15_requested_10_eligible_1000_calls", func() ([]time.Duration, error) {
		return diskSampleCalls(db, 1000, 15, 10, nil, nil, true)
	})
	phase("full_sparse_locked_spent_retention_audit", func() ([]time.Duration, error) { return nil, auditDiskFixture(db, population, true) })
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	if err := os.RemoveAll(path); err != nil {
		t.Fatal("clean synthetic DB:", err)
	}
	report.Passed = true
}

func TestRingDiskCrashDurablyBuildsNewNotOldSlots(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "synthetic-only"), []byte("ring-disk-rehearsal-synthetic-only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "synthetic.leveldb")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	const population = uint64(20_000)
	if err := seedDiskFixture(db, population, false); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	if err := db.InvalidateRingSamplingIndex(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	cmd := exec.Command(os.Args[0], "-test.run=^TestRingSamplingDiskRehearsal$")
	cmd.Env = append(os.Environ(), "APEROD_RING_DISK_REHEARSAL=1", "APEROD_RING_DISK_PHASE=crash",
		"APEROD_RING_DISK_ROOT="+root, "APEROD_RING_DISK_REPORT_ROOT="+root,
		fmt.Sprintf("APEROD_RING_DISK_OLD_TAIL=%d", population-population/100-1), "APEROD_RING_DISK_CRASH_AT=1000")
	output, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 73 {
		t.Fatalf("partial child: %s %v", output, err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ready, err := ringSamplingCount(db.db); err != nil || ready {
		t.Fatal("crash published readiness", err)
	}
	if value, err := db.db.Get(ringSamplingSlotKey(1000), nil); err != nil || len(value) != 32 {
		t.Fatal("new durable partial slot missing", err)
	}
	if value, err := db.db.Get([]byte("m/rehearsal-partial-fsync"), nil); err != nil || !bytes.Equal(value, []byte{1}) {
		t.Fatal("journal sync marker missing", err)
	}
	if _, err := db.SampleRingMembers(15, nil); err == nil {
		t.Fatal("partial index accepted")
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	if err := auditDiskFixture(db, population, false); err != nil {
		t.Fatal(err)
	}
}
