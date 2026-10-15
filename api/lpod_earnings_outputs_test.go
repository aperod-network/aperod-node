// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api_test

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// This uses a synthetic, signed v3 funding transition and its real earnings
// index. No public node, transfer, deposit, or wallet secret is involved.
func TestLPoDEarningsOutputsServesOnlyIndexedPureRewards(t *testing.T) {
	db, migration, leader := syntheticLPoDV3Funding(t)

	srv, _ := buildChainServer(t, 0)
	srv.SetStore(db)
	tip, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	finalized := false
	srv.SetLPoDConfig(migration, func(h uint64, hash crypto.Hash32) bool {
		return finalized && h == height && hash == tip
	})

	unfinalizedCode, unfinalized := restGet(t, srv, "/api/v1/lpod/earnings-outputs?address="+string(leader))
	if unfinalizedCode != http.StatusServiceUnavailable || unfinalized["error"] == nil {
		t.Fatalf("earnings endpoint exposed an unfinalized synthetic payout: %#v", unfinalized)
	}
	finalized = true
	code, body := restGet(t, srv, "/api/v1/lpod/earnings-outputs?address="+string(leader))
	if code != http.StatusOK || body["state"] != "active" || body["checkpoint_hash"] != fmt.Sprintf("%x", tip[:]) {
		t.Fatalf("finalized earnings response: status=%d body=%#v", code, body)
	}
	if _, ok := body["snapshot_id"]; ok {
		t.Fatalf("legacy unpinned response unexpectedly contains snapshot_id: %#v", body)
	}
	if _, ok := body["checkpoint_height"]; ok {
		t.Fatalf("legacy unpinned response unexpectedly contains checkpoint_height: %#v", body)
	}
	outputs, ok := body["outputs"].([]interface{})
	if !ok || len(outputs) == 0 {
		t.Fatalf("synthetic canonical leader reward missing from API: %#v", body["outputs"])
	}
	checkpoint, err := db.LoadLPoDCheckpointAt(tip)
	if err != nil || checkpoint == nil || len(checkpoint.Positions) != 0 {
		t.Fatalf("synthetic no-position checkpoint unavailable: checkpoint=%+v err=%v", checkpoint, err)
	}
	block, err := db.GetRawBlock(tip)
	if err != nil || block == nil {
		t.Fatalf("synthetic funding block unavailable: %v", err)
	}
	var funding core.Block
	if err := json.Unmarshal(block, &funding); err != nil {
		t.Fatal(err)
	}
	payoutHash := funding.Txs[0].Hash()
	for _, value := range outputs {
		row, ok := value.(map[string]interface{})
		amount, amountOK := row["amount_napro"].(string)
		if !ok || row["tx_hash"] != fmt.Sprintf("%x", payoutHash[:]) ||
			!amountOK || amount == "" || amount == "0" ||
			row["out_idx"] == nil || row["one_time_pub"] == nil {
			t.Fatalf("API returned a malformed/non-payout earnings ref: %#v", value)
		}
	}
}

