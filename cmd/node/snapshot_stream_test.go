package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
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