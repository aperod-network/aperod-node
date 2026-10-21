package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
)

type snapshotWriteProbe struct {
	bytes.Buffer
	largestWrite int
}

func (p *snapshotWriteProbe) Write(b []byte) (int, error) {
	if len(b) > p.largestWrite {
		p.largestWrite = len(b)
	}
	return p.Buffer.Write(b)
}

func TestWriteStartupSnapshotJSONPreservesSchemaInBoundedChunks(t *testing.T) {
	snap := startupSnapshot{
		Version:   snapVersion,
		TipHeight: 1234,
		TxTotal:   25,
		SavedAt:   time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC),
		UTXOs: core.UTXOSnapshot{
			ActiveUTXOs: make([]*core.UTXO, 601),
			StakedUTXOs: []*core.UTXO{{}},
		},
	}
	for i := range snap.UTXOs.ActiveUTXOs {
		snap.UTXOs.ActiveUTXOs[i] = &core.UTXO{}
	}
	var out snapshotWriteProbe
	if err := writeStartupSnapshotJSON(&out, snap); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var gotObject, wantObject any
	if err := json.Unmarshal(out.Bytes(), &gotObject); err != nil {
		t.Fatalf("streamed JSON invalid: %v", err)
	}
	if err := json.Unmarshal(want, &wantObject); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotObject, wantObject) {
		t.Fatal("streamed snapshot changed the persisted JSON schema")
	}
	if out.largestWrite >= len(want)/2 {
		t.Fatalf("writer buffered too much at once: largest=%d, total=%d", out.largestWrite, len(want))
	}
}

type failingSnapshotWriter struct{}

func (failingSnapshotWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestWriteStartupSnapshotJSONPropagatesWriteErrors(t *testing.T) {
	err := writeStartupSnapshotJSON(failingSnapshotWriter{}, startupSnapshot{})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("want writer failure, got %v", err)
	}
}

type snapshotScaleProbe struct {
	bytesWritten int64
	largestWrite int
	nextSample   int64
	peakHeap     uint64
}

func (p *snapshotScaleProbe) Write(b []byte) (int, error) {
	p.bytesWritten += int64(len(b))
	if len(b) > p.largestWrite {
		p.largestWrite = len(b)
	}
	if p.bytesWritten >= p.nextSample {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		if mem.HeapAlloc > p.peakHeap {
			p.peakHeap = mem.HeapAlloc
		}
		p.nextSample += 8 << 20
	}
	return len(b), nil
}

// Run explicitly with APEROD_SNAPSHOT_SCALE_TEST=1. It builds a production-
// sized set and streams the full JSON to a counting sink without retaining the
// output. Keeping it opt-in avoids making every package test allocate hundreds
// of MiB.
func TestSnapshotViewTwoMillionUTXOsHasBoundedMemory(t *testing.T) {
	if os.Getenv("APEROD_SNAPSHOT_SCALE_TEST") != "1" {
		t.Skip("set APEROD_SNAPSHOT_SCALE_TEST=1 to run the 2M UTXO memory test")
	}
	const count = 2_000_000
	records := make([]*core.UTXO, count)
	for i := range records {
		var txHash [32]byte
		var oneTimePub [32]byte
		binary.LittleEndian.PutUint64(txHash[24:], uint64(i+1))
		binary.LittleEndian.PutUint64(oneTimePub[24:], uint64(i+1))
		records[i] = &core.UTXO{TxHash: txHash, OneTimePub: oneTimePub}
	}
	utxos := core.NewUTXOSet()
	utxos.RestoreFromSnapshot(core.UTXOSnapshot{ActiveUTXOs: records})
	records = nil
	runtime.GC()

	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	snapshot := utxos.TakeSnapshotView()
	var afterView runtime.MemStats
	runtime.ReadMemStats(&afterView)
	if len(snapshot.ActiveUTXOs) != count {
		t.Fatalf("snapshot view has %d UTXOs, want %d", len(snapshot.ActiveUTXOs), count)
	}
	viewAlloc := afterView.TotalAlloc - before.TotalAlloc
	if viewAlloc > 64<<20 {
		t.Fatalf("snapshot view allocated %d MiB for pointer indexes; limit is 64 MiB", viewAlloc>>20)
	}

	probe := &snapshotScaleProbe{
		nextSample: 8 << 20,
		peakHeap:   afterView.HeapAlloc,
	}
	snap := startupSnapshot{
		Version: snapVersion,
		UTXOs:   snapshot,
	}
	compressedPath := filepath.Join(t.TempDir(), "snapshot.json.gz")
	compressedFile, err := os.Create(compressedPath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(compressedFile)
	writeErr := writeStartupSnapshotJSON(io.MultiWriter(gz, probe), snap)
	gzipErr := gz.Close()
	fileErr := compressedFile.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if gzipErr != nil {
		t.Fatal(gzipErr)
	}
	if fileErr != nil {
		t.Fatal(fileErr)
	}
	compressedInfo, err := os.Stat(compressedPath)
	if err != nil {
		t.Fatal(err)
	}
	if compressedInfo.Size() == 0 {
		t.Fatal("gzip snapshot file is empty")
	}
	if probe.bytesWritten < count*100 {
		t.Fatalf("stream wrote only %d bytes for %d UTXOs", probe.bytesWritten, count)
	}
	if probe.largestWrite > 256<<10 {
		t.Fatalf("snapshot writer emitted a %d-byte burst; limit is 256 KiB", probe.largestWrite)
	}
	if extra := probe.peakHeap - afterView.HeapAlloc; extra > 128<<20 {
		t.Fatalf("snapshot JSON grew live heap by %d MiB; limit is 128 MiB", extra>>20)
	}
	t.Logf("2M UTXOs: view allocated %d MiB; JSON/gzip peak grew heap by %d MiB; wrote %d MiB JSON / %d MiB gzip (largest write %d KiB)",
		viewAlloc>>20,
		(probe.peakHeap-afterView.HeapAlloc)>>20,
		probe.bytesWritten>>20,
		compressedInfo.Size()>>20,
		probe.largestWrite>>10)
}
