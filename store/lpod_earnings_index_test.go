// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/syndtr/goleveldb/leveldb"
)

func TestLPoDEarningsIndexFundingRollbackReplayAndPagination(t *testing.T) {
	db, path, migration, priv, address := trustedLPoDV3FundingFixture(t)
	defer db.Close()
	parent, _, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	funding, settlement, raw := lpodActivationBlock(t, db, migration, priv, address)
	if err := db.CommitRawBlockWithAVM(funding.Hash(), LPoDV2ActivationHeight, raw, nil, crypto.Hash32{}, settlement); err != nil {
		t.Fatalf("commit v3 funding: %v", err)
	}
	checkpoint, err := db.LoadLPoDCheckpoint()
	if err != nil || checkpoint == nil || checkpoint.Allocation == nil || checkpoint.Allocation.Version != 3 {
		t.Fatalf("v3 funding checkpoint unavailable: %v", err)
	}
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := db.LPoDEarningsIndexReady(checkpoint.Allocation.FundingBlock, tipHash, tipHeight); err != nil || !ready {
		t.Fatalf("earnings index not ready after funding replay: ready=%t err=%v", ready, err)
	}
	paid, _, err := db.LPoDEarningsOutputs(address, "", 128)
	if err != nil || len(paid) == 0 {
		t.Fatalf("expected canonical reward-only payout refs: outputs=%d err=%v", len(paid), err)
	}
	for _, u := range paid {
		if u.AmountNAPRO == 0 || u.TxHash != funding.Txs[0].Hash() || u.OutputIndex >= uint32(len(funding.Txs[0].Outputs)) {
			t.Fatalf("invalid reward-only payout ref: %+v", u)
		}
	}
	// Simulate an already-funded v3 database upgraded from the older index
	// schema: the funding block is durable, but its versioned readiness/data
	// have never been written by this binary.
	if err := db.db.Delete(lpodEarningsReadyKey, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.clearLPoDEarningsRows(); err != nil {
		t.Fatal(err)
	}
	// This funded database predates the modern genesis hash index format. Keep
	// the authenticated genesis identity while replacing only its stored index
	// with the signed pre-oracle historical hash.
	genesis, err := db.readLPoDCanonicalBlock(0)
	if err != nil {
		t.Fatal(err)
	}
	heightBytes, timestampBytes, roundBytes := make([]byte, 8), make([]byte, 8), make([]byte, 4)
	legacyHash := crypto.HashBytes(heightBytes, genesis.Header.PrevHash[:], genesis.Header.MerkleRoot[:],
		timestampBytes, roundBytes, genesis.Header.ValidatorPub)
	legacySignature, err := priv.Sign(legacyHash)
	if err != nil {
		t.Fatal(err)
	}
	genesis.Header.Signature = legacySignature
	legacyRaw, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(legacyHash, 0, legacyRaw); err != nil {
		t.Fatal(err)
	}
	if _, err := db.readLPoDCanonicalBlock(0); err == nil {
		t.Fatal("legacy genesis unexpectedly passed modern block hash indexing")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen already-funded v3 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.BackfillLPoDEarningsIndex(migration); err != nil {
		t.Fatalf("backfill an already-funded v3 database: %v", err)
	}
	tipHash, tipHeight, err = db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := db.LPoDEarningsIndexReady(checkpoint.Allocation.FundingBlock, tipHash, tipHeight); err != nil || !ready {
		t.Fatalf("historical backfill did not publish tip-bound readiness: ready=%t err=%v", ready, err)
	}
	paidAfterBackfill, _, err := db.LPoDEarningsOutputs(address, "", 128)
	if err != nil || len(paidAfterBackfill) != len(paid) {
		t.Fatalf("backfill output refs differ: before=%d after=%d err=%v", len(paid), len(paidAfterBackfill), err)
	}

	if err := db.PutTip(parent, LPoDV2ActivationHeight-1); err != nil {
		t.Fatalf("rollback funding: %v", err)
	}
	if ready, err := db.LPoDEarningsIndexReady(checkpoint.Allocation.FundingBlock, parent, LPoDV2ActivationHeight-1); err != nil || ready {
		t.Fatalf("rollback retained earnings readiness: ready=%t err=%v", ready, err)
	}
	rolledBack, _, err := db.LPoDEarningsOutputs(address, "", 128)
	if err != nil || len(rolledBack) != 0 {
		t.Fatalf("rollback retained earnings output refs: rows=%d err=%v", len(rolledBack), err)
	}
	if err := db.CommitRawBlockWithAVM(funding.Hash(), LPoDV2ActivationHeight, raw, nil, crypto.Hash32{}, settlement); err != nil {
		t.Fatalf("replay v3 funding: %v", err)
	}
	checkpoint, err = db.LoadLPoDCheckpoint()
	if err != nil || checkpoint == nil || checkpoint.Allocation == nil {
		t.Fatalf("replayed checkpoint unavailable: %v", err)
	}
	tipHash, tipHeight, err = db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := db.LPoDEarningsIndexReady(checkpoint.Allocation.FundingBlock, tipHash, tipHeight); err != nil || !ready {
		t.Fatalf("earnings index not ready after replay: ready=%t err=%v", ready, err)
	}

	// Exercise the bounded address index pagination independently of whether
	// this funding block itself paid a reward.
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	pageAddress := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	prefix := lpodEarningsPrefix(pageAddress)
	batch := new(leveldb.Batch)
	for i := uint64(1); i <= 129; i++ {
		var suffix [12]byte
		binary.BigEndian.PutUint64(suffix[:8], i)
		u := StoredUTXO{BlockHeight: i, AmountNAPRO: i}
		u.TxHash = crypto.HashBytes([]byte{byte(i), 1})
		u.OutputIndex = uint32(i)
		u.OneTimePub[0] = byte(i)
		u.TxPubKey[0] = byte(i + 1)
		u.AmountCommit[0] = byte(i + 2)
		u.EncAmount[0] = byte(i + 3)
		binary.BigEndian.PutUint32(suffix[8:], u.OutputIndex)
		key := append(append([]byte{}, prefix...), suffix[:]...)
		blockHash := crypto.HashBytes([]byte{byte(i), 2})
		key = append(key, blockHash[:]...)
		key = append(key, u.TxHash[:]...)
		value, err := json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		batch.Put(key, value)
	}
	if err := db.db.Write(batch, nil); err != nil {
		t.Fatal(err)
	}
	page, cursor, err := db.LPoDEarningsOutputs(pageAddress, "", 128)
	if err != nil || len(page) != 128 || cursor == "" {
		t.Fatalf("first earnings page: rows=%d cursor=%q err=%v", len(page), cursor, err)
	}
	page, cursor, err = db.LPoDEarningsOutputs(pageAddress, cursor, 128)
	if err != nil || len(page) != 1 || cursor != "" {
		t.Fatalf("second earnings page: rows=%d cursor=%q err=%v", len(page), cursor, err)
	}
	if _, _, err := db.LPoDEarningsOutputs(pageAddress, "", 129); err == nil {
		t.Fatal("earnings page limit above 128 was accepted")
	}
}

func TestLPoDPureRewardClassificationFailsClosedForMixedOutputs(t *testing.T) {
	tests := []struct {
		name      string
		reward    uint64
		principal uint64
		want      bool
	}{
		{name: "reward-only", reward: 1, want: true},
		{name: "mixed reward and returned principal", reward: 1, principal: 1, want: false},
		{name: "principal-only", principal: 1, want: false},
		{name: "zero", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLPoDPureReward(tt.reward, tt.principal); got != tt.want {
				t.Fatalf("isLPoDPureReward(%d, %d)=%t, want %t", tt.reward, tt.principal, got, tt.want)
			}
		})
	}
}

func TestLPoDEarningsMixedPayoutBackfillRollbackAndReplay(t *testing.T) {
	db, _, migration, priv, _ := trustedLPoDV3FundingFixture(t)
	defer db.Close()
	addressKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, addressKeys)
	funding, settlement, raw := lpodActivationBlock(t, db, migration, priv, address)
	if err := db.CommitRawBlockWithAVM(funding.Hash(), migration.Height, raw, nil, crypto.Hash32{}, settlement); err != nil {
		t.Fatalf("commit funding block: %v", err)
	}

	sourceSecret := crypto.ScalarFromUint64(7)
	sourcePub, err := crypto.ScalarMulBase(sourceSecret)
	if err != nil {
		t.Fatal(err)
	}
	sourceTx := crypto.HashBytes([]byte("test-only authenticated LPoD source"))
	action := core.LPoDPositionAction{
		Action: core.LPoDDeposit, Genesis: migration.Genesis,
		SourceTx: sourceTx, SourceIndex: 0, SourcePub: sourcePub,
		Vault: crypto.Point32(priv.Public()), Beneficiary: address,
		Owner: addressKeys.Spend.Public, Amount: 100 * lpod.Unit,
		Blind: crypto.BlindFactor(crypto.ScalarFromUint64(9)),
	}
	action.PositionID = core.LPoDPositionID(action.Genesis, action.SourceTx, action.SourceIndex)
	deposit, err := core.BuildLPoDPositionTx(action, addressKeys.Spend.Private, sourceSecret)
	if err != nil {
		t.Fatalf("build signed deposit: %v", err)
	}
	depositBlock, _, _, _ := commitLPoDStoreTestBlock(t, db, migration, priv, address, *deposit)

	withdrawAction := action
	withdrawAction.Action = core.LPoDWithdraw
	withdrawAction.Nonce = 1
	withdraw, err := core.BuildLPoDPositionTx(withdrawAction, addressKeys.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatalf("build signed withdrawal: %v", err)
	}
	mixedBlock, mixedRequest, mixedCheckpoint, mixedPayout := commitLPoDStoreTestBlock(t, db, migration, priv, address, *withdraw)
	if len(mixedPayout.Outputs) == 0 || mixedRequest == nil {
		t.Fatal("withdrawal block did not produce a payout output")
	}
	if mixedRequest.Transactions[0].IsLPoDPosition() != true {
		t.Fatal("withdrawal operation was not included in canonical settlement")
	}
	if len(mixedPayout.Outputs) != 1 {
		t.Fatalf("expected same-beneficiary reward/principal aggregate, got %d outputs", len(mixedPayout.Outputs))
	}
	depositCheckpoint, err := db.LoadLPoDCheckpointAt(depositBlock.Hash())
	if err != nil || depositCheckpoint == nil ||
		mixedCheckpoint.PrincipalReturned <= depositCheckpoint.PrincipalReturned ||
		mixedCheckpoint.State.LeaderPaid <= depositCheckpoint.State.LeaderPaid {
		t.Fatal("withdrawal payout did not combine returned principal with a canonical reward")
	}
	rows, _, err := db.LPoDEarningsOutputs(address, "", 128)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.TxHash == mixedPayout.Hash() || row.BlockHeight == mixedBlock.Header.Height {
			t.Fatalf("mixed principal/reward output was indexed as earnings: %+v", row)
		}
	}

	if err := db.PutTip(depositBlock.Hash(), depositBlock.Header.Height); err != nil {
		t.Fatalf("rollback mixed payout block: %v", err)
	}
	if ready, err := db.LPoDEarningsIndexReady(funding.Hash(), funding.Hash(), funding.Header.Height); err != nil || ready {
		t.Fatalf("rollback left a stale ready marker: ready=%t err=%v", ready, err)
	}
	mixedRaw, err := json.Marshal(mixedBlock)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitRawBlockWithAVM(mixedBlock.Hash(), mixedBlock.Header.Height, mixedRaw, nil, crypto.Hash32{}, mixedRequest); err != nil {
		t.Fatalf("replay mixed payout block: %v", err)
	}
	if err := db.BackfillLPoDEarningsIndex(migration); err != nil {
		t.Fatalf("rebuild index after rollback/replay: %v", err)
	}
	rows, _, err = db.LPoDEarningsOutputs(address, "", 128)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.TxHash == mixedPayout.Hash() || row.BlockHeight == mixedBlock.Header.Height {
			t.Fatalf("replayed mixed output was indexed as earnings: %+v", row)
		}
	}
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := db.LPoDEarningsIndexReady(funding.Hash(), tipHash, tipHeight); err != nil || !ready {
		t.Fatalf("replayed backfill did not restore complete readiness: ready=%t err=%v", ready, err)
	}
}

