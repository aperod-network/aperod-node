// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

func TestDailyAuditRegistryEvidenceReplaysStakeAndEpochTransition(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	entry := &core.ValidatorEntry{
		PubKey: pub, StakeNAPR: 3 * core.MinStakeNAPR, Status: core.ValidatorActive,
		StakeGeneration: 1,
	}
	before := core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{pub.Hex(): entry}}
	topUpAmount, withdrawAmount := core.MinStakeNAPR, core.MinStakeNAPR
	topUpSignature, err := priv.Sign(core.StakeSignMsg(core.StakeDeposit, pub, topUpAmount))
	if err != nil {
		t.Fatal(err)
	}
	topUpExtra, err := core.EncodeStakeExtra(core.StakeDeposit, pub, topUpAmount, topUpSignature)
	if err != nil {
		t.Fatal(err)
	}
	withdrawSignature, err := priv.Sign(core.StakeSignMsg(core.StakePartialWithdraw, pub, withdrawAmount))
	if err != nil {
		t.Fatal(err)
	}
	withdrawExtra, err := core.EncodeStakeExtra(core.StakePartialWithdraw, pub, withdrawAmount, withdrawSignature)
	if err != nil {
		t.Fatal(err)
	}
	block := &core.Block{Header: core.BlockHeader{Height: 5}, Txs: []core.Transaction{
		{Version: core.TxVersionStake, Extra: topUpExtra},
		{Version: core.TxVersionStake, Extra: withdrawExtra},
	}}
	beforeBytes, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var replayStart core.RegistrySnapshot
	if err := json.Unmarshal(beforeBytes, &replayStart); err != nil {
		t.Fatal(err)
	}
	replayed := core.NewValidatorRegistry()
	replayed.RestoreFromSnapshot(replayStart)
	if err := replayed.ReplayBlockStakeTxs(block.Txs, block.Header.Height); err != nil {
		t.Fatalf("replay stake txs: %v", err)
	}
	after := replayed.TakeSnapshot()
	record := &store.LPoDBlockAudit{
		RegistryBefore: &before, RegistryAfter: &after,
		PreviousStake: map[string]store.LPoDValidatorStake{
			pub.Hex(): {Amount: 3 * core.MinStakeNAPR, Active: true},
		},
		// LPoD's pre-operation stake projection includes the partial withdrawal,
		// but deliberately does not project a same-block top-up.
		Stake: map[string]store.LPoDValidatorStake{
			pub.Hex(): {Amount: 2 * core.MinStakeNAPR, Active: true},
		},
	}
	server := &Server{}
	projected, _, err := server.verifyDailyRegistryEvidence(context.Background(), block, record, nil)
	if err != nil {
		t.Fatalf("verify top-up/partial-withdrawal evidence: %v", err)
	}
	if got := projected[pub.Hex()]; got.Amount != 2*core.MinStakeNAPR || !got.Active {
		t.Fatalf("projected stake = %#v, want 2x minimum and active", got)
	}
	record.RegistryAfter.DynamicMinNAPR++
	if _, _, err := server.verifyDailyRegistryEvidence(context.Background(), block, record, nil); err == nil {
		t.Fatal("tampered post-operation snapshot was accepted")
	}

	// Registry replay uses the snapshot's dynamic minimum, but the auxiliary
	// LPoD Stake projection intentionally uses the original static minimum.
	staticBefore := core.RegistrySnapshot{
		Validators: map[string]*core.ValidatorEntry{
			pub.Hex(): {
				PubKey: pub, StakeNAPR: 2 * core.MinStakeNAPR, Status: core.ValidatorActive,
				StakeGeneration: 1,
			},
		},
		DynamicMinNAPR: 1,
	}
	staticWithdrawAmount := 3 * core.MinStakeNAPR / 2
	staticWithdrawSig, err := priv.Sign(core.StakeSignMsg(core.StakePartialWithdraw, pub, staticWithdrawAmount))
	if err != nil {
		t.Fatal(err)
	}
	staticWithdrawExtra, err := core.EncodeStakeExtra(core.StakePartialWithdraw, pub, staticWithdrawAmount, staticWithdrawSig)
	if err != nil {
		t.Fatal(err)
	}
	staticBlock := &core.Block{
		Header: core.BlockHeader{Height: 6},
		Txs:    []core.Transaction{{Version: core.TxVersionStake, Extra: staticWithdrawExtra}},
	}
	staticStartBytes, err := json.Marshal(staticBefore)
	if err != nil {
		t.Fatal(err)
	}
	var staticReplayStart core.RegistrySnapshot
	if err := json.Unmarshal(staticStartBytes, &staticReplayStart); err != nil {
		t.Fatal(err)
	}
	staticReplay := core.NewValidatorRegistry()
	staticReplay.RestoreFromSnapshot(staticReplayStart)
	if err := staticReplay.ReplayBlockStakeTxs(staticBlock.Txs, staticBlock.Header.Height); err != nil {
		t.Fatalf("replay partial withdrawal below static minimum but above dynamic minimum: %v", err)
	}
	staticAfter := staticReplay.TakeSnapshot()
	staticRecord := &store.LPoDBlockAudit{
		RegistryBefore: &staticBefore, RegistryAfter: &staticAfter,
		PreviousStake: map[string]store.LPoDValidatorStake{
			pub.Hex(): {Amount: 2 * core.MinStakeNAPR, Active: true},
		},
		Stake: map[string]store.LPoDValidatorStake{
			pub.Hex(): {Amount: core.MinStakeNAPR / 2, Active: false},
		},
	}
	staticProjection, _, err := server.verifyDailyRegistryEvidence(context.Background(), staticBlock, staticRecord, nil)
	if err != nil {
		t.Fatalf("verify static-minimum stake projection: %v", err)
	}
	if got := staticProjection[pub.Hex()]; got.Amount != 2*core.MinStakeNAPR || !got.Active {
		t.Fatalf("effective static-minimum stake = %#v, want prior active stake fallback", got)
	}

	pending := &core.ValidatorEntry{
		PubKey: pub, StakeNAPR: core.MinStakeNAPR, Status: core.ValidatorPending,
		ActivationEpoch: 1, StakeGeneration: 1,
	}
	epochBefore := core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{pub.Hex(): pending}}
	preBoundarySnapshot := &epochBefore
	var beforeBoundary *core.RegistrySnapshot
	for height := core.EpochLength - 2; height < core.EpochLength; height++ {
		preBoundaryBlock := &core.Block{Header: core.BlockHeader{Height: height}}
		preBoundaryRecord := &store.LPoDBlockAudit{
			RegistryBefore: preBoundarySnapshot, RegistryAfter: preBoundarySnapshot,
			PreviousStake: map[string]store.LPoDValidatorStake{
				pub.Hex(): {Amount: core.MinStakeNAPR, Active: false},
			},
			Stake: map[string]store.LPoDValidatorStake{
				pub.Hex(): {Amount: core.MinStakeNAPR, Active: false},
			},
		}
		var expectedBefore *core.RegistrySnapshot
		if height > core.EpochLength-2 {
			expectedBefore = beforeBoundary
		}
		_, beforeBoundary, err = server.verifyDailyRegistryEvidence(
			context.Background(), preBoundaryBlock, preBoundaryRecord, expectedBefore)
		if err != nil {
			t.Fatalf("verify non-boundary pre-epoch snapshot at height %d: %v", height, err)
		}
		if beforeBoundary.Validators[pub.Hex()].Status != core.ValidatorPending {
			t.Fatalf("pending validator activated before the epoch boundary at height %d", height)
		}
		preBoundarySnapshot = beforeBoundary
	}
	epochBlock := &core.Block{Header: core.BlockHeader{Height: core.EpochLength}}
	epochAfter := *beforeBoundary
	epochRecord := &store.LPoDBlockAudit{
		RegistryBefore: beforeBoundary, RegistryAfter: &epochAfter,
		PreviousStake: map[string]store.LPoDValidatorStake{
			pub.Hex(): {Amount: core.MinStakeNAPR, Active: false},
		},
		Stake: map[string]store.LPoDValidatorStake{
			pub.Hex(): {Amount: core.MinStakeNAPR, Active: false},
		},
	}
	_, expectedNext, err := server.verifyDailyRegistryEvidence(context.Background(), epochBlock, epochRecord, beforeBoundary)
	if err != nil {
		t.Fatalf("verify epoch-boundary snapshot: %v", err)
	}
	if expectedNext.Validators[pub.Hex()].Status != core.ValidatorActive {
		t.Fatal("epoch transition did not activate eligible pending validator")
	}
	nextBlock := &core.Block{Header: core.BlockHeader{Height: core.EpochLength + 1}}
	nextRecord := &store.LPoDBlockAudit{
		RegistryBefore: expectedNext, RegistryAfter: expectedNext,
		PreviousStake: map[string]store.LPoDValidatorStake{
			pub.Hex(): {Amount: core.MinStakeNAPR, Active: true},
		},
		Stake: map[string]store.LPoDValidatorStake{
			pub.Hex(): {Amount: core.MinStakeNAPR, Active: true},
		},
	}
	if _, _, err := server.verifyDailyRegistryEvidence(context.Background(), nextBlock, nextRecord, expectedNext); err != nil {
		t.Fatalf("verify subsequent snapshot after epoch transition: %v", err)
	}
}