func TestLPoDEarningsOutputsPaginatesLegacyCursorAt128Rows(t *testing.T) {
	dbPath := t.TempDir()
	db, migration, address, _ := syntheticLPoDV3FundingAt(t, dbPath)
	_, fixtureHeight, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	db = seedSyntheticEarningsRows(t, dbPath, db, address, fixtureHeight)

	srv, _ := buildChainServer(t, 0)
	srv.SetStore(db)
	tip, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	srv.SetLPoDConfig(migration, func(h uint64, hash crypto.Hash32) bool {
		return h == height && hash == tip
	})

	basePath := "/api/v1/lpod/earnings-outputs?address=" + string(address)
	firstCode, first := restGet(t, srv, basePath)
	firstRows, ok := first["outputs"].([]interface{})
	firstCursor, cursorOK := first["next_cursor"].(string)
	wantCheckpoint := fmt.Sprintf("%x", tip[:])
	if firstCode != http.StatusOK || !ok || len(firstRows) != 128 ||
		first["state"] != "active" || first["checkpoint_hash"] != wantCheckpoint ||
		!cursorOK || firstCursor == "" {
		t.Fatalf("first legacy earnings page: status=%d body=%#v", firstCode, first)
	}
	if _, ok := first["snapshot_id"]; ok {
		t.Fatalf("legacy response unexpectedly contains snapshot_id: %#v", first)
	}
	if _, ok := first["checkpoint_height"]; ok {
		t.Fatalf("legacy response unexpectedly contains checkpoint_height: %#v", first)
	}
	for index, value := range firstRows {
		row, ok := value.(map[string]interface{})
		if !ok || row["block_height"] != float64(fixtureHeight) ||
			row["out_idx"] != float64(index+1) {
			t.Fatalf("first page is not in index order at position %d: %#v", index, value)
		}
	}

	decodedCursor, err := base64.RawURLEncoding.DecodeString(firstCursor)
	if err != nil {
		t.Fatalf("decode legacy cursor: %v", err)
	}
	cursorParts := strings.SplitN(string(decodedCursor), ":", 2)
	if len(cursorParts) != 2 || cursorParts[0] != wantCheckpoint {
		t.Fatalf("legacy cursor is not bound to the returned checkpoint: %q", decodedCursor)
	}

	secondCode, second := restGet(t, srv, basePath+"&cursor="+firstCursor)
	secondRows, ok := second["outputs"].([]interface{})
	if secondCode != http.StatusOK || !ok || len(secondRows) != 1 ||
		second["state"] != "active" || second["checkpoint_hash"] != wantCheckpoint ||
		second["next_cursor"] != "" {
		t.Fatalf("second legacy earnings page: status=%d body=%#v", secondCode, second)
	}
	lastRow, ok := secondRows[0].(map[string]interface{})
	if !ok || lastRow["block_height"] != float64(fixtureHeight) ||
		lastRow["out_idx"] != float64(129) {
		t.Fatalf("second page did not continue after row 128: %#v", secondRows)
	}
	if _, ok := second["snapshot_id"]; ok {
		t.Fatalf("legacy response unexpectedly contains snapshot_id: %#v", second)
	}
	if _, ok := second["checkpoint_height"]; ok {
		t.Fatalf("legacy response unexpectedly contains checkpoint_height: %#v", second)
	}

	if fixturePath := os.Getenv("APERO_NODE_LPOD_EARNINGS_PAGE_FIXTURE"); fixturePath != "" {
		fixture, err := json.Marshal(map[string]interface{}{
			"address":           string(address),
			"checkpoint_hash":   wantCheckpoint,
			"checkpoint_height": height,
			"first_page":        first,
			"second_page":       second,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath, fixture, 0o600); err != nil {
			t.Fatalf("write sanitized earnings fixture: %v", err)
		}
	}
}

func TestLPoDEarningsOutputsRejectsCursorFromDifferentAddress(t *testing.T) {
	dbPath := t.TempDir()
	db, migration, addressA, _ := syntheticLPoDV3FundingAt(t, dbPath)
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	addressB := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	if addressA == addressB {
		t.Fatal("synthetic wallets unexpectedly share an address")
	}
	_, fixtureHeight, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	db = seedSyntheticEarningsRows(t, dbPath, db, addressA, fixtureHeight, addressB)

	srv, _ := buildChainServer(t, 0)
	srv.SetStore(db)
	tip, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	srv.SetLPoDConfig(migration, func(h uint64, hash crypto.Hash32) bool {
		return h == height && hash == tip
	})

	firstCode, first := restGet(
		t,
		srv,
		"/api/v1/lpod/earnings-outputs?address="+string(addressA),
	)
	firstRows, ok := first["outputs"].([]interface{})
	cursor, cursorOK := first["next_cursor"].(string)
	if firstCode != http.StatusOK || !ok || len(firstRows) != 128 || !cursorOK || cursor == "" {
		t.Fatalf("first wallet page did not return a continuation cursor: status=%d body=%#v", firstCode, first)
	}
	decodedCursor, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatalf("decode wallet A cursor: %v", err)
	}
	cursorParts := strings.SplitN(string(decodedCursor), ":", 2)
	if len(cursorParts) != 2 {
		t.Fatalf("wallet A cursor has an invalid envelope: %q", decodedCursor)
	}
	if _, _, err := db.LPoDEarningsOutputs(addressB, cursorParts[1], 128); err == nil {
		t.Fatal("store accepted wallet A's LevelDB cursor for wallet B")
	}

	walletBCode, walletB := restGet(
		t,
		srv,
		"/api/v1/lpod/earnings-outputs?address="+string(addressB),
	)
	walletBRows, ok := walletB["outputs"].([]interface{})
	if walletBCode != http.StatusOK || !ok || len(walletBRows) != 128 {
		t.Fatalf("wallet B's own first page was not available: status=%d body=%#v", walletBCode, walletB)
	}
	firstBRow, ok := walletBRows[0].(map[string]interface{})
	if !ok || firstBRow["tx_hash"] == nil {
		t.Fatalf("wallet B's first indexed output is malformed: %#v", walletBRows[0])
	}
	firstARow, ok := firstRows[0].(map[string]interface{})
	if !ok || firstARow["tx_hash"] == firstBRow["tx_hash"] {
		t.Fatalf("wallet B page exposed wallet A's indexed output: A=%#v B=%#v", firstARow, firstBRow)
	}

	reusedCode, reused := restGet(
		t,
		srv,
		"/api/v1/lpod/earnings-outputs?"+url.Values{
			"address": {string(addressB)},
			"cursor":  {cursor},
		}.Encode(),
	)
	if reusedCode != http.StatusBadRequest || reused["error"] != "invalid cursor" ||
		reused["outputs"] != nil {
		t.Fatalf("cursor from wallet A was not rejected for wallet B: status=%d body=%#v", reusedCode, reused)
	}
}

