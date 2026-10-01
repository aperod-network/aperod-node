// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestWalletSnapshotManagerBoundsExpiryAndInflightRelease(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	manager := newWalletSnapshotManager()
	t.Cleanup(manager.closeAll)
	var nextID int
	addSession := func(manager *walletSnapshotManager, address crypto.Address, expiresAt time.Time) string {
		t.Helper()
		read, err := db.NewWalletReadSnapshot()
		if err != nil {
			t.Fatalf("create LevelDB snapshot: %v", err)
		}
		nextID++
		id := fmt.Sprintf("test-%d", nextID)
		entry := &walletSnapshotEntry{
			id: id, address: address, read: read, bytes: 1,
			expiresAt: expiresAt,
		}
		manager.mu.Lock()
		manager.entries[id] = entry
		manager.active++
		manager.activeByAddress[address]++
		manager.checkpointBytes++
		manager.mu.Unlock()
		return id
	}
	newAddress := func() crypto.Address {
		t.Helper()
		keys, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatal(err)
		}
		return crypto.AddressFromKeys(crypto.MainnetByte, keys)
	}

	address := newAddress()
	addSession(manager, address, time.Now().Add(walletSnapshotTTL))
	addSession(manager, address, time.Now().Add(walletSnapshotTTL))
	srv := &Server{walletSnapshots: manager}
	captureCalls := 0
	srv.walletSnapshotCapture = func() (*store.WalletReadSnapshot, error) {
		captureCalls++
		return db.NewWalletReadSnapshot()
	}
	if _, err := manager.capture(context.Background(), srv, address); err == nil {
		t.Fatal("capture exceeded per-address limit")
	}
	if captureCalls != 0 {
		t.Fatalf("per-address rejection called snapshot capturer %d times", captureCalls)
	}

	// Complete the sixteen-session global budget across eight addresses.
	for i := 0; i < 7; i++ {
		other := newAddress()
		addSession(manager, other, time.Now().Add(walletSnapshotTTL))
		addSession(manager, other, time.Now().Add(walletSnapshotTTL))
	}
	if got := manager.active; got != walletSnapshotGlobalLimit {
		t.Fatalf("active synthetic sessions=%d, want %d", got, walletSnapshotGlobalLimit)
	}
	if _, err := manager.capture(context.Background(), srv, newAddress()); err == nil {
		t.Fatal("capture exceeded global session limit")
	}
	if captureCalls != 0 {
		t.Fatalf("global rejection called snapshot capturer %d times", captureCalls)
	}

	// Releasing a token while a reader lease is in flight retires the token
	// immediately but defers the LevelDB snapshot release until that lease exits.
	leaseManager := newWalletSnapshotManager()
	t.Cleanup(leaseManager.closeAll)
	leaseAddress := newAddress()
	leaseID := addSession(leaseManager, leaseAddress, time.Now().Add(walletSnapshotTTL))
	entry := leaseManager.entries[leaseID]

	lease, err := leaseManager.acquire(leaseID, leaseAddress)
	if err != nil {
		t.Fatalf("acquire in-flight lease: %v", err)
	}
	releaseServer := &Server{walletSnapshots: leaseManager}
	releaseRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/wallet/snapshot",
		strings.NewReader(fmt.Sprintf(`{"address":%q,"snapshot_id":%q}`, leaseAddress, leaseID)))
	releaseResponse := httptest.NewRecorder()
	releaseServer.restWalletSnapshot(releaseResponse, releaseRequest)
	if releaseResponse.Code != http.StatusOK {
		t.Fatalf("HTTP release of in-flight session returned %d: %s", releaseResponse.Code, releaseResponse.Body.String())
	}
	if leaseManager.active != 1 || entry.read == nil {
		t.Fatal("closed session released its reader before the in-flight lease returned")
	}
	lease.Release()
	if leaseManager.active != 0 || entry.read != nil || leaseManager.checkpointBytes != 0 {
		t.Fatal("final lease return did not release the LevelDB view and accounting")
	}

	expiredManager := newWalletSnapshotManager()
	t.Cleanup(expiredManager.closeAll)
	expiredAddress := newAddress()
	expiredID := addSession(expiredManager, expiredAddress, time.Now().Add(-time.Second))
	expiredEntry := expiredManager.entries[expiredID]

	if _, err := expiredManager.acquire(expiredID, expiredAddress); err != errWalletSnapshotExpired {
		t.Fatalf("expired session acquire error=%v, want %v", err, errWalletSnapshotExpired)
	}
	if expiredManager.active != 0 || expiredEntry.read != nil {
		t.Fatal("expired session did not release its idle LevelDB view")
	}

	memoryBoundManager := newWalletSnapshotManager()
	memoryBoundManager.checkpointBytes = walletSnapshotCheckpointLimit
	memoryBoundServer := &Server{walletSnapshots: memoryBoundManager}
	memoryCaptureCalls := 0
	memoryBoundServer.walletSnapshotCapture = func() (*store.WalletReadSnapshot, error) {
		memoryCaptureCalls++
		return db.NewWalletReadSnapshot()
	}
	if _, err := memoryBoundManager.capture(context.Background(), memoryBoundServer, newAddress()); err == nil {
		t.Fatal("capture exceeded aggregate checkpoint memory budget")
	}
	if memoryCaptureCalls != 0 {
		t.Fatalf("memory-bound rejection called snapshot capturer %d times", memoryCaptureCalls)
	}
}