func TestDailyAuditBusyReturnsTooManyRequests(t *testing.T) {
	server := &Server{}
	server.lpodAuditDailyMu.Lock()
	defer server.lpodAuditDailyMu.Unlock()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?date=2024-01-02", nil)
	server.restLPoDAuditDaily(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("busy audit status %d, want 429: %s", recorder.Code, recorder.Body.String())
	}
}

func TestDailyAuditCanceledRequestStopsBeforeStoreScan(t *testing.T) {
	server := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/?date=2024-01-02", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	server.restLPoDAuditDaily(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "request was cancelled") {
		t.Fatalf("cancelled request response %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestVerifyDailyPayoutRejectsShiftedOutputAmount(t *testing.T) {
	wallet, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, wallet)
	parent := crypto.HashBytes([]byte("payout-test-parent"))
	digest := crypto.HashBytes([]byte("payout-test-checkpoint"))
	const amount = uint64(100)
	output, err := core.BuildLPoDPayoutOutput(address, amount, 4, parent, digest)
	if err != nil {
		t.Fatal(err)
	}
	extra, err := json.Marshal(core.LPoDPayoutAuthorization{Leader: address, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	block := &core.Block{Header: core.BlockHeader{Height: 4, PrevHash: parent}, Txs: []core.Transaction{
		{Version: core.TxVersionLPoDPayout, Extra: extra, Outputs: []core.Output{output}},
	}}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	hash := block.Txs[0].Hash()
	record := &store.LPoDBlockAudit{
		PayoutTransactionHash: hash,
		Outputs: []store.LPoDAuditOutputRef{{
			TransactionHash: hash, OutputIndex: 0, Beneficiary: string(address), Amount: amount,
		}},
	}
	if err := verifyDailyPayoutOutputs(block, record, digest, map[crypto.Address]uint64{address: amount}); err != nil {
		t.Fatalf("valid output rejected: %v", err)
	}
	record.Outputs[0].Amount++
	if err := verifyDailyPayoutOutputs(block, record, digest, map[crypto.Address]uint64{address: amount}); err == nil ||
		!strings.Contains(err.Error(), "beneficiary or amount") {
		t.Fatalf("shifted output reference accepted: %v", err)
	}
	record.Outputs[0].Amount = amount
	shifted, err := core.BuildLPoDPayoutOutput(address, amount+1, 4, parent, digest)
	if err != nil {
		t.Fatal(err)
	}
	block.Txs[0].Outputs[0] = shifted
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	if err := verifyDailyPayoutOutputs(block, record, digest, map[crypto.Address]uint64{address: amount}); err == nil ||
		!strings.Contains(err.Error(), "commitments or keys") {
		t.Fatalf("shifted canonical output accepted: %v", err)
	}
}

func TestDailyBurnsUsesCanonicalFeeBasis(t *testing.T) {
	tx := core.Transaction{Version: core.TxVersionBase, Inputs: []core.RingInput{{}}}
	minimum := uint64(tx.Size()) * 3
	tx.Fee = minimum + 500
	block := &core.Block{
		Header: core.BlockHeader{Height: 7, BaseFee: 3},
		Txs:    []core.Transaction{tx},
	}
	base, intentional, avm, total, err := dailyBurns(block, 0)
	if err != nil {
		t.Fatal(err)
	}
	if base != minimum || intentional != 0 || avm != 0 || total != minimum {
		t.Fatalf("fee burn = (%d,%d,%d,%d), want (%d,0,0,%d)",
			base, intentional, avm, total, minimum, minimum)
	}
	zero := uint64(0)
	record := &store.LPoDBlockAudit{
		ProtocolBaseFeeBurnNAPRO: &zero, SignedIntentionalBurnNAPRO: &zero,
		AVMGasBurnNAPRO: &zero, TotalBurnNAPRO: &zero,
	}
	if err := verifyDailyBurnRecord(block, record, 0); err == nil {
		t.Fatal("forged zero fee-burn row was accepted")
	}
}

func TestVerifyDailyPositionRowsRejectsAmountShiftWithSameAggregate(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	parent := &core.Block{Header: core.BlockHeader{
		Height: 0, Timestamp: 1_700_000_000_000_000_000, ValidatorPub: pub,
		MerkleRoot: core.MerkleRoot(nil),
	}}
	if err := parent.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitRawBlockWithAVM(parent.Hash(), 0, raw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	wallet, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	beneficiary := crypto.AddressFromKeys(crypto.MainnetByte, wallet)
	vaultPoint := crypto.Point32(pub)
	vaultID := hex.EncodeToString(vaultPoint[:])
	const elapsed = uint64(1_000_000_000)
	const denominator = uint64(100) * lpod.YearSeconds * 1_000_000_000
	prior := &store.LPoDCheckpoint{Positions: make(map[string]store.LPoDPosition)}
	after := &store.LPoDCheckpoint{Positions: make(map[string]store.LPoDPosition)}
	positionRows := make([]store.LPoDAuditPosition, 0, 2)
	guardianStake := uint64(50_000_000_000_000)
	totalStake := guardianStake + 100_000_000_000
	tier, err := lpod.TierFor(totalStake)
	if err != nil {
		t.Fatal(err)
	}
	for i, amount := range []uint64{20_000_000_000_000, 30_000_000_000_000} {
		source := crypto.HashBytes([]byte{byte(i + 1)})
		genesis := parent.Hash()
		idHash := core.LPoDPositionID(genesis, source, 0)
		id := fmt.Sprintf("%x", idHash[:])
		carryBefore := uint64(i + 11)
		deposit := core.LPoDPositionAction{
			Action: core.LPoDDeposit, Genesis: genesis, PositionID: idHash,
			Vault: vaultPoint, SourceTx: source, Beneficiary: beneficiary, Amount: amount,
		}
		old := store.LPoDPosition{Deposit: deposit, Due: uint64(100 + i), APRCarry: carryBefore}
		prior.Positions[id] = old
		numerator := new(big.Int).SetUint64(amount)
		numerator.Mul(numerator, new(big.Int).SetUint64(tier.APRPercent))
		numerator.Mul(numerator, new(big.Int).SetUint64(elapsed))
		numerator.Add(numerator, new(big.Int).SetUint64(carryBefore))
		q, rem := new(big.Int), new(big.Int)
		q.QuoRem(numerator, new(big.Int).SetUint64(denominator), rem)
		accrued, carryAfter := q.Uint64(), rem.Uint64()
		newPosition := old
		newPosition.Due += accrued
		newPosition.APRCarry = carryAfter
		after.Positions[id] = newPosition
		positionRows = append(positionRows, store.LPoDAuditPosition{
			ID: id, Beneficiary: string(beneficiary), SignedVault: vaultID,
			EffectiveVault: vaultID, EffectiveVaultAfter: vaultID,
			DueBefore: old.Due, Accrued: accrued, DueAfter: newPosition.Due,
			APRCarryBefore: carryBefore, APRCarryAfter: carryAfter,
		})
	}
	record := &store.LPoDBlockAudit{
		Positions: positionRows,
		Vaults: []store.LPoDAuditVault{{
			ID: vaultID, ValidatorStake: 100_000_000_000, GuardianStake: guardianStake,
			TotalStake: totalStake, TierStake: totalStake,
			APRPercent: tier.APRPercent, LeaderPercent: tier.LeaderPercent,
		}},
	}
	block := &core.Block{Header: core.BlockHeader{
		Height: 1, PrevHash: parent.Hash(), Timestamp: parent.Header.Timestamp + int64(elapsed),
	}}
	server := &Server{blockStore: db}
	verifiedStake := map[string]store.LPoDValidatorStake{
		vaultID: {Amount: 100_000_000_000, Active: true},
	}
	if _, _, _, _, _, err := server.verifyDailyPositionRowsWithParent(block, record, prior, after, nil, verifiedStake); err != nil {
		t.Fatalf("valid position rows rejected: %v", err)
	}
	verifiedStake[vaultID] = store.LPoDValidatorStake{Amount: 1, Active: true}
	if _, _, _, _, _, err := server.verifyDailyPositionRowsWithParent(block, record, prior, after, nil, verifiedStake); err == nil {
		t.Fatal("position rows with a mismatched operator-attested validator stake were accepted")
	}
	// Swapping accrued amounts preserves the sum but violates each position's
	// principal/carry-derived accrual.
	record.Positions[0].Accrued, record.Positions[1].Accrued =
		record.Positions[1].Accrued, record.Positions[0].Accrued
	if _, _, _, _, _, err := server.verifyDailyPositionRows(block, record, prior, after); err == nil {
		t.Fatal("same-sum shifted position accruals were accepted")
	}
}
