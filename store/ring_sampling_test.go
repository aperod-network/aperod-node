package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func samplingDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sparseRingOutput(i byte) *StoredUTXO {
	var u StoredUTXO
	// Deliberately unequal key-space gaps, not contiguous ordinal keys.
	u.OneTimePub[0] = []byte{0, 1, 128, 255, 200, 100}[int(i)%6]
	u.OneTimePub[31] = i + 1
	u.TxHash[0] = i + 1
	u.AmountCommit[0] = i + 10
	return &u
}

func putSamplingOutput(t *testing.T, db *DB, u *StoredUTXO) {
	t.Helper()
	if err := db.PutUTXO(u.TxHash, u.OutputIndex, u); err != nil {
		t.Fatal(err)
	}
}

func sampleIdentity(sample []core.DecoyUTXO) string {
	var result string
	for _, u := range sample {
		result += fmt.Sprintf("%02x", u.OneTimePub[31])
	}
	return result
}

func TestRingSamplingSparseKeysEveryOrderedSampleIsEquiprobable(t *testing.T) {
	db := samplingDB(t)
	for i := byte(0); i < 4; i++ {
		putSamplingOutput(t, db, sparseRingOutput(i))
	}
	orders := map[string]int{}
	for a := byte(0); a < 4; a++ {
		for b := byte(0); b < 3; b++ {
			for c := byte(0); c < 2; c++ {
				got, err := db.sampleRingMembers(3, nil, bytes.NewReader([]byte{a, b, c}))
				if err != nil || len(got) != 3 {
					t.Fatalf("draw %d/%d/%d: %v %v", a, b, c, got, err)
				}
				seen := map[crypto.Point32]bool{}
				for _, u := range got {
					if seen[u.OneTimePub] {
						t.Fatal("repeated member")
					}
					seen[u.OneTimePub] = true
					canonical, err := db.LookupRingMember(u.OneTimePub)
					if err != nil || canonical.AmountCommit != u.AmountCommit {
						t.Fatal("canonical pair changed")
					}
				}
				orders[sampleIdentity(got)]++
			}
		}
	}
	if len(orders) != 24 {
		t.Fatalf("got %d ordered samples, want 24", len(orders))
	}
	for order, times := range orders {
		if times != 1 {
			t.Fatalf("order %s chosen %d times", order, times)
		}
	}
	for a := byte(0); a < 4; a++ {
		got, err := db.sampleRingMembers(1, nil, bytes.NewReader([]byte{a}))
		if err != nil || got[0].OneTimePub != sparseRingOutput(a).OneTimePub {
			t.Fatalf("single draw %d: %v %v", a, got, err)
		}
	}
}

func TestRingSamplingFilteredPermutationRemainsUniform(t *testing.T) {
	db := samplingDB(t)
	for i := byte(0); i < 4; i++ {
		putSamplingOutput(t, db, sparseRingOutput(i))
	}
	locked := sparseRingOutput(4)
	locked.ProtocolLocked = true
	putSamplingOutput(t, db, locked)
	exclude := map[crypto.Point32]bool{sparseRingOutput(1).OneTimePub: true}
	orders := map[string]int{}
	for a := byte(0); a < 4; a++ {
		for b := byte(0); b < 3; b++ {
			for c := byte(0); c < 2; c++ {
				got, err := db.sampleRingMembers(2, exclude, bytes.NewReader([]byte{a, b, c}))
				if err != nil || len(got) != 2 {
					t.Fatalf("filtered draw: %v %v", got, err)
				}
				for _, member := range got {
					if exclude[member.OneTimePub] || member.OneTimePub == locked.OneTimePub {
						t.Fatal("excluded/locked member selected")
					}
				}
				orders[sampleIdentity(got)]++
			}
		}
	}
	if len(orders) != 6 {
		t.Fatalf("filtered orders: %v", orders)
	}
	for _, times := range orders {
		if times != 4 {
			t.Fatalf("filtered selection not uniform: %v", orders)
		}
	}
}

