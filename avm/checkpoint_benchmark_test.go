package avm

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/aperod/aperod/crypto"
)

const (
	checkpointBenchmarkFixturePathEnv    = "AVM_CHECKPOINT_BENCH_FIXTURE_PATH"
	checkpointBenchmarkFixtureEntriesEnv = "AVM_CHECKPOINT_BENCH_FIXTURE_ENTRIES"
)

var (
	checkpointBenchmarkCheckpointSink *AVMCheckpoint
	checkpointBenchmarkStoreSink      *MemoryStore
)

// TestGenerateSyntheticAVMCheckpointBenchmarkFixture is an opt-in helper for
// preparing a fixture in a separate process before measuring read/restore RSS.
// The fixture is synthetic and must not be used as chain or consensus evidence.
func TestGenerateSyntheticAVMCheckpointBenchmarkFixture(t *testing.T) {
	path := os.Getenv(checkpointBenchmarkFixturePathEnv)
	countText := os.Getenv(checkpointBenchmarkFixtureEntriesEnv)
	if path == "" && countText == "" {
		t.Skip("set AVM_CHECKPOINT_BENCH_FIXTURE_PATH and AVM_CHECKPOINT_BENCH_FIXTURE_ENTRIES to generate a synthetic fixture")
	}
	if path == "" || countText == "" {
		t.Fatalf("both %s and %s are required", checkpointBenchmarkFixturePathEnv, checkpointBenchmarkFixtureEntriesEnv)
	}
	count, err := strconv.Atoi(countText)
	if err != nil || count < 1 || count > maxAVMCheckpointEntries {
		t.Fatalf("invalid synthetic fixture entry count %q", countText)
	}
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("refusing to overwrite existing benchmark fixture %q", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect benchmark fixture path: %v", err)
	}
	checkpoint := syntheticAVMCheckpoint(t, count)
	if err := WriteCheckpointFile(path, checkpoint); err != nil {
		t.Fatalf("write synthetic checkpoint fixture: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat synthetic checkpoint fixture: %v", err)
	}
	t.Logf("synthetic_fixture_entries=%d file_bytes=%d", count, info.Size())
}

// BenchmarkAVMCheckpointSynthetic measures JSON checkpoint writing, bounded
// file decoding, and loading checkpoint entries into the in-memory AVM store.
// Entries use deterministic 32-byte keys and 128-byte values; this is a
// synthetic workload, not a substitute for a verified full-chain rehearsal.
func BenchmarkAVMCheckpointSynthetic(b *testing.B) {
	for _, count := range []int{16_384, 131_072, 524_288} {
		count := count
		b.Run(fmt.Sprintf("entries_%d", count), func(b *testing.B) {
			b.Run("write", func(b *testing.B) {
				checkpoint := syntheticAVMCheckpoint(b, count)
				path := filepath.Join(b.TempDir(), "checkpoint.json")
				b.ReportAllocs()
				b.ReportMetric(float64(count), "entries/op")
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := WriteCheckpointFile(path, checkpoint); err != nil {
						b.Fatalf("write checkpoint: %v", err)
					}
				}
				b.StopTimer()
				info, err := os.Stat(path)
				if err != nil {
					b.Fatalf("stat checkpoint: %v", err)
				}
				b.ReportMetric(float64(info.Size()), "file-B")
			})

			b.Run("read", func(b *testing.B) {
				path := benchmarkFixturePath(b)
				fileBytes := checkpointFixtureSize(b, path)
				b.SetBytes(fileBytes)
				b.ReportAllocs()
				b.ReportMetric(float64(count), "entries/op")
				b.ReportMetric(float64(fileBytes), "file-B")
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					checkpoint, found, err := ReadCheckpointFile(path)
					if err != nil || !found {
						b.Fatalf("read checkpoint: found=%v err=%v", found, err)
					}
					if len(checkpoint.Entries) != count {
						b.Fatalf("read %d entries, want %d", len(checkpoint.Entries), count)
					}
					checkpointBenchmarkCheckpointSink = checkpoint
				}
			})

			b.Run("restore", func(b *testing.B) {
				path := benchmarkFixturePath(b)
				fileBytes := checkpointFixtureSize(b, path)
				b.SetBytes(fileBytes)
				b.ReportAllocs()
				b.ReportMetric(float64(count), "entries/op")
				b.ReportMetric(float64(fileBytes), "file-B")
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					checkpoint, found, err := ReadCheckpointFile(path)
					if err != nil || !found {
						b.Fatalf("read checkpoint for restore: found=%v err=%v", found, err)
					}
					writes := make([]Write, len(checkpoint.Entries))
					for j, entry := range checkpoint.Entries {
						writes[j] = Write(entry)
					}
					restored := NewMemoryStore()
					if err := restored.Apply(writes); err != nil {
						b.Fatalf("apply restored state: %v", err)
					}
					if len(restored.data) != count {
						b.Fatalf("restored %d entries, want %d", len(restored.data), count)
					}
					checkpointBenchmarkStoreSink = restored
				}
			})
		})
	}
}

func syntheticAVMCheckpoint(tb testing.TB, count int) *AVMCheckpoint {
	tb.Helper()
	entries := make([]CheckpointEntry, count)
	for i := range entries {
		key := make([]byte, 32)
		copy(key, []byte("avm-storage-v1"))
		binary.BigEndian.PutUint64(key[24:], uint64(i))

		value := make([]byte, 128)
		binary.BigEndian.PutUint64(value[:8], uint64(i))
		for j := 8; j < len(value); j++ {
			value[j] = byte((uint64(i)*31 + uint64(j)*17) & 0xff)
		}
		entries[i] = CheckpointEntry{Key: key, Value: value}
	}
	checkpoint, err := NewQuorumCheckpointDraft(
		1,
		2,
		crypto.HashStr("synthetic-avm-checkpoint-genesis"),
		crypto.HashStr("synthetic-avm-checkpoint-tip"),
		crypto.HashStr("synthetic-avm-checkpoint-write-set"),
		entries,
	)
	if err != nil {
		tb.Fatalf("build synthetic checkpoint: %v", err)
	}
	return checkpoint
}

func benchmarkFixturePath(tb testing.TB) string {
	tb.Helper()
	path := os.Getenv(checkpointBenchmarkFixturePathEnv)
	if path == "" {
		tb.Fatalf("set %s to a separately generated synthetic fixture", checkpointBenchmarkFixturePathEnv)
	}
	return path
}

func checkpointFixtureSize(tb testing.TB, path string) int64 {
	tb.Helper()
	info, err := os.Stat(path)
	if err != nil {
		tb.Fatalf("stat synthetic fixture: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAVMCheckpointFileBytes {
		tb.Fatalf("synthetic fixture has unsupported size or type: size=%d mode=%s", info.Size(), info.Mode())
	}
	return info.Size()
}