func seedSyntheticEarningsRows(
	t *testing.T,
	path string,
	db *store.DB,
	address crypto.Address,
	height uint64,
	additionalAddresses ...crypto.Address,
) *store.DB {
	t.Helper()
	existing, _, err := db.LPoDEarningsOutputs(address, "", 128)
	if err != nil || len(existing) == 0 {
		t.Fatalf("synthetic funding did not create an earnings row: rows=%d err=%v", len(existing), err)
	}
	template := existing[0]
	if err := db.Close(); err != nil {
		t.Fatalf("close synthetic store before LevelDB fixture seeding: %v", err)
	}

	raw, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatalf("open temporary LevelDB for synthetic earnings rows: %v", err)
	}
	batch := new(leveldb.Batch)
	addresses := append([]crypto.Address{address}, additionalAddresses...)
	for _, owner := range addresses {
		addressHash := crypto.HashBytes([]byte(owner))
		prefix := append([]byte("lpod/earnings-output/v2/"), addressHash[:]...)
		iter := raw.NewIterator(util.BytesPrefix(prefix), nil)
		for iter.Next() {
			batch.Delete(append([]byte(nil), iter.Key()...))
		}
		if err := iter.Error(); err != nil {
			iter.Release()
			raw.Close()
			t.Fatalf("scan synthetic earnings index: %v", err)
		}
		iter.Release()

		for i := uint64(1); i <= 129; i++ {
			row := template
			row.BlockHeight = height
			row.OutputIndex = uint32(i)
			row.TxHash = crypto.HashBytes([]byte(fmt.Sprintf("synthetic-earnings-tx-%s-%d", owner, i)))
			row.AmountNAPRO = 1_000_000 + i

			var suffix [12]byte
			binary.BigEndian.PutUint64(suffix[:8], row.BlockHeight)
			binary.BigEndian.PutUint32(suffix[8:], row.OutputIndex)
			blockHash := crypto.HashBytes([]byte(fmt.Sprintf("synthetic-earnings-block-%s-%d", owner, i)))
			key := append(append(append([]byte(nil), prefix...), suffix[:]...), blockHash[:]...)
			key = append(key, row.TxHash[:]...)
			value, err := json.Marshal(row)
			if err != nil {
				raw.Close()
				t.Fatalf("marshal synthetic earnings row %d for %s: %v", i, owner, err)
			}
			batch.Put(key, value)
		}
	}
	if err := raw.Write(batch, nil); err != nil {
		raw.Close()
		t.Fatalf("write synthetic earnings rows: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close temporary LevelDB after seeding: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen synthetic store after seeding: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	return reopened
}

func syntheticLPoDV3Funding(t *testing.T) (*store.DB, *store.LPoDMigration, crypto.Address) {
db, m, address, _ := syntheticLPoDV3FundingWithSigner(t)
return db, m, address
}

func syntheticLPoDV3FundingWithSigner(t *testing.T) (*store.DB, *store.LPoDMigration, crypto.Address, crypto.ValidatorPrivKey) {
return syntheticLPoDV3FundingAt(t, t.TempDir())
}

func syntheticLPoDV3FundingAt(t *testing.T, path string, withLiquidity ...bool) (*store.DB, *store.LPoDMigration, crypto.Address, crypto.ValidatorPrivKey) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
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
if len(withLiquidity) > 0 && withLiquidity[0] {
snapshot.NominalCirculatingBefore += store.GenesisLiquidityAmount
}
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
return db, migration, leader, validator
}