func TestRingSamplingShortfallsAndBoundedRequests(t *testing.T) {
	db := samplingDB(t)
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	empty, err := db.sampleRingMembers(3, nil, bytes.NewReader(nil))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty pool: %v %v", empty, err)
	}
	for i := byte(0); i < 4; i++ {
		putSamplingOutput(t, db, sparseRingOutput(i))
	}
	exclude := map[crypto.Point32]bool{sparseRingOutput(1).OneTimePub: true}
	got, err := db.sampleRingMembers(15, exclude, bytes.NewReader([]byte{0, 0, 0}))
	if err != nil || len(got) != 3 {
		t.Fatalf("shortfall must return every eligible member: %v %v", got, err)
	}
	for _, n := range []int{0, -1} {
		got, err := db.sampleRingMembers(n, nil, nil)
		if got != nil || err != nil {
			t.Fatal("nonpositive count changed")
		}
	}
	excludedOutput := sparseRingOutput(2)
	got, err = db.sampleRingMembersExcluding(15, nil, map[core.UTXOKey]bool{
		{TxHash: excludedOutput.TxHash, OutputIndex: excludedOutput.OutputIndex}: true,
	}, bytes.NewReader([]byte{0, 0, 0}))
	if err != nil || len(got) != 3 {
		t.Fatalf("outpoint exclusion should return every eligible member: %v %v", got, err)
	}
	for _, candidate := range got {
		if candidate.OneTimePub == excludedOutput.OneTimePub {
			t.Fatal("staked outpoint was selected as a ring member")
		}
	}
	for _, entropy := range []io.Reader{nil, bytes.NewReader(nil), bytes.NewReader([]byte{0})} {
		got, err := db.sampleRingMembers(3, nil, entropy)
		if got != nil || err == nil {
			t.Fatalf("entropy fault returned partial success: %v %v", got, err)
		}
	}
	got, err = db.sampleRingMembers(maxRingSamplingRequest+1, nil, bytes.NewReader(nil))
	if got != nil || err == nil {
		t.Fatal("oversized count accepted")
	}
	oversized := make(map[crypto.Point32]bool)
	for i := 0; i <= maxRingSamplingRequest; i++ {
		var pub crypto.Point32
		binary.BigEndian.PutUint64(pub[:8], uint64(i))
		oversized[pub] = true
	}
	if got, err := db.sampleRingMembers(1, oversized, bytes.NewReader(nil)); got != nil || err == nil {
		t.Fatal("oversized exclusions accepted")
	}
}

func TestRingOrdinalRejectionAndEntropyBounds(t *testing.T) {
	value, err := uniformRingOrdinal(bytes.NewReader([]byte{3, 2}), 3)
	if err != nil || value != 2 {
		t.Fatalf("out-of-range draw not rejected: %d %v", value, err)
	}
	entropy := bytes.NewReader(bytes.Repeat([]byte{255}, maxRingEntropyRejections+1))
	if _, err := uniformRingOrdinal(entropy, 3); err == nil || entropy.Len() != 1 {
		t.Fatal("unbounded or biased rejection fallback")
	}
}

type samplingReaderFunc func([]byte, *opt.ReadOptions) ([]byte, error)

func (f samplingReaderFunc) Get(key []byte, options *opt.ReadOptions) ([]byte, error) {
	return f(key, options)
}

func TestRingSamplingStorageFaultDiscardsPartialSelection(t *testing.T) {
	db := samplingDB(t)
	for i := byte(0); i < 4; i++ {
		putSamplingOutput(t, db, sparseRingOutput(i))
	}
	snapshot, err := db.db.GetSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Release()
	for failAt := 1; failAt <= 14; failAt++ {
		calls := 0
		reader := samplingReaderFunc(func(k []byte, o *opt.ReadOptions) ([]byte, error) {
			calls++
			if calls == failAt {
				return nil, errors.New("injected storage read fault")
			}
			return snapshot.Get(k, o)
		})
		got, err := sampleRingSnapshot(reader, 3, nil, bytes.NewReader([]byte{0, 0, 0}))
		if got != nil || err == nil {
			t.Fatalf("storage fault %d leaked partial selection: %v %v", failAt, got, err)
		}
	}
	db.Close()
	if got, err := db.SampleRingMembers(3, nil); got != nil || err == nil {
		t.Fatal("closed backend fell back instead of failing")
	}
}

