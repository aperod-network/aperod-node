package main

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
)

func TestShutdownWaitsForPeriodicWriterBeforeSavingFinalTip(t *testing.T) {
	dir := t.TempDir()
	db, blocks := buildChainInStore(t, dir, 2)
	stop := make(chan struct{})
	engineDone := make(chan struct{})
	go func() {
		<-stop
		close(engineDone)
	}()
	gate := &sync.Mutex{}
	gate.Lock() // simulate the periodic snapshot writer
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	finished := make(chan struct{})
	go func() {
		performShutdownWithGate(stop, engineDone, db, core.NewUTXOSet(),
			core.NewValidatorRegistry(), dir, log, nil, gate)
		close(finished)
	}()
	select {
	case <-engineDone:
	case <-time.After(3 * time.Second):
		gate.Unlock()
		t.Fatal("shutdown did not stop the engine before waiting for writer")
	}
	select {
	case <-finished:
		t.Fatal("shutdown completed while periodic writer still held the gate")
	case <-time.After(25 * time.Millisecond):
	}
	if _, err := os.Stat(snapshotPath(dir, blocks[len(blocks)-1].Header.Height)); !os.IsNotExist(err) {
		t.Fatalf("shutdown published while writer was active: %v", err)
	}
	gate.Unlock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not resume after periodic writer completed")
	}
	if _, err := os.Stat(snapshotPath(dir, blocks[len(blocks)-1].Header.Height)); err != nil {
		t.Fatalf("final snapshot not published: %v", err)
	}
}