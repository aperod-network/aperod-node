// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
	"github.com/syndtr/goleveldb/leveldb"
)

func TestWalletSnapshotPagesAndSpentnessStayAtCapturedTip(t *testing.T) {
	db, migration, address, dbPath, validator, validatorPub := syntheticWalletSnapshotLPoDV3Funding(t)
	db, liveRef, spentRef, spentKI, pendingKI, initialHash, initialHeight :=
		seedWalletSnapshotAPIFixture(t, db, dbPath, address)

	chain := snapshotTestChain(t)
	mempool := core.NewMempool(core.DefaultMempoolConfig())
	srv := snapshotTestServer(chain, mempool)
	srv.SetStore(db)
	finalized := atomic.Bool{}
	var finalityCalls atomic.Int32
	srv.SetLPoDConfig(migration, func(height uint64, hash crypto.Hash32) bool {
		finalityCalls.Add(1)
		return finalized.Load() && height == initialHeight && hash == initialHash
	})
	// Production capture wiring must do only the LevelDB snapshot operation;
	// validation and all subsequent reads happen after this callback returns.
	srv.SetWalletSnapshotCapture(func() (*store.WalletReadSnapshot, error) {
		return db.NewWalletReadSnapshot()
	})
	httpServer := httptest.NewServer(srv)
	snapshotIDs := make([]string, 0, 16)
	t.Cleanup(func() {
		for _, id := range snapshotIDs {
			snapshotRequest(t, httpServer.Client(), http.MethodDelete,
				snapshotURL(httpServer.URL, "/api/v1/wallet/snapshot", address, id, ""), nil)
		}
		httpServer.Close()
	})

	unfinalizedURL := httpServer.URL + "/api/v1/lpod/wallet-outputs?address=" + url.QueryEscape(string(address)) + "&snapshot=1"
	code, rejected := snapshotRequest(t, httpServer.Client(), http.MethodGet, unfinalizedURL, nil)
	if code != http.StatusServiceUnavailable || rejected["code"] != "SNAPSHOT_UNAVAILABLE" {
		t.Fatalf("unfinalized capture should fail closed: status=%d body=%#v", code, rejected)
	}
	if got := finalityCalls.Load(); got != 1 {
		t.Fatalf("finality callback count for rejected capture=%d, want 1", got)
	}

	finalized.Store(true)
	firstURL := httpServer.URL + "/api/v1/lpod/wallet-outputs?address=" + url.QueryEscape(string(address)) + "&snapshot=1"

	failedDeliveryContext, cancelFailedDelivery := context.WithCancel(context.Background())
	failedWriter := &failedSnapshotResponseWriter{cancel: cancelFailedDelivery}
	srv.ServeHTTP(failedWriter, httptest.NewRequest(http.MethodGet, firstURL, nil).WithContext(failedDeliveryContext))
	cancelFailedDelivery()
	if failedWriter.status != http.StatusOK || failedWriter.writeErr == nil {
		t.Fatalf("simulated failed first response did not reach the failing writer: %#v", failedWriter)
	}
	if got := activeSnapshotSessions(srv.walletSnapshots); got != 0 {
		t.Fatalf("failed first response retained %d snapshot sessions", got)
	}

	// A request canceled while the snapshot factory is blocked must be marked
	// abandoned. When capture eventually returns its ID, the reader is released
	// and no usable session is published.
	originalCapture := func() (*store.WalletReadSnapshot, error) {
		return db.NewWalletReadSnapshot()
	}
	factoryStarted := make(chan struct{})
	factoryRelease := make(chan struct{})
	factoryReleased := false
	srv.SetWalletSnapshotCapture(func() (*store.WalletReadSnapshot, error) {
		close(factoryStarted)
		<-factoryRelease
		return originalCapture()
	})
	defer srv.SetWalletSnapshotCapture(originalCapture)
	defer func() {
		if !factoryReleased {
			close(factoryRelease)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelledRequest := httptest.NewRequest(http.MethodGet, firstURL, nil).WithContext(ctx)
	cancelledResponse := httptest.NewRecorder()
	cancelledDone := make(chan struct{})
	go func() {
		srv.ServeHTTP(cancelledResponse, cancelledRequest)
		close(cancelledDone)
	}()
	select {
	case <-factoryStarted:
	case <-time.After(5 * time.Second):
		cancel()
		close(factoryRelease)
		factoryReleased = true
		<-cancelledDone
		t.Fatal("snapshot factory was not entered")
	}
	cancel()
	close(factoryRelease)
	factoryReleased = true
	select {
	case <-cancelledDone:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled snapshot request did not return after capture completed")
	}
	if cancelledResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("canceled capture response status=%d body=%s", cancelledResponse.Code, cancelledResponse.Body.String())
	}
	if got := activeSnapshotSessions(srv.walletSnapshots); got != 0 {
		t.Fatalf("canceled factory retained %d snapshot sessions", got)
	}
	srv.SetWalletSnapshotCapture(originalCapture)

	successfulContext, cancelSuccessfulContext := context.WithCancel(context.Background())
	successfulResponse := httptest.NewRecorder()
	srv.ServeHTTP(successfulResponse,
		httptest.NewRequest(http.MethodGet, firstURL, nil).WithContext(successfulContext))
	if successfulResponse.Code != http.StatusOK {
		t.Fatalf("start pinned wallet page: %d %s", successfulResponse.Code, successfulResponse.Body.String())
	}
	cancelSuccessfulContext()
	var first map[string]interface{}
	if err := json.Unmarshal(successfulResponse.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first successful snapshot response: %v", err)
	}
	snapshotID := stringField(t, first, "snapshot_id")
	snapshotIDs = append(snapshotIDs, snapshotID)
	if len(snapshotID) != 64 {
		t.Fatalf("snapshot id length=%d, want 64 hex chars", len(snapshotID))
	}
	if fixturePath := os.Getenv("APERO_NODE_WALLET_PAGE_FIXTURE"); fixturePath != "" {
		firstCursor := stringField(t, first, "next_cursor")
		secondURL := snapshotURL(
			httpServer.URL,
			"/api/v1/lpod/wallet-outputs",
			address,
			snapshotID,
			firstCursor,
		)
		secondStatus, second := snapshotRequest(t, httpServer.Client(), http.MethodGet, secondURL, nil)
		if secondStatus != http.StatusOK {
			t.Fatalf("continue actual wallet output HTTP page: status=%d body=%#v", secondStatus, second)
		}
		if second["snapshot_id"] != snapshotID ||
			second["checkpoint_hash"] != first["checkpoint_hash"] ||
			second["checkpoint_height"] != first["checkpoint_height"] ||
			second["through_height"] != first["through_height"] ||
			second["through_hash"] != first["through_hash"] {
			t.Fatalf("continuation page changed its pinned checkpoint metadata: first=%#v second=%#v", first, second)
		}
		for _, field := range []string{"resume_height", "resume_hash", "resume_event_index"} {
			if _, found := second[field]; found {
				t.Fatalf("native output page must expose its opaque raw resume cursor, not synthetic %s", field)
			}
		}
		secondPage, err := json.Marshal(second)
		if err != nil {
			t.Fatalf("encode actual second wallet output HTTP page: %v", err)
		}
		fixture, err := json.Marshal(map[string]json.RawMessage{
			"first_page":  successfulResponse.Body.Bytes(),
			"second_page": secondPage,
		})
		if err != nil {
			t.Fatalf("encode actual wallet output HTTP page pair: %v", err)
		}
		if err := os.WriteFile(fixturePath, fixture, 0o600); err != nil {
			t.Fatalf("write actual wallet output HTTP page fixture: %v", err)
		}
		t.Skip("actual Go wallet output HTTP page pair exported for the Node adapter test")
	}
	if first["checkpoint_height"] != float64(initialHeight) ||
		first["checkpoint_hash"] != fmt.Sprintf("%x", initialHash[:]) ||
		first["through_height"] != float64(initialHeight) ||
		first["through_hash"] != fmt.Sprintf("%x", initialHash[:]) {
		t.Fatalf("initial page checkpoint anchor: %#v", first)
	}
	firstRows := first["outputs"].([]interface{})
	if len(firstRows) != 128 {
		t.Fatalf("first page returned %d outputs, want 128", len(firstRows))
	}
	cursor := stringField(t, first, "next_cursor")
	if cursor == "" {
		t.Fatal("first page should return a continuation cursor")
	}
	resumeCursor := stringField(t, first, "resume_cursor")
	if resumeCursor == "" {
		t.Fatal("initial native page should return a lease-independent resume_cursor")
	}
	for _, field := range []string{"resume_height", "resume_hash", "resume_event_index"} {
		if _, found := first[field]; found {
			t.Fatalf("native output page must expose its opaque raw resume cursor, not synthetic %s", field)
		}
	}

// A replacement lease must accept the real opaque address-index cursor
// emitted by the prior HTTP page while keeping its original through anchor.
resumeQuery := url.Values{
"address":       {string(address)},
"snapshot":      {"1"},
"through_height": {fmt.Sprint(initialHeight)},
"through_hash":   {fmt.Sprintf("%x", initialHash[:])},
"resume_cursor":  {resumeCursor},
}
resumeURL := httpServer.URL + "/api/v1/lpod/wallet-outputs?" + resumeQuery.Encode()
resumeStatus, resumedPage := snapshotRequest(t, httpServer.Client(), http.MethodGet, resumeURL, nil)
if resumeStatus != http.StatusOK {
t.Fatalf("resume native outputs from real raw index cursor: status=%d body=%#v", resumeStatus, resumedPage)
}
resumeSnapshotID := stringField(t, resumedPage, "snapshot_id")
snapshotIDs = append(snapshotIDs, resumeSnapshotID)
if resumedPage["through_height"] != float64(initialHeight) ||
resumedPage["through_hash"] != fmt.Sprintf("%x", initialHash[:]) ||
stringField(t, resumedPage, "resume_cursor") == "" {
t.Fatalf("renewed native page changed its anchor or omitted its raw cursor: %#v", resumedPage)
}
priorRefs := make(map[string]struct{}, len(firstRows))
for _, value := range firstRows {
row := value.(map[string]interface{})
priorRefs[fmt.Sprintf("%v:%v", row["tx_hash"], row["out_idx"])] = struct{}{}
}
renewedRows := resumedPage["outputs"].([]interface{})
if len(renewedRows) == 0 {
t.Fatal("renewed native page unexpectedly had no continuation outputs")
}
for _, value := range renewedRows {
row := value.(map[string]interface{})
if _, duplicated := priorRefs[fmt.Sprintf("%v:%v", row["tx_hash"], row["out_idx"])]; duplicated {
t.Fatalf("raw-cursor renewal repeated prior native output: %#v", row)
}
}
releaseStatus, releaseResponse := snapshotRequest(
	t,
	httpServer.Client(),
	http.MethodDelete,
	snapshotURL(httpServer.URL, "/api/v1/wallet/snapshot", address, resumeSnapshotID, ""),
	nil,
)
if releaseStatus != http.StatusOK || releaseResponse["released"] != true {
	t.Fatalf("release raw-cursor renewal snapshot: status=%d body=%#v", releaseStatus, releaseResponse)
}
snapshotIDs = snapshotIDs[:len(snapshotIDs)-1]

	// A second independent session lets the test prove the opaque cursor is
	// bound to its originating snapshot token.
	code, second := snapshotRequest(t, httpServer.Client(), http.MethodGet, firstURL, nil)
	if code != http.StatusOK {
		t.Fatalf("start second pinned session: %d %#v", code, second)
	}
	secondID := stringField(t, second, "snapshot_id")
	snapshotIDs = append(snapshotIDs, secondID)
	crossSessionCursor := snapshotURL(httpServer.URL, "/api/v1/lpod/wallet-outputs", address, secondID, cursor)
	code, cursorMismatch := snapshotRequest(t, httpServer.Client(), http.MethodGet, crossSessionCursor, nil)
	if code != http.StatusConflict || cursorMismatch["code"] != "SNAPSHOT_CURSOR_MISMATCH" {
		t.Fatalf("cursor from another session accepted: status=%d body=%#v", code, cursorMismatch)
	}

	otherKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	otherAddress := crypto.AddressFromKeys(crypto.MainnetByte, otherKeys)
	wrongAddress := snapshotURL(httpServer.URL, "/api/v1/lpod/positions", otherAddress, snapshotID, "")
	code, wrongAddressResponse := snapshotRequest(t, httpServer.Client(), http.MethodGet, wrongAddress, nil)
	if code != http.StatusConflict || wrongAddressResponse["code"] != "SNAPSHOT_ADDRESS_MISMATCH" {
		t.Fatalf("session accepted a different address: status=%d body=%#v", code, wrongAddressResponse)
	}
	unknown := snapshotURL(httpServer.URL, "/api/v1/lpod/positions", address, hex.EncodeToString(make([]byte, 32)), "")
	code, unknownResponse := snapshotRequest(t, httpServer.Client(), http.MethodGet, unknown, nil)
	if code != http.StatusConflict || unknownResponse["code"] != "SNAPSHOT_EXPIRED" {
		t.Fatalf("unknown snapshot token status=%d body=%#v", code, unknownResponse)
	}
	invalidAddressURL := httpServer.URL + "/api/v1/lpod/wallet-outputs?address=invalid&snapshot=1"
	code, invalidAddress := snapshotRequest(t, httpServer.Client(), http.MethodGet, invalidAddressURL, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid address accepted for snapshot creation: status=%d body=%#v", code, invalidAddress)
	}

	// The address may hold at most two sessions.
	thirdAddressSession := httpServer.URL + "/api/v1/lpod/wallet-outputs?address=" + url.QueryEscape(string(address)) + "&snapshot=1"
	code, cappedAddress := snapshotRequest(t, httpServer.Client(), http.MethodGet, thirdAddressSession, nil)
	if code != http.StatusServiceUnavailable || cappedAddress["code"] != "SNAPSHOT_UNAVAILABLE" {
		t.Fatalf("per-address session cap not enforced: status=%d body=%#v", code, cappedAddress)
	}

	// Fill the remaining slots with distinct addresses, then verify the global
	// limit rejects the seventeenth live lease.
	for i := 0; i < 14; i++ {
		keys, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatal(err)
		}
		other := crypto.AddressFromKeys(crypto.MainnetByte, keys)
		target := httpServer.URL + "/api/v1/lpod/wallet-outputs?address=" + url.QueryEscape(string(other)) + "&snapshot=1"
		code, body := snapshotRequest(t, httpServer.Client(), http.MethodGet, target, nil)
		if code != http.StatusOK {
			t.Fatalf("create bounded session %d: status=%d body=%#v", i, code, body)
		}
		snapshotIDs = append(snapshotIDs, stringField(t, body, "snapshot_id"))
	}
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	lastAddress := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	lastURL := httpServer.URL + "/api/v1/lpod/wallet-outputs?address=" + url.QueryEscape(string(lastAddress)) + "&snapshot=1"
	code, cappedGlobal := snapshotRequest(t, httpServer.Client(), http.MethodGet, lastURL, nil)
	if code != http.StatusServiceUnavailable || cappedGlobal["code"] != "SNAPSHOT_UNAVAILABLE" {
		t.Fatalf("global session cap not enforced: status=%d body=%#v", code, cappedGlobal)
	}

	// Advance the live store using a real next LPoD transition. This also moves
	// the live wallet index while sessions A/B retain the original LevelDB view.
	advancedHash, advancedHeight := advanceSyntheticLPoDV3(t, db, address, validator, validatorPub)
	if advancedHeight != initialHeight+1 || advancedHash == initialHash {
		t.Fatalf("live store did not advance: height=%d hash=%x", advancedHeight, advancedHash[:])
	}

	pageTwoURL := snapshotURL(httpServer.URL, "/api/v1/lpod/wallet-outputs", address, snapshotID, cursor)
	code, pageTwo := snapshotRequest(t, httpServer.Client(), http.MethodGet, pageTwoURL, nil)
	if code != http.StatusOK {
		t.Fatalf("pinned continuation after live advance: status=%d body=%#v", code, pageTwo)
	}
	if pageTwo["checkpoint_height"] != float64(initialHeight) ||
		pageTwo["checkpoint_hash"] != fmt.Sprintf("%x", initialHash[:]) ||
		pageTwo["through_height"] != float64(initialHeight) ||
		pageTwo["through_hash"] != fmt.Sprintf("%x", initialHash[:]) ||
		pageTwo["resume_cursor"] == "" ||
		len(pageTwo["outputs"].([]interface{})) == 0 {
		t.Fatalf("continuation mixed live tip with pinned page: %#v", pageTwo)
	}

	positionsURL := snapshotURL(httpServer.URL, "/api/v1/lpod/positions", address, snapshotID, "")
	code, positions := snapshotRequest(t, httpServer.Client(), http.MethodGet, positionsURL, nil)
	if code != http.StatusOK || positions["state"] != "active" || positions["address"] != string(address) ||
		positions["checkpoint_hash"] != fmt.Sprintf("%x", initialHash[:]) ||
		positions["checkpoint_height"] != float64(initialHeight) {
		t.Fatalf("pinned positions response: status=%d body=%#v", code, positions)
	}
	if _, hasLiveVaults := positions["vaults"]; hasLiveVaults {
		t.Fatalf("snapshot positions unexpectedly included live vault projection: %#v", positions["vaults"])
	}
	if rows, ok := positions["positions"].([]interface{}); !ok || rows == nil {
		t.Fatalf("pinned positions were not returned as an array: %#v", positions["positions"])
	}

	utxoURL := httpServer.URL + "/api/v1/utxo/" + liveRef.txHash + "/" + fmt.Sprint(liveRef.index) +
		"?snapshot_id=" + url.QueryEscape(snapshotID) + "&address=" + url.QueryEscape(string(address))
	code, pinnedUTXO := snapshotRequest(t, httpServer.Client(), http.MethodGet, utxoURL, nil)
	if code != http.StatusOK || pinnedUTXO["snapshot_id"] != snapshotID ||
		pinnedUTXO["checkpoint_hash"] != fmt.Sprintf("%x", initialHash[:]) {
		t.Fatalf("pinned UTXO lookup: status=%d body=%#v", code, pinnedUTXO)
	}

	unspentKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	spentKIHex := hex.EncodeToString(spentKI[:])
	pendingKIHex := hex.EncodeToString(pendingKI[:])
	unspentKI, err := crypto.ComputeKeyImage(unspentKeys.Spend.Private, unspentKeys.Spend.Public)
	if err != nil {
		t.Fatal(err)
	}
	unspentKIHex := hex.EncodeToString(unspentKI[:])
	postURL := httpServer.URL + "/api/v1/wallet/key-images"
	statusRequest := map[string]interface{}{
		"snapshot_id": snapshotID, "address": string(address),
		"key_images": []string{spentKIHex, pendingKIHex, unspentKIHex},
		"refs": []map[string]interface{}{
			{"tx_hash": spentRef.txHash, "out_idx": spentRef.index},
			{"tx_hash": liveRef.txHash, "out_idx": liveRef.index},
			{"tx_hash": spentRef.txHash, "out_idx": spentRef.index},
		},
	}
	code, initialStatuses := snapshotRequest(t, httpServer.Client(), http.MethodPost, postURL, statusRequest)
	if code != http.StatusOK {
		t.Fatalf("pinned spentness request: status=%d body=%#v", code, initialStatuses)
	}
	initialSpentMap := initialStatuses["spent"].(map[string]interface{})
	initialCanonicalMap := initialStatuses["canonical_spent"].(map[string]interface{})
	initialPendingMap := initialStatuses["pending_locked"].(map[string]interface{})
	if initialSpentMap[spentKIHex] != true || initialSpentMap[unspentKIHex] != true ||
		initialSpentMap[pendingKIHex] != false ||
		initialCanonicalMap[pendingKIHex] != false || initialPendingMap[pendingKIHex] != false {
		t.Fatalf("pinned KI/direct-spent statuses: %#v", initialSpentMap)
	}
	pendingTx := core.Transaction{
		Version: core.TxVersionCommitmentBinding,
		Inputs: []core.RingInput{{
			KeyImage: pendingKI,
			Ring:     make([]crypto.Point32, crypto.RingSize),
		}},
		Outputs:     []core.Output{{}},
		Signatures:  []*crypto.MLSAGSignature{nil},
		RangeProofs: []*crypto.RangeProof{nil},
	}
	pendingTx.Fee = pendingTx.MinFeeAt(mempool.MempoolConfig().BaseFeePerByte)
	if err := mempool.Add(pendingTx); err != nil {
		t.Fatalf("add structurally-valid test mempool transaction: %v", err)
	}
	code, freshStatuses := snapshotRequest(t, httpServer.Client(), http.MethodPost, postURL, statusRequest)
	if code != http.StatusOK || freshStatuses["spent"].(map[string]interface{})[pendingKIHex] != true ||
		freshStatuses["canonical_spent"].(map[string]interface{})[pendingKIHex] != false ||
		freshStatuses["pending_locked"].(map[string]interface{})[pendingKIHex] != true {
		t.Fatalf("pending key image did not refresh for pinned session: status=%d body=%#v", code, freshStatuses)
	}

	// A wrong address cannot release the token, and explicit release retires it.
	releaseURL := httpServer.URL + "/api/v1/wallet/snapshot?address=" + url.QueryEscape(string(otherAddress)) +
		"&snapshot_id=" + url.QueryEscape(snapshotID)
	code, wrongRelease := snapshotRequest(t, httpServer.Client(), http.MethodDelete, releaseURL, nil)
	if code != http.StatusConflict || wrongRelease["code"] != "SNAPSHOT_ADDRESS_MISMATCH" {
		t.Fatalf("wrong-address release accepted: status=%d body=%#v", code, wrongRelease)
	}
	releaseURL = httpServer.URL + "/api/v1/wallet/snapshot?address=" + url.QueryEscape(string(address)) +
		"&snapshot_id=" + url.QueryEscape(snapshotID)
	code, released := snapshotRequest(t, httpServer.Client(), http.MethodDelete, releaseURL, nil)
	if code != http.StatusOK || released["released"] != true {
		t.Fatalf("release pinned session: status=%d body=%#v", code, released)
	}
	code, expired := snapshotRequest(t, httpServer.Client(), http.MethodGet, positionsURL, nil)
	if code != http.StatusConflict || expired["code"] != "SNAPSHOT_EXPIRED" {
		t.Fatalf("released session remained usable: status=%d body=%#v", code, expired)
	}
	if got := finalityCalls.Load(); got != 19 {
		t.Fatalf("finality callback reran for pinned reads: calls=%d, want 19 creation checks including raw-cursor renewal", got)
	}
}

