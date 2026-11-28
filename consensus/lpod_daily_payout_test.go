// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package consensus

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/internal/testutil"
	"github.com/aperod/aperod/store"
	"github.com/aperod/aperod/wallet"
)

func acceptPositionBlockAt(
	t *testing.T,
	e *Engine,
	priv crypto.ValidatorPrivKey,
	timestamp int64,
	operations ...core.Transaction,
) *core.Block {
	return acceptPositionBlockWithRewardAt(
		t, e, priv, crypto.Address(e.cfg.RewardAddress), timestamp, operations...,
	)
}

func acceptPositionBlockWithRewardAt(
	t *testing.T,
	e *Engine,
	priv crypto.ValidatorPrivKey,
	rewardAddress crypto.Address,
	timestamp int64,
	operations ...core.Transaction,
) *core.Block {
	t.Helper()
	parent := e.chain.Tip()
	if parent == nil || timestamp <= parent.Header.Timestamp {
		t.Fatal("daily payout rehearsal timestamp must advance the canonical parent")
	}
	b := &core.Block{Header: core.BlockHeader{
		Height: parent.Header.Height + 1, Round: parent.Header.Round + 1,
		PrevHash: parent.Hash(), Timestamp: timestamp, ValidatorPub: priv.Public(),
		BaseFee: nextBaseFee(parent.Header.BaseFee, parent.Size()),
	}, Txs: operations}
	r, checkpoint, payments, err := e.prepareLPoD(b, rewardAddress)
	if err != nil {
		t.Fatal(err)
	}
	payout, err := e.cfg.Store.PayoutLPoD(b.Header.Height, r, checkpoint, payments)
	if err != nil {
		t.Fatal(err)
	}
	b.Txs = append([]core.Transaction{payout, core.LPoDCheckpointTx(checkpoint.Digest())}, operations...)
	b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
	if err := b.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	if err := e.handleIncomingBlock(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLPoDDailyValidatorPayoutIsOwnerOpenableAndReorgSafe(t *testing.T) {
	const mnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	keys, err := wallet.DeriveFromMnemonic(mnemonic, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	e, db, deposit, _, owner, priv, dbPath := positionEngineWithOwner(t, keys.Keys)
	e.cfg.LPoDValidatorDailyUpgradeHeight = 3

	acceptPositionBlock(t, e, priv, 3_000_000_000)
	beforeActivation, err := db.LoadLPoDCheckpoint()
	if err != nil || beforeActivation == nil {
		t.Fatalf("load pre-activation checkpoint: %v", err)
	}
	e.cfg.OnCanonicalBlock = func(block *core.Block, prepared *avm.PreparedBlock) error {
		raw, err := json.Marshal(block)
		if err != nil {
			return err
		}
		if err := avm.CommitCanonicalBlock(db, block, raw, prepared); err != nil {
			return err
		}
		if prepared.LPoD != nil {
			registryAfter := e.cfg.Registry.TakeSnapshot()
			prepared.LPoD.RegistryAfter = &registryAfter
			if err := db.PersistLPoDAuditAt(block, prepared.LPoD, 0); err != nil {
				return err
			}
			audit, err := db.LoadLPoDAuditAt(block.Hash())
			if err != nil || audit == nil || audit.UnavailableReason != "" {
				return fmt.Errorf("daily payout audit evidence incomplete: audit=%+v err=%v", audit, err)
			}
		}
		return nil
	}
	rewardAddress := crypto.AddressFromKeys(crypto.MainnetByte, owner)
	e.cfg.RewardAddress = string(rewardAddress)
	activationBlock := acceptPositionBlock(t, e, priv, 3_000_000_000, *deposit)
	afterActivation, err := db.LoadLPoDCheckpoint()
	if err != nil || afterActivation == nil {
		t.Fatalf("load activation checkpoint: %v", err)
	}
	if afterActivation.ValidatorDailyPayoutVersion != 1 ||
		afterActivation.DailyPayoutVersion != 1 ||
		afterActivation.State.LeaderPaid != beforeActivation.State.LeaderPaid ||
		afterActivation.State.PendingLeader == 0 ||
		len(activationBlock.Txs[0].Outputs) != 0 {
		t.Fatalf("activation paid a validator before the daily boundary: checkpoint=%+v outputs=%d",
			afterActivation, len(activationBlock.Txs[0].Outputs))
	}
	boundary, err := store.LPoDNextDailyBoundaryAfter24h(activationBlock.Header.Timestamp)
	if err != nil {
		t.Fatal(err)
	}

	server := api.NewServer(":0", e.chain, e.pool, e.utxos,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetStore(db)
	server.SetLPoDConfig(e.cfg.LPoDMigration, func(uint64, crypto.Hash32) bool { return true })
	server.SetReady()
	server.SetUTXOReady()
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	readAPIOutputs := func() []struct {
		Hash   string `json:"tx_hash"`
		Index  uint32 `json:"out_idx"`
		Height uint64 `json:"block_height"`
	} {
		t.Helper()
		response, err := http.Get(httpServer.URL + "/api/v1/lpod/wallet-outputs?address=" +
			url.QueryEscape(string(rewardAddress)))
		if err != nil {
			t.Fatal(err)
		}
		defer testutil.CleanupClose(t, response.Body)
		var body struct {
			State   string `json:"state"`
			Outputs []struct {
				Hash   string `json:"tx_hash"`
				Index  uint32 `json:"out_idx"`
				Height uint64 `json:"block_height"`
			} `json:"outputs"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil ||
			response.StatusCode != http.StatusOK || body.State != "active" {
			t.Fatalf("wallet outputs API status=%d state=%q err=%v", response.StatusCode, body.State, err)
		}
		return body.Outputs
	}
	if outputs := readAPIOutputs(); len(outputs) != 0 {
		t.Fatalf("daily reward appeared before 20:00: %+v", outputs)
	}

	beforeBoundary := acceptPositionBlockAt(t, e, priv, boundary-1_000_000_000)
	beforeBoundaryState, err := db.LoadLPoDCheckpoint()
	if err != nil || beforeBoundaryState == nil {
		t.Fatalf("load pre-boundary checkpoint: %v", err)
	}
	if beforeBoundaryState.State.LeaderPaid != beforeActivation.State.LeaderPaid ||
		beforeBoundaryState.State.PendingLeader <= afterActivation.State.PendingLeader ||
		len(beforeBoundary.Txs[0].Outputs) != 0 || len(readAPIOutputs()) != 0 {
		t.Fatalf("validator reward was visible before 20:00: checkpoint=%+v outputs=%d",
			beforeBoundaryState.State, len(beforeBoundary.Txs[0].Outputs))
	}
	beforeAudit, err := db.LoadLPoDAuditAt(beforeBoundary.Hash())
	if err != nil || beforeAudit == nil {
		t.Fatalf("load pre-boundary daily audit: %v", err)
	}
	for _, validator := range beforeAudit.Vaults {
		if validator.ActualLeaderPaid != 0 {
			t.Fatalf("daily audit reported a validator payment before 20:00: %+v", validator)
		}
	}

	boundaryBlock := acceptPositionBlockAt(t, e, priv, boundary)
	afterBoundary, err := db.LoadLPoDCheckpoint()
	if err != nil || afterBoundary == nil {
		t.Fatalf("load post-boundary checkpoint: %v", err)
	}
	if afterBoundary.State.PendingLeader != 0 ||
		afterBoundary.State.LeaderPaid <= beforeActivation.State.LeaderPaid ||
		afterBoundary.NextValidatorPayoutNS <= boundary {
		t.Fatalf("daily validator reward did not settle at its boundary: %+v", afterBoundary)
	}
	apiOutputs := readAPIOutputs()
	payoutHash := boundaryBlock.Txs[0].Hash()
	if len(apiOutputs) != 1 || apiOutputs[0].Height != boundaryBlock.Header.Height ||
		apiOutputs[0].Hash != fmt.Sprintf("%x", payoutHash[:]) {
		t.Fatalf("canonical daily payout was not indexed exactly once: %+v", apiOutputs)
	}
	boundaryAudit, err := db.LoadLPoDAuditAt(boundaryBlock.Hash())
	if err != nil || boundaryAudit == nil {
		t.Fatalf("load boundary daily audit: %v", err)
	}
	var auditedLeaderPaid uint64
	for _, validator := range boundaryAudit.Vaults {
		auditedLeaderPaid += validator.ActualLeaderPaid
	}
	if auditedLeaderPaid != afterBoundary.State.LeaderPaid-beforeActivation.State.LeaderPaid {
		t.Fatalf("daily audit does not match the validator output: audit=%d checkpoint=%d",
			auditedLeaderPaid, afterBoundary.State.LeaderPaid-beforeActivation.State.LeaderPaid)
	}
	rows, _, err := db.LPoDWalletOutputs(rewardAddress, "", 128)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read indexed owner payout: rows=%d err=%v", len(rows), err)
	}
	row := rows[0]
	h, err := crypto.ScanForOutput(owner.View.Private, owner.Spend.Public, row.TxPubKey, row.OneTimePub)
	if err != nil || h == nil {
		t.Fatalf("wallet could not open the daily payout: %v", err)
	}
	amount := core.DecryptAmount(row.EncAmount, h)
	want := (afterBoundary.State.LeaderPaid - beforeActivation.State.LeaderPaid) +
		(afterBoundary.State.AngelPaid - beforeActivation.State.AngelPaid)
	if amount != want {
		t.Fatalf("opened payout amount=%d, want validator+Guardian total=%d",
			amount, want)
	}
	owned := e.utxos.Get(row.TxHash, row.OutputIndex)
	if owned == nil || owned.ProtocolLocked {
		t.Fatal("canonical daily payout is not an available spendable output")
	}
	durableDigest := afterBoundary.Digest()

	httpServer.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen daily payout database: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	e.cfg.Store = reopened
	e.store = reopened
	e.cfg.OnCanonicalBlock = func(block *core.Block, prepared *avm.PreparedBlock) error {
		raw, err := json.Marshal(block)
		if err != nil {
			return err
		}
		return avm.CommitCanonicalBlock(reopened, block, raw, prepared)
	}
	restarted, err := reopened.LoadLPoDCheckpoint()
	if err != nil || restarted == nil || restarted.Digest() != durableDigest ||
		restarted.State.PendingLeader != 0 {
		t.Fatalf("daily payout checkpoint changed after restart: checkpoint=%+v err=%v", restarted, err)
	}
	rows, _, err = reopened.LPoDWalletOutputs(rewardAddress, "", 128)
	if err != nil || len(rows) != 1 || rows[0].TxHash != row.TxHash ||
		rows[0].OutputIndex != row.OutputIndex {
		t.Fatalf("daily wallet credit changed after restart: rows=%d err=%v", len(rows), err)
	}

	if err := e.utxos.RollbackBlock(boundaryBlock); err != nil {
		t.Fatal(err)
	}
	if err := e.chain.RollbackLastBlock(boundaryBlock); err != nil {
		t.Fatal(err)
	}
	if err := reopened.PutTip(beforeBoundary.Hash(), beforeBoundary.Header.Height); err != nil {
		t.Fatal(err)
	}
	rows, _, err = reopened.LPoDWalletOutputs(rewardAddress, "", 128)
	if err != nil || len(rows) != 0 {
		t.Fatalf("orphan daily payout survived rollback: rows=%d err=%v", len(rows), err)
	}
	e.slashing = newSlashingDetector()
	if err := e.handleIncomingBlock(boundaryBlock); err != nil {
		t.Fatalf("replay canonical daily payout: %v", err)
	}
	replayed, err := reopened.LoadLPoDCheckpoint()
	if err != nil || replayed == nil || replayed.Digest() != durableDigest {
		t.Fatalf("daily payout replay changed checkpoint: checkpoint=%+v err=%v", replayed, err)
	}
	rows, _, err = reopened.LPoDWalletOutputs(rewardAddress, "", 128)
	if err != nil || len(rows) != 1 || rows[0].TxHash != row.TxHash ||
		rows[0].OutputIndex != row.OutputIndex {
		t.Fatalf("daily wallet credit duplicated or disappeared after reorg replay: rows=%d err=%v", len(rows), err)
	}
}

func TestLPoDDailyPayoutRoutesMultipleValidatorsToPinnedWallets(t *testing.T) {
	e, db, _, _, _, firstPriv, dbPath := positionEngine(t)
	secondPriv, secondPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	thirdPriv, thirdPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	validatorPrivs := []crypto.ValidatorPrivKey{firstPriv, secondPriv, thirdPriv}
	validatorPubs := []crypto.ValidatorPubKey{
		firstPriv.Public(), secondPub, thirdPub,
	}
	privByValidator := make(map[string]crypto.ValidatorPrivKey, len(validatorPrivs))
	for _, priv := range validatorPrivs {
		privByValidator[priv.Public().Hex()] = priv
	}
	e.cfg.Validators = validatorPubs
	e.cfg.Registry.InitFromGenesis(validatorPubs, core.MinStakeNAPR)
	migration := e.cfg.LPoDMigration
	migration.TrustedValidators = append([]crypto.ValidatorPubKey(nil), validatorPubs...)
	migration.Attestations = nil
	for _, priv := range validatorPrivs {
		signature, err := priv.Sign(migration.AttestationMessage())
		if err != nil {
			t.Fatal(err)
		}
		migration.Attestations = append(migration.Attestations, store.LPoDAttestation{
			Validator: priv.Public(), Signature: signature,
		})
	}
	e.cfg.LPoDValidatorDailyUpgradeHeight = 3

	walletA, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	walletB, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	addressA := crypto.AddressFromKeys(crypto.MainnetByte, walletA)
	addressB := crypto.AddressFromKeys(crypto.MainnetByte, walletB)
	firstAddresses := map[string]crypto.Address{
		validatorPubs[0].Hex(): addressA,
		validatorPubs[1].Hex(): addressB,
		validatorPubs[2].Hex(): addressA,
	}
	dailyBlocksByValidator := make(map[string]int)
	rotatedRewardAddress := false
	dailyPayoutBoundary := int64(0)

	e.cfg.OnCanonicalBlock = func(block *core.Block, prepared *avm.PreparedBlock) error {
		raw, err := json.Marshal(block)
		if err != nil {
			return err
		}
		if err := avm.CommitCanonicalBlock(db, block, raw, prepared); err != nil {
			return err
		}
		if prepared.LPoD != nil {
			registryAfter := e.cfg.Registry.TakeSnapshot()
			prepared.LPoD.RegistryAfter = &registryAfter
			if err := db.PersistLPoDAuditAt(block, prepared.LPoD, 0); err != nil {
				return err
			}
			audit, err := db.LoadLPoDAuditAt(block.Hash())
			if err != nil || audit == nil || audit.UnavailableReason != "" {
				return fmt.Errorf("multi-validator payout audit incomplete: audit=%+v err=%v", audit, err)
			}
		}
		return nil
	}

	appendScheduled := func(timestamp int64, daily bool) (*core.Block, crypto.Address, string) {
		t.Helper()
		parent := e.chain.Tip()
		proposer := e.proposerAt(parent.Header.Round + 1)
		if proposer == nil {
			t.Fatal("multi-validator schedule has no proposer")
		}
		validator := proposer.Hex()
		priv, ok := privByValidator[validator]
		if !ok {
			t.Fatalf("scheduled validator %s has no signing key", validator)
		}
		rewardAddress := firstAddresses[validator]
		rotatedThisBlock := false
		if daily && timestamp < dailyPayoutBoundary &&
			validator == validatorPubs[0].Hex() && dailyBlocksByValidator[validator] > 0 {
			rewardAddress = addressB
			rotatedRewardAddress = true
			rotatedThisBlock = true
		}
		block := acceptPositionBlockWithRewardAt(t, e, priv, rewardAddress, timestamp)
		if daily {
			dailyBlocksByValidator[validator]++
			if rotatedThisBlock {
				checkpoint, err := db.LoadLPoDCheckpoint()
				if err != nil || checkpoint == nil {
					t.Fatalf("load checkpoint after reward-address rotation: %v", err)
				}
				pinned := false
				for _, payout := range checkpoint.PendingValidatorPayouts {
					if payout.Validator == validatorPubs[0].Hex() {
						pinned = payout.Address == addressA
					}
				}
				if !pinned {
					t.Fatalf("validator reward was no longer pending after address rotation: validator=%s height=%d time=%d next=%d pending=%+v",
						validatorPubs[0].Hex(), block.Header.Height, timestamp,
						checkpoint.NextValidatorPayoutNS, checkpoint.PendingValidatorPayouts)
				}
			}
		}
		return block, rewardAddress, validator
	}

	// Apply the authorized migration with the expanded validator registry, but
	// keep the daily payout gate closed until the next block.
	appendScheduled(3_000_000_000, false)
	beforeActivation, err := db.LoadLPoDCheckpoint()
	if err != nil || beforeActivation == nil {
		t.Fatalf("load pre-daily checkpoint: %v", err)
	}
	baselineRowsA, _, err := db.LPoDWalletOutputs(addressA, "", 128)
	if err != nil {
		t.Fatal(err)
	}
	baselineRowsB, _, err := db.LPoDWalletOutputs(addressB, "", 128)
	if err != nil {
		t.Fatal(err)
	}
	rowsEqual := func(got, want []store.StoredUTXO) bool {
		if len(got) != len(want) {
			return false
		}
		keys := make(map[string]bool, len(want))
		for _, row := range want {
			keys[fmt.Sprintf("%x:%d", row.TxHash, row.OutputIndex)] = true
		}
		for _, row := range got {
			key := fmt.Sprintf("%x:%d", row.TxHash, row.OutputIndex)
			if !keys[key] {
				return false
			}
		}
		return true
	}
	outputsForTx := func(rows []store.StoredUTXO, hash crypto.Hash32) []store.StoredUTXO {
		var selected []store.StoredUTXO
		for _, row := range rows {
			if row.TxHash == hash {
				selected = append(selected, row)
			}
		}
		return selected
	}
	activationBlock, _, activationValidator := appendScheduled(4_000_000_000, true)
	if activationBlock.Header.Height != e.cfg.LPoDValidatorDailyUpgradeHeight {
		t.Fatalf("daily activation height=%d, want %d", activationBlock.Header.Height, e.cfg.LPoDValidatorDailyUpgradeHeight)
	}
	afterActivation, err := db.LoadLPoDCheckpoint()
	if err != nil || afterActivation == nil || afterActivation.State.PendingLeader == 0 {
		t.Fatalf("validator %s did not accrue a deferred daily reward: checkpoint=%+v err=%v",
			activationValidator, afterActivation, err)
	}
	boundary, err := store.LPoDNextDailyBoundaryAfter24h(activationBlock.Header.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	dailyPayoutBoundary = boundary

	// Six rounds after activation cover a second scheduled appearance by each
	// validator. One changes its configured recipient mid-window, which must
	// not redirect rewards already attributed to its first address.
	for i := 0; i < 6; i++ {
		timestamp := boundary - int64(6-i)*1_000_000_000
		appendScheduled(timestamp, true)
	}
	if !rotatedRewardAddress {
		t.Fatal("the test schedule did not exercise an in-window reward-address change")
	}
	beforeBoundary, err := db.LoadLPoDCheckpoint()
	if err != nil || beforeBoundary == nil || len(beforeBoundary.PendingValidatorPayouts) != len(validatorPubs) {
		t.Fatalf("not all validators accrued in the same daily window: checkpoint=%+v err=%v", beforeBoundary, err)
	}
	preBoundaryTip := e.chain.Tip()
	if preBoundaryTip == nil {
		t.Fatal("missing chain tip before daily payout boundary")
	}
	for _, pub := range validatorPubs {
		if dailyBlocksByValidator[pub.Hex()] < 2 {
			t.Fatalf("validator %s did not accrue more than once: counts=%v", pub.Hex(), dailyBlocksByValidator)
		}
	}
	if rows, _, err := db.LPoDWalletOutputs(addressA, "", 128); err != nil || !rowsEqual(rows, baselineRowsA) {
		t.Fatalf("shared wallet changed before the daily boundary: rows=%d err=%v", len(rows), err)
	}
	if rows, _, err := db.LPoDWalletOutputs(addressB, "", 128); err != nil || !rowsEqual(rows, baselineRowsB) {
		t.Fatalf("second wallet changed before the daily boundary: rows=%d err=%v", len(rows), err)
	}

	pendingByValidator := make(map[string]uint64, len(beforeBoundary.PendingValidatorPayouts))
	expectedByAddress := make(map[crypto.Address]uint64, 2)
	for _, payout := range beforeBoundary.PendingValidatorPayouts {
		pendingByValidator[payout.Validator] = payout.AmountNAPRO
		expectedByAddress[payout.Address] += payout.AmountNAPRO
	}
	boundaryBlock, boundaryRewardAddress, boundaryValidator := appendScheduled(boundary, true)
	afterBoundary, err := db.LoadLPoDCheckpoint()
	if err != nil || afterBoundary == nil || afterBoundary.State.PendingLeader != 0 ||
		len(afterBoundary.PendingValidatorPayouts) != 0 {
		t.Fatalf("daily boundary did not settle all validator rewards: checkpoint=%+v err=%v", afterBoundary, err)
	}
	if afterBoundary.State.LeaderPaid < beforeBoundary.State.LeaderPaid+beforeBoundary.State.PendingLeader {
		t.Fatal("paid validator total does not include the prior daily window")
	}
	boundaryReward := afterBoundary.State.LeaderPaid -
		beforeBoundary.State.LeaderPaid - beforeBoundary.State.PendingLeader
	pinnedAddress := boundaryRewardAddress
	if _, found := pendingByValidator[boundaryValidator]; found {
		for _, payout := range beforeBoundary.PendingValidatorPayouts {
			if payout.Validator == boundaryValidator {
				pinnedAddress = payout.Address
				break
			}
		}
	}
	expectedByAddress[pinnedAddress] += boundaryReward
	expectedByValidator := pendingByValidator
	expectedByValidator[boundaryValidator] += boundaryReward
	if len(expectedByAddress) != 2 || expectedByAddress[addressA] == 0 || expectedByAddress[addressB] == 0 {
		t.Fatalf("fixture did not aggregate a shared recipient alongside a distinct owner: %v", expectedByAddress)
	}
	if got := len(boundaryBlock.Txs[0].Outputs); got != len(expectedByAddress) {
		t.Fatalf("daily payout outputs=%d, want one per distinct owner (%d)", got, len(expectedByAddress))
	}

	audit, err := db.LoadLPoDAuditAt(boundaryBlock.Hash())
	if err != nil || audit == nil || !audit.Available {
		t.Fatalf("load multi-validator boundary audit: audit=%+v err=%v", audit, err)
	}
	auditedByValidator := make(map[string]uint64, len(audit.Vaults))
	auditedByAddress := make(map[crypto.Address]uint64, len(audit.Outputs))
	for _, validator := range audit.Vaults {
		if validator.ActualLeaderPaid > 0 {
			auditedByValidator[validator.ID] = validator.ActualLeaderPaid
		}
	}
	for _, output := range audit.Outputs {
		auditedByAddress[crypto.Address(output.Beneficiary)] += output.Amount
	}
	for validator, amount := range expectedByValidator {
		if auditedByValidator[validator] != amount {
			t.Fatalf("audit payout for validator %s=%d, want %d", validator, auditedByValidator[validator], amount)
		}
	}
	for address, amount := range expectedByAddress {
		if auditedByAddress[address] != amount {
			t.Fatalf("audit payout for owner %s=%d, want %d", address, auditedByAddress[address], amount)
		}
	}

	boundaryTxHash := boundaryBlock.Txs[0].Hash()
	allRowsA, _, err := db.LPoDWalletOutputs(addressA, "", 128)
	if err != nil {
		t.Fatal(err)
	}
	allRowsB, _, err := db.LPoDWalletOutputs(addressB, "", 128)
	if err != nil {
		t.Fatal(err)
	}
	rowsA := outputsForTx(allRowsA, boundaryTxHash)
	rowsB := outputsForTx(allRowsB, boundaryTxHash)
	if len(rowsA) != 1 || len(rowsB) != 1 ||
		rowsA[0].TxHash != rowsB[0].TxHash || rowsA[0].OutputIndex == rowsB[0].OutputIndex {
		t.Fatalf("distinct-owner wallet outputs are not separate outputs of the same payout: A=%+v B=%+v err=%v",
			rowsA, rowsB, err)
	}
	openAmount := func(owner *crypto.WalletKeyPair, row store.StoredUTXO, want uint64) {
		t.Helper()
		shared, err := crypto.ScanForOutput(owner.View.Private, owner.Spend.Public, row.TxPubKey, row.OneTimePub)
		if err != nil || shared == nil || core.DecryptAmount(row.EncAmount, shared) != want {
			t.Fatalf("wallet could not open its exact daily payout: want=%d err=%v", want, err)
		}
		owned := e.utxos.Get(row.TxHash, row.OutputIndex)
		if owned == nil || owned.ProtocolLocked {
			t.Fatal("daily validator payout is not spendable")
		}
	}
	cannotOpen := func(owner *crypto.WalletKeyPair, row store.StoredUTXO) {
		t.Helper()
		shared, err := crypto.ScanForOutput(owner.View.Private, owner.Spend.Public, row.TxPubKey, row.OneTimePub)
		if err != nil || shared != nil {
			t.Fatalf("another wallet opened a payout output: shared=%v err=%v", shared != nil, err)
		}
	}
	openAmount(walletA, rowsA[0], expectedByAddress[addressA])
	openAmount(walletB, rowsB[0], expectedByAddress[addressB])
	cannotOpen(walletB, rowsA[0])
	cannotOpen(walletA, rowsB[0])

	durableHash, durableHeight := boundaryTxHash, boundaryBlock.Header.Height
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen multi-validator payout database: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	e.cfg.Store = reopened
	e.store = reopened
	e.cfg.OnCanonicalBlock = func(block *core.Block, prepared *avm.PreparedBlock) error {
		raw, err := json.Marshal(block)
		if err != nil {
			return err
		}
		return avm.CommitCanonicalBlock(reopened, block, raw, prepared)
	}
	allRowsA, _, err = reopened.LPoDWalletOutputs(addressA, "", 128)
	if err != nil || len(outputsForTx(allRowsA, durableHash)) != 1 {
		t.Fatalf("shared-owner payout changed after restart: rows=%+v err=%v", rowsA, err)
	}

	if err := e.utxos.RollbackBlock(boundaryBlock); err != nil {
		t.Fatal(err)
	}
	if err := e.chain.RollbackLastBlock(boundaryBlock); err != nil {
		t.Fatal(err)
	}
	if err := reopened.PutTip(preBoundaryTip.Hash(), beforeBoundary.State.LastHeight); err != nil {
		t.Fatal(err)
	}
	for _, address := range []crypto.Address{addressA, addressB} {
		rows, _, err := reopened.LPoDWalletOutputs(address, "", 128)
		baseline := baselineRowsA
		if address == addressB {
			baseline = baselineRowsB
		}
		if err != nil || !rowsEqual(rows, baseline) {
			t.Fatalf("rollback changed the pre-window wallet index for %s: rows=%d err=%v", address, len(rows), err)
		}
	}
	e.slashing = newSlashingDetector()
	if err := e.handleIncomingBlock(boundaryBlock); err != nil {
		t.Fatalf("replay multi-validator boundary payout: %v", err)
	}
	replayed, err := reopened.LoadLPoDCheckpoint()
	if err != nil || replayed == nil || replayed.State.LastHeight != durableHeight ||
		replayed.Digest() != afterBoundary.Digest() {
		t.Fatalf("multi-validator payout changed after replay: checkpoint=%+v err=%v", replayed, err)
	}
	allRowsA, _, err = reopened.LPoDWalletOutputs(addressA, "", 128)
	if err != nil || len(outputsForTx(allRowsA, durableHash)) != 1 ||
		!rowsEqual(allRowsA, append(append([]store.StoredUTXO(nil), baselineRowsA...), outputsForTx(allRowsA, durableHash)...)) {
		t.Fatalf("shared-owner payout duplicated or disappeared after replay: rows=%+v err=%v", rowsA, err)
	}
	allRowsB, _, err = reopened.LPoDWalletOutputs(addressB, "", 128)
	if err != nil || len(outputsForTx(allRowsB, durableHash)) != 1 ||
		!rowsEqual(allRowsB, append(append([]store.StoredUTXO(nil), baselineRowsB...), outputsForTx(allRowsB, durableHash)...)) {
		t.Fatalf("second-owner payout duplicated or disappeared after replay: rows=%+v err=%v", rowsB, err)
	}
}