func TestRingSamplingCorruptionFailsClosed(t *testing.T) {
	for _, fault := range []string{"count", "slot", "reverse", "payload", "key", "lock", "tail"} {
		t.Run(fault, func(t *testing.T) {
			db := samplingDB(t)
			for i := byte(0); i < 4; i++ {
				putSamplingOutput(t, db, sparseRingOutput(i))
			}
			u := sparseRingOutput(3) // tail checked on every nonempty request
			switch fault {
			case "count":
				db.db.Put(ringSamplingCountKey, []byte{1}, nil)
			case "slot":
				db.db.Delete(ringSamplingSlotKey(3), nil)
			case "reverse":
				db.db.Put(ringSamplingReverseKey(u.OneTimePub), ringSamplingUint(2), nil)
			case "payload":
				db.db.Put(ringMemberKey(u.OneTimePub), []byte("{"), nil)
			case "key":
				u.OneTimePub = sparseRingOutput(1).OneTimePub
				data, _ := json.Marshal(u)
				db.db.Put(ringMemberKey(sparseRingOutput(3).OneTimePub), data, nil)
			case "lock":
				u.ProtocolLocked = true
				data, _ := json.Marshal(u)
				db.db.Put(ringMemberKey(u.OneTimePub), data, nil)
			case "tail":
				db.db.Put(ringSamplingSlotKey(4), sparseRingOutput(0).OneTimePub[:], nil)
			}
			if got, err := db.sampleRingMembers(3, nil, bytes.NewReader([]byte{0, 0, 0})); got != nil || err == nil {
				t.Fatalf("corrupt %s accepted: %v %v", fault, got, err)
			}
		})
	}
}

type mutationEntropy struct {
	once   sync.Once
	mutate func()
	reader io.Reader
}

func (m *mutationEntropy) Read(p []byte) (int, error) {
	m.once.Do(m.mutate)
	return m.reader.Read(p)
}

func TestRingSamplingSnapshotAtomicWritesRollbackAndReopen(t *testing.T) {
	path := t.TempDir()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	for i := byte(0); i < 4; i++ {
		putSamplingOutput(t, db, sparseRingOutput(i))
	}
	entropy := &mutationEntropy{reader: bytes.NewReader([]byte{0, 0, 0}), mutate: func() {
		if err := db.DeleteUTXO(sparseRingOutput(1).TxHash, 0); err != nil {
			t.Fatal(err)
		}
		putSamplingOutput(t, db, sparseRingOutput(4))
	}}
	old, err := db.sampleRingMembers(4, nil, entropy)
	if err != nil || sampleIdentity(old) != "01040302" {
		t.Fatalf("sample mixed snapshots: %v %v", sampleIdentity(old), err)
	}
	// Spending is NOT deletion. A spent member remains selectable.
	if err := db.MarkUTXOSpent(sparseRingOutput(0).TxHash, 0); err != nil {
		t.Fatal(err)
	}
	locked := sparseRingOutput(2)
	locked.ProtocolLocked = true
	putSamplingOutput(t, db, locked)
	got, err := db.sampleRingMembers(10, nil, bytes.NewReader([]byte{0, 0}))
	if err != nil || len(got) != 3 {
		t.Fatalf("lock/rollback population: %v %v", got, err)
	}
	seen := map[crypto.Point32]bool{}
	for _, u := range got {
		seen[u.OneTimePub] = true
	}
	if !seen[sparseRingOutput(0).OneTimePub] || seen[locked.OneTimePub] || seen[sparseRingOutput(1).OneTimePub] {
		t.Fatal("spent, locked or rolled-back semantics wrong")
	}
	locked.ProtocolLocked = false
	putSamplingOutput(t, db, locked)
	putSamplingOutput(t, db, locked) // idempotence, no duplicate ordinal
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := db.sampleRingMembers(10, nil, bytes.NewReader([]byte{0, 0, 0}))
	if err != nil || len(reopened) != 4 {
		t.Fatalf("durable index after reopen: %v %v", reopened, err)
	}
}

