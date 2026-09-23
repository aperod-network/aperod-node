// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package consensus

import (
	"encoding/hex"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

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
