// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
)

func TestLPoDDailyBoundaryWaitsForFullDayAndUsesMoscowTime(t *testing.T) {
	location := time.FixedZone("MSK", 3*60*60)
	cases := []struct {
		name  string
		start time.Time
		want  time.Time
	}{
		{"before-evening", time.Date(2026, 10, 1, 19, 0, 0, 0, location), time.Date(2026, 10, 2, 20, 0, 0, 0, location)},
		{"five-minutes-before", time.Date(2026, 10, 1, 19, 55, 0, 0, location), time.Date(2026, 10, 2, 20, 0, 0, 0, location)},
		{"exact-evening", time.Date(2026, 10, 1, 20, 0, 0, 0, location), time.Date(2026, 10, 2, 20, 0, 0, 0, location)},
		{"one-second-after-evening", time.Date(2026, 10, 1, 20, 0, 1, 0, location), time.Date(2026, 10, 3, 20, 0, 0, 0, location)},
		{"after-evening", time.Date(2026, 10, 1, 21, 30, 0, 0, location), time.Date(2026, 10, 3, 20, 0, 0, 0, location)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := LPoDNextDailyBoundaryAfter24h(test.start.UnixNano())
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want.UnixNano() || got < test.start.Add(24*time.Hour).UnixNano() {
				t.Fatalf("next daily boundary = %s, want %s after 24 hours",
					time.Unix(0, got).In(location), test.want)
			}
		})
	}
	if _, err := LPoDNextDailyBoundaryAfter24h(0); err == nil {
		t.Fatal("accepted an empty payout anchor")
	}
	if _, err := LPoDNextDailyBoundaryAfter24h(int64(^uint64(0) >> 1)); err == nil {
		t.Fatal("accepted an overflowing payout anchor")
	}
}