func TestRingSamplingLegacyBackfillInterruptedIndexAndIdempotence(t *testing.T) {
	db := samplingDB(t)
	batch := new(leveldb.Batch)
	for i := byte(0); i < 4; i++ {
		u := sparseRingOutput(i)
		u.ProtocolLocked = i == 3
		data, _ := json.Marshal(u)
		batch.Put(utxoKey(u.TxHash, 0), data)
	}
	// Simulate a stopped/interrupted derived build with orphan slots. No
	// count/readiness marker may exist until all canonical entries are indexed.
	batch.Put(ringSamplingSlotKey(999), []byte("stale"))
	batch.Put(ringSamplingReverseKey(sparseRingOutput(5).OneTimePub), []byte("stale"))
	if err := db.db.Write(batch, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := db.SampleRingMembers(1, nil); got != nil || err == nil {
		t.Fatal("legacy/unready index silently sampled")
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Get(ringSamplingSlotKey(999), nil); !errors.Is(err, leveldb.ErrNotFound) {
		t.Fatal("interrupted tail not removed")
	}
	got, err := db.sampleRingMembers(10, nil, bytes.NewReader([]byte{0, 0}))
	if err != nil || len(got) != 3 {
		t.Fatalf("backfill failed: %v %v", got, err)
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	again, err := db.sampleRingMembers(10, nil, bytes.NewReader([]byte{0, 0}))
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatal("idempotent prepare changed sampling")
	}
}

func TestRingSamplingInterruptedPreparedRMBuildRestartsAfterReopen(t *testing.T) {
	path := t.TempDir()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Existing v1 rm/ store: deliberately enough entries to flush a partial
	// 1000-member ordinal batch before reaching the malformed last record.
	batch := new(leveldb.Batch)
	batch.Put([]byte("m/ring-member-index-v1"), []byte{1})
	for i := uint64(1); i <= 1001; i++ {
		var u StoredUTXO
		binary.BigEndian.PutUint64(u.OneTimePub[:8], i)
		u.AmountCommit[0] = 1
		data, _ := json.Marshal(u)
		batch.Put(ringMemberKey(u.OneTimePub), data)
	}
	var broken crypto.Point32
	broken[0] = 255
	batch.Put(ringMemberKey(broken), []byte("{"))
	if err := db.db.Write(batch, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureRingMemberIndex(); err == nil {
		t.Fatal("malformed member was silently skipped")
	}
	if _, ready, err := ringSamplingCount(db.db); err != nil || ready {
		t.Fatal("partial build published readiness")
	}
	if data, err := db.db.Get(ringSamplingSlotKey(999), nil); err != nil || len(data) != 32 {
		t.Fatal("fixture did not exercise a flushed partial batch")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The canonical source is explicitly repaired in the temporary fixture.
	if err := db.db.Delete(ringMemberKey(broken), nil); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	count, ready, err := ringSamplingCount(db.db)
	if err != nil || !ready || count != 1001 {
		t.Fatalf("restart: count=%d ready=%t err=%v", count, ready, err)
	}
}

func TestRingSamplingFailedMutationCannotPartiallyWriteCanonicalState(t *testing.T) {
	db := samplingDB(t)
	u := sparseRingOutput(0)
	putSamplingOutput(t, db, u)
	// Metadata corruption must refuse both insertion and rollback before
	// writing u/, rm/, su/ or any derived index entry.
	if err := db.db.Put(ringSamplingCountKey, []byte("bad"), nil); err != nil {
		t.Fatal(err)
	}
	next := sparseRingOutput(1)
	if err := db.PutUTXO(next.TxHash, 0, next); err == nil {
		t.Fatal("insert continued after index fault")
	}
	if got, err := db.GetUTXO(next.TxHash, 0); err != nil || got != nil {
		t.Fatal("failed insert wrote canonical output")
	}
	if err := db.DeleteUTXO(u.TxHash, 0); err == nil {
		t.Fatal("rollback continued after index fault")
	}
	if got, err := db.GetUTXO(u.TxHash, 0); err != nil || got == nil {
		t.Fatal("failed rollback deleted canonical output")
	}
	// A closed real backend fails snapshot acquisition and mutations.
	db.Close()
	if err := db.PutUTXO(next.TxHash, 0, next); err == nil {
		t.Fatal("closed backend accepted write")
	}
	if err := db.DeleteUTXO(u.TxHash, 0); err == nil {
		t.Fatal("closed backend accepted rollback")
	}
}

func TestRingSamplingLegacyWriterCompatibilityRequiresExplicitFence(t *testing.T) {
	db := samplingDB(t)
	for i := byte(0); i < 4; i++ {
		putSamplingOutput(t, db, sparseRingOutput(i))
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	if err := db.InvalidateRingSamplingIndex(); err != nil {
		t.Fatal(err)
	}
	// Simulate old software writing only canonical namespaces. It cannot
	// update or invalidate an index format it does not know about.
	u := sparseRingOutput(4)
	data, _ := json.Marshal(u)
	batch := new(leveldb.Batch)
	batch.Put(utxoKey(u.TxHash, 0), data)
	batch.Put(ringMemberKey(u.OneTimePub), data)
	batch.Delete(ringMemberKey(sparseRingOutput(1).OneTimePub))
	batch.Delete(utxoKey(sparseRingOutput(1).TxHash, 0))
	if err := db.db.Write(batch, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := db.SampleRingMembers(4, nil); got != nil || err == nil {
		t.Fatal("stale derived slots sampled after legacy writes")
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	got, err := db.sampleRingMembers(10, nil, bytes.NewReader([]byte{0, 0, 0}))
	if err != nil || len(got) != 4 {
		t.Fatalf("legacy rebuild: %v %v", got, err)
	}
	seen := map[crypto.Point32]bool{}
	for _, u := range got {
		seen[u.OneTimePub] = true
	}
	if !seen[u.OneTimePub] || seen[sparseRingOutput(1).OneTimePub] {
		t.Fatal("legacy addition/rollback not reflected by rebuild")
	}
}

func TestRingSamplingConcurrentWritersKeepDenseBijection(t *testing.T) {
	db := samplingDB(t)
	const writers = 8
	const each = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for worker := 0; worker < writers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				i := uint64(worker*each + j + 1)
				var u StoredUTXO
				binary.BigEndian.PutUint64(u.OneTimePub[:8], i)
				binary.BigEndian.PutUint64(u.TxHash[:8], i)
				if err := db.PutUTXO(u.TxHash, 0, &u); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// Zero draws repeatedly pick the swapped tail: a complete permutation,
	// not repeated ordinal zero. 256 requires at most one byte per draw.
	got, err := db.sampleRingMembers(writers*each, nil, bytes.NewReader(make([]byte, writers*each)))
	if err != nil || len(got) != writers*each {
		t.Fatalf("concurrent dense population: %d %v", len(got), err)
	}
}

// Complexity proof using an on-demand reader, NOT a claim of real disk
// throughput. Five million canonical pairs need no five-million-entry cache.
func TestRingSamplingMillionEntryPopulationHasBoundedReads(t *testing.T) {
	const population = uint64(5_000_000)
	calls := 0
	reader := samplingReaderFunc(func(key []byte, _ *opt.ReadOptions) ([]byte, error) {
		calls++
		if bytes.Equal(key, ringSamplingCountKey) {
			return ringSamplingUint(population), nil
		}
		if bytes.HasPrefix(key, ringSamplingSlotPrefix) {
			i := binary.BigEndian.Uint64(key[len(ringSamplingSlotPrefix):])
			if i >= population {
				return nil, leveldb.ErrNotFound
			}
			var pub crypto.Point32
			binary.BigEndian.PutUint64(pub[:8], i+1)
			return pub[:], nil
		}
		i := binary.BigEndian.Uint64(key[len(key)-32:len(key)-24]) - 1
		if bytes.HasPrefix(key, ringSamplingReversePrefix) {
			return ringSamplingUint(i), nil
		}
		u := StoredUTXO{}
		binary.BigEndian.PutUint64(u.OneTimePub[:8], i+1)
		u.AmountCommit[0] = 1
		return json.Marshal(u)
	})
	got, err := sampleRingSnapshot(reader, 15, nil, bytes.NewReader(make([]byte, 45)))
	if err != nil || len(got) != 15 || calls != 50 {
		t.Fatalf("population-sized work or failed sample: len=%d calls=%d err=%v", len(got), calls, err)
	}
}
