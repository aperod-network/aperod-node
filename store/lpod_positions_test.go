// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
)

// An explicitly isolated depleted-reserve arithmetic snapshot exercises arrears.
// Real deposits, same-block refunds and ordinary spending are tested in consensus.
func TestLPoDImmediatePrincipalReturnRetainsAndRepaysArrears(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	leader, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, owner)
	leaderAddress := crypto.AddressFromKeys(crypto.MainnetByte, leader)
	const deadline = 3
	const principal = 100 * lpod.Unit
	const debt = 50 * lpod.Unit
	genesis := crypto.HashBytes([]byte("depleted-reserve-arithmetic-fixture-genesis"))
	source := crypto.HashBytes([]byte("already-consumed-authenticated-source"))
	action := core.LPoDPositionAction{Action: core.LPoDDeposit, Genesis: genesis,
		PositionID: core.LPoDPositionID(genesis, source, 0), SourceTx: source, Vault: crypto.Point32(pub),
		Amount: principal, Owner: owner.Spend.Public, Beneficiary: address}
	checkpoint := &LPoDCheckpoint{
		State: lpod.State{FundingDebit: lpod.InitialNAPRO, Balance: 0,
			RewardInflow: 300_000_000, LeaderPaid: 24_000_000, AngelPaid: lpod.InitialNAPRO + 276_000_000,
			DeficitOutflow: lpod.InitialNAPRO, UnfundedLiability: debt,
			AccruedLiability: lpod.InitialNAPRO + 276_000_000 + debt, LastHeight: deadline - 1},
		Carries: map[string]lpod.Carry{pub.Hex(): {}},
		Allocation: &LPoDAllocation{Version: 1, PositionLifecycleVersion: 1, Genesis: genesis, FundingHeight: 2, HistoricalIssued: principal,
			InitialValidatorRemaining: 2_000_000_000 * lpod.Unit, ValidatorRemaining: 2_000_000_000*lpod.Unit - 300_000_000,
			Remaining: LPoDPublicAllocationNAPRO - principal - lpod.InitialNAPRO},
		PrincipalDeposited: principal, PrincipalLocked: principal,
		Positions: map[string]LPoDPosition{lpodPositionID(action): {Deposit: action, Due: debt}},
	}
	parent := &core.Block{Header: core.BlockHeader{Height: deadline - 1, Timestamp: 1, ValidatorPub: pub},
		Txs: []core.Transaction{{Version: core.TxVersionLPoDPayout}, core.LPoDCheckpointTx(checkpoint.Digest())}}
	parent.Header.MerkleRoot = core.MerkleRoot(parent.Txs)
	parent.Header.Sign(priv)
	raw, _ := json.Marshal(parent)
	if err := db.PutRawBlock(parent.Hash(), parent.Header.Height, raw); err != nil {
		t.Fatal(err)
	}
	cpraw, _ := json.Marshal(checkpoint)
	if err := db.db.Put(lpodKey(parent.Hash()), cpraw, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(parent.Hash(), parent.Header.Height); err != nil {
		t.Fatal(err)
	}
	withdrawAction := action
	withdrawAction.Action = core.LPoDWithdraw
	withdrawAction.Nonce = 1
	withdraw, err := core.BuildLPoDPositionTx(withdrawAction, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	request := &LPoDSettlement{Parent: parent.Hash(), PositionProtocol: true, Timestamp: 3_000_000_001, Transactions: []core.Transaction{*withdraw},
		Proposer: pub.Hex(), Leader: leaderAddress, Stake: map[string]LPoDValidatorStake{pub.Hex(): {Amount: 100_000 * lpod.Unit, Active: true}}}
	c, payments, err := db.PreviewLPoD(deadline, request)
	if err != nil {
		t.Fatal(err)
	}
	if c.PrincipalLocked != 0 || c.PrincipalReturned != principal || c.State.UnfundedLiability == 0 ||
		c.Positions[lpodPositionID(action)].Due != c.State.UnfundedLiability {
		t.Fatal("principal mixed into rewards or lost")
	}
	payout, err := db.PayoutLPoD(deadline, request, c, payments)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := core.BuildLPoDPayoutOutput(address, principal+c.State.AngelPaid-checkpoint.State.AngelPaid, deadline, request.Parent, c.Digest())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, out := range payout.Outputs {
		if out == expected {
			found = true
		}
	}
	if !found {
		t.Fatal("principal was not immediately returned alongside funded arrears")
	}
	block := &core.Block{Header: core.BlockHeader{Height: deadline, PrevHash: parent.Hash(), Timestamp: request.Timestamp, ValidatorPub: pub},
		Txs: []core.Transaction{payout, core.LPoDCheckpointTx(c.Digest()), *withdraw}}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	block.Header.Sign(priv)
	raw, _ = json.Marshal(block)
	if err := db.CommitRawBlockWithAVM(block.Hash(), deadline, raw, nil, crypto.Hash32{}, request); err != nil {
		t.Fatal(err)
	}
	request.Parent = block.Hash()
	request.Timestamp += 3_000_000_000
	if _, _, err := db.PreviewLPoD(deadline+1, request); err == nil {
		t.Fatal("withdrawal replay refunded principal twice")
	}
	request.Transactions = nil
	next, payments, err := db.PreviewLPoD(deadline+1, request)
	if err != nil {
		t.Fatal(err)
	}
	payout, err = db.PayoutLPoD(deadline+1, request, next, payments)
	if err != nil {
		t.Fatal(err)
	}
	if next.PrincipalReturned != principal || next.PrincipalLocked != 0 || len(next.Positions) != 1 ||
		next.State.AccruedLiability != c.State.AccruedLiability || next.State.UnfundedLiability >= c.State.UnfundedLiability {
		t.Fatal("principal paid twice, APR continued, or outstanding arrears were lost")
	}
	expected, err = core.BuildLPoDPayoutOutput(address, next.State.AngelPaid-c.State.AngelPaid, deadline+1, request.Parent, next.Digest())
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, out := range payout.Outputs {
		if out == expected {
			found = true
		}
	}
	if !found {
		t.Fatal("post-exit arrears were not paid separately from principal")
	}
}
