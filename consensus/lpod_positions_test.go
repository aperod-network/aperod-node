// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package consensus

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"testing"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

func positionEngine(t *testing.T) (*Engine, *store.DB, *core.Transaction, core.LPoDPositionAction, *crypto.WalletKeyPair, crypto.ValidatorPrivKey, string) {
	return positionEngineWithOwner(t, nil)
}
func positionEngineWithOwner(t *testing.T, owner *crypto.WalletKeyPair) (*Engine, *store.DB, *core.Transaction, core.LPoDPositionAction, *crypto.WalletKeyPair, crypto.ValidatorPrivKey, string) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(key.Destroy)
	wallet, err := crypto.GenerateWalletKeys()
	if owner != nil {
		wallet = owner
	}
	if err != nil {
		t.Fatal(err)
	}
	leader, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, wallet)
	path := t.TempDir()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	chain := core.NewChain()
	utxos := core.NewUTXOSet()
	g := &core.Block{Header: core.BlockHeader{Height: 0, Timestamp: 1, ValidatorPub: pub, MerkleRoot: core.MerkleRoot(nil)}}
	g.Header.Sign(priv)
	if err := chain.SetGenesis(g); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(g)
	if err := db.CommitRawBlockWithAVM(g.Hash(), 0, raw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	// A full, provably issued legacy output; the governance witness accounts
	// for this actual issuance rather than conjuring a position from a session.
	const principal = 99_900_000 * lpod.Unit
	source, err := core.BuildMintTx(address, principal, 1)
	if err != nil {
		t.Fatal(err)
	}
	parent := &core.Block{Header: core.BlockHeader{Height: 1, Round: 1, Timestamp: 2, PrevHash: g.Hash(), ValidatorPub: pub}, Txs: []core.Transaction{*source}}
	parent.Header.MerkleRoot = core.MerkleRoot(parent.Txs)
	parent.Header.Sign(priv)
	if err := chain.AddBlock(parent); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(parent)
	if err := db.CommitRawBlockWithAVM(parent.Hash(), 1, raw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	utxos.ApplyBlock(parent)
	blind, err := crypto.DeterministicMintBlindV2(wallet.Spend.Public, principal, 1)
	if err != nil {
		t.Fatal(err)
	}
	m := &store.LPoDMigration{Version: 1, PositionLifecycleVersion: 1, Height: 2, Genesis: g.Hash(), HistoricalIssued: principal,
		ValidatorRemaining: 2_000_000_000*lpod.Unit - 3*lpod.Unit,
		Openings:           []store.LPoDOpening{{Height: 1, Amount: principal, Blind: blind}}}
	m.BodyRoot = store.LPoDBodyRootStep(store.LPoDBodyRootStep(crypto.HashBytes([]byte("aperod/lpod/historical-bodies/v1")), g), parent)
	m.ReconciliationRoot = m.Root()
	sig, _ := priv.Sign(m.AttestationMessage())
	m.Attestations = []store.LPoDAttestation{{Validator: pub, Signature: sig}}
	db.StoreStakingPoolRemaining(m.ValidatorRemaining)
	registry := core.NewValidatorRegistry()
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, 100_000*lpod.Unit)
	verifier := core.NewTxVerifier(utxos)
	poolConfig := core.DefaultMempoolConfig()
	pool := core.NewMempool(poolConfig)
	e := NewEngine(Config{MyKey: key, Validators: []crypto.ValidatorPubKey{pub}, Registry: registry, Store: db,
		LPoDMigration: m, StakingPoolNAPR: 2_000_000_000 * lpod.Unit, RewardAuthorizationActivationHeight: 2,
		RewardAddress: string(crypto.AddressFromKeys(crypto.MainnetByte, leader)), BFTThreshold: 0.667,
		OnCanonicalBlock: func(b *core.Block, p *avm.PreparedBlock) error {
			raw, err := json.Marshal(b)
			if err != nil {
				return err
			}
			return avm.CommitCanonicalBlock(db, b, raw, p)
		},
	}, chain, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.SetTxVerifier(verifier, utxos)
	a := core.LPoDPositionAction{Action: core.LPoDDeposit, Genesis: g.Hash(), Vault: crypto.Point32(pub),
		Beneficiary: address, Owner: wallet.Spend.Public, SourceTx: source.Hash(), SourcePub: source.Outputs[0].OneTimePub,
		Amount: principal, Blind: blind}
	a.PositionID = core.LPoDPositionID(a.Genesis, a.SourceTx, 0)
	sourcePrivate, err := crypto.AddScalars(wallet.Spend.Private, crypto.ScalarFromUint64(1))
	if err != nil {
		t.Fatal(err)
	}
	deposit, err := core.BuildLPoDPositionTx(a, wallet.Spend.Private, sourcePrivate)
	if err != nil {
		t.Fatal(err)
	}
	return e, db, deposit, a, wallet, priv, path
}

func acceptPositionBlock(t *testing.T, e *Engine, priv crypto.ValidatorPrivKey, elapsed int64, operations ...core.Transaction) *core.Block {
	t.Helper()
	parent := e.chain.Tip()
	b := &core.Block{Header: core.BlockHeader{Height: parent.Header.Height + 1, Round: parent.Header.Round + 1,
		PrevHash: parent.Hash(), Timestamp: parent.Header.Timestamp + elapsed, ValidatorPub: priv.Public(),
		BaseFee: nextBaseFee(parent.Header.BaseFee, parent.Size())}, Txs: operations}
	r, c, payments, err := e.prepareLPoD(b, crypto.Address(e.cfg.RewardAddress))
	if err != nil {
		t.Fatal(err)
	}
	payout, err := e.cfg.Store.PayoutLPoD(b.Header.Height, r, c, payments)
	if err != nil {
		t.Fatal(err)
	}
	b.Txs = append([]core.Transaction{payout, core.LPoDCheckpointTx(c.Digest())}, operations...)
	b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
	b.Header.Sign(priv)
	if err := e.handleIncomingBlock(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLPoDAuthenticDepositSurplusDeficitPayoutAndWithdrawal(t *testing.T) {
	e, db, deposit, a, owner, priv, _ := positionEngine(t)
	if err := e.txVerifier.VerifyTx(deposit); err != nil {
		t.Fatal(err)
	}
	depositBlock := acceptPositionBlock(t, e, priv, 3_000_000_000, *deposit)
	c, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(a.PositionID[:])
	if c.PrincipalLocked != a.Amount || len(c.Positions) != 1 || c.Positions[id].Due != 0 {
		t.Fatal("deposit principal not conserved")
	}
	if e.utxos.Get(a.SourceTx, a.SourceIndex) != nil || !e.utxos.IsSpent(deposit.Inputs[0].KeyImage) {
		t.Fatal("principal UTXO was not consumed with linked key image")
	}
	if err := e.txVerifier.VerifyTx(deposit); err == nil {
		t.Fatal("deposit replay accepted")
	}
	beforeBalance := c.State.Balance
	acceptPositionBlock(t, e, priv, 3_000_000_000)
	c, err = db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if c.State.Balance <= beforeBalance || c.State.AngelPaid == 0 {
		t.Fatal("surplus credit or Angels reward missing")
	}
	beforeBalance = c.State.Balance
	beforePaid := c.State.AngelPaid
	deficit := acceptPositionBlock(t, e, priv, 15_000_000_000)
	c, err = db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if c.State.Balance >= beforeBalance || c.State.DeficitOutflow == 0 || c.State.AngelPaid <= beforePaid {
		t.Fatal("funded deficit withdrawal missing")
	}
	paid := c.State.AngelPaid - beforePaid
	expected, err := core.BuildLPoDPayoutOutput(a.Beneficiary, paid, deficit.Header.Height, deficit.Header.PrevHash, c.Digest())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	var paidUTXO core.OwnedUTXO
	for i, out := range deficit.Txs[0].Outputs {
		if out == expected {
			u := e.utxos.Get(deficit.Txs[0].Hash(), uint32(i))
			found = u != nil && !u.ProtocolLocked
			if u != nil {
				hs, err := crypto.ScanForOutput(owner.View.Private, a.Owner, out.TxPubKey, out.OneTimePub)
				if err != nil || hs == nil {
					t.Fatal("wallet cannot discover payout")
				}
				blind, err := crypto.DeterministicPaymentBlind(*hs, paid)
				if err != nil {
					t.Fatal(err)
				}
				paidUTXO = core.OwnedUTXO{UTXO: *u, Amount: paid, Blind: blind, HsScalar: *hs}
			}
		}
	}
	if !found {
		t.Fatal("Angel payout not a real spendable wallet UTXO")
	}
	// Build real ring history before exiting, so the returned principal can be
	// spent in the very next block, without advancing an unbonding clock.
	for i := 0; i < 18; i++ {
		acceptPositionBlock(t, e, priv, 3_000_000_000)
	}
	c, err = db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	beforeWithdrawal := c.State
	priorAPRCarry := c.Positions[id].APRCarry
	withdrawAction := a
	withdrawAction.Action = core.LPoDWithdraw
	withdrawAction.Nonce = 1
	withdraw, err := core.BuildLPoDPositionTx(withdrawAction, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	withdrawBlock := acceptPositionBlock(t, e, priv, 3_000_000_000, *withdraw)
	c, err = db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if c.Positions[id].UnlockHeight != withdrawBlock.Header.Height || !c.Positions[id].Returned ||
		c.PrincipalLocked != 0 || c.PrincipalReturned != a.Amount || c.PrincipalDeposited != a.Amount {
		t.Fatal("withdrawal did not return exact principal in its canonical block")
	}
	n := new(big.Int).SetUint64(a.Amount)
	n.Mul(n, big.NewInt(10)) // the authenticated 100M vault's APR
	n.Mul(n, big.NewInt(3_000_000_000))
	n.Add(n, new(big.Int).SetUint64(priorAPRCarry))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(n, new(big.Int).SetUint64(100*lpod.YearSeconds*1_000_000_000), rem)
	if c.State.AccruedLiability-beforeWithdrawal.AccruedLiability != q.Uint64() ||
		c.Positions[id].APRCarry != rem.Uint64() {
		t.Fatal("exit changed APR or rounding through its canonical inclusion boundary")
	}
	refundAmount := a.Amount + c.State.AngelPaid - beforeWithdrawal.AngelPaid
	refundOutput, err := core.BuildLPoDPayoutOutput(a.Beneficiary, refundAmount, withdrawBlock.Header.Height, withdrawBlock.Header.PrevHash, c.Digest())
	if err != nil {
		t.Fatal(err)
	}
	var refundUTXO core.OwnedUTXO
	found = false
	for i, out := range withdrawBlock.Txs[0].Outputs {
		if out != refundOutput {
			continue
		}
		u := e.utxos.Get(withdrawBlock.Txs[0].Hash(), uint32(i))
		if u == nil || u.ProtocolLocked {
			t.Fatal("canonical principal refund is unavailable")
		}
		hs, err := crypto.ScanForOutput(owner.View.Private, a.Owner, out.TxPubKey, out.OneTimePub)
		if err != nil || hs == nil {
			t.Fatal("owner cannot discover principal refund")
		}
		blind, err := crypto.DeterministicPaymentBlind(*hs, refundAmount)
		if err != nil {
			t.Fatal(err)
		}
		refundUTXO = core.OwnedUTXO{UTXO: *u, Amount: refundAmount, Blind: blind, HsScalar: *hs}
		found = true
	}
	if !found {
		t.Fatal("withdrawal checkpoint omitted the actual principal output")
	}
	refundRecipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	refundSpend, err := core.NewTxBuilder(owner.Spend.Private, owner.View.Private, a.Owner, []core.OwnedUTXO{refundUTXO}, core.InitialBaseFeePerByte).
		WithVersion(core.TxVersionCLSAG).WithDecoySet(e.utxos).
		Build(a.Amount, crypto.AddressFromKeys(crypto.MainnetByte, refundRecipient), a.Beneficiary)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.txVerifier.VerifyTx(&refundSpend.Tx); err != nil {
		t.Fatalf("principal not immediately spendable: %v", err)
	}
	e.cfg.RingCTCLSAGActivationHeight = 2
	beforePaid = c.State.AngelPaid
	acceptPositionBlock(t, e, priv, 15_000_000_000, refundSpend.Tx)
	if !e.utxos.IsSpent(refundSpend.Tx.Inputs[0].KeyImage) {
		t.Fatal("next-block principal spend did not commit")
	}
	c, err = db.LoadLPoDCheckpoint()
	if err != nil || c.State.AngelPaid != beforePaid || c.State.AccruedLiability != beforePaid ||
		c.PrincipalReturned != a.Amount || c.PrincipalLocked != 0 || len(c.Positions) != 0 {
		t.Fatal("withdrawing principal kept accruing APR")
	}
	if err := c.State.Validate(); err != nil {
		t.Fatal(err)
	}
	// The earlier Angels output remains independently spendable after exit.
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	builder := core.NewTxBuilder(owner.Spend.Private, owner.View.Private, a.Owner, []core.OwnedUTXO{paidUTXO}, core.InitialBaseFeePerByte).
		WithVersion(core.TxVersionCLSAG).WithDecoySet(e.utxos)
	spend, err := builder.Build(lpod.Unit, crypto.AddressFromKeys(crypto.MainnetByte, recipient), a.Beneficiary)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.txVerifier.VerifyTx(&spend.Tx); err != nil {
		t.Fatalf("real Angels output cannot be spent: %v", err)
	}
	e.cfg.RingCTCLSAGActivationHeight = 2
	acceptPositionBlock(t, e, priv, 3_000_000_000, spend.Tx)
	if !e.utxos.IsSpent(spend.Tx.Inputs[0].KeyImage) {
		t.Fatal("ordinary spend of Angels payout did not commit")
	}
	// Immutable checkpoint reads survive selecting a prior branch; the deposit
	// itself is not repaid or forgotten by a mutable web/session balance.
	old, err := db.LoadLPoDCheckpointAt(depositBlock.Hash())
	if err != nil || old.Positions[id].Nonce != 0 || old.PrincipalLocked != a.Amount {
		t.Fatal("canonical historical position lost")
	}
}

func TestLPoDAccruesVaultWhoseValidatorDidNotPropose(t *testing.T) {
	e, db, deposit, _, _, priv, _ := positionEngine(t)
	acceptPositionBlock(t, e, priv, 3_000_000_000, *deposit)
	other, otherPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.Registry.InitFromGenesis([]crypto.ValidatorPubKey{otherPub}, 100_000*lpod.Unit)
	if !e.proposerAt(e.chain.Tip().Header.Round + 1).Equals(otherPub) {
		acceptPositionBlock(t, e, priv, 3_000_000_000)
	}
	before, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	block := acceptPositionBlock(t, e, other, 15_000_000_000)
	after, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if block.Header.ValidatorPub.Equals(priv.Public()) || after.State.AngelPaid <= before.State.AngelPaid ||
		after.State.DeficitOutflow <= before.State.DeficitOutflow {
		t.Fatal("non-proposer vault stopped earning its APR")
	}
}

func TestLPoDPartialExitAuthenticatedTopupAndFinalExit(t *testing.T) {
	e, db, deposit, a, owner, priv, _ := positionEngine(t)
	depositBlock := acceptPositionBlock(t, e, priv, 3_000_000_000, *deposit)
	const part = 10_000_000 * lpod.Unit
	exit := a
	exit.Action = core.LPoDWithdraw
	exit.Nonce = 1
	exit.WithdrawAmount = part
	tx, err := core.BuildLPoDPositionTx(exit, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	block := acceptPositionBlock(t, e, priv, 1, *tx)
	c, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(a.PositionID[:])
	if c.PrincipalLocked != a.Amount-part || c.PrincipalReturned != part || c.State.AngelPaid != 0 ||
		c.Positions[id].Withdrawn != part || c.Positions[id].Nonce != 1 || c.Positions[id].Returned {
		t.Fatal("partial exit did not conserve remaining active principal")
	}
	if err := e.utxos.RollbackBlock(block); err != nil {
		t.Fatal(err)
	}
	if err := e.chain.RollbackLastBlock(block); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(depositBlock.Hash(), depositBlock.Header.Height); err != nil {
		t.Fatal(err)
	}
	rolled, err := db.LoadLPoDCheckpoint()
	if err != nil || rolled.PrincipalLocked != a.Amount || rolled.PrincipalReturned != 0 || rolled.Positions[id].Nonce != 0 {
		t.Fatal("partial-exit reorg lost principal or nonce")
	}
	if err := e.handleIncomingBlock(block); err != nil {
		t.Fatal(err)
	}
	for _, request := range []core.LPoDPositionAction{
		exit,
		func() core.LPoDPositionAction {
			v := exit
			v.Nonce = 2
			v.WithdrawAmount = a.Amount - part + 1
			return v
		}(),
	} {
		bad, err := core.BuildLPoDPositionTx(request, owner.Spend.Private, crypto.Scalar32{})
		if err != nil {
			t.Fatal(err)
		}
		draft := &core.Block{Header: core.BlockHeader{Height: block.Header.Height + 1, PrevHash: block.Hash(),
			Timestamp: block.Header.Timestamp + 1, ValidatorPub: priv.Public()}, Txs: []core.Transaction{*bad}}
		if _, _, _, err := e.prepareLPoD(draft, crypto.Address(e.cfg.RewardAddress)); err == nil {
			t.Fatal("signed partial replay or excess-principal withdrawal accepted")
		}
	}
	refund, err := core.BuildLPoDPayoutOutput(a.Beneficiary, part, block.Header.Height, block.Header.PrevHash, c.Digest())
	if err != nil {
		t.Fatal(err)
	}
	top := a
	top.Amount = part
	found := false
	for i, out := range block.Txs[0].Outputs {
		if out != refund {
			continue
		}
		top.SourceTx = block.Txs[0].Hash()
		top.SourceIndex = uint32(i)
		top.SourcePub = out.OneTimePub
		hs, err := crypto.ScanForOutput(owner.View.Private, a.Owner, out.TxPubKey, out.OneTimePub)
		if err != nil || hs == nil {
			t.Fatal("cannot discover immediately returned partial principal")
		}
		top.Blind, err = crypto.DeterministicPaymentBlind(*hs, part)
		if err != nil {
			t.Fatal(err)
		}
		sourcePrivate, err := crypto.AddScalars(owner.Spend.Private, *hs)
		if err != nil {
			t.Fatal(err)
		}
		top.PositionID = core.LPoDPositionID(a.Genesis, top.SourceTx, top.SourceIndex)
		topup, err := core.BuildLPoDPositionTx(top, owner.Spend.Private, sourcePrivate)
		if err != nil {
			t.Fatal(err)
		}
		acceptPositionBlock(t, e, priv, 15_000_000_000, *topup)
		if !e.utxos.IsSpent(topup.Inputs[0].KeyImage) {
			t.Fatal("topup failed to consume actual returned UTXO")
		}
		found = true
	}
	if !found {
		t.Fatal("partial principal was not paid as a real output")
	}
	after, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	n := new(big.Int).SetUint64(a.Amount - part)
	n.Mul(n, big.NewInt(9)) // remaining 90M vault is now in its proper lower tier
	n.Mul(n, big.NewInt(15_000_000_000))
	n.Add(n, new(big.Int).SetUint64(c.Positions[id].APRCarry))
	n.Div(n, new(big.Int).SetUint64(100*lpod.YearSeconds*1_000_000_000))
	if after.State.AccruedLiability-c.State.AccruedLiability != n.Uint64() ||
		after.PrincipalLocked != a.Amount || after.PrincipalDeposited != a.Amount+part || len(after.Positions) != 2 {
		t.Fatal("partial APR base, authenticated topup or live totals incorrect")
	}
	exit.WithdrawAmount = 0
	exit.Nonce = 2
	full, err := core.BuildLPoDPositionTx(exit, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	top.Action = core.LPoDWithdraw
	top.Nonce = 1
	closeTop, err := core.BuildLPoDPositionTx(top, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	finalBlock := acceptPositionBlock(t, e, priv, 3_000_000_000, *full, *closeTop)
	closed, err := db.LoadLPoDCheckpoint()
	if err != nil || closed.PrincipalLocked != 0 || closed.PrincipalReturned != a.Amount+part ||
		closed.PrincipalDeposited != closed.PrincipalReturned {
		t.Fatal("final exits did not return only the remaining original and additional principal")
	}
	expected, err := core.BuildLPoDPayoutOutput(a.Beneficiary, a.Amount+closed.State.AngelPaid-after.State.AngelPaid,
		finalBlock.Header.Height, finalBlock.Header.PrevHash, closed.Digest())
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, out := range finalBlock.Txs[0].Outputs {
		if out == expected {
			found = true
		}
	}
	if !found {
		t.Fatal("aggregated final output re-paid an earlier partial exit or omitted remaining principal")
	}
}

func TestLPoDTransitionToExistingTailSchedule(t *testing.T) {
	e, db, _, priv := lpodConsensusFixture(t)
	m := e.cfg.LPoDMigration
	m.ValidatorRemaining = 2 * lpod.Unit
	m.ReconciliationRoot = m.Root()
	sig, err := priv.Sign(m.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	m.Attestations = []store.LPoDAttestation{{Validator: priv.Public(), Signature: sig}}
	if err := db.StoreStakingPoolRemaining(m.ValidatorRemaining); err != nil {
		t.Fatal(err)
	}
	if err := e.tick(); err != nil {
		t.Fatal(err)
	}
	first, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if first.State.RewardInflow != 2*lpod.Unit || first.Allocation.ValidatorRemaining != 0 || first.Allocation.TailIssued != 0 {
		t.Fatal("last partial pool draw changed")
	}
	if err := e.tick(); err != nil {
		t.Fatal(err)
	}
	c, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if c.Allocation.TailIssued != lpod.Unit || c.State.RewardInflow != 3*lpod.Unit {
		t.Fatal("existing one-APRO tail halted or unaccounted")
	}
	total := m.HistoricalIssued + 1_000_000_000*lpod.Unit + c.Allocation.Remaining +
		c.Allocation.ValidatorRemaining + c.State.Balance + c.State.LeaderPaid + c.State.AngelPaid
	if total != 10_000_000_000*lpod.Unit+c.Allocation.TailIssued {
		t.Fatal("tail-era supply equation failed")
	}
}

func TestLPoDPositionRejectsForgedOwnershipAndChain(t *testing.T) {
	e, _, deposit, a, owner, _, _ := positionEngine(t)
	for _, name := range []string{"signature", "amount", "beneficiary", "genesis", "key_image"} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(deposit)
			var tx core.Transaction
			json.Unmarshal(raw, &tx)
			switch name {
			case "signature":
				tx.Signatures[0].SS[0][0] ^= 1
			case "key_image":
				tx.Inputs[0].KeyImage[0] ^= 1
			default:
				change := a
				if name == "amount" {
					change.Amount++
				}
				if name == "genesis" {
					change.Genesis[0] ^= 1
				}
				if name == "beneficiary" {
					keys, _ := crypto.GenerateWalletKeys()
					change.Beneficiary = crypto.AddressFromKeys(crypto.MainnetByte, keys)
				}
				tx.Extra, _ = json.Marshal(change)
			}
			if err := e.txVerifier.VerifyTx(&tx); err == nil {
				t.Fatal("forged position authenticated")
			}
		})
	}
	// A valid owner signature is insufficient without the existing position.
	a.Action = core.LPoDWithdraw
	a.Nonce = 1
	withdraw, err := core.BuildLPoDPositionTx(a, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	parent := e.chain.Tip()
	b := &core.Block{Header: core.BlockHeader{Height: 2, PrevHash: parent.Hash(), Timestamp: parent.Header.Timestamp + 3_000_000_000, ValidatorPub: e.cfg.MyKey.Public()}, Txs: []core.Transaction{*withdraw}}
	if _, _, _, err := e.prepareLPoD(b, crypto.Address(e.cfg.RewardAddress)); err == nil {
		t.Fatal("unknown principal withdrawn")
	}
}

func TestLPoDPositionsRestartRollbackAndWithdrawalReplay(t *testing.T) {
	e, db, deposit, a, owner, priv, path := positionEngine(t)
	depositBlock := acceptPositionBlock(t, e, priv, 3_000_000_000, *deposit)
	parentState, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	a.Action = core.LPoDWithdraw
	a.Nonce = 1
	withdraw, err := core.BuildLPoDPositionTx(a, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	block := acceptPositionBlock(t, e, priv, 3_000_000_000, *withdraw)
	if err := e.utxos.RollbackBlock(block); err != nil {
		t.Fatal(err)
	}
	if err := e.chain.RollbackLastBlock(block); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(depositBlock.Hash(), depositBlock.Header.Height); err != nil {
		t.Fatal(err)
	}
	for i := range block.Txs[0].Outputs {
		if e.utxos.Get(block.Txs[0].Hash(), uint32(i)) != nil {
			t.Fatal("rollback left spendable refund in memory")
		}
		u, err := db.GetUTXO(block.Txs[0].Hash(), uint32(i))
		if err != nil || u != nil {
			t.Fatal("rollback left refund in persistent UTXO index")
		}
	}
	rolled, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(parentState)
	got, _ := json.Marshal(rolled)
	if string(want) != string(got) {
		t.Fatal("rollback did not restore principal, APR carry and nonce exactly")
	}
	if err := e.handleIncomingBlock(block); err != nil {
		t.Fatalf("exact canonical replay failed: %v", err)
	}
	before, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if before.PrincipalReturned != a.Amount || before.PrincipalLocked != 0 {
		t.Fatal("canonical replay did not restore precisely one immediate refund")
	}
	want, _ = json.Marshal(before)
	db.Close()
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	got, _ = json.Marshal(after)
	if string(want) != string(got) {
		t.Fatal("restart changed canonical positions or liabilities")
	}
	chain := core.NewChain()
	utxos := core.NewUTXOSet()
	for h := uint64(0); h <= block.Header.Height; h++ {
		raw, err := reopened.GetRawBlockByHeight(h)
		if err != nil {
			t.Fatal(err)
		}
		var b core.Block
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		if h == 0 {
			err = chain.SetGenesis(&b)
		} else {
			err = chain.AddBlock(&b)
		}
		if err != nil {
			t.Fatal(err)
		}
		utxos.ApplyBlock(&b)
	}
	cfg := e.cfg
	cfg.Store = reopened
	cfg.OnCanonicalBlock = func(b *core.Block, p *avm.PreparedBlock) error {
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		return avm.CommitCanonicalBlock(reopened, b, raw, p)
	}
	restarted := NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), e.log)
	restarted.SetTxVerifier(core.NewTxVerifier(utxos), utxos)
	if !restarted.IsFinalizedHash(block.Header.Height, block.Hash()) {
		t.Fatal("restart lost the durable quorum finality certificate")
	}
	entry, found := restarted.cfg.Registry.GetEntry(priv.Public())
	if !found {
		t.Fatal("restart lost validator authorization state")
	}
	auth := core.StakeWithdrawalAuthorizationV2{Action: core.StakeWithdraw, PubKey: priv.Public(),
		Genesis: restarted.cfg.LPoDMigration.Genesis, Nonce: entry.StakeAuthNonce + 1,
		Generation: entry.StakeGeneration, ExpiryHeight: block.Header.Height + 10}
	auth.StakeRef = core.StakeReferenceV2(auth.Genesis, auth.PubKey, auth.Generation, entry.StakeNAPR)
	auth.Signature, err = priv.Sign(core.StakeWithdrawalSignMsgV2(auth))
	if err != nil {
		t.Fatal(err)
	}
	extra, err := core.EncodeStakeWithdrawalExtraV2(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.cfg.Registry.ValidateBlockStakeTxs([]core.Transaction{{
		Version: core.TxVersionStake, Extra: extra,
	}}, block.Header.Height+1); err != nil {
		t.Fatalf("restart could not validate a fresh v2 withdrawal: %v", err)
	}
	if err := restarted.txVerifier.VerifyTx(deposit); err == nil {
		t.Fatal("restart lost consumed principal key image")
	}
	draft := &core.Block{Header: core.BlockHeader{Height: block.Header.Height + 1, PrevHash: block.Hash(),
		Timestamp: block.Header.Timestamp + 3_000_000_000, ValidatorPub: priv.Public()}, Txs: []core.Transaction{*withdraw}}
	if _, _, _, err := restarted.prepareLPoD(draft, crypto.Address(cfg.RewardAddress)); err == nil {
		t.Fatal("withdrawal nonce replay survived restart")
	}
	acceptPositionBlock(t, restarted, priv, 3_000_000_000)
}

func TestLPoDPreservesSignedValidatorWithdrawalAndDraftProgress(t *testing.T) {
	e, db, _, a, owner, priv, _ := positionEngine(t)
	// A stale but correctly signed native request must not wedge production or
	// cause the unrelated, valid validator withdrawal to be dropped.
	a.Action = core.LPoDWithdraw
	a.Nonce = 1
	stale, err := core.BuildLPoDPositionTx(a, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pool.Add(*stale); err != nil {
		t.Fatal(err)
	}
	stake := lifecycleFullExit(t, e, priv, e.chain.Tip().Header.Height+10)
	if !stake.IsStake() || !stake.IsCoinbase() {
		t.Fatal("fixture must exercise historical zero-input stake classification")
	}
	if err := e.pool.Add(stake); err != nil {
		t.Fatal(err)
	}
	b, err := e.produceBlock(2, 2, e.chain.Tip())
	if err != nil {
		t.Fatalf("queued stake froze producer: %v", err)
	}
	if len(b.Txs) != 3 || !b.Txs[2].IsStake() {
		t.Fatal("valid validator withdrawal omitted or stale position stalled draft")
	}
	if err := e.handleIncomingBlock(b); err != nil {
		t.Fatalf("signed stake withdrawal rejected under LPoD: %v", err)
	}
	entry, found := e.cfg.Registry.GetEntry(priv.Public())
	if !found || entry.Status != core.ValidatorUnbonding || entry.StakeNAPR != 100_000*lpod.Unit ||
		entry.UnbondEndBlock != b.Header.Height+core.UnbondingBlocks {
		t.Fatal("LPoD bypassed existing validator withdrawal semantics")
	}
	c, err := db.LoadLPoDCheckpoint()
	if err != nil || c == nil || c.State.LastHeight != 2 {
		t.Fatalf("stake block did not atomically settle LPoD: %v", err)
	}
}
