// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
)

func TestLPoDDurableCanonicalCheckpoint(t *testing.T) {
	path := t.TempDir()
	db, err := Open(path)
	if err != nil { t.Fatal(err) }
	defer func() { db.Close() }()
	parent := crypto.HashBytes([]byte("accounting-test-only-parent"))
	block := crypto.HashBytes([]byte("accounting-test-only-block"))
	if err := db.PutTip(parent, 0); err != nil { t.Fatal(err) }
	if c, err := db.LoadLPoDCheckpoint(); c != nil || err != nil { t.Fatalf("fresh ledger is funded: %+v %v", c, err) }
	request := &LPoDSettlement{Parent:parent, Vaults:[]lpod.Vault{
		{ID:"a", TotalStake:100_000*lpod.Unit, GuardianStake:100*lpod.Unit+1, ActualIncome:1000, ElapsedSeconds:1},
	}}
	if err := db.CommitRawBlockWithAVM(block, 1, []byte("test"), nil, crypto.Hash32{}, request); err == nil {
		t.Fatal("must not create initial funding")
	}
	// Test-only DB injection. No production initializer accepts this fixture.
	initial := LPoDCheckpoint{State:lpod.State{FundingDebit:lpod.InitialNAPRO, Balance:lpod.InitialNAPRO}}
	data, err := json.Marshal(initial)
	if err != nil { t.Fatal(err) }
	if err := db.db.Put(lpodKey(parent), data, nil); err != nil { t.Fatal(err) }
	if err := db.CommitRawBlockWithAVM(block, 1, []byte("test"), nil, crypto.Hash32{}); err == nil {
		t.Fatal("must not skip funded settlement")
	}
	if err := db.CommitRawBlockWithAVM(block, 1, []byte("test"), nil, crypto.Hash32{}, request); err != nil { t.Fatal(err) }
	settled, err := db.LoadLPoDCheckpoint()
	if err != nil || settled == nil { t.Fatalf("load: %+v %v", settled, err) }
	if settled.State.LastHeight != 1 || settled.Carries["a"].APR == 0 { t.Fatal("missing settlement/carry") }
	if err := db.Close(); err != nil { t.Fatal(err) }
	db, err = Open(path)
	if err != nil { t.Fatal(err) }
	restarted, err := db.LoadLPoDCheckpoint()
	if err != nil || !reflect.DeepEqual(settled, restarted) { t.Fatalf("restart: %+v %v", restarted, err) }
	if err := db.CommitRawBlockWithAVM(block, 1, []byte("test"), nil, crypto.Hash32{}, request); err == nil { t.Fatal("duplicate accepted") }
	// The canonical tip rollback selects the entire old state, not merely balance.
	if err := db.PutTip(parent, 0); err != nil { t.Fatal(err) }
	rolledBack, err := db.LoadLPoDCheckpoint()
	if err != nil || !reflect.DeepEqual(rolledBack, &initial) { t.Fatalf("rollback: %+v %v", rolledBack, err) }
	if err := db.CommitRawBlockWithAVM(block, 1, []byte("test"), nil, crypto.Hash32{}, request); err != nil { t.Fatal(err) }
	replayed, err := db.LoadLPoDCheckpoint()
	if err != nil || !reflect.DeepEqual(replayed, settled) { t.Fatalf("replay: %+v %v", replayed, err) }
	// Rejected transitions must leave block, AVM, checkpoint and tip unchanged.
	bad := crypto.HashBytes([]byte("rejected"))
	request.Parent = block
	request.Vaults = append(request.Vaults, request.Vaults[0])
	if err := db.CommitRawBlockWithAVM(bad, 2, []byte("bad"), []AVMWrite{{Key:[]byte("must-not-commit"), Value:[]byte("bad")}}, crypto.Hash32{}, request); err == nil {
		t.Fatal("invalid settlement committed")
	}
	if _, exists, err := db.GetAVMState([]byte("must-not-commit")); err != nil || exists { t.Fatalf("partial AVM commit: %v", err) }
	if c, err := db.lpodCheckpoint(bad); err != nil || c != nil { t.Fatalf("partial checkpoint: %+v %v", c, err) }
	if h, height, err := db.GetTip(); err != nil || h != block || height != 1 { t.Fatalf("partial tip: %v", err) }
}