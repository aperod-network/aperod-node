package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
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

func TestSIGTERMWaitsForPeriodicWriterBeforeFinalSnapshot(t *testing.T) {
	dir := t.TempDir()
	db, blocks := buildChainInStore(t, dir, 2)
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	stop := make(chan struct{})
	engineDone := make(chan struct{})
	go func() {
		<-stop
		close(engineDone)
	}()

	gate := &sync.Mutex{}
	gate.Lock()
	releaseWriter := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWriter) }) }
	defer release()
	writerDone := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		defer close(writerDone)
		defer gate.Unlock()
		<-releaseWriter
		tip := blocks[len(blocks)-1]
		writerErr <- saveStartupSnapshot(dir, startupSnapshot{
			Version:    snapVersion,
			TipHeight:  tip.Header.Height,
			TipHashHex: fmt.Sprintf("%x", tip.Hash()),
			UTXOs:      utxos.TakeSnapshotView(),
			Registry:   registry.TakeSnapshot(),
		})
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to test process: %v", err)
	}
	select {
	case <-sigCh:
	case <-time.After(3 * time.Second):
		t.Fatal("test process did not receive SIGTERM")
	}

	finished := make(chan struct{})
	go func() {
		performShutdownWithGate(stop, engineDone, db, utxos, registry, dir, log, nil, gate)
		close(finished)
	}()
	select {
	case <-engineDone:
	case <-time.After(3 * time.Second):
		release()
		t.Fatal("SIGTERM shutdown did not stop the engine")
	}
	select {
	case <-finished:
		release()
		t.Fatal("SIGTERM shutdown saved its final snapshot before the periodic writer finished")
	case <-time.After(25 * time.Millisecond):
	}

	release()
	select {
	case <-writerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("periodic writer did not finish")
	}
	if err := <-writerErr; err != nil {
		t.Fatalf("periodic snapshot writer: %v", err)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM shutdown did not save the final snapshot")
	}
	if snap := tryLoadSnapshotFile(snapshotPath(dir, blocks[len(blocks)-1].Header.Height),
		blocks[len(blocks)-1].Header.Height); snap == nil {
		t.Fatal("final snapshot is missing or unreadable after SIGTERM")
	}
}
