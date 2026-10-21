package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// Use an actual closed LevelDB backend, not a handler error mock. A fresh
// lease over the healthy database must still resume the saved raw position.
func TestWalletOutputSnapshotQueryFailureKeepsDurablePosition(t *testing.T) {
	db, migration, address, path, _, _ := syntheticWalletSnapshotLPoDV3Funding(t)
	db, _, _, _, _, tipHash, height := seedWalletSnapshotAPIFixture(t, db, path, address)
	server := snapshotTestServer(snapshotTestChain(t), core.NewMempool(core.DefaultMempoolConfig()))
	server.SetStore(db)
	server.SetLPoDConfig(migration, func(h uint64, hash crypto.Hash32) bool {
		return h == height && hash == tipHash
	})
	server.SetWalletSnapshotCapture(func() (*store.WalletReadSnapshot, error) {
		return db.NewWalletReadSnapshot()
	})
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() { server.walletSnapshots.closeAll(); httpServer.Close() })
	firstURL := httpServer.URL + "/api/v1/lpod/wallet-outputs?address=" + url.QueryEscape(string(address)) + "&snapshot=1"
	status, first := snapshotRequest(t, httpServer.Client(), http.MethodGet, firstURL, nil)
	if status != http.StatusOK {
		t.Fatalf("valid control failed: %d %#v", status, first)
	}
	id, cursor, raw := stringField(t, first, "snapshot_id"), stringField(t, first, "next_cursor"), stringField(t, first, "resume_cursor")
	continuation := snapshotURL(httpServer.URL, "/api/v1/lpod/wallet-outputs", address, id, cursor)
	status, expected := snapshotRequest(t, httpServer.Client(), http.MethodGet, continuation, nil)
	if status != http.StatusOK || len(expected["outputs"].([]interface{})) != 2 {
		t.Fatalf("valid suffix control failed: %d %#v", status, expected)
	}
	afterHeight := height - 1
	afterHash, found, readErr := db.GetCanonicalHash(afterHeight)
	if readErr != nil || !found {
		t.Fatalf("missing valid after-bound fixture: %v", readErr)
	}
	throughFields := "&through_height=" + fmt.Sprint(height) + "&through_hash=" + fmt.Sprintf("%x", tipHash[:])
	afterFields := "&after_height=" + fmt.Sprint(afterHeight) + "&after_hash=" + fmt.Sprintf("%x", afterHash[:])
	targets := []string{
		snapshotURL(httpServer.URL, "/api/v1/lpod/wallet-outputs", address, id, ""),
		continuation, continuation + throughFields, continuation + afterFields, continuation + throughFields + afterFields,
	}
	// All bounded variants are valid continuations while storage is healthy.
	for _, target := range targets[1:] {
		status, control := snapshotRequest(t, httpServer.Client(), http.MethodGet, target, nil)
		if status != http.StatusOK || !reflect.DeepEqual(control["outputs"], expected["outputs"]) {
			t.Fatalf("bounded positive control failed: %s: %d %#v", target, status, control)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// No cursor: exercises NewIterator/First/Error on the unavailable view.
	// Bound cursor: exercises canonical lookup before Seek on the same view.
	for _, target := range targets {
		response, err := httpServer.Client().Get(target)
		if err != nil {
			t.Fatal(err)
		}
		if response.Header.Get("Retry-After") != "5" {
			t.Fatal("missing retry guidance")
		}
		response.Body.Close()
		status, failure := snapshotRequest(t, httpServer.Client(), http.MethodGet, target, nil)
		if status != http.StatusServiceUnavailable || failure["code"] != "SNAPSHOT_QUERY_FAILED" ||
			failure["error"] != "pinned wallet output query temporarily unavailable" {
			t.Fatalf("storage failure was not a sanitized operational error: %d %#v", status, failure)
		}
		for _, field := range []string{"outputs", "resume_cursor", "next_cursor"} {
			if _, exists := failure[field]; exists {
				t.Fatalf("failure published progress: %s", field)
			}
		}
		if strings.Contains(fmt.Sprint(failure), "closed") {
			t.Fatal("private storage error leaked")
		}
	}
	var err error
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server.SetStore(db)
	query := url.Values{"address": {string(address)}, "snapshot": {"1"},
		"through_height": {fmt.Sprint(height)}, "through_hash": {fmt.Sprintf("%x", tipHash[:])}, "resume_cursor": {raw}}
	status, resumed := snapshotRequest(t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/v1/lpod/wallet-outputs?"+query.Encode(), nil)
	if status != http.StatusOK || !reflect.DeepEqual(resumed["outputs"], expected["outputs"]) ||
		resumed["through_hash"] != first["through_hash"] || resumed["through_height"] != first["through_height"] {
		t.Fatalf("fresh lease did not resume the exact valid suffix: %d %#v", status, resumed)
	}
	server.walletSnapshots.closeAll()
	query.Set("after_height", fmt.Sprint(afterHeight))
	query.Set("after_hash", fmt.Sprintf("%x", afterHash[:]))
	status, resumed = snapshotRequest(t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/v1/lpod/wallet-outputs?"+query.Encode(), nil)
	if status != http.StatusOK || !reflect.DeepEqual(resumed["outputs"], expected["outputs"]) ||
		resumed["through_hash"] != first["through_hash"] || resumed["through_height"] != first["through_height"] {
		t.Fatalf("bounded fresh lease changed durable continuation: %d %#v", status, resumed)
	}
	server.walletSnapshots.closeAll()
	query.Set("resume_cursor", "invalid-not-hex")
	status, invalid := snapshotRequest(t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/v1/lpod/wallet-outputs?"+query.Encode(), nil)
	if status != http.StatusBadRequest || invalid["code"] != "INVALID_CURSOR" {
		t.Fatalf("invalid caller cursor lost its definitive semantics: %d %#v", status, invalid)
	}
}
