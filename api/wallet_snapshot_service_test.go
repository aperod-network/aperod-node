// SPDX-License-Identifier: Apache-2.0
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// Test-only service: real REST handlers and LevelDB, synthetic data, no P2P,
// signer service, production credentials, database or consensus worker.
type walletSnapshotLoopback struct {
	node          *Server
	http          *httptest.Server
	query         url.Values
	spentKeyImage string
	mu            sync.Mutex
	active        int
	idle          chan struct{}
	stop          chan struct{}
	stopOnce      sync.Once
	canceled      chan struct{}
	cancelOnce    sync.Once
}

func newWalletSnapshotLoopback(t *testing.T) *walletSnapshotLoopback {
	t.Helper()
	db, migration, address, path, validator, pub := syntheticWalletSnapshotLPoDV3Funding(t)
	db, _, _, spentKI, _, parent, height := seedWalletSnapshotAPIFixture(t, db, path, address)
	// Ordinary inert output rows make metadata lookup and pagination real.
	// No transaction is broadcast and no private key/amount is returned.
	var keyImage crypto.KeyImage
	copy(keyImage[:], crypto.HashToCurvePoint([]byte("service-inert-input")).Bytes())
	tx := core.Transaction{Version: core.TxVersionBase, Inputs: []core.RingInput{{KeyImage: keyImage}}}
	for i := 0; i < 3; i++ {
		var point crypto.Point32
		copy(point[:], crypto.HashToCurvePoint([]byte(fmt.Sprintf("service-output-%d", i))).Bytes())
		tx.Outputs = append(tx.Outputs, core.Output{OneTimePub: point, TxPubKey: point})
	}
	tip, tipHeight := advanceSyntheticLPoDV3(t, db, address, validator, pub, tx)
	node := snapshotTestServer(snapshotTestChain(t), core.NewMempool(core.DefaultMempoolConfig()))
	node.SetStore(db)
	node.SetLPoDConfig(migration, func(h uint64, hash crypto.Hash32) bool {
		return h == tipHeight && hash == tip
	})
	node.SetWalletSnapshotCapture(func() (*store.WalletReadSnapshot, error) { return db.NewWalletReadSnapshot() })
	f := &walletSnapshotLoopback{node: node, idle: make(chan struct{}), stop: make(chan struct{}), canceled: make(chan struct{}), query: url.Values{
		"address": {string(address)}, "from_height": {fmt.Sprint(height)}, "from_hash": {fmt.Sprintf("%x", parent[:])},
		"witness_height": {fmt.Sprint(tipHeight)}, "witness_hash": {fmt.Sprintf("%x", tip[:])}, "limit": {"1"},
	}}
	f.spentKeyImage = fmt.Sprintf("%x", spentKI[:])
	close(f.idle)
	mux := http.NewServeMux()
	mux.HandleFunc("/_fixture/state", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		idle := f.idle
		f.mu.Unlock()
		select {
		case <-idle:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			http.Error(w, "native handler did not finish", 500)
			return
		}
		m := node.walletSnapshots
		m.mu.Lock()
		defer m.mu.Unlock()
		inflight := 0
		for _, entry := range m.entries {
			inflight += entry.inflight
		}
		writeJSON(w, 200, map[string]int{"active": m.active, "entries": len(m.entries), "readers": inflight, "bytes": m.checkpointBytes})
	})
	mux.HandleFunc("/_fixture/expire", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		m := node.walletSnapshots
		m.mu.Lock()
		for _, entry := range m.entries {
			entry.expiresAt = time.Now().Add(-time.Second)
		}
		m.expireLocked(time.Now())
		m.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"expired": true})
	})
	mux.HandleFunc("/_fixture/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, 200, map[string]bool{"stopping": true})
		f.stopOnce.Do(func() { close(f.stop) })
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fixture-Cancel-Witness") == "1" {
			go func() {
				<-r.Context().Done()
				f.cancelOnce.Do(func() { close(f.canceled) })
			}()
		}
		f.mu.Lock()
		if f.active == 0 {
			f.idle = make(chan struct{})
		}
		f.active++
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.active--
			if f.active == 0 {
				close(f.idle)
			}
			f.mu.Unlock()
		}()
		node.ServeHTTP(w, r)
	})
	f.http = httptest.NewServer(mux)
	t.Cleanup(func() {
		f.http.Close()
		node.walletSnapshots.closeAll()
		_ = db.Close()
	})
	return f
}

