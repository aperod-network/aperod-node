// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// This fixture owns an actual LevelDB view, not a mocked release counter.
func newSnapshotLifecycleEntry(t *testing.T) (*walletSnapshotManager, *walletSnapshotEntry, *store.WalletReadSnapshot) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	manager := newWalletSnapshotManager()
	t.Cleanup(manager.closeAll)
	entry := &walletSnapshotEntry{
		id: "lifecycle-fixture", address: crypto.Address("synthetic-lifecycle-owner"),
		read: read, bytes: 17, expiresAt: time.Now().Add(walletSnapshotTTL),
	}
	manager.entries[entry.id] = entry
	manager.active = 1
	manager.activeByAddress[entry.address] = 1
	manager.checkpointBytes = entry.bytes
	return manager, entry, read
}

func TestWalletSnapshotExpiryWaitsForLastReaderAndIsIdempotent(t *testing.T) {
	manager, entry, read := newSnapshotLifecycleEntry(t)
	first, err := manager.acquire(entry.id, entry.address)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	last, err := manager.acquire(entry.id, entry.address)
	if err != nil {
		t.Fatal(err)
	}
	defer last.Release()

	manager.mu.Lock()
	entry.expiresAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()
	manager.expire()
	manager.expire()
	manager.closeAll()
	if _, err := manager.acquire(entry.id, entry.address); !errors.Is(err, errWalletSnapshotExpired) {
		t.Fatalf("expired token remained acquirable: %v", err)
	}
	if err := manager.close(entry.id, entry.address); !errors.Is(err, errWalletSnapshotExpired) {
		t.Fatalf("repeated token close: %v", err)
	}
	if len(manager.entries) != 0 || !entry.closed || entry.inflight != 2 ||
		entry.read != read || manager.active != 1 ||
		manager.activeByAddress[entry.address] != 1 || manager.checkpointBytes != 17 {
		t.Fatal("expiry failed to retire the token or evicted an in-flight view")
	}
	if _, _, err := read.GetTip(); err != nil {
		t.Fatalf("expiry closed a physical view still used by readers: %v", err)
	}

	first.Release()
	first.Release()
	manager.expire()
	manager.closeAll()
	if entry.inflight != 1 || manager.active != 1 || entry.read != read ||
		manager.activeByAddress[entry.address] != 1 || manager.checkpointBytes != 17 {
		t.Fatal("first/duplicate reader release freed the last reader's view")
	}
	if _, _, err := read.GetTip(); err != nil {
		t.Fatalf("remaining reader lost its pinned view: %v", err)
	}

	last.Release()
	last.Release()
	manager.expire()
	manager.closeAll()
	if entry.inflight != 0 || entry.read != nil || entry.bytes != 0 ||
		manager.active != 0 || len(manager.activeByAddress) != 0 ||
		manager.checkpointBytes != 0 || len(manager.entries) != 0 {
		t.Fatal("final/duplicate release leaked or double-decremented snapshot accounting")
	}
	// Do not access read after release: this goleveldb version drops its DB
	// pointer on Release, so a subsequent Get can panic rather than return
	// ErrSnapshotReleased. The manager must remove the view reference.
	read.Release() // Wrapper-level duplicate release must remain harmless.
}

type snapshotLifecycleTransport func(*http.Request) (*http.Response, error)

func (f snapshotLifecycleTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type snapshotLifecycleBody struct {
	io.ReadCloser
	reads          int
	firstJSONBytes int
	beforeEOFRead  func()
	sawEOF         *atomic.Bool
	unblockHandler func()
}

func (b *snapshotLifecycleBody) Read(p []byte) (int, error) {
	b.reads++
	if b.reads == 1 {
		// Deliver exactly the complete first JSON value, regardless of TCP
		// fragmentation. EOF cannot arrive until a subsequent read.
		return io.ReadFull(b.ReadCloser, p[:b.firstJSONBytes])
	}
	b.beforeEOFRead()
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.sawEOF.Store(true)
	}
	return n, err
}

func (b *snapshotLifecycleBody) Close() error {
	// Do not strand the server if the old one-value decoder regresses.
	b.unblockHandler()
	return b.ReadCloser.Close()
}

func TestSnapshotRequestFinishesHTTPResponseBeforeLeaseAssertions(t *testing.T) {
	manager, entry, _ := newSnapshotLifecycleEntry(t)
	finishResponse := make(chan struct{})
	var finishOnce sync.Once
	unblock := func() { finishOnce.Do(func() { close(finishResponse) }) }
	defer unblock()
	var sawEOF atomic.Bool
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lease, err := manager.acquire(entry.id, entry.address)
		if err != nil {
			t.Errorf("acquire HTTP reader: %v", err)
			return
		}
		defer lease.Release()
		if err := manager.close(entry.id, entry.address); err != nil {
			t.Errorf("retire HTTP snapshot token: %v", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		// Deliver the whole JSON before returning/releasing the reader.
		// The client must consume the terminal HTTP chunk, not just JSON.
		w.(http.Flusher).Flush()
		select {
		case <-finishResponse:
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			t.Error("client did not finish or cancel the HTTP response")
		}
	}))
	t.Cleanup(httpServer.Close)
	client := httpServer.Client()
	client.Timeout = 3 * time.Second
	baseTransport := client.Transport
	client.Transport = snapshotLifecycleTransport(func(r *http.Request) (*http.Response, error) {
		response, err := baseTransport.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		response.Body = &snapshotLifecycleBody{
			ReadCloser:     response.Body,
			firstJSONBytes: len("{\"ok\":true}\n"),
			beforeEOFRead: func() {
				unblock()
			},
			sawEOF:         &sawEOF,
			unblockHandler: unblock,
		}
		return response, nil
	})
	code, body := snapshotRequest(t, client, http.MethodGet, httpServer.URL, nil)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("unexpected lifecycle HTTP response: %d %#v", code, body)
	}
	if !sawEOF.Load() {
		t.Fatal("HTTP helper returned after JSON without reading through EOF")
	}
	if got := activeSnapshotSessions(manager); got != 0 {
		t.Fatalf("completed HTTP handler retained %d sessions", got)
	}
}
