// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
	"github.com/syndtr/goleveldb/leveldb"
)

func TestLPoDPoolFinalizedFullExitExcludesRetainedPositions(t *testing.T) {
	// Persist a valid depleted-reserve projection snapshot. This tests API
	// finality gating and conservation, not migration authorization or BFT;
	// authenticated exits and arrears creation are covered by consensus tests.
	srv, chain := buildChainServer(t, 0)
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	genesis := chain.Genesis().Hash()
	root := crypto.HashBytes([]byte("api-finalized-exit-projection"))
	const principal = 150 * lpod.Unit
	const due = 7 * lpod.Unit
	const reward = 3 * lpod.Unit
	c := &store.LPoDCheckpoint{
		State: lpod.State{FundingDebit: lpod.InitialNAPRO, RewardInflow: reward, LeaderPaid: 24_000_000,
			AngelPaid: lpod.InitialNAPRO + reward - 24_000_000, DeficitOutflow: lpod.InitialNAPRO,
			AccruedLiability: lpod.InitialNAPRO + reward - 24_000_000 + due, UnfundedLiability: due, LastHeight: 2},
		Allocation: &store.LPoDAllocation{Version: 1, PositionLifecycleVersion: 1, Genesis: genesis, FundingHeight: 2, ReconciliationRoot: root,
			HistoricalIssued: principal, InitialValidatorRemaining: 2_000_000_000 * lpod.Unit,
			ValidatorRemaining: 2_000_000_000*lpod.Unit - reward,
			Remaining:          6_000_000_000*lpod.Unit - principal},
		PrincipalDeposited: principal, PrincipalReturned: principal,
		Positions: map[string]store.LPoDPosition{},
	}
	for i, amount := range []uint64{100 * lpod.Unit, 50 * lpod.Unit} {
		source := crypto.HashBytes([]byte{byte(i)})
		id := core.LPoDPositionID(genesis, source, 0)
		p := store.LPoDPosition{Deposit: core.LPoDPositionAction{Action: core.LPoDDeposit, Genesis: genesis,
			PositionID: id, SourceTx: source, Vault: crypto.Point32(pub), Owner: owner.Spend.Public,
			Beneficiary: crypto.AddressFromKeys(crypto.MainnetByte, owner), Amount: amount},
			Nonce: 1, Withdrawn: amount, Returned: true, UnlockHeight: 2}
		if i == 0 {
			p.Due = due
		}
		c.Positions[fmt.Sprintf("%x", id[:])] = p
	}
	b := &core.Block{Header: core.BlockHeader{Height: 2, ValidatorPub: pub},
		Txs: []core.Transaction{{Version: core.TxVersionLPoDPayout}, core.LPoDCheckpointTx(c.Digest())}}
	b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
	b.Header.Sign(priv)
	path := t.TempDir()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(b.Hash(), 2, raw); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(b.Hash(), 2); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Write only this test fixture's checkpoint while the store is closed.
	fixtureDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	hash := b.Hash()
	if err := fixtureDB.Put(append([]byte("lpod/checkpoint/v1/"), hash[:]...), cp, nil); err != nil {
		t.Fatal(err)
	}
	if err := fixtureDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv.SetStore(db)
	finalized := false
	srv.SetLPoDConfig(&store.LPoDMigration{Height: 2, Genesis: genesis, ReconciliationRoot: root},
		func(h uint64, got crypto.Hash32) bool { return finalized && h == 2 && got == hash })
	_, body := restGet(t, srv, "/api/v1/lpod-pool")
	if body["state"] != "pending" || body["position_count"] != nil {
		t.Fatalf("unfinalized projection exposed: %#v", body)
	}
	finalized = true
	code, body := restGet(t, srv, "/api/v1/lpod-pool")
	if code != http.StatusOK || body["state"] != "active" || body["position_count"] != float64(0) ||
		body["principal_locked_napro"] != "0" || body["total_guardian_stake_napro"] != "0" ||
		body["principal_deposited_napro"] != fmt.Sprint(principal) || body["principal_returned_napro"] != fmt.Sprint(principal) ||
		body["unfunded_liability_napro"] != fmt.Sprint(due) {
		t.Fatalf("finalized exit projection: %#v", body)
	}
	persisted, err := db.LoadLPoDCheckpoint()
	if err != nil || len(persisted.Positions) != 2 {
		t.Fatal("API must not delete closed records to obtain zero open count")
	}
}

func TestLPoDPoolUnfundedContract(t *testing.T) {
	srv, chain := buildChainServer(t, 0)
	// Legacy Guardian configuration is never funding proof for LPoD.
	srv.SetGuardianFundConfig(1, chain.Tip().Hash())
	code, body := restGet(t, srv, "/api/v1/lpod-pool")
	if code != http.StatusOK || body["version"] != float64(1) || body["state"] != "disabled" ||
		body["accounting_basis"] != "canonical_protocol_ledger" || body["initial_napro"] != "100000000000000000" {
		t.Fatalf("contract: %d %#v", code, body)
	}
	if body["immediate_exit_supported"] != true || body["partial_exit_supported"] != true ||
		body["additional_deposits_supported"] != true {
		t.Fatal("native immediate-exit capability must not imply an activated fund")
	}
	for _, field := range []string{"balance_napro", "reward_inflow_napro", "leader_paid_napro", "angel_paid_napro",
		"surplus_inflow_napro", "deficit_outflow_napro", "unfunded_liability_napro",
		"funding_debit_napro", "total_guardian_stake_napro", "last_settled_height"} {
		if value, present := body[field]; !present || value != nil {
			t.Fatalf("%s must be explicitly null", field)
		}
	}
}

func TestLPoDPoolRegisteredReadOnly(t *testing.T) {
	srv, _ := buildChainServer(t, 0)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, httptest.NewRequest(method, "/api/v1/lpod-pool", nil))
			if rr.Code != http.StatusMethodNotAllowed {
				t.Fatalf("registered read-only route returned %d, want 405", rr.Code)
			}
		})
	}
}
