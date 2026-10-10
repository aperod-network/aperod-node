// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
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

func syntheticLPoDV3Funding(t *testing.T) (*store.DB, *store.LPoDMigration, crypto.Address) {
	t.Helper()
	db, err := store.Open(t.TempDir())
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
	return db, migration, leader
}
