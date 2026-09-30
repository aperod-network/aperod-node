// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package consensus

import (
	"encoding/hex"
	"testing"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

func TestLPoDRegistryAuditSnapshotsBracketWithdrawalsBeforeEpochUpdate(t *testing.T) {
	e, _, deposit, _, _, sourcePriv, _ := positionEngine(t)
	acceptPositionBlock(t, e, sourcePriv, 3_000_000_000, *deposit)

	_, pendingPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_, seededPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	registryState := e.cfg.Registry.TakeSnapshot()
	registryState.Validators[pendingPub.Hex()] = &core.ValidatorEntry{
		PubKey: pendingPub, StakeNAPR: 100_000 * lpod.Unit,
		Status: core.ValidatorPending, ActivationEpoch: 1,
	}
	registryState.Validators[seededPub.Hex()] = &core.ValidatorEntry{
		PubKey: seededPub, StakeNAPR: core.MinStakeNAPR, Status: core.ValidatorPending, Seeded: true,
	}
	e.cfg.Registry.RestoreFromSnapshot(registryState)

	originalCommit := e.cfg.OnCanonicalBlock
	var prepared *store.LPoDSettlement
	e.cfg.OnCanonicalBlock = func(block *core.Block, state *avm.PreparedBlock) error {
		if err := originalCommit(block, state); err != nil {
			return err
		}
		if state.LPoD != nil {
			registryAfter := e.cfg.Registry.TakeSnapshot()
			state.LPoD.RegistryAfter = &registryAfter
			prepared = state.LPoD
		}
		return nil
	}
	exitTx := lifecycleFullExit(t, e, sourcePriv, e.chain.Tip().Header.Height+10)
	producer := e.proposerAt(e.chain.Tip().Header.Round + 1)
	if producer == nil || !producer.Equals(sourcePriv.Public()) {
		t.Fatalf("test fixture has no key for scheduled producer %v", producer)
	}
	acceptPositionBlock(t, e, sourcePriv, 3_000_000_000, exitTx)
	if prepared == nil || prepared.RegistryBefore == nil || prepared.RegistryAfter == nil {
		t.Fatal("canonical callback did not preserve both registry snapshots")
	}
	sourceID := sourcePriv.Public().Hex()
	beforeSource := prepared.RegistryBefore.Validators[sourceID]
	afterSource := prepared.RegistryAfter.Validators[sourceID]
	if beforeSource == nil || beforeSource.Status != core.ValidatorActive ||
		prepared.PreviousStake[sourceID].Active != true ||
		prepared.Stake[sourceID].Active != false ||
		afterSource == nil || afterSource.Status != core.ValidatorUnbonding {
		t.Fatalf("withdrawal snapshots do not bracket the stake operation: before=%+v after=%+v stake=%+v",
			beforeSource, afterSource, prepared.Stake[sourceID])
	}
	if prepared.RegistryBefore.Validators[seededPub.Hex()] == nil {
		t.Fatal("pre-operation snapshot omitted seeded registry entry")
	}
	if _, included := prepared.AuditPreviousStake[seededPub.Hex()]; !included {
		t.Fatal("full audit projection omitted seeded registry entry")
	}
	if _, included := prepared.PreviousStake[seededPub.Hex()]; included {
		t.Fatal("audit-only seeded entry changed the historical routing candidate set")
	}

	// On a boundary, the callback snapshot is the pre-epoch registry. Simulate
	// the subsequent epoch transition separately, as the read API must do.
	pendingBeforeEpoch := prepared.RegistryAfter.Validators[pendingPub.Hex()]
	if pendingBeforeEpoch == nil || pendingBeforeEpoch.Status != core.ValidatorPending {
		t.Fatalf("expected pre-epoch pending snapshot, got %+v", pendingBeforeEpoch)
	}
	e.cfg.Registry.UpdateEpoch(core.EpochLength)
	if prepared.RegistryAfter.Validators[pendingPub.Hex()].Status != core.ValidatorPending {
		t.Fatal("post-epoch simulation mutated the captured post-block snapshot")
	}
	if current, ok := e.cfg.Registry.GetEntry(pendingPub); !ok || current.Status != core.ValidatorActive {
		t.Fatalf("separate epoch simulation did not activate pending validator: %+v", current)
	}
}

func TestLPoDPartialWithdrawalUsesStaticRoutingMinimum(t *testing.T) {
	e, _, deposit, _, _, sourcePriv, _ := positionEngine(t)
	acceptPositionBlock(t, e, sourcePriv, 3_000_000_000, *deposit)
	var err error

	registryState := e.cfg.Registry.TakeSnapshot()
	sourceID := sourcePriv.Public().Hex()
	source := registryState.Validators[sourceID]
	if source == nil {
		t.Fatal("validator missing from registry snapshot")
	}
	var secondPriv crypto.ValidatorPrivKey
	var secondPub crypto.ValidatorPubKey
	for secondPub == nil || secondPub.Hex() < sourceID {
		secondPriv, secondPub, err = crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
	}
	const excess = uint64(100)
	const withdrawal = uint64(125)
	source.StakeNAPR = core.MinStakeNAPR + excess
	registryState.DynamicMinNAPR = core.MinStakeNAPR / 2
	registryState.Validators[secondPub.Hex()] = &core.ValidatorEntry{
		PubKey: secondPub, StakeNAPR: core.MinStakeNAPR, Status: core.ValidatorActive,
	}
	e.cfg.Registry.RestoreFromSnapshot(registryState)

	originalCommit := e.cfg.OnCanonicalBlock
	var prepared *store.LPoDSettlement
	e.cfg.OnCanonicalBlock = func(block *core.Block, state *avm.PreparedBlock) error {
		if err := originalCommit(block, state); err != nil {
			return err
		}
		if state.LPoD != nil {
			registryAfter := e.cfg.Registry.TakeSnapshot()
			state.LPoD.RegistryAfter = &registryAfter
			prepared = state.LPoD
		}
		return nil
	}

	tx := lifecyclePartialWithdrawal(t, e, sourcePriv, withdrawal, e.chain.Tip().Header.Height+10)
	producer := e.proposerAt(e.chain.Tip().Header.Round + 1)
	producerPriv := sourcePriv
	if producer != nil && producer.Equals(secondPub) {
		producerPriv = secondPriv
	} else if producer == nil || !producer.Equals(sourcePriv.Public()) {
		t.Fatalf("test fixture has no key for scheduled producer %v", producer)
	}
	acceptPositionBlock(t, e, producerPriv, 3_000_000_000, tx)
	if prepared == nil {
		t.Fatal("canonical commit did not retain the prepared LPoD settlement")
	}
	wantStake := core.MinStakeNAPR + excess - withdrawal
	if wantStake >= core.MinStakeNAPR || wantStake < registryState.DynamicMinNAPR {
		t.Fatal("test stake is not between static and dynamic thresholds")
	}
	if prepared.Stake[sourceID].Amount != wantStake || prepared.Stake[sourceID].Active {
		t.Fatalf("LPoD routing did not apply static minimum: %+v", prepared.Stake[sourceID])
	}
	after := prepared.RegistryAfter.Validators[sourceID]
	if after == nil || after.StakeNAPR != wantStake || after.Status != core.ValidatorActive {
		t.Fatalf("registry did not retain dynamic-minimum eligibility: %+v", after)
	}
}

func lifecycleFullExit(t *testing.T, e *Engine, priv crypto.ValidatorPrivKey, expiry uint64) core.Transaction {
	t.Helper()
	entry, found := e.cfg.Registry.GetEntry(priv.Public())
	if !found {
		t.Fatal("validator missing")
	}
	a := core.StakeWithdrawalAuthorizationV2{Action: core.StakeWithdraw, PubKey: priv.Public(),
		Genesis: e.cfg.LPoDMigration.Genesis, Nonce: entry.StakeAuthNonce + 1,
		Generation: entry.StakeGeneration, ExpiryHeight: expiry}
	a.StakeRef = core.StakeReferenceV2(a.Genesis, a.PubKey, a.Generation, entry.StakeNAPR)
	sig, err := priv.Sign(core.StakeWithdrawalSignMsgV2(a))
	if err != nil {
		t.Fatal(err)
	}
	a.Signature = sig
	extra, err := core.EncodeStakeWithdrawalExtraV2(a)
	if err != nil {
		t.Fatal(err)
	}
	return core.Transaction{Version: core.TxVersionStake, Extra: extra}
}

func lifecyclePartialWithdrawal(t *testing.T, e *Engine, priv crypto.ValidatorPrivKey, amount, expiry uint64) core.Transaction {
	t.Helper()
	entry, found := e.cfg.Registry.GetEntry(priv.Public())
	if !found {
		t.Fatal("validator missing")
	}
	a := core.StakeWithdrawalAuthorizationV2{Action: core.StakePartialWithdraw, PubKey: priv.Public(),
		Amount: amount, Genesis: e.cfg.LPoDMigration.Genesis, Nonce: entry.StakeAuthNonce + 1,
		Generation: entry.StakeGeneration, ExpiryHeight: expiry}
	a.StakeRef = core.StakeReferenceV2(a.Genesis, a.PubKey, a.Generation, entry.StakeNAPR)
	sig, err := priv.Sign(core.StakeWithdrawalSignMsgV2(a))
	if err != nil {
		t.Fatal(err)
	}
	a.Signature = sig
	extra, err := core.EncodeStakeWithdrawalExtraV2(a)
	if err != nil {
		t.Fatal(err)
	}
	return core.Transaction{Version: core.TxVersionStake, Extra: extra}
}

func TestLPoDValidatorExitDeterministicallyRoutesAndPreservesAuthorization(t *testing.T) {
	e, db, deposit, action, owner, sourcePriv, _ := positionEngine(t)
	acceptPositionBlock(t, e, sourcePriv, 3_000_000_000, *deposit)

	destinationPriv, destinationPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.Registry.InitFromGenesis([]crypto.ValidatorPubKey{destinationPub}, core.MinStakeNAPR)
	exitProposer := sourcePriv
	if expected := e.proposerAt(e.chain.Tip().Header.Round + 1); expected != nil && expected.Equals(destinationPub) {
		exitProposer = destinationPriv
	}
	exitBlock := acceptPositionBlock(t, e, exitProposer, 3_000_000_000, lifecycleFullExit(t, e, sourcePriv, e.chain.Tip().Header.Height+10))

	checkpoint, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(action.PositionID[:])
	position := checkpoint.Positions[id]
	if position.Deposit.Vault != action.Vault || position.EffectiveVault == nil ||
		store.LPoDEffectiveVault(position) != crypto.Point32(destinationPub) || position.RouteHeight != exitBlock.Header.Height {
		t.Fatalf("immutable source or effective route mismatch: %+v", position)
	}
	if position.Returned || checkpoint.PrincipalLocked != action.Amount {
		t.Fatal("reassignment returned or changed guardian principal")
	}

	// The owner still signs the original immutable action. Routing is protocol
	// state and must neither require nor permit a replacement owner signature.
	withdrawAction := action
	withdrawAction.Action = core.LPoDWithdraw
	withdrawAction.Nonce = 1
	withdrawAction.WithdrawAmount = lpod.Unit
	withdraw, err := core.BuildLPoDPositionTx(withdrawAction, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	acceptPositionBlock(t, e, destinationPriv, 3_000_000_000, *withdraw)
	checkpoint, err = db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Positions[id].Withdrawn != lpod.Unit ||
		*checkpoint.Positions[id].EffectiveVault != crypto.Point32(destinationPub) {
		t.Fatal("withdrawal using original signed vault failed after reassignment")
	}
}

func TestLPoDValidatorExitWithoutCandidateReturnsSpendablePrincipal(t *testing.T) {
	e, db, deposit, action, owner, sourcePriv, _ := positionEngine(t)
	acceptPositionBlock(t, e, sourcePriv, 3_000_000_000, *deposit)
	exitBlock := acceptPositionBlock(t, e, sourcePriv, 3_000_000_000, lifecycleFullExit(t, e, sourcePriv, e.chain.Tip().Header.Height+10))

	checkpoint, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	position := checkpoint.Positions[hex.EncodeToString(action.PositionID[:])]
	if !position.Returned || position.Withdrawn != action.Amount || position.UnlockHeight != exitBlock.Header.Height ||
		checkpoint.PrincipalLocked != 0 || checkpoint.PrincipalReturned != action.Amount {
		t.Fatalf("automatic principal refund was not conserved: %+v", position)
	}
	if position.Due != checkpoint.State.UnfundedLiability {
		t.Fatal("automatic principal refund discarded earned unpaid liability")
	}

	var refunded uint64
	for _, out := range exitBlock.Txs[0].Outputs {
		hs, err := crypto.ScanForOutput(owner.View.Private, owner.Spend.Public, out.TxPubKey, out.OneTimePub)
		if err != nil {
			t.Fatal(err)
		}
		if hs != nil {
			refunded += core.DecryptAmount(out.EncAmount, hs)
		}
	}
	if refunded < action.Amount {
		t.Fatalf("real v10 payout returned %d, want at least principal %d", refunded, action.Amount)
	}

	// The returned output is part of the ordinary canonical UTXO set and wallet
	// discovery index, rather than an application-side balance adjustment.
	found := false
	for i := range exitBlock.Txs[0].Outputs {
		if dbUTXO := e.utxos.Get(exitBlock.Txs[0].Hash(), uint32(i)); dbUTXO != nil {
			found = true
		}
	}
	if !found || !exitBlock.Txs[0].IsLPoDPayout() || exitBlock.Txs[0].Version != core.TxVersionLPoDPayout {
		t.Fatal("automatic refund did not create canonical spendable payout output")
	}
}