func TestLPoDDailyAccrualWaitsForBoundaryAndTopUpResetsOnlyPendingAmount(t *testing.T) {
	path := t.TempDir()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
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
	const principal = 100 * lpod.Unit
	const debt = 50 * lpod.Unit
	const pendingDue = 7 * lpod.Unit
	location := time.FixedZone("MSK", 3*60*60)
	boundary := time.Date(2026, 10, 2, 20, 0, 0, 0, location)
	genesis := crypto.HashBytes([]byte("daily-position-fixture-genesis"))
	sourceHash := crypto.HashBytes([]byte("daily-position-fixture-source"))
	address := crypto.AddressFromKeys(crypto.MainnetByte, owner)
	deposit := core.LPoDPositionAction{
		Action: core.LPoDDeposit, Genesis: genesis, Vault: crypto.Point32(pub),
		Beneficiary: address, Owner: owner.Spend.Public, SourceTx: sourceHash,
		SourcePub: owner.Spend.Public, Amount: principal,
		PositionID: core.LPoDPositionID(genesis, sourceHash, 0),
		Blind:      crypto.BlindFactor(crypto.ScalarFromUint64(31)),
	}
	id := lpodPositionID(deposit)
	checkpoint := &LPoDCheckpoint{
		State: lpod.State{FundingDebit: lpod.InitialNAPRO, Balance: 0,
			RewardInflow: 300_000_000, LeaderPaid: 24_000_000, AngelPaid: lpod.InitialNAPRO + 276_000_000,
			DeficitOutflow: lpod.InitialNAPRO, UnfundedLiability: debt,
			AccruedLiability: lpod.InitialNAPRO + 276_000_000 + debt, LastHeight: 2},
		Carries: map[string]lpod.Carry{pub.Hex(): {}},
		Allocation: &LPoDAllocation{Version: 1, PositionLifecycleVersion: 1, Genesis: genesis, FundingHeight: 2,
			HistoricalIssued: principal, InitialValidatorRemaining: 2_000_000_000 * lpod.Unit,
			ValidatorRemaining: 2_000_000_000*lpod.Unit - 300_000_000,
			Remaining:          LPoDPublicAllocationNAPRO - principal - lpod.InitialNAPRO},
		DailyPayoutVersion: 1, PendingLiability: pendingDue,
		PrincipalDeposited: principal, PrincipalLocked: principal,
		Positions: map[string]LPoDPosition{id: {
			Deposit: deposit, Due: debt, PendingDue: pendingDue, NextPayoutNS: boundary.UnixNano(),
		}},
	}
	parent := &core.Block{Header: core.BlockHeader{Height: 2, Timestamp: boundary.Add(-3 * time.Second).UnixNano(), ValidatorPub: pub},
		Txs: []core.Transaction{{Version: core.TxVersionLPoDPayout}, core.LPoDCheckpointTx(checkpoint.Digest())}}
	parent.Header.MerkleRoot = core.MerkleRoot(parent.Txs)
	if err := parent.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(parent.Hash(), parent.Header.Height, raw); err != nil {
		t.Fatal(err)
	}
	cpraw, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Put(lpodKey(parent.Hash()), cpraw, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(parent.Hash(), parent.Header.Height); err != nil {
		t.Fatal(err)
	}
	persisted, err := db.LoadLPoDCheckpoint()
	if err != nil || persisted == nil || persisted.Digest() != checkpoint.Digest() ||
		persisted.DailyPayoutVersion != 1 || persisted.PendingLiability != pendingDue {
		t.Fatalf("daily checkpoint did not persist canonical accrued state: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err = db.LoadLPoDCheckpoint()
	if err != nil || persisted == nil || persisted.Digest() != checkpoint.Digest() ||
		persisted.Positions[id].PendingDue != pendingDue {
		t.Fatalf("daily checkpoint replay changed pending accrual or digest: %v", err)
	}
	request := &LPoDSettlement{
		Parent: parent.Hash(), PositionProtocol: true, DailyPayout: true,
		Timestamp: boundary.Add(-time.Second).UnixNano(), Proposer: pub.Hex(),
		Leader: crypto.AddressFromKeys(crypto.MainnetByte, leader),
		Stake:  map[string]LPoDValidatorStake{pub.Hex(): {Amount: 100_000 * lpod.Unit, Active: true}},
	}
	beforeBoundary, _, err := db.PreviewLPoD(3, request)
	if err != nil {
		t.Fatal(err)
	}
	if beforeBoundary.Positions[id].Due != debt || beforeBoundary.Positions[id].PendingDue < pendingDue {
		t.Fatal("unrecognized accrual was paid before the daily boundary")
	}
	request.Timestamp = boundary.Add(time.Second).UnixNano()
	atBoundary, boundaryPayments, err := db.PreviewLPoD(3, request)
	if err != nil {
		t.Fatal(err)
	}
	paidAtBoundary := uint64(0)
	for _, payment := range boundaryPayments {
		paidAtBoundary += payment.Angels
	}
	recognized := atBoundary.Positions[id].Due + paidAtBoundary
	if recognized < debt+pendingDue || atBoundary.Positions[id].PendingDue != 0 ||
		atBoundary.Positions[id].NextPayoutNS <= boundary.UnixNano() {
		t.Fatalf("daily boundary did not recognize pending accrual and schedule the next payout: due=%d paid=%d pending=%d next=%s payments=%+v",
			atBoundary.Positions[id].Due, paidAtBoundary, atBoundary.Positions[id].PendingDue,
			time.Unix(0, atBoundary.Positions[id].NextPayoutNS).In(location), boundaryPayments)
	}

	topUpHS := crypto.ScalarFromUint64(41)
	topUpPrivate, err := crypto.AddScalars(topUpHS, owner.Spend.Private)
	if err != nil {
		t.Fatal(err)
	}
	topUpPub, err := crypto.ScalarMulBase(topUpPrivate)
	if err != nil {
		t.Fatal(err)
	}
	topUp := deposit
	topUp.Action, topUp.Nonce = core.LPoDTopUp, 1
	topUp.SourceTx, topUp.SourceIndex, topUp.SourcePub = crypto.HashBytes([]byte("daily-top-up-source")), 3, topUpPub
	topUp.Amount, topUp.Blind = 5*lpod.Unit, crypto.BlindFactor(crypto.ScalarFromUint64(43))
	request.Timestamp = boundary.Add(-time.Second).UnixNano()
	request.Transactions = nil
	tooMuch := topUp
	tooMuch.Amount = 100_000_000*lpod.Unit - request.Stake[pub.Hex()].Amount - principal + 1
	tooMuch.Blind = crypto.BlindFactor(crypto.ScalarFromUint64(47))
	tooMuchTx, err := core.BuildLPoDPositionTx(tooMuch, owner.Spend.Private, topUpPrivate)
	if err != nil {
		t.Fatal(err)
	}
	request.Transactions = []core.Transaction{*tooMuchTx}
	if _, _, err := db.PreviewLPoD(3, request); err == nil {
		t.Fatal("top-up beyond the active vault capacity was accepted")
	}
	wrongNonce := topUp
	wrongNonce.Nonce = 2
	wrongNonceTx, err := core.BuildLPoDPositionTx(wrongNonce, owner.Spend.Private, topUpPrivate)
	if err != nil {
		t.Fatal(err)
	}
	request.Transactions = []core.Transaction{*wrongNonceTx}
	if _, _, err := db.PreviewLPoD(3, request); err == nil {
		t.Fatal("top-up replay with a skipped position nonce was accepted")
	}
	tx, err := core.BuildLPoDPositionTx(topUp, owner.Spend.Private, topUpPrivate)
	if err != nil {
		t.Fatal(err)
	}
	request.Transactions = []core.Transaction{*tx}
	afterTopUp, _, err := db.PreviewLPoD(3, request)
	if err != nil {
		t.Fatal(err)
	}
	updated := afterTopUp.Positions[id]
	if updated.Due != debt || updated.PendingDue != 0 || updated.Nonce != 1 ||
		len(updated.TopUps) != 1 || updated.NextPayoutNS <= request.Timestamp ||
		afterTopUp.PrincipalLocked != principal+topUp.Amount {
		t.Fatal("top-up reset recognized debt, failed to reset pending accrual, or did not lock the exact new principal")
	}

	commitPreview := func(height uint64, parent *core.Block, timestamp int64, operations ...core.Transaction) (*core.Block, *LPoDCheckpoint) {
		t.Helper()
		request.Parent = parent.Hash()
		request.Timestamp = timestamp
		request.Transactions = operations
		next, payments, err := db.PreviewLPoD(height, request)
		if err != nil {
			t.Fatal(err)
		}
		payout, err := db.PayoutLPoD(height, request, next, payments)
		if err != nil {
			t.Fatal(err)
		}
		txs := []core.Transaction{payout, core.LPoDCheckpointTx(next.Digest())}
		txs = append(txs, operations...)
		block := &core.Block{Header: core.BlockHeader{
			Height: height, Round: parent.Header.Round + 1, Timestamp: timestamp,
			PrevHash: parent.Hash(), ValidatorPub: pub,
		}, Txs: txs}
		block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
		if err := block.Header.Sign(priv); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.PutRawBlock(block.Hash(), height, raw); err != nil {
			t.Fatal(err)
		}
		cpraw, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.db.Put(lpodKey(block.Hash()), cpraw, nil); err != nil {
			t.Fatal(err)
		}
		if err := db.PutTip(block.Hash(), height); err != nil {
			t.Fatal(err)
		}
		return block, next
	}
	// Commit sequential previews so the next transition uses its own canonical
	// parent timestamp, as a real replay does.
	request.Timestamp = boundary.Add(-time.Second).UnixNano()
	topUpBlock, _ := commitPreview(3, parent, request.Timestamp, *tx)
	withdrawAmount := 2 * lpod.Unit
	withdraw := deposit
	withdraw.Action, withdraw.Nonce, withdraw.WithdrawAmount = core.LPoDWithdraw, 2, withdrawAmount
	withdrawTx, err := core.BuildLPoDPositionTx(withdraw, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	withdrawBlock, afterPartial := commitPreview(4, topUpBlock, topUpBlock.Header.Timestamp+int64(15*time.Second), *withdrawTx)
	partial := afterPartial.Positions[id]
	if partial.Withdrawn != withdrawAmount || partial.PendingDue == 0 || partial.Due != debt ||
		afterPartial.PrincipalLocked != principal+topUp.Amount-withdrawAmount {
		t.Fatal("partial withdrawal did not preserve same-block accrued liability and remaining principal")
	}
	_, afterNextBlock := commitPreview(5, withdrawBlock, withdrawBlock.Header.Timestamp+int64(15*time.Second))
	nextPosition := afterNextBlock.Positions[id]
	tier, err := lpod.TierFor(request.Stake[pub.Hex()].Amount + principal + topUp.Amount - withdrawAmount)
	if err != nil {
		t.Fatal(err)
	}
	expectedEarned := new(big.Int).SetUint64(principal + topUp.Amount - withdrawAmount)
	expectedEarned.Mul(expectedEarned, new(big.Int).SetUint64(tier.APRPercent))
	expectedEarned.Mul(expectedEarned, big.NewInt(int64(15*time.Second)))
	expectedEarned.Add(expectedEarned, new(big.Int).SetUint64(partial.APRCarry))
	expectedEarned.Quo(expectedEarned, new(big.Int).SetUint64(lpodAPRDenominator))
	if !expectedEarned.IsUint64() || nextPosition.PendingDue-partial.PendingDue != expectedEarned.Uint64() {
		t.Fatal("post-withdrawal accrual did not use the remaining principal and preserved fractional carry")
	}
}

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
