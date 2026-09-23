// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
)

func lpodMigrationFixture(t *testing.T) (*DB, *LPoDMigration, crypto.ValidatorPrivKey, crypto.Address) {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, ValidatorPub: pub}}
	genesis.Header.MerkleRoot = core.MerkleRoot(nil)
	if err := genesis.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(genesis)
	if err := db.CommitRawBlockWithAVM(genesis.Hash(), 0, raw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	tx, err := core.BuildMintTx(address, 3*lpod.Unit, 1)
	if err != nil {
		t.Fatal(err)
	}
	b := &core.Block{Header: core.BlockHeader{Height: 1, PrevHash: genesis.Hash(), ValidatorPub: pub}, Txs: []core.Transaction{*tx}}
	b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
	if err := b.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(b)
	if err := db.CommitRawBlockWithAVM(b.Hash(), 1, raw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	_, spend, _, _ := crypto.DecodeAddress(address)
	blind, err := crypto.DeterministicMintBlindV2(spend, 3*lpod.Unit, 1)
	if err != nil {
		t.Fatal(err)
	}
	m := &LPoDMigration{Version: 1, Height: 2, Genesis: genesis.Hash(), Openings: []LPoDOpening{{Height: 1, Amount: 3 * lpod.Unit, Blind: blind}}}
	m.BodyRoot = LPoDBodyRootStep(LPoDBodyRootStep(crypto.HashBytes([]byte("aperod/lpod/historical-bodies/v1")), genesis), b)
	m.HistoricalIssued = 3 * lpod.Unit
	m.ValidatorRemaining = lpodValidatorRemaining(1)
	m.TrustedValidators = []crypto.ValidatorPubKey{pub}
	m.ReconciliationRoot = m.Root()
	signature, err := priv.Sign(m.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	m.Attestations = []LPoDAttestation{{Validator: pub, Signature: signature}}
	if err := db.StoreStakingPoolRemaining(lpodValidatorRemaining(1)); err != nil {
		t.Fatal(err)
	}
	return db, m, priv, address
}

func lpodActivationBlock(t *testing.T, db *DB, m *LPoDMigration, priv crypto.ValidatorPrivKey, address crypto.Address) (*core.Block, *LPoDSettlement, []byte) {
	t.Helper()
	parent, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	parentBlock, err := db.readLPoDCanonicalBlock(height)
	if err != nil {
		t.Fatal(err)
	}
	r := &LPoDSettlement{Parent: parent, Migration: m, PositionProtocol: true,
		Timestamp: parentBlock.Header.Timestamp + 3_000_000_000, Proposer: priv.Public().Hex(), Leader: address,
		Stake: map[string]LPoDValidatorStake{priv.Public().Hex(): {Amount: 100_000 * lpod.Unit, Active: true}}}
	c, pay, err := db.PreviewLPoD(height+1, r)
	if err != nil {
		t.Fatal(err)
	}
	reward, err := db.PayoutLPoD(height+1, r, c, pay)
	if err != nil {
		t.Fatal(err)
	}
	b := &core.Block{Header: core.BlockHeader{Height: height + 1, PrevHash: parent, ValidatorPub: priv.Public(), Timestamp: r.Timestamp},
		Txs: []core.Transaction{reward, core.LPoDCheckpointTx(c.Digest())}}
	b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
	if err := b.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return b, r, raw
}

func TestLPoDRealMigrationAndPayout(t *testing.T) {
	db, m, priv, address := lpodMigrationFixture(t)
	issued, parent, err := db.VerifyLPoDMigration(m)
	if err != nil || issued != 3*lpod.Unit {
		t.Fatalf("reconciliation: %d %v", issued, err)
	}
	b, r, raw := lpodActivationBlock(t, db, m, priv, address)
	if err := db.CommitRawBlockWithAVM(b.Hash(), 2, raw, nil, crypto.Hash32{}, r); err != nil {
		t.Fatal(err)
	}
	c, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if c.Allocation.Remaining != 9_000_000_000*lpod.Unit-m.ValidatorRemaining-issued-lpod.InitialNAPRO ||
		c.State.Balance != lpod.InitialNAPRO+276_000_000 || c.State.LeaderPaid != 24_000_000 {
		t.Fatalf("allocation or real leader reward not conserved: %+v", c)
	}
	if c.Allocation.FundingBlock != b.Hash() {
		t.Fatal("missing canonical funding block")
	}
	if err := db.CommitRawBlockWithAVM(b.Hash(), 2, raw, nil, crypto.Hash32{}, r); err == nil {
		t.Fatal("duplicate funding accepted")
	}
	if err := db.CheckLPoDConfig(nil); err == nil {
		t.Fatal("disabled funded config accepted")
	}
	changed := *m
	changed.Height++
	changed.ReconciliationRoot = changed.Root()
	if err := db.CheckLPoDConfig(&changed); err == nil {
		t.Fatal("changed fork accepted")
	}
	if err := db.PutTip(parent, 1); err != nil {
		t.Fatal(err)
	}
	if cp, err := db.LoadLPoDCheckpoint(); err != nil || cp != nil {
		t.Fatal("rollback retained funding")
	}
	if err := db.CommitRawBlockWithAVM(b.Hash(), 2, raw, nil, crypto.Hash32{}, r); err != nil {
		t.Fatalf("replay: %v", err)
	}
	next, request, nextRaw := lpodActivationBlock(t, db, nil, priv, address)
	if err := db.CommitRawBlockWithAVM(next.Hash(), 3, nextRaw, nil, crypto.Hash32{}, request); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(b.Hash(), 2); err != nil {
		t.Fatal(err)
	}
	cp, err := db.LoadLPoDCheckpoint()
	if err != nil || cp.State != c.State {
		t.Fatal("rollback did not restore all accounting")
	}
}

func TestLPoDMigrationRejectsUnprovenHistory(t *testing.T) {
	for _, name := range []string{"missing", "wrong_amount", "wrong_genesis", "duplicate", "height", "root"} {
		t.Run(name, func(t *testing.T) {
			db, m, _, _ := lpodMigrationFixture(t)
			switch name {
			case "missing":
				m.Openings = nil
			case "wrong_amount":
				m.Openings[0].Amount++
			case "wrong_genesis":
				m.Genesis[0] ^= 1
			case "duplicate":
				m.Openings = append(m.Openings, m.Openings[0])
			case "height":
				m.Height++
			case "root":
				m.ReconciliationRoot[0] ^= 1
			}
			if name != "root" {
				m.ReconciliationRoot = m.Root()
			}
			if _, _, err := db.VerifyLPoDMigration(m); err == nil {
				t.Fatal("unproven issuance accepted")
			}
		})
	}
}

func TestLPoDPayoutFailureIsAtomic(t *testing.T) {
	db, m, priv, address := lpodMigrationFixture(t)
	b, r, _ := lpodActivationBlock(t, db, m, priv, address)
	// A proposer cannot keep the full old reward in addition to the pool credit.
	full, err := core.BuildAuthorizedRewardTx(address, 3*lpod.Unit, 2, b.Header.PrevHash, priv)
	if err != nil {
		t.Fatal(err)
	}
	b.Txs[0] = *full
	b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
	b.Header.Sign(priv)
	raw, _ := json.Marshal(b)
	if err := db.CommitRawBlockWithAVM(b.Hash(), 2, raw, []AVMWrite{{Key: []byte("bad"), Value: []byte("bad")}}, crypto.Hash32{}, r); err == nil {
		t.Fatal("extra reward minted")
	}
	if _, height, _ := db.GetTip(); height != 1 {
		t.Fatal("partial tip committed")
	}
	if c, _ := db.LoadLPoDCheckpoint(); c != nil {
		t.Fatal("partial pool funded")
	}
	if err := db.CheckLPoDConfig(nil); err != nil {
		t.Fatal("failed block bound activation")
	}
	if _, found, _ := db.GetAVMState([]byte("bad")); found {
		t.Fatal("partial AVM effects")
	}
}

func TestLPoDReconciliationRejectsEqualHashHistoricalBodySubstitution(t *testing.T) {
	db, m, priv, _ := lpodMigrationFixture(t)
	genesis, err := db.readLPoDCanonicalBlock(0)
	if err != nil {
		t.Fatal(err)
	}
	original, err := db.readLPoDCanonicalBlock(1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(original)
	var substituted core.Block
	json.Unmarshal(raw, &substituted)
	tx := &substituted.Txs[0]
	output := tx.Outputs[0]
	tx.Extra = append(tx.Extra, output.OneTimePub[:]...)
	tx.Extra = append(tx.Extra, output.AmountCommit[:]...)
	tx.Extra = append(tx.Extra, output.TxPubKey[:]...)
	tx.Extra = append(tx.Extra, output.EncAmount[:]...)
	tx.Outputs = nil
	if tx.Hash() != original.Txs[0].Hash() || substituted.Hash() != original.Hash() {
		t.Fatal("fixture does not demonstrate historical unframed-hash ambiguity")
	}
	read := func(h uint64) (*core.Block, error) {
		if h == 0 {
			return genesis, nil
		}
		return &substituted, nil
	}
	if _, _, err := ReconcileLPoD(m, read); err == nil {
		t.Fatal("same-hash alternate body passed attested body root")
	}
	// An attacker can recompute an internally consistent replacement witness,
	// but cannot authorize its changed unambiguous issuance/body commitment.
	forged := *m
	forged.Openings = nil
	forged.HistoricalIssued = 0
	forged.BodyRoot = LPoDBodyRootStep(LPoDBodyRootStep(crypto.HashBytes([]byte("aperod/lpod/historical-bodies/v1")), genesis), &substituted)
	forged.ReconciliationRoot = forged.Root()
	if _, _, err := ReconcileLPoD(&forged, read); err == nil {
		t.Fatal("self-hashed replacement witness substituted for quorum authorization")
	}
	// Self-selected authorities are not a witness-file field and no signature
	// may be silently ignored or counted twice.
	forged = *m
	forged.Attestations = append(forged.Attestations, forged.Attestations[0])
	if _, _, err := db.VerifyLPoDMigration(&forged); err == nil {
		t.Fatal("duplicate quorum counted")
	}
	_ = priv
}

func TestLPoDValidatorBudgetIsAttestedAndNotDoubleCharged(t *testing.T) {
	db, m, priv, address := lpodMigrationFixture(t)
	b, r, raw := lpodActivationBlock(t, db, m, priv, address)
	if err := db.CommitRawBlockWithAVM(b.Hash(), 2, raw, nil, crypto.Hash32{}, r); err != nil {
		t.Fatal(err)
	}
	c, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	// All issued coins plus the protected development reserve, unspent public
	// budget, residual validator budget, LPoD balance and actual payments = 10B.
	total := m.HistoricalIssued + 1_000_000_000*lpod.Unit + c.Allocation.Remaining +
		c.Allocation.ValidatorRemaining + c.State.Balance + c.State.LeaderPaid + c.State.AngelPaid
	if total != 10_000_000_000*lpod.Unit {
		t.Fatalf("historical validator rewards double-charged or supply changed: %d", total)
	}
}