func activeSnapshotSessions(manager *walletSnapshotManager) int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.active
}

func snapshotTestServer(chain *core.Chain, mempool *core.Mempool) *Server {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{
		chain:           chain,
		mempool:         mempool,
		utxos:           core.NewUTXOSet(),
		log:             logger,
		mux:             http.NewServeMux(),
		hub:             &Hub{clients: make(map[*wsClient]struct{}), connPerIP: make(map[string]int), log: logger},
		rateLimiter:     NewRateLimiter(),
		walletSnapshots: newWalletSnapshotManager(),
		startupID:       "wallet-snapshot-test",
	}
	srv.registerRoutes()
	return srv
}

func snapshotTestChain(t *testing.T) *core.Chain {
	t.Helper()
	validator, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	txs := []core.Transaction{core.CoinbaseTx(crypto.Point32(validatorPub), 1_000_000)}
	genesis := &core.Block{Header: core.BlockHeader{
		Height: 0, ValidatorPub: validatorPub, Timestamp: 1,
		MerkleRoot: core.MerkleRoot(txs),
	}, Txs: txs}
	if err := genesis.Header.Sign(validator); err != nil {
		t.Fatal(err)
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	return chain
}

func syntheticWalletSnapshotLPoDV3Funding(t *testing.T) (
	*store.DB,
	*store.LPoDMigration,
	crypto.Address,
	string,
	crypto.ValidatorPrivKey,
	crypto.ValidatorPubKey,
) {
	t.Helper()
	path := t.TempDir()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	validator, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	walletKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	leader := crypto.AddressFromKeys(crypto.MainnetByte, walletKeys)

	genesis := &core.Block{Header: core.BlockHeader{Height: 0, ValidatorPub: validatorPub}}
	genesis.Header.MerkleRoot = core.MerkleRoot(nil)
	if err := genesis.Header.Sign(validator); err != nil {
		t.Fatal(err)
	}
	genesisRaw, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitRawBlockWithAVM(genesis.Hash(), 0, genesisRaw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	parent := &core.Block{Header: core.BlockHeader{
		Height: store.LPoDV2ActivationHeight - 1, PrevHash: genesis.Hash(),
		ValidatorPub: validatorPub, Timestamp: 1,
	}}
	parent.Header.MerkleRoot = core.MerkleRoot(nil)
	if err := parent.Header.Sign(validator); err != nil {
		t.Fatal(err)
	}
	parentRaw, err := json.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(parent.Hash(), parent.Header.Height, parentRaw); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(parent.Hash(), parent.Header.Height); err != nil {
		t.Fatal(err)
	}
	if err := db.StoreStakingPoolRemaining(2_000_000_000 * lpod.Unit); err != nil {
		t.Fatal(err)
	}
	migration := &store.LPoDMigration{
		Version: 3, PositionLifecycleVersion: 1, Height: store.LPoDV2ActivationHeight,
		Genesis: genesis.Hash(), ParentHash: parent.Hash(),
		SnapshotRoot:       crypto.HashBytes([]byte("synthetic signed v3 snapshot")),
		TrustAssumption:    store.LPoDV3TrustAssumption,
		ValidatorRemaining: 2_000_000_000 * lpod.Unit,
		NominalEligible:    6_000_000_000 * lpod.Unit,
		TrustedValidators:  []crypto.ValidatorPubKey{validatorPub},
	}
	snapshot := store.LPoDNominalSnapshot{
		Basis:                    store.LPoDV3NominalBasis,
		NominalCirculatingBefore: 6_000_000_000 * lpod.Unit,
		EligibleNominal:          migration.NominalEligible,
		EligibilityRule:          store.LPoDV3EligibilityRule,
		ValidatorRemaining:       migration.ValidatorRemaining,
	}
	migration.NominalSnapshot = &snapshot
	migration.SnapshotRoot = snapshot.Root()
	migration.ReconciliationRoot = migration.Root()
	signature, err := validator.Sign(migration.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	migration.Attestations = []store.LPoDAttestation{{Validator: validatorPub, Signature: signature}}

	request := &store.LPoDSettlement{
		Parent: parent.Hash(), Migration: migration, PositionProtocol: true,
		Timestamp: parent.Header.Timestamp + 3_000_000_000,
		Proposer:  validatorPub.Hex(), Leader: leader,
		Stake: map[string]store.LPoDValidatorStake{
			validatorPub.Hex(): {Amount: 100_000 * lpod.Unit, Active: true},
		},
	}
	checkpoint, payments, err := db.PreviewLPoD(store.LPoDV2ActivationHeight, request)
	if err != nil {
		t.Fatalf("preview synthetic v3 funding: %v", err)
	}
	payout, err := db.PayoutLPoD(store.LPoDV2ActivationHeight, request, checkpoint, payments)
	if err != nil {
		t.Fatalf("build synthetic v3 payout: %v", err)
	}
	block := &core.Block{
		Header: core.BlockHeader{
			Height: store.LPoDV2ActivationHeight, PrevHash: parent.Hash(),
			ValidatorPub: validatorPub, Timestamp: request.Timestamp,
		},
		Txs: []core.Transaction{payout, core.LPoDCheckpointTx(checkpoint.Digest())},
	}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	if err := block.Header.Sign(validator); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitRawBlockWithAVM(block.Hash(), block.Header.Height, raw, nil, crypto.Hash32{}, request); err != nil {
		t.Fatalf("commit synthetic v3 funding: %v", err)
	}
	return db, migration, leader, path, validator, validatorPub
}

type failedSnapshotResponseWriter struct {
	header   http.Header
	status   int
	writeErr error
	cancel   context.CancelFunc
}

func (w *failedSnapshotResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failedSnapshotResponseWriter) WriteHeader(status int) { w.status = status }

func (w *failedSnapshotResponseWriter) Write([]byte) (int, error) {
	if w.cancel != nil {
		w.cancel()
	}
	w.writeErr = errors.New("simulated disconnected client")
	return 0, w.writeErr
}

type snapshotRef struct {
	txHash string
	index  uint32
}

func seedWalletSnapshotAPIFixture(
	t *testing.T,
	db *store.DB,
	path string,
	address crypto.Address,
) (*store.DB, snapshotRef, snapshotRef, crypto.KeyImage, crypto.KeyImage, crypto.Hash32, uint64) {
	t.Helper()
	tipHash, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	liveHash := crypto.HashBytes([]byte("wallet-snapshot-live-ref"))
	live := store.StoredUTXO{TxHash: liveHash, OutputIndex: 0, BlockHeight: height}
	if err := db.PutUTXO(liveHash, 0, &live); err != nil {
		t.Fatal(err)
	}
	spentHash := crypto.HashBytes([]byte("wallet-snapshot-direct-spent-ref"))
	spent := store.StoredUTXO{TxHash: spentHash, OutputIndex: 7, BlockHeight: height}
	if err := db.PutUTXO(spentHash, spent.OutputIndex, &spent); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkUTXOSpent(spentHash, spent.OutputIndex); err != nil {
		t.Fatal(err)
	}
	spentKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	spentKI, err := crypto.ComputeKeyImage(spentKeys.Spend.Private, spentKeys.Spend.Public)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkKeyImageSpent(spentKI); err != nil {
		t.Fatal(err)
	}
	pendingKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	pendingKI, err := crypto.ComputeKeyImage(pendingKeys.Spend.Private, pendingKeys.Spend.Public)
	if err != nil {
		t.Fatal(err)
	}

	addrHash := crypto.HashBytes([]byte(address))
	prefix := append([]byte("lpod/wallet/v1/"), addrHash[:]...)
	type walletOutput struct {
		hash  crypto.Hash32
		index uint32
		value store.StoredUTXO
	}
	indexed := make([]walletOutput, 129)
	for i := range indexed {
		hash := crypto.HashBytes([]byte(fmt.Sprintf("wallet-snapshot-indexed-output-%03d", i)))
		value := store.StoredUTXO{TxHash: hash, OutputIndex: uint32(i), BlockHeight: height}
		keyHash := crypto.HashBytes([]byte(fmt.Sprintf("wallet-snapshot-one-time-pub-%03d", i)))
		copy(value.OneTimePub[:], keyHash[:])
		txPubHash := crypto.HashBytes([]byte(fmt.Sprintf("wallet-snapshot-tx-pub-%03d", i)))
		copy(value.TxPubKey[:], txPubHash[:])
		if err := db.PutUTXO(hash, uint32(i), &value); err != nil {
			t.Fatal(err)
		}
		indexed[i] = walletOutput{hash: hash, index: uint32(i), value: value}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rawDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range indexed {
		var suffix [12]byte
		binary.BigEndian.PutUint64(suffix[:8], height)
		binary.BigEndian.PutUint32(suffix[8:], row.index)
		key := append([]byte(nil), prefix...)
		key = append(key, suffix[:]...)
		key = append(key, tipHash[:]...)
		key = append(key, row.hash[:]...)
		value, err := json.Marshal(row.value)
		if err != nil {
			t.Fatal(err)
		}
		if err := rawDB.Put(key, value, nil); err != nil {
			t.Fatalf("inject bounded address-index row %d: %v", i, err)
		}
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db,
		snapshotRef{txHash: fmt.Sprintf("%x", liveHash[:]), index: 0},
		snapshotRef{txHash: fmt.Sprintf("%x", spentHash[:]), index: spent.OutputIndex},
		spentKI, pendingKI, tipHash, height
}

func advanceSyntheticLPoDV3(
	t *testing.T,
	db *store.DB,
	leader crypto.Address,
	validator crypto.ValidatorPrivKey,
	validatorPub crypto.ValidatorPubKey,
) (crypto.Hash32, uint64) {
	t.Helper()
	parentHash, parentHeight, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	parentRaw, err := db.GetRawBlock(parentHash)
	if err != nil {
		t.Fatal(err)
	}
	var parent core.Block
	if err := json.Unmarshal(parentRaw, &parent); err != nil {
		t.Fatal(err)
	}
	request := &store.LPoDSettlement{
		Parent: parentHash, PositionProtocol: true,
		Timestamp: parent.Header.Timestamp + 3_000_000_000,
		Proposer:  validatorPub.Hex(), Leader: leader,
		Stake: map[string]store.LPoDValidatorStake{
			validatorPub.Hex(): {Amount: 100_000 * 100_000_000, Active: true},
		},
	}
	nextHeight := parentHeight + 1
	checkpoint, payments, err := db.PreviewLPoD(nextHeight, request)
	if err != nil {
		t.Fatalf("preview post-capture LPoD block: %v", err)
	}
	payout, err := db.PayoutLPoD(nextHeight, request, checkpoint, payments)
	if err != nil {
		t.Fatalf("build post-capture LPoD payout: %v", err)
	}
	block := &core.Block{
		Header: core.BlockHeader{
			Height: nextHeight, PrevHash: parentHash, ValidatorPub: validatorPub,
			Timestamp: request.Timestamp,
		},
		Txs: []core.Transaction{payout, core.LPoDCheckpointTx(checkpoint.Digest())},
	}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	if err := block.Header.Sign(validator); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitRawBlockWithAVM(block.Hash(), nextHeight, raw, nil, crypto.Hash32{}, request); err != nil {
		t.Fatalf("commit post-capture LPoD block: %v", err)
	}
	return block.Hash(), nextHeight
}

func snapshotURL(base, route string, address crypto.Address, id, cursor string) string {
	query := url.Values{"address": []string{string(address)}}
	if id != "" {
		query.Set("snapshot_id", id)
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	return base + route + "?" + query.Encode()
}

func snapshotRequest(
	t *testing.T,
	client *http.Client,
	method, target string,
	requestBody interface{},
) (int, map[string]interface{}) {
	t.Helper()
	var body *bytes.Reader
	if requestBody == nil {
		body = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]interface{}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&decoded); err != nil {
	t.Fatalf("decode %s response: %v", target, err)
	}
	// Finish the chunked response before asserting deferred lease accounting.
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
	t.Fatalf("finish %s response: expected EOF, got %v (extra=%#v)", target, err, extra)
	}
	return response.StatusCode, decoded
}

func stringField(t *testing.T, body map[string]interface{}, field string) string {
	t.Helper()
	value, ok := body[field].(string)
	if !ok || value == "" {
		t.Fatalf("response field %q is missing: %#v", field, body)
	}
	return value
}

func TestWalletOutputSnapshotRenewsFromRawResumeCursorAtFixedTarget(t *testing.T) {
	db, migration, address, path, validator, validatorPub := syntheticWalletSnapshotLPoDV3Funding(t)
	db, _, _, _, _, throughHash, throughHeight := seedWalletSnapshotAPIFixture(t, db, path, address)
	finalized := map[uint64]crypto.Hash32{throughHeight: throughHash}
	server := snapshotTestServer(snapshotTestChain(t), core.NewMempool(core.DefaultMempoolConfig()))
	server.SetStore(db)
	server.SetLPoDConfig(migration, func(height uint64, hash crypto.Hash32) bool {
		return finalized[height] == hash
	})
	server.SetWalletSnapshotCapture(func() (*store.WalletReadSnapshot, error) {
		return db.NewWalletReadSnapshot()
	})
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		server.walletSnapshots.closeAll()
		httpServer.Close()
	})

	firstQuery := url.Values{
		"address":        {string(address)},
		"snapshot":       {"1"},
		"through_height": {fmt.Sprint(throughHeight)},
		"through_hash":   {fmt.Sprintf("%x", throughHash[:])},
	}
	firstURL := httpServer.URL + "/api/v1/lpod/wallet-outputs?" + firstQuery.Encode()
	status, first := snapshotRequest(t, httpServer.Client(), http.MethodGet, firstURL, nil)
	if status != http.StatusOK {
		t.Fatalf("initial native output page: status=%d body=%#v", status, first)
	}
	firstID := stringField(t, first, "snapshot_id")
	firstRows := first["outputs"].([]interface{})
	if len(firstRows) != 128 ||
		first["through_height"] != float64(throughHeight) ||
		first["through_hash"] != fmt.Sprintf("%x", throughHash[:]) ||
		first["checkpoint_height"] != float64(throughHeight) ||
		first["checkpoint_hash"] != fmt.Sprintf("%x", throughHash[:]) {
		t.Fatalf("initial native page did not expose exact checkpoint/target metadata: %#v", first)
	}
	rawResumeCursor := stringField(t, first, "resume_cursor")
	if rawResumeCursor == stringField(t, first, "next_cursor") {
		t.Fatalf("raw native resume cursor was confused with lease cursor: %#v", first)
	}
	firstSeen := make(map[string]bool, len(firstRows))
	outputIdentity := func(row interface{}) string {
		output := row.(map[string]interface{})
		return fmt.Sprintf("%s/%v", output["tx_hash"], output["out_idx"])
	}
	for _, row := range firstRows {
		identity := outputIdentity(row)
		if firstSeen[identity] {
			t.Fatalf("initial native page duplicated output %s", identity)
		}
		firstSeen[identity] = true
	}
	firstNextCursor := stringField(t, first, "next_cursor")
	expectedSuffixURL := snapshotURL(httpServer.URL, "/api/v1/lpod/wallet-outputs", address, firstID, firstNextCursor)
	status, expectedSuffixPage := snapshotRequest(t, httpServer.Client(), http.MethodGet, expectedSuffixURL, nil)
	if status != http.StatusOK {
		t.Fatalf("read original-lease suffix for renewal comparison: status=%d body=%#v", status, expectedSuffixPage)
	}
	expectedSuffix := expectedSuffixPage["outputs"].([]interface{})

	// Expire the original short lease. Only the opaque native address-index
	// position and immutable through anchor cross into the replacement lease.
	server.walletSnapshots.mu.Lock()
	oldEntry := server.walletSnapshots.entries[firstID]
	oldEntry.expiresAt = time.Now().Add(-time.Second)
	server.walletSnapshots.mu.Unlock()
	server.walletSnapshots.expire()
	if got := activeSnapshotSessions(server.walletSnapshots); got != 0 {
		t.Fatalf("expired native lease retained %d active sessions", got)
	}

	advancedHash, advancedHeight := advanceSyntheticLPoDV3(t, db, address, validator, validatorPub)
	finalized[advancedHeight] = advancedHash
	reorgedCursorBytes, err := hex.DecodeString(rawResumeCursor)
	if err != nil {
		t.Fatal(err)
	}
	walletPrefixLength := len("lpod/wallet/v1/") + len(crypto.Hash32{})
	reorgedCursorBytes[walletPrefixLength+12] ^= 1
	reorgedCursorQuery := url.Values{
		"address":        {string(address)},
		"snapshot":       {"1"},
		"through_height": {fmt.Sprint(throughHeight)},
		"through_hash":   {fmt.Sprintf("%x", throughHash[:])},
		"resume_cursor":  {hex.EncodeToString(reorgedCursorBytes)},
	}
	reorgedCursorURL := httpServer.URL + "/api/v1/lpod/wallet-outputs?" + reorgedCursorQuery.Encode()
	status, reorgedCursor := snapshotRequest(t, httpServer.Client(), http.MethodGet, reorgedCursorURL, nil)
	if status != http.StatusConflict || reorgedCursor["code"] != "REORG" {
		t.Fatalf("noncanonical raw native resume cursor did not fail closed: status=%d body=%#v", status, reorgedCursor)
	}
	renewQuery := url.Values{
		"address":        {string(address)},
		"snapshot":       {"1"},
		"through_height": {fmt.Sprint(throughHeight)},
		"through_hash":   {fmt.Sprintf("%x", throughHash[:])},
		"resume_cursor":  {rawResumeCursor},
	}
	renewURL := httpServer.URL + "/api/v1/lpod/wallet-outputs?" + renewQuery.Encode()
	status, renewed := snapshotRequest(t, httpServer.Client(), http.MethodGet, renewURL, nil)
	if status != http.StatusOK {
		t.Fatalf("renew native output page from raw cursor: status=%d body=%#v", status, renewed)
	}
	renewedID := stringField(t, renewed, "snapshot_id")
	renewedRows := renewed["outputs"].([]interface{})
	nextCursor, _ := renewed["next_cursor"].(string)
	if renewedID == firstID || len(renewedRows) != len(expectedSuffix) ||
		renewed["checkpoint_height"] != float64(advancedHeight) ||
		renewed["checkpoint_hash"] != fmt.Sprintf("%x", advancedHash[:]) ||
		renewed["through_height"] != float64(throughHeight) ||
		renewed["through_hash"] != fmt.Sprintf("%x", throughHash[:]) || nextCursor != "" {
		t.Fatalf("renewed page changed its fixed target or failed to return the exact suffix: %#v", renewed)
	}
	if len(renewedRows) != len(expectedSuffix) {
		t.Fatalf("renewed native suffix duplicated or skipped an output: %#v", renewedRows)
	}
	for i, row := range renewedRows {
		identity := outputIdentity(row)
		if firstSeen[identity] || identity != outputIdentity(expectedSuffix[i]) {
			t.Fatalf("renewed suffix row %d duplicated or changed identity: got=%s want=%s", i, identity, outputIdentity(expectedSuffix[i]))
		}
		firstSeen[identity] = true
	}
	wrongTargetQuery := url.Values{
		"address":        {string(address)},
		"snapshot_id":    {renewedID},
		"through_height": {fmt.Sprint(advancedHeight)},
		"through_hash":   {fmt.Sprintf("%x", advancedHash[:])},
	}
	wrongTargetURL := httpServer.URL + "/api/v1/lpod/wallet-outputs?" + wrongTargetQuery.Encode()
	status, wrongTarget := snapshotRequest(t, httpServer.Client(), http.MethodGet, wrongTargetURL, nil)
	if status != http.StatusConflict || wrongTarget["code"] != "SNAPSHOT_CURSOR_MISMATCH" {
		t.Fatalf("renewed lease silently expanded its original through target: status=%d body=%#v", status, wrongTarget)
	}
}
