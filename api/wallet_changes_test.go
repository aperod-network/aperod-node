package api

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
	"github.com/syndtr/goleveldb/leveldb"
)

func TestWalletBlockEventsIncludeOrdinaryOutputsAndPublicSpends(t *testing.T) {
	var pub crypto.Point32
	copy(pub[:], crypto.HashToCurvePoint([]byte("wallet delta test output")).Bytes())
	var keyImage crypto.KeyImage
	copy(keyImage[:], crypto.HashToCurvePoint([]byte("wallet delta test key image")).Bytes())
	source := crypto.Hash32{4, 5, 6}
	extra, err := core.EncodeStakeExtraV3(
		core.StakeDeposit, crypto.ValidatorPubKey(make([]byte, 32)), 17,
		make([]byte, 64), source, 3, crypto.BlindFactor{}, make([]byte, 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	block := &core.Block{
		Header: core.BlockHeader{Height: 12},
		Txs: []core.Transaction{
			{
				Version: core.TxVersionBase,
				Inputs:  []core.RingInput{{KeyImage: keyImage}},
				Outputs: []core.Output{{OneTimePub: pub}},
			},
			{Version: core.TxVersionStake, Extra: extra},
		},
	}
	events, err := walletBlockEvents(block, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want output + key image + direct stake ref", len(events))
	}
	if events[0]["kind"] != "output" {
		t.Fatalf("event order starts with %v, want output", events[0]["kind"])
	}
	output := events[0]["output"].(map[string]interface{})
	if output["block_height"] != uint64(12) || output["out_idx"] != uint32(0) {
		t.Fatalf("unexpected output metadata: %+v", output)
	}
	if output["opening_status"] != "OUTPUT_OPENING_UNAVAILABLE" {
		t.Fatalf("transparent output without an indexed public amount must fail closed: %+v", output)
	}
	if events[1]["kind"] != "spent_key_image" ||
		events[1]["key_image_hex"] != hex.EncodeToString(keyImage[:]) {
		t.Fatalf("unexpected key image event: %+v", events[1])
	}
	if events[2]["kind"] != "spent_ref" || events[2]["tx_hash"] != hex.EncodeToString(source[:]) ||
		events[2]["out_idx"] != uint32(3) {
		t.Fatalf("unexpected direct stake spend event: %+v", events[2])
	}
}

func TestWalletBlockEventsExposeOnlyAuthoritativeTransparentMintAmount(t *testing.T) {
	ownerKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	foreignKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	ownerAddress := crypto.AddressFromKeys(crypto.MainnetByte, ownerKeys)
	foreignAddress := crypto.AddressFromKeys(crypto.MainnetByte, foreignKeys)
	const amount = uint64(987654321)
	ownerMint, err := core.BuildMintTx(ownerAddress, amount, 22)
	if err != nil {
		t.Fatal(err)
	}
	foreignMint, err := core.BuildMintTx(foreignAddress, amount, 22)
	if err != nil {
		t.Fatal(err)
	}
	var stealthTxPub crypto.Point32
	copy(stealthTxPub[:], crypto.HashToCurvePoint([]byte("private stealth tx pub")).Bytes())
	block := &core.Block{Header: core.BlockHeader{Height: 22}, Txs: []core.Transaction{
		*ownerMint,
		*foreignMint,
		{Version: core.TxVersionBase, Outputs: []core.Output{{TxPubKey: stealthTxPub}}},
	}}
	events, err := walletBlockEvents(block, func(txHash crypto.Hash32, _ uint32) (uint64, bool, error) {
		if txHash == ownerMint.Hash() {
			return amount, true, nil
		}
		return 0, false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerOutput := events[0]["output"].(map[string]interface{})
	if ownerOutput["amount_napr"] != fmt.Sprint(amount) {
		t.Fatalf("known public mint opening was not emitted exactly: %+v", ownerOutput)
	}
	foreignOutput := events[1]["output"].(map[string]interface{})
	if foreignOutput["opening_status"] != "OUTPUT_OPENING_UNAVAILABLE" {
		t.Fatalf("foreign/unknown mint output was silently discarded: %+v", foreignOutput)
	}
	stealthOutput := events[2]["output"].(map[string]interface{})
	if _, exists := stealthOutput["amount_napr"]; exists {
		t.Fatalf("confidential stealth amount leaked in public event: %+v", stealthOutput)
	}
	if _, exists := stealthOutput["opening_status"]; exists {
		t.Fatalf("ordinary stealth output incorrectly failed transparent-opening check: %+v", stealthOutput)
	}
}

func TestWalletChangesCursorIsAuthenticatedAndSnapshotBound(t *testing.T) {
	serverKey := []byte("private per-snapshot server key, not the public session ID")
	cursor := walletChangesCursor{
		SnapshotID: "snapshot", Address: "address", FromHeight: 2,
		FromHash: "from", Target: 8, TargetHash: "target", Height: 4, EventIndex: 9,
	}
	token := signWalletChangesCursor(serverKey, cursor)
	parsed, err := parseWalletChangesCursor(serverKey, "snapshot", token)
	if err != nil || parsed != cursor {
		t.Fatalf("cursor round trip=%+v err=%v", parsed, err)
	}
	if _, err := parseWalletChangesCursor(serverKey, "another-snapshot", token); err == nil {
		t.Fatal("accepted cursor signed for another snapshot")
	}
	forged := signWalletChangesCursor([]byte("snapshot"), cursor)
	if _, err := parseWalletChangesCursor(serverKey, "snapshot", forged); err == nil {
		t.Fatal("accepted cursor forged using the known public snapshot ID")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 1
	if _, err := parseWalletChangesCursor(serverKey, "snapshot", base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("accepted tampered cursor")
	}
}

func TestWalletChangesPagesAreIdempotentAndIncludeNativeStakeSpend(t *testing.T) {
	db, migration, address, path, _, _ := syntheticWalletSnapshotLPoDV3Funding(t)
	parent, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	baseHash, found, err := db.GetCanonicalHash(height - 1)
	if err != nil || !found || baseHash == (crypto.Hash32{}) {
		t.Fatalf("read bootstrap base hash: %x err=%v", baseHash, err)
	}
	witnessServer := snapshotTestServer(snapshotTestChain(t), core.NewMempool(core.DefaultMempoolConfig()))
	witnessServer.SetStore(db)
	witnessServer.SetLPoDConfig(migration, func(candidateHeight uint64, candidateHash crypto.Hash32) bool {
		return candidateHeight == height && candidateHash == parent
	})
	witnessServer.SetWalletSnapshotCapture(func() (*store.WalletReadSnapshot, error) {
		return db.NewWalletReadSnapshot()
	})
	witnessHTTP := httptest.NewServer(witnessServer)
	defer witnessHTTP.Close()
	witnessQuery := url.Values{
		"address":        {string(address)},
		"from_height":    {fmt.Sprint(height - 1)},
		"from_hash":      {fmt.Sprintf("%x", baseHash[:])},
		"witness_height": {fmt.Sprint(height)},
		"witness_hash":   {fmt.Sprintf("%x", parent[:])},
		"limit":          {"1"},
	}
	witnessURL := witnessHTTP.URL + "/api/v1/wallet/changes?" + witnessQuery.Encode()
	status, witnessResponse := snapshotRequest(t, witnessHTTP.Client(), http.MethodGet, witnessURL, nil)
	if status != http.StatusOK || witnessResponse["complete"] != true {
		t.Fatalf("canonical descendant witness should permit new lease: status=%d body=%#v", status, witnessResponse)
	}
	releaseURL := witnessHTTP.URL + "/api/v1/wallet/snapshot?address=" + url.QueryEscape(string(address)) +
		"&snapshot_id=" + url.QueryEscape(witnessResponse["snapshot_id"].(string))
	if status, released := snapshotRequest(t, witnessHTTP.Client(), http.MethodDelete, releaseURL, nil); status != http.StatusOK || released["released"] != true {
		t.Fatalf("release witnessed lease: status=%d body=%#v", status, released)
	}
	witnessQuery.Set("resume_height", fmt.Sprint(height))
	witnessQuery.Set("resume_hash", fmt.Sprintf("%x", parent[:]))
	witnessQuery.Set("resume_event_index", "1")
	status, resumedWitness := snapshotRequest(
		t, witnessHTTP.Client(), http.MethodGet,
		witnessHTTP.URL+"/api/v1/wallet/changes?"+witnessQuery.Encode(), nil,
	)
	if status != http.StatusOK || resumedWitness["complete"] != true ||
		len(resumedWitness["changes"].([]interface{})) != 0 {
		t.Fatalf("new witnessed lease did not resume the persisted prefix: status=%d body=%#v", status, resumedWitness)
	}
	resumedRelease := witnessHTTP.URL + "/api/v1/wallet/snapshot?address=" + url.QueryEscape(string(address)) +
		"&snapshot_id=" + url.QueryEscape(resumedWitness["snapshot_id"].(string))
	if status, released := snapshotRequest(t, witnessHTTP.Client(), http.MethodDelete, resumedRelease, nil); status != http.StatusOK || released["released"] != true {
		t.Fatalf("release resumed lease: status=%d body=%#v", status, released)
	}
	witnessQuery.Set("resume_height", fmt.Sprint(height+1))
	witnessQuery.Set("resume_hash", fmt.Sprintf("%x", parent[:]))
	witnessQuery.Set("resume_event_index", "0")
	status, nextHeightResume := snapshotRequest(
		t, witnessHTTP.Client(), http.MethodGet,
		witnessHTTP.URL+"/api/v1/wallet/changes?"+witnessQuery.Encode(), nil,
	)
	if status != http.StatusOK || nextHeightResume["complete"] != true {
		t.Fatalf("resume at witness+1 did not use the prior canonical hash: status=%d body=%#v", status, nextHeightResume)
	}
	nextHeightRelease := witnessHTTP.URL + "/api/v1/wallet/snapshot?address=" + url.QueryEscape(string(address)) +
		"&snapshot_id=" + url.QueryEscape(nextHeightResume["snapshot_id"].(string))
	if status, released := snapshotRequest(t, witnessHTTP.Client(), http.MethodDelete, nextHeightRelease, nil); status != http.StatusOK || released["released"] != true {
		t.Fatalf("release witness+1 lease: status=%d body=%#v", status, released)
	}
	witnessQuery.Set("resume_height", fmt.Sprint(height))
	witnessQuery.Set("resume_hash", fmt.Sprintf("%x", parent[:]))
	witnessQuery.Set("resume_event_index", "2")
	status, badResumeOffset := snapshotRequest(
		t, witnessHTTP.Client(), http.MethodGet,
		witnessHTTP.URL+"/api/v1/wallet/changes?"+witnessQuery.Encode(), nil,
	)
	if status != http.StatusConflict || badResumeOffset["code"] != "SNAPSHOT_CURSOR_MISMATCH" {
		t.Fatalf("resume offset beyond stable event list accepted: status=%d body=%#v", status, badResumeOffset)
	}
	witnessQuery.Set("resume_event_index", "1")
	witnessQuery.Set("resume_height", fmt.Sprint(height-1))
	status, badResumeHeight := snapshotRequest(
		t, witnessHTTP.Client(), http.MethodGet,
		witnessHTTP.URL+"/api/v1/wallet/changes?"+witnessQuery.Encode(), nil,
	)
	if status != http.StatusConflict || badResumeHeight["code"] != "SNAPSHOT_CURSOR_MISMATCH" {
		t.Fatalf("resume height below the original base accepted: status=%d body=%#v", status, badResumeHeight)
	}
	witnessQuery.Set("resume_height", fmt.Sprint(height))
	witnessQuery.Set("resume_hash", hex.EncodeToString(make([]byte, 32)))
	status, badResumeHash := snapshotRequest(
		t, witnessHTTP.Client(), http.MethodGet,
		witnessHTTP.URL+"/api/v1/wallet/changes?"+witnessQuery.Encode(), nil,
	)
	if status != http.StatusConflict || badResumeHash["code"] != "REORG" {
		t.Fatalf("resume hash mismatch accepted: status=%d body=%#v", status, badResumeHash)
	}
	witnessQuery.Set("resume_hash", fmt.Sprintf("%x", parent[:]))
	witnessQuery.Set("witness_hash", hex.EncodeToString(make([]byte, 32)))
	status, reorgedWitness := snapshotRequest(
		t, witnessHTTP.Client(), http.MethodGet,
		witnessHTTP.URL+"/api/v1/wallet/changes?"+witnessQuery.Encode(), nil,
	)
	if status != http.StatusConflict || reorgedWitness["code"] != "REORG" {
		t.Fatalf("noncanonical partial-target witness did not return REORG: status=%d body=%#v", status, reorgedWitness)
	}

	source := crypto.Hash32{8, 9, 10}
	stakeExtra, err := core.EncodeStakeExtraV3(
		core.StakeDeposit, crypto.ValidatorPubKey(make([]byte, 32)), 23,
		make([]byte, 64), source, 6, crypto.BlindFactor{}, make([]byte, 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	mintTx, err := core.BuildMintTx(address, 23, height+1)
	if err != nil {
		t.Fatal(err)
	}
	block := &core.Block{
		Header: core.BlockHeader{Height: height + 1, PrevHash: parent, Timestamp: int64(height + 2)},
		Txs: []core.Transaction{
			*mintTx,
			{Version: core.TxVersionStake, Extra: stakeExtra},
		},
	}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rawDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	hash := block.Hash()
	var heightBytes [8]byte
	binary.LittleEndian.PutUint64(heightBytes[:], block.Header.Height)
	var heightKeyBytes [8]byte
	binary.BigEndian.PutUint64(heightKeyBytes[:], block.Header.Height)
	for key, value := range map[string][]byte{
		"b/" + string(hash[:]):           raw,
		"h/" + string(heightKeyBytes[:]): hash[:],
		"m/tip/hash":                     hash[:],
		"m/tip/height":                   heightBytes[:],
	} {
		if err := rawDB.Put([]byte(key), value, nil); err != nil {
			t.Fatal(err)
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
	if err := db.PutUTXO(mintTx.Hash(), 0, &store.StoredUTXO{
		TxHash: mintTx.Hash(), OutputIndex: 0, OneTimePub: mintTx.Outputs[0].OneTimePub,
		TxPubKey: mintTx.Outputs[0].TxPubKey, AmountCommit: mintTx.Outputs[0].AmountCommit,
		BlockHeight: height + 1, AmountNAPRO: 23,
	}); err != nil {
		t.Fatal(err)
	}
	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	id := "public-session-id"
	entry := &walletSnapshotEntry{
		id: id, address: address, read: read, hash: block.Hash(), height: block.Header.Height,
		cursorKey: crypto.HashBytes([]byte("private cursor key")), expiresAt: time.Now().Add(time.Hour),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &Server{
		mux: http.NewServeMux(), log: logger, walletSnapshots: newWalletSnapshotManager(),
		mempool:     core.NewMempool(core.DefaultMempoolConfig()),
		hub:         &Hub{clients: make(map[*wsClient]struct{}), connPerIP: make(map[string]int), log: logger},
		rateLimiter: NewRateLimiter(),
	}
	server.walletSnapshots.entries[id] = entry
	server.walletSnapshots.active = 1
	server.walletSnapshots.activeByAddress[address] = 1
	server.registerRoutes()
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		_ = server.walletSnapshots.close(id, address)
		httpServer.Close()
	})
	query := url.Values{
		"address":     {string(address)},
		"snapshot_id": {id},
		"from_height": {fmt.Sprint(height)},
		"from_hash":   {fmt.Sprintf("%x", parent[:])},
		"limit":       {"1"},
	}
	firstURL := httpServer.URL + "/api/v1/wallet/changes?" + query.Encode()
	status, first := snapshotRequest(t, httpServer.Client(), http.MethodGet, firstURL, nil)
	if status != http.StatusOK || first["complete"] != false {
		t.Fatalf("first delta page status=%d body=%#v", status, first)
	}
	firstEvents, ok := first["changes"].([]interface{})
	if !ok || len(firstEvents) != 1 || firstEvents[0].(map[string]interface{})["kind"] != "output" {
		t.Fatalf("first delta page events=%#v", first["changes"])
	}
	if first["resume_height"] != float64(height+1) ||
		first["resume_hash"] != fmt.Sprintf("%x", hash[:]) ||
		first["resume_event_index"] != float64(1) {
		t.Fatalf("incomplete event page lacks an exact next-event position: %#v", first)
	}
	firstOutput := firstEvents[0].(map[string]interface{})["output"].(map[string]interface{})
	if firstOutput["amount_napr"] != "23" {
		t.Fatalf("snapshot-indexed transparent mint amount missing: %+v", firstOutput)
	}
	reorgQuery := url.Values{}
	for key, values := range query {
		reorgQuery[key] = append([]string(nil), values...)
	}
	reorgQuery.Set("from_hash", hex.EncodeToString(make([]byte, 32)))
	reorgURL := httpServer.URL + "/api/v1/wallet/changes?" + reorgQuery.Encode()
	status, reorg := snapshotRequest(t, httpServer.Client(), http.MethodGet, reorgURL, nil)
	if status != http.StatusConflict || reorg["code"] != "REORG" {
		t.Fatalf("noncanonical from_hash did not return REORG: status=%d body=%#v", status, reorg)
	}
	cursor := first["cursor"].(string)
	query.Set("cursor", cursor)
	lastURL := httpServer.URL + "/api/v1/wallet/changes?" + query.Encode()
	status, last := snapshotRequest(t, httpServer.Client(), http.MethodGet, lastURL, nil)
	if status != http.StatusOK || last["complete"] != true || last["pending_complete"] != true {
		t.Fatalf("final delta page status=%d body=%#v", status, last)
	}
	lastEvents, ok := last["changes"].([]interface{})
	if !ok || len(lastEvents) != 1 || lastEvents[0].(map[string]interface{})["kind"] != "spent_ref" {
		t.Fatalf("final delta page events=%#v", last["changes"])
	}
	status, repeated := snapshotRequest(t, httpServer.Client(), http.MethodGet, lastURL, nil)
	if status != http.StatusOK || repeated["complete"] != true ||
		repeated["changes"].([]interface{})[0].(map[string]interface{})["kind"] != "spent_ref" {
		t.Fatalf("repeated cursor page is not idempotent: status=%d body=%#v", status, repeated)
	}
	if len(last["pending_key_images"].([]interface{})) != 0 {
		t.Fatalf("empty mempool pending image set=%#v", last["pending_key_images"])
	}
}

func TestWalletChangesMissingWitnessHeightRequiresReconciliation(t *testing.T) {
	path := t.TempDir()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var parent crypto.Hash32
	hashes := make([]crypto.Hash32, 3)
	for height := uint64(0); height < 3; height++ {
		block := &core.Block{Header: core.BlockHeader{
			Height: height, PrevHash: parent, MerkleRoot: core.MerkleRoot(nil), Timestamp: int64(height + 1),
		}}
		hash := block.Hash()
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CommitRawBlockWithAVM(hash, height, raw, nil, crypto.Hash32{}); err != nil {
			t.Fatal(err)
		}
		hashes[height] = hash
		parent = hash
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rawDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var missingHeight [8]byte
	binary.BigEndian.PutUint64(missingHeight[:], 1)
	if err := rawDB.Delete(append([]byte("h/"), missingHeight[:]...), nil); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	addressKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, addressKeys)
	id := "missing-witness-session"
	entry := &walletSnapshotEntry{
		id: id, address: address, read: read, hash: hashes[2], height: 2,
		cursorKey: crypto.HashBytes([]byte("private witness cursor secret")), expiresAt: time.Now().Add(time.Hour),
	}
	server := snapshotTestServer(snapshotTestChain(t), core.NewMempool(core.DefaultMempoolConfig()))
	server.walletSnapshots.entries[id] = entry
	server.walletSnapshots.active = 1
	server.walletSnapshots.activeByAddress[address] = 1
	httpServer := httptest.NewServer(server)
	defer func() {
		_ = server.walletSnapshots.close(id, address)
		httpServer.Close()
	}()
	query := url.Values{
		"address":        {string(address)},
		"snapshot_id":    {id},
		"from_height":    {"0"},
		"from_hash":      {fmt.Sprintf("%x", hashes[0][:])},
		"witness_height": {"1"},
		"witness_hash":   {fmt.Sprintf("%x", hashes[1][:])},
	}
	status, response := snapshotRequest(
		t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/v1/wallet/changes?"+query.Encode(), nil,
	)
	if status != http.StatusConflict || response["code"] != "RECONCILIATION_REQUIRED" {
		t.Fatalf("missing witness canonical index did not stop replay: status=%d body=%#v", status, response)
	}
}

func TestWalletChangesEmptyBlocksReturnBoundedResumeProgress(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var parent crypto.Hash32
	hashes := make([]crypto.Hash32, 130)
	for height := uint64(0); height <= 130; height++ {
		block := &core.Block{Header: core.BlockHeader{
			Height: height, PrevHash: parent, MerkleRoot: core.MerkleRoot(nil), Timestamp: int64(height + 1),
		}}
		hash := block.Hash()
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CommitRawBlockWithAVM(hash, height, raw, nil, crypto.Hash32{}); err != nil {
			t.Fatal(err)
		}
		if height < uint64(len(hashes)) {
			hashes[height] = hash
		}
		parent = hash
	}
	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	addressKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, addressKeys)
	id := "empty-block-progress-session"
	entry := &walletSnapshotEntry{
		id: id, address: address, read: read, hash: parent, height: 130,
		cursorKey: crypto.HashBytes([]byte("private empty-block cursor secret")), expiresAt: time.Now().Add(time.Hour),
	}
	server := snapshotTestServer(snapshotTestChain(t), core.NewMempool(core.DefaultMempoolConfig()))
	server.walletSnapshots.entries[id] = entry
	server.walletSnapshots.active = 1
	server.walletSnapshots.activeByAddress[address] = 1
	httpServer := httptest.NewServer(server)
	defer func() {
		_ = server.walletSnapshots.close(id, address)
		httpServer.Close()
	}()
	query := url.Values{
		"address":     {string(address)},
		"snapshot_id": {id},
		"from_height": {"0"},
		"from_hash":   {fmt.Sprintf("%x", hashes[0][:])},
		"limit":       {"1"},
	}
	firstURL := httpServer.URL + "/api/v1/wallet/changes?" + query.Encode()
	status, first := snapshotRequest(t, httpServer.Client(), http.MethodGet, firstURL, nil)
	if status != http.StatusOK || first["complete"] != false ||
		first["resume_height"] != float64(walletChangesMaxHeights+1) ||
		first["resume_event_index"] != float64(0) ||
		first["resume_hash"] != fmt.Sprintf("%x", hashes[walletChangesMaxHeights+1][:]) {
		t.Fatalf("empty-block page did not return bounded forward progress: status=%d body=%#v", status, first)
	}
	query.Set("cursor", first["cursor"].(string))
	lastURL := httpServer.URL + "/api/v1/wallet/changes?" + query.Encode()
	status, last := snapshotRequest(t, httpServer.Client(), http.MethodGet, lastURL, nil)
	if status != http.StatusOK || last["complete"] != true || len(last["changes"].([]interface{})) != 0 {
		t.Fatalf("empty-block cursor page did not finish cleanly: status=%d body=%#v", status, last)
	}
}