func (f *walletSnapshotLoopback) state(t *testing.T, active int) {
	t.Helper()
	code, body := snapshotRequest(t, f.http.Client(), "GET", f.http.URL+"/_fixture/state", nil)
	if code != 200 || body["active"] != float64(active) || body["entries"] != float64(active) || body["readers"] != float64(0) {
		t.Fatalf("snapshot accounting: code=%d state=%v want active=%d, no in-flight reader", code, body, active)
	}
	if active == 0 && body["bytes"] != float64(0) {
		t.Fatalf("retained checkpoint bytes: %v", body)
	}
}

// The Node/Vitest service integration invokes this same fixture explicitly.
// Normal Go suites instead exercise cancellation over an actual TCP connection.
func TestWalletSnapshotLoopbackService(t *testing.T) {
	f := newWalletSnapshotLoopback(t)
	if os.Getenv("APEROD_TEST_SNAPSHOT_SERVICE") == "1" {
		info, err := json.Marshal(map[string]interface{}{"url": f.http.URL, "query": f.query.Encode(), "spent_key_image": f.spentKeyImage})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("APEROD_SNAPSHOT_FIXTURE=%s\n", info)
		select {
		case <-f.stop:
		case <-time.After(2 * time.Minute):
			t.Fatal("test service was not stopped")
		}
		f.state(t, 0)
		return
	}
	target := f.http.URL + "/api/v1/wallet/changes?" + f.query.Encode()
	code, first := snapshotRequest(t, f.http.Client(), "GET", target, nil)
	if code != 200 || first["complete"] != false {
		t.Fatalf("paginated fixture: %d %v", code, first)
	}
	f.state(t, 1)

	// Hold capture before the first page, close TCP via cancellation, then let
	// the captured real LevelDB view return. Completion must not retire A.
	f.node.walletSnapshotCaptureMu.RLock()
	original := f.node.walletSnapshotCapture
	f.node.walletSnapshotCaptureMu.RUnlock()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	f.node.SetWalletSnapshotCapture(func() (*store.WalletReadSnapshot, error) {
		close(entered)
		<-release
		return original()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Fixture-Cancel-Witness", "1")
	result := make(chan error, 1)
	go func() {
		response, err := f.http.Client().Do(req)
		if response != nil {
			response.Body.Close()
		}
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("capture not entered")
	}
	// Observe cancellation at the server, not merely client-side Do returning.
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled client received a response")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not cancel")
	}
	select {
	case <-f.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not observe TCP cancellation")
	}
	releaseOnce.Do(func() { close(release) })
	f.state(t, 1)
	f.node.SetWalletSnapshotCapture(original)
	next, err := url.ParseQuery(f.query.Encode())
	if err != nil {
		t.Fatal(err)
	}
	next.Set("snapshot_id", first["snapshot_id"].(string))
	next.Set("cursor", first["cursor"].(string))
	code, page := snapshotRequest(t, f.http.Client(), "GET", f.http.URL+"/api/v1/wallet/changes?"+next.Encode(), nil)
	if code != 200 || page["snapshot_id"] != first["snapshot_id"] {
		t.Fatalf("other reader's snapshot lost: %d %v", code, page)
	}
	snapshotRequest(t, f.http.Client(), "DELETE", snapshotURL(f.http.URL, "/api/v1/wallet/snapshot", crypto.Address(f.query.Get("address")), first["snapshot_id"].(string), ""), nil)
	f.state(t, 0)
}