func TestLPoDEarningsBackfillRejectsStaleTipHeightIndex(t *testing.T) {
	db, _, migration, priv, address := trustedLPoDV3FundingFixture(t)
	defer db.Close()
	funding, settlement, raw := lpodActivationBlock(t, db, migration, priv, address)
	if err := db.CommitRawBlockWithAVM(funding.Hash(), migration.Height, raw, nil, crypto.Hash32{}, settlement); err != nil {
		t.Fatalf("commit funding block: %v", err)
	}
	targetHash, targetHeight, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	stale := *funding
	stale.Header.Timestamp++
	staleRaw, err := json.Marshal(&stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(stale.Hash(), targetHeight, staleRaw); err != nil {
		t.Fatal(err)
	}
	if err := db.BackfillLPoDEarningsIndex(migration); err == nil {
		t.Fatal("backfill accepted durable tip whose canonical height index disagrees")
	}
	afterHash, afterHeight, err := db.GetTip()
	if err != nil || afterHash != targetHash || afterHeight != targetHeight {
		t.Fatalf("backfill changed the live chain tip: hash=%x height=%d err=%v", afterHash, afterHeight, err)
	}
	if ready, err := db.LPoDEarningsIndexReady(funding.Hash(), targetHash, targetHeight); err != nil || ready {
		t.Fatalf("stale height index left readiness published: ready=%t err=%v", ready, err)
	}
}

func commitLPoDStoreTestBlock(t *testing.T, db *DB, migration *LPoDMigration, priv crypto.ValidatorPrivKey, leader crypto.Address, operations ...core.Transaction) (*core.Block, *LPoDSettlement, *LPoDCheckpoint, core.Transaction) {
	t.Helper()
	parentHash, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	parent, err := db.readLPoDCanonicalBlock(height)
	if err != nil {
		t.Fatal(err)
	}
	request := &LPoDSettlement{
		Parent: parentHash, PositionProtocol: true, Timestamp: parent.Header.Timestamp + int64(LPoDMaxElapsedNS),
		Proposer: priv.Public().Hex(), Leader: leader,
		Stake:         map[string]LPoDValidatorStake{priv.Public().Hex(): {Amount: 100_000 * lpod.Unit, Active: true}},
		PreviousStake: map[string]LPoDValidatorStake{priv.Public().Hex(): {Amount: 100_000 * lpod.Unit, Active: true}},
		Transactions:  operations,
	}
	next, payments, err := db.PreviewLPoD(height+1, request)
	if err != nil {
		t.Fatalf("preview LPoD transition: %v", err)
	}
	payout, err := db.PayoutLPoD(height+1, request, next, payments)
	if err != nil {
		t.Fatalf("reconstruct LPoD payout: %v", err)
	}
	txs := []core.Transaction{payout, core.LPoDCheckpointTx(next.Digest())}
	txs = append(txs, operations...)
	block := &core.Block{Header: core.BlockHeader{Height: height + 1, PrevHash: parentHash,
		Timestamp: request.Timestamp, ValidatorPub: priv.Public()}, Txs: txs}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	if err := block.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitRawBlockWithAVM(block.Hash(), block.Header.Height, raw, nil, crypto.Hash32{}, request); err != nil {
		t.Fatalf("commit LPoD transition: %v", err)
	}
	return block, request, next, payout
}
