// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/json"
	"fmt"
	"reflect"
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
	m := &LPoDMigration{Version: 1, PositionLifecycleVersion: 1, Height: 2, Genesis: genesis.Hash(), Openings: []LPoDOpening{{Height: 1, Amount: 3 * lpod.Unit, Blind: blind}}}
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

func trustedLPoDV2Fixture(t *testing.T) (*LPoDMigration, crypto.ValidatorPrivKey) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	m := &LPoDMigration{
		Version: 2, PositionLifecycleVersion: 1, Height: LPoDV2ActivationHeight,
		Genesis:              crypto.HashBytes([]byte("test genesis")),
		ParentHash:           crypto.HashBytes([]byte("canonical immediate parent")),
		SnapshotRoot:         crypto.HashBytes([]byte("attested opaque snapshot commitment")),
		TrustAssumption:      LPoDV2TrustAssumption,
		HistoricalIssued:     1_000_000_000 * lpod.Unit,
		HistoricalSaleRemain: 4_000_000_000 * lpod.Unit,
		ValidatorRemaining:   2_000_000_000 * lpod.Unit,
		TrustedValidators:    []crypto.ValidatorPubKey{pub},
	}
	m.ReconciliationRoot = m.Root()
	sig, err := priv.Sign(m.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	m.Attestations = []LPoDAttestation{{Validator: pub, Signature: sig}}
	return m, priv
}

func trustedLPoDV3Fixture(t *testing.T) (*LPoDMigration, crypto.ValidatorPrivKey) {
	t.Helper()
	m, priv := trustedLPoDV2Fixture(t)
	m.Version = 3
	m.TrustAssumption = LPoDV3TrustAssumption
	m.HistoricalIssued = 0
	m.HistoricalSaleRemain = 0
	m.NominalEligible = 6_000_000_000 * lpod.Unit
	setTrustedLPoDV3Snapshot(m)
	m.ReconciliationRoot = m.Root()
	sig, err := priv.Sign(m.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	m.Attestations = []LPoDAttestation{{Validator: priv.Public(), Signature: sig}}
	return m, priv
}

func TestLPoDTrustedNominalV3AuthorizationAndDomains(t *testing.T) {
	m, _ := trustedLPoDV3Fixture(t)
	if err := m.VerifyAuthorization(); err != nil {
		t.Fatalf("valid nominal reclassification rejected: %v", err)
	}
	v1 := &LPoDMigration{Version: 1, PositionLifecycleVersion: 1, Height: 1}
	v2, _ := trustedLPoDV2Fixture(t)
	if m.Root() == v1.Root() || m.Root() == v2.Root() ||
		m.AttestationMessage() == v1.AttestationMessage() ||
		m.AttestationMessage() == v2.AttestationMessage() {
		t.Fatal("version 3 reused a prior version's root or authorization domain")
	}

	changed := *m
	changed.NominalEligible++
	changed.ReconciliationRoot = changed.Root()
	if err := changed.VerifyAuthorization(); err == nil {
		t.Fatal("changed nominal allocation retained the old signature")
	}
	changedSnapshot := *m
	tamperedSnapshot := *m.NominalSnapshot
	changedSnapshot.NominalSnapshot = &tamperedSnapshot
	changedSnapshot.NominalSnapshot.NominalCirculatingBefore--
	if err := changedSnapshot.VerifyAuthorization(); err == nil {
		t.Fatal("tampered nominal snapshot retained the old commitment and signature")
	}

	// A different authority ordering commits the same canonical set.
	ordered := *m
	_, extraAuthority, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	ordered.TrustedValidators = append(ordered.TrustedValidators, extraAuthority)
	reordered := ordered
	reordered.TrustedValidators = []crypto.ValidatorPubKey{ordered.TrustedValidators[1], ordered.TrustedValidators[0]}
	if ordered.Root() != reordered.Root() {
		t.Fatal("v3 root depends on authority-list order instead of canonical set")
	}
}

func setTrustedLPoDV3Snapshot(m *LPoDMigration) {
	snapshot := LPoDNominalSnapshot{
		Basis:                          LPoDV3NominalBasis,
		NominalCirculatingBefore:       6_000_000_000 * lpod.Unit,
		EligibleNominal:                m.NominalEligible,
		PreexistingGuardianReservation: 0,
		ValidatorRemaining:             m.ValidatorRemaining,
		EligibilityRule:                LPoDV3EligibilityRule,
	}
	m.NominalSnapshot = &snapshot
	m.SnapshotRoot = snapshot.Root()
}

func TestLPoDTrustedNominalV3RequiresStrictAuthorityQuorum(t *testing.T) {
	m, _ := trustedLPoDV3Fixture(t)
	m.TrustedValidators = nil
	privates := make([]crypto.ValidatorPrivKey, 4)
	for i := range privates {
		priv, pub, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
		privates[i] = priv
		m.TrustedValidators = append(m.TrustedValidators, pub)
	}
	m.ReconciliationRoot = m.Root()
	m.Attestations = nil
	for i := 0; i < 3; i++ {
		sig, err := privates[i].Sign(m.AttestationMessage())
		if err != nil {
			t.Fatal(err)
		}
		m.Attestations = append(m.Attestations, LPoDAttestation{
			Validator: privates[i].Public(),
			Signature: sig,
		})
	}
	if err := m.VerifyAuthorization(); err != nil {
		t.Fatalf("strict greater-than-two-thirds quorum rejected: %v", err)
	}
	m.Attestations = m.Attestations[:2]
	if err := m.VerifyAuthorization(); err == nil {
		t.Fatal("exactly half of the trusted authorities accepted")
	}
}

func TestLPoDTrustedNominalV3FailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*LPoDMigration)
	}{
		{"below funding minimum", func(m *LPoDMigration) { m.NominalEligible = lpod.InitialNAPRO - 1 }},
		{"above public nominal cap", func(m *LPoDMigration) { m.NominalEligible = LPoDV3NominalAllocationMaxNAPRO + 1 }},
		{"missing snapshot", func(m *LPoDMigration) { m.SnapshotRoot = crypto.Hash32{} }},
		{"missing manifest", func(m *LPoDMigration) { m.NominalSnapshot = nil }},
		{"nominal circulation below eligible", func(m *LPoDMigration) { m.NominalSnapshot.NominalCirculatingBefore = m.NominalEligible - 1 }},
		{"nominal circulation above cap", func(m *LPoDMigration) {
			m.NominalSnapshot.NominalCirculatingBefore = LPoDV3NominalCirculationMaxNAPRO + 1
		}},
		{"guardian overlap", func(m *LPoDMigration) { m.NominalSnapshot.PreexistingGuardianReservation = 1 }},
		{"validator declaration mismatch", func(m *LPoDMigration) { m.NominalSnapshot.ValidatorRemaining-- }},
		{"wrong basis", func(m *LPoDMigration) { m.NominalSnapshot.Basis = "historical_issuance_proof" }},
		{"wrong eligibility rule", func(m *LPoDMigration) { m.NominalSnapshot.EligibilityRule = "wallet_balance" }},
		{"missing parent", func(m *LPoDMigration) { m.ParentHash = crypto.Hash32{} }},
		{"wrong trust assumption", func(m *LPoDMigration) { m.TrustAssumption = LPoDV2TrustAssumption }},
		{"historical issuance claim", func(m *LPoDMigration) { m.HistoricalIssued = lpod.Unit }},
		{"historical sale claim", func(m *LPoDMigration) { m.HistoricalSaleRemain = lpod.Unit }},
		{"history openings", func(m *LPoDMigration) { m.Openings = []LPoDOpening{{Amount: lpod.Unit}} }},
		{"history body root", func(m *LPoDMigration) { m.BodyRoot = crypto.HashBytes([]byte("historical body")) }},
		{"zero authority", func(m *LPoDMigration) {
			m.TrustedValidators = []crypto.ValidatorPubKey{make(crypto.ValidatorPubKey, 32)}
		}},
		{"duplicate authority", func(m *LPoDMigration) {
			m.TrustedValidators = append(m.TrustedValidators, m.TrustedValidators[0])
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, priv := trustedLPoDV3Fixture(t)
			tt.mutate(m)
			m.ReconciliationRoot = m.Root()
			sig, err := priv.Sign(m.AttestationMessage())
			if err != nil {
				t.Fatal(err)
			}
			m.Attestations = []LPoDAttestation{{Validator: priv.Public(), Signature: sig}}
			if err := m.VerifyAuthorization(); err == nil {
				t.Fatal("invalid nominal reclassification accepted")
			}
		})
	}

	m, _ := trustedLPoDV3Fixture(t)
	m.Attestations = nil
	if err := m.VerifyAuthorization(); err == nil {
		t.Fatal("nominal reclassification without an authority quorum accepted")
	}
}

func trustedLPoDV3FundingFixture(t *testing.T) (*DB, string, *LPoDMigration, crypto.ValidatorPrivKey, crypto.Address) {
	t.Helper()
	db, path, m, priv, address := trustedLPoDV2FundingFixture(t)
	m.Version = 3
	m.TrustAssumption = LPoDV3TrustAssumption
	m.HistoricalIssued = 0
	m.HistoricalSaleRemain = 0
	m.NominalEligible = 6_000_000_000 * lpod.Unit
	setTrustedLPoDV3Snapshot(m)
	m.ReconciliationRoot = m.Root()
	sig, err := priv.Sign(m.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	m.Attestations = []LPoDAttestation{{Validator: priv.Public(), Signature: sig}}
	return db, path, m, priv, address
}

func TestLPoDTrustedNominalV3FundingRestartRollbackAndUTXOConservation(t *testing.T) {
	db, path, m, priv, address := trustedLPoDV3FundingFixture(t)
	sentinelHash := crypto.HashBytes([]byte("existing user UTXO"))
	sentinel := &StoredUTXO{TxHash: sentinelHash, OutputIndex: 7, BlockHeight: 1}
	sentinel.OneTimePub[0] = 1
	if err := db.PutUTXO(sentinelHash, sentinel.OutputIndex, sentinel); err != nil {
		t.Fatal(err)
	}
	beforeUTXO, err := db.GetUTXO(sentinelHash, sentinel.OutputIndex)
	if err != nil || beforeUTXO == nil {
		t.Fatalf("sentinel user UTXO setup failed: %v", err)
	}

	parent, _, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	funding, settlement, raw := lpodActivationBlock(t, db, m, priv, address)
	if err := db.CommitRawBlockWithAVM(funding.Hash(), LPoDV2ActivationHeight, raw, nil, crypto.Hash32{}, settlement); err != nil {
		t.Fatalf("valid nominal reclassification failed: %v", err)
	}
	checkpoint, err := db.LoadLPoDCheckpoint()
	if err != nil || checkpoint == nil || checkpoint.Allocation == nil {
		t.Fatalf("funded v3 checkpoint unavailable: %v", err)
	}
	allocation := checkpoint.Allocation
	if allocation.Version != 3 || allocation.FundingHeight != LPoDV2ActivationHeight ||
		allocation.FundingBlock != funding.Hash() || allocation.FundingParent != parent ||
		allocation.ReconciliationRoot != m.ReconciliationRoot ||
		allocation.NominalEligible != m.NominalEligible ||
		allocation.NominalSnapshot == nil ||
		!reflect.DeepEqual(allocation.NominalSnapshot, m.NominalSnapshot) ||
		allocation.HistoricalIssued != 0 || allocation.DeclaredSaleRemaining != 0 ||
		allocation.Remaining != 5_000_000_000*lpod.Unit ||
		checkpoint.State.FundingDebit != lpod.InitialNAPRO ||
		checkpoint.State.Balance != lpod.InitialNAPRO+checkpoint.State.SurplusInflow {
		t.Fatalf("unexpected v3 reclassification ledger: %+v state=%+v", allocation, checkpoint.State)
	}
	afterUTXO, err := db.GetUTXO(sentinelHash, sentinel.OutputIndex)
	if err != nil || !reflect.DeepEqual(beforeUTXO, afterUTXO) {
		t.Fatalf("nominal reclassification modified a user UTXO: before=%+v after=%+v err=%v", beforeUTXO, afterUTXO, err)
	}
	binding, err := db.get([]byte("lpod/activation/v3"))
	root := m.Root()
	if err != nil || string(binding) != string(root[:]) {
		t.Fatalf("durable v3 activation binding missing or wrong: %v", err)
	}
	if err := db.CheckLPoDConfig(nil); err == nil {
		t.Fatal("disabling funded v3 checkpoint accepted")
	}
	changed := *m
	changed.NominalEligible++
	changed.ReconciliationRoot = changed.Root()
	if err := db.CheckLPoDConfig(&changed); err == nil {
		t.Fatal("changed v3 checkpoint accepted")
	}
	downgrade := *m
	downgrade.Version = 2
	if err := db.CheckLPoDConfig(&downgrade); err == nil {
		t.Fatal("downgrade from v3 to v2 accepted")
	}

	next, nextSettlement, nextRaw := lpodActivationBlock(t, db, nil, priv, address)
	if err := db.CommitRawBlockWithAVM(next.Hash(), LPoDV2ActivationHeight+1, nextRaw, nil, crypto.Hash32{}, nextSettlement); err != nil {
		t.Fatalf("later checkpoint rejected v3 funding record: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CheckLPoDConfig(m); err != nil {
		t.Fatalf("same v3 binding rejected after restart: %v", err)
	}
	restarted, err := db.LoadLPoDCheckpoint()
	if err != nil || restarted == nil || restarted.State.LastHeight != LPoDV2ActivationHeight+1 ||
		restarted.Allocation.NominalEligible != m.NominalEligible ||
		restarted.Allocation.ReconciliationRoot != m.Root() {
		t.Fatalf("v3 checkpoint not durable after restart: %v", err)
	}
	if err := db.PutTip(parent, LPoDV2ActivationHeight-1); err != nil {
		t.Fatalf("rollback to exact v3 funding parent failed: %v", err)
	}
	if cp, err := db.LoadLPoDCheckpoint(); err != nil || cp != nil {
		t.Fatalf("rollback retained v3 funding: checkpoint=%v err=%v", cp, err)
	}
	if pool, found, err := db.LoadStakingPoolRemaining(); err != nil || !found || pool != m.ValidatorRemaining {
		t.Fatalf("rollback changed existing validator pool: pool=%d found=%v err=%v", pool, found, err)
	}
	if err := db.CommitRawBlockWithAVM(funding.Hash(), LPoDV2ActivationHeight, raw, nil, crypto.Hash32{}, settlement); err != nil {
		t.Fatalf("same signed v3 activation replay after rollback failed: %v", err)
	}
	replayed, err := db.LoadLPoDCheckpoint()
	if err != nil || replayed == nil || replayed.Allocation.NominalEligible != m.NominalEligible {
		t.Fatalf("replayed v3 checkpoint missing: %v", err)
	}
}

func TestLPoDTrustedNominalV3FundingRequiresMatchingPool(t *testing.T) {
	db, _, m, priv, address := trustedLPoDV3FundingFixture(t)
	defer db.Close()
	if err := db.StoreStakingPoolRemaining(m.ValidatorRemaining - 1); err != nil {
		t.Fatal(err)
	}
	parent, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	request := &LPoDSettlement{
		Parent: parent, Migration: m, PositionProtocol: true,
		Proposer: priv.Public().Hex(), Leader: address,
		Stake: map[string]LPoDValidatorStake{priv.Public().Hex(): {Amount: 100_000 * lpod.Unit, Active: true}},
	}
	if _, _, err := db.PreviewLPoD(height+1, request); err == nil {
		t.Fatal("v3 reclassification funded without the matching preexisting validator pool")
	}
	if checkpoint, err := db.LoadLPoDCheckpoint(); err != nil || checkpoint != nil {
		t.Fatalf("failed v3 funding wrote an allocation checkpoint: %v", err)
	}
}

func TestLPoDTrustedNominalV3RejectsConflictingMigrationBinding(t *testing.T) {
	db, _, m, _, _ := trustedLPoDV3FundingFixture(t)
	defer db.Close()
	if err := db.put([]byte("lpod/activation/v2"), []byte("preexisting-v2-activation")); err != nil {
		t.Fatal(err)
	}
	parent, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	request := &LPoDSettlement{Parent: parent, Migration: m, PositionProtocol: true}
	if _, _, err := db.PreviewLPoD(height+1, request); err == nil {
		t.Fatal("version 3 activation ignored a durable version 2 binding")
	}
	if checkpoint, err := db.LoadLPoDCheckpoint(); err != nil || checkpoint != nil {
		t.Fatalf("cross-version rejection wrote a funded checkpoint: %v", err)
	}
}

func TestLPoDTrustedCheckpointV2AuthorizationAndDomains(t *testing.T) {
	m, _ := trustedLPoDV2Fixture(t)
	if err := m.VerifyAuthorization(); err != nil {
		t.Fatalf("valid explicitly trusted checkpoint rejected: %v", err)
	}
	v1 := &LPoDMigration{Version: 1, PositionLifecycleVersion: 1, Height: 1}
	if m.Root() == v1.Root() || m.AttestationMessage() == v1.AttestationMessage() {
		t.Fatal("version 2 reused version 1 root or authorization domain")
	}
	ordered := *m
	_, extraAuthority, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	ordered.TrustedValidators = append(ordered.TrustedValidators, extraAuthority)
	reordered := ordered
	reordered.TrustedValidators = []crypto.ValidatorPubKey{ordered.TrustedValidators[1], ordered.TrustedValidators[0]}
	if ordered.Root() != reordered.Root() {
		t.Fatal("v2 root depends on authority-list order instead of canonical set")
	}
	if ordered.Root() == m.Root() {
		t.Fatal("v2 root does not bind its trusted authority set")
	}

	tampered := *m
	tampered.SnapshotRoot[0] ^= 1
	tampered.ReconciliationRoot = tampered.Root()
	if err := tampered.VerifyAuthorization(); err == nil {
		t.Fatal("changed snapshot commitment retained the original signature")
	}
}

func TestLPoDTrustedCheckpointV2AllowsMultipleSignedHeights(t *testing.T) {
	for _, height := range []uint64{2, 17, LPoDV2ActivationHeight + 91} {
		t.Run(fmt.Sprint(height), func(t *testing.T) {
			m, priv := trustedLPoDV2Fixture(t)
			m.Height = height
			m.ReconciliationRoot = m.Root()
			sig, err := priv.Sign(m.AttestationMessage())
			if err != nil {
				t.Fatal(err)
			}
			m.Attestations = []LPoDAttestation{{Validator: priv.Public(), Signature: sig}}
			if err := m.VerifyAuthorization(); err != nil {
				t.Fatalf("valid checkpoint at signed height %d rejected: %v", height, err)
			}
		})
	}
}

func TestLPoDV1RejectsVersion2OnlyFields(t *testing.T) {
	db, m, _, _ := lpodMigrationFixture(t)
	withV2Data := *m
	withV2Data.SnapshotRoot = crypto.HashBytes([]byte("unbound legacy extension"))
	if err := withV2Data.VerifyAuthorization(); err == nil {
		t.Fatal("v1 authorization accepted a v2-only field")
	}
	if _, _, err := db.VerifyLPoDMigration(&withV2Data); err == nil {
		t.Fatal("v1 preview/funding path accepted a v2-only field")
	}
}

func TestLPoDV1CheckpointDigestKeepsLegacyEncoding(t *testing.T) {
	cp := LPoDCheckpoint{
		State:   lpod.State{FundingDebit: lpod.InitialNAPRO, Balance: lpod.InitialNAPRO, LastHeight: 4},
		Carries: map[string]lpod.Carry{"vault": {APR: 7, Leader: 3}},
		Allocation: &LPoDAllocation{
			Version: 1, PositionLifecycleVersion: 1, Genesis: crypto.HashBytes([]byte("g")),
			FundingHeight: 4, FundingBlock: crypto.HashBytes([]byte("funding")),
			ReconciliationRoot: crypto.HashBytes([]byte("reconciliation")),
			HistoricalIssued:   13, Remaining: 17, ValidatorRemaining: 19,
			InitialValidatorRemaining: 23, TailIssued: 29,
		},
		Positions:          map[string]LPoDPosition{},
		PrincipalDeposited: 31, PrincipalLocked: 37, PrincipalReturned: 41,
	}
	allocation := cp.Allocation
	type oldAllocation struct {
		Version                   uint8         `json:"version"`
		PositionLifecycleVersion  uint8         `json:"position_lifecycle_version"`
		Genesis                   crypto.Hash32 `json:"genesis"`
		FundingHeight             uint64        `json:"funding_height"`
		FundingBlock              crypto.Hash32 `json:"funding_block"`
		ReconciliationRoot        crypto.Hash32 `json:"reconciliation_root"`
		HistoricalIssued          uint64        `json:"historical_issued_napro,string"`
		Remaining                 uint64        `json:"remaining_napro,string"`
		ValidatorRemaining        uint64        `json:"validator_remaining_napro,string"`
		InitialValidatorRemaining uint64        `json:"initial_validator_remaining_napro,string"`
		TailIssued                uint64        `json:"tail_issued_napro,string"`
	}
	type oldCheckpoint struct {
		State              lpod.State              `json:"state"`
		Carries            map[string]lpod.Carry   `json:"carries"`
		Allocation         *oldAllocation          `json:"allocation,omitempty"`
		Positions          map[string]LPoDPosition `json:"positions,omitempty"`
		PrincipalDeposited uint64                  `json:"principal_deposited_napro,string"`
		PrincipalLocked    uint64                  `json:"principal_locked_napro,string"`
		PrincipalReturned  uint64                  `json:"principal_returned_napro,string"`
	}
	legacy := oldCheckpoint{
		State: cp.State, Carries: cp.Carries, Positions: cp.Positions,
		PrincipalDeposited: cp.PrincipalDeposited, PrincipalLocked: cp.PrincipalLocked,
		PrincipalReturned: cp.PrincipalReturned,
		Allocation: &oldAllocation{
			Version: allocation.Version, PositionLifecycleVersion: allocation.PositionLifecycleVersion,
			Genesis: allocation.Genesis, FundingHeight: allocation.FundingHeight,
			ReconciliationRoot: allocation.ReconciliationRoot, HistoricalIssued: allocation.HistoricalIssued,
			Remaining: allocation.Remaining, ValidatorRemaining: allocation.ValidatorRemaining,
			InitialValidatorRemaining: allocation.InitialValidatorRemaining, TailIssued: allocation.TailIssued,
		},
	}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	expected := crypto.HashBytes([]byte("aperod/lpod/checkpoint/v1"), encoded)
	if got := cp.Digest(); got != expected {
		t.Fatalf("v1 checkpoint digest changed from its legacy encoding: got %x expected %x", got, expected)
	}
}

func TestLPoDTrustedCheckpointV2FailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*LPoDMigration)
	}{
		{"wrong height", func(m *LPoDMigration) { m.Height++ }},
		{"missing snapshot root", func(m *LPoDMigration) { m.SnapshotRoot = crypto.Hash32{} }},
		{"missing parent", func(m *LPoDMigration) { m.ParentHash = crypto.Hash32{} }},
		{"wrong trust assumption", func(m *LPoDMigration) { m.TrustAssumption = "PROOF" }},
		{"insufficient sale allocation", func(m *LPoDMigration) { m.HistoricalSaleRemain = lpod.InitialNAPRO - 1 }},
		{"oversubscribed allocations", func(m *LPoDMigration) { m.HistoricalSaleRemain = 7_000_000_001 * lpod.Unit }},
		{"zero authority", func(m *LPoDMigration) {
			m.TrustedValidators = []crypto.ValidatorPubKey{make(crypto.ValidatorPubKey, 32)}
		}},
		{"duplicate authority", func(m *LPoDMigration) {
			m.TrustedValidators = append(m.TrustedValidators, m.TrustedValidators[0])
		}},
		{"v1 proof fields collide", func(m *LPoDMigration) {
			m.Openings = []LPoDOpening{{Amount: lpod.Unit}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := trustedLPoDV2Fixture(t)
			tt.mutate(m)
			m.ReconciliationRoot = m.Root()
			if err := m.VerifyAuthorization(); err == nil {
				t.Fatal("invalid trusted checkpoint accepted")
			}
		})
	}
}

func TestLPoDTrustedCheckpointV2RequiresExactCanonicalParentAndGenesis(t *testing.T) {
	db, _, _, _ := lpodMigrationFixture(t)
	m, priv := trustedLPoDV2Fixture(t)
	genesis, err := db.readLPoDCanonicalBlock(0)
	if err != nil {
		t.Fatal(err)
	}
	tip, _, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	m.Genesis = genesis.Hash()
	m.ParentHash = tip
	m.ReconciliationRoot = m.Root()
	signature, err := priv.Sign(m.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	m.Attestations = []LPoDAttestation{{Validator: priv.Public(), Signature: signature}}
	if _, _, err := db.VerifyLPoDMigration(m); err == nil {
		t.Fatal("trusted checkpoint accepted a non-activation-height canonical tip")
	}
}

func TestLPoDTrustedCheckpointV2AcceptsSignedPreOracleGenesisOnly(t *testing.T) {
	db, _, m, priv, _ := trustedLPoDV2FundingFixture(t)
	genesis, err := db.readLPoDCanonicalBlock(0)
	if err != nil {
		t.Fatal(err)
	}
	height, timestamp, round := make([]byte, 8), make([]byte, 8), make([]byte, 4)
	h := &genesis.Header
	legacyHash := crypto.HashBytes(height, h.PrevHash[:], h.MerkleRoot[:],
		timestamp, round, h.ValidatorPub)
	sig, err := priv.Sign(legacyHash)
	if err != nil {
		t.Fatal(err)
	}
	h.Signature = sig
	raw, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(legacyHash, 0, raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.VerifyLPoDMigration(m); err != nil {
		t.Fatalf("authentic pre-oracle genesis rejected: %v", err)
	}
	if _, err := db.readLPoDCanonicalBlock(0); err == nil {
		t.Fatal("historical genesis exception leaked into the v1 body verifier")
	}

	h.Signature[0] ^= 1
	tampered, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(legacyHash, 0, tampered); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.VerifyLPoDMigration(m); err == nil {
		t.Fatal("unsigned historical genesis accepted")
	}
}

func trustedLPoDV2FundingFixture(t *testing.T) (*DB, string, *LPoDMigration, crypto.ValidatorPrivKey, crypto.Address) {
	t.Helper()
	path := t.TempDir()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
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
	genesisRaw, _ := json.Marshal(genesis)
	if err := db.CommitRawBlockWithAVM(genesis.Hash(), 0, genesisRaw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	parent := &core.Block{Header: core.BlockHeader{
		Height: LPoDV2ActivationHeight - 1, PrevHash: genesis.Hash(), ValidatorPub: pub, Timestamp: 1,
	}}
	parent.Header.MerkleRoot = core.MerkleRoot(nil)
	if err := parent.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	parentRaw, _ := json.Marshal(parent)
	if err := db.PutRawBlock(parent.Hash(), parent.Header.Height, parentRaw); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(parent.Hash(), parent.Header.Height); err != nil {
		t.Fatal(err)
	}
	if err := db.StoreStakingPoolRemaining(2_000_000_000 * lpod.Unit); err != nil {
		t.Fatal(err)
	}
	m := &LPoDMigration{
		Version: 2, PositionLifecycleVersion: 1, Height: LPoDV2ActivationHeight,
		Genesis: genesis.Hash(), ParentHash: parent.Hash(),
		SnapshotRoot:         crypto.HashBytes([]byte("test-only opaque signed snapshot root")),
		TrustAssumption:      LPoDV2TrustAssumption,
		HistoricalIssued:     1_000_000_000 * lpod.Unit,
		HistoricalSaleRemain: 4_000_000_000 * lpod.Unit,
		ValidatorRemaining:   2_000_000_000 * lpod.Unit,
		TrustedValidators:    []crypto.ValidatorPubKey{pub},
	}
	m.ReconciliationRoot = m.Root()
	sig, err := priv.Sign(m.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	m.Attestations = []LPoDAttestation{{Validator: pub, Signature: sig}}
	return db, path, m, priv, address
}

func TestLPoDTrustedCheckpointV2FundingRestartRollbackAndReplay(t *testing.T) {
	db, path, m, priv, address := trustedLPoDV2FundingFixture(t)
	parent, _, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	funding, settlement, raw := lpodActivationBlock(t, db, m, priv, address)
	if err := db.CommitRawBlockWithAVM(funding.Hash(), LPoDV2ActivationHeight, raw, nil, crypto.Hash32{}, settlement); err != nil {
		t.Fatalf("valid trusted checkpoint funding failed: %v", err)
	}
	checkpoint, err := db.LoadLPoDCheckpoint()
	if err != nil || checkpoint == nil || checkpoint.Allocation == nil {
		t.Fatalf("funded v2 checkpoint unavailable: %v", err)
	}
	allocation := checkpoint.Allocation
	if allocation.Version != 2 || allocation.FundingHeight != LPoDV2ActivationHeight ||
		allocation.FundingBlock != funding.Hash() || allocation.FundingParent != parent ||
		allocation.ReconciliationRoot != m.ReconciliationRoot ||
		allocation.Remaining != 3_000_000_000*lpod.Unit ||
		checkpoint.State.FundingDebit != lpod.InitialNAPRO {
		t.Fatalf("unexpected v2 funded allocation: %+v", allocation)
	}
	binding, err := db.get([]byte("lpod/activation/v2"))
	root := m.Root()
	if err != nil || string(binding) != string(root[:]) {
		t.Fatalf("durable v2 activation binding missing or wrong: %v", err)
	}

	next, nextSettlement, nextRaw := lpodActivationBlock(t, db, nil, priv, address)
	if err := db.CommitRawBlockWithAVM(next.Hash(), LPoDV2ActivationHeight+1, nextRaw, nil, crypto.Hash32{}, nextSettlement); err != nil {
		t.Fatalf("later checkpoint rejected funding parent's identity: %v", err)
	}
	later, err := db.LoadLPoDCheckpoint()
	if err != nil || later == nil || later.State.LastHeight != LPoDV2ActivationHeight+1 ||
		later.Allocation.FundingBlock != funding.Hash() {
		t.Fatalf("later funded checkpoint unavailable: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CheckLPoDConfig(m); err != nil {
		t.Fatalf("same authority/checkpoint binding rejected after restart: %v", err)
	}
	restarted, err := db.LoadLPoDCheckpoint()
	if err != nil || restarted == nil || restarted.State.LastHeight != LPoDV2ActivationHeight+1 ||
		restarted.Allocation.ReconciliationRoot != m.Root() {
		t.Fatalf("funded checkpoint not durable after restart: %v", err)
	}
	changedAuthority := *m
	_, otherPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	changedAuthority.TrustedValidators = []crypto.ValidatorPubKey{otherPub}
	if err := db.CheckLPoDConfig(&changedAuthority); err == nil {
		t.Fatal("authority-set change reinterpreted durable activation after restart")
	}

	if err := db.PutTip(parent, LPoDV2ActivationHeight-1); err != nil {
		t.Fatalf("rollback to exact funding parent failed: %v", err)
	}
	if cp, err := db.LoadLPoDCheckpoint(); err != nil || cp != nil {
		t.Fatalf("rollback retained v2 funding: checkpoint=%v err=%v", cp, err)
	}
	if pool, found, err := db.LoadStakingPoolRemaining(); err != nil || !found || pool != m.ValidatorRemaining {
		t.Fatalf("rollback changed existing validator pool: pool=%d found=%v err=%v", pool, found, err)
	}
	if err := db.CommitRawBlockWithAVM(funding.Hash(), LPoDV2ActivationHeight, raw, nil, crypto.Hash32{}, settlement); err != nil {
		t.Fatalf("same signed activation replay after rollback failed: %v", err)
	}
	replayed, err := db.LoadLPoDCheckpoint()
	if err != nil || replayed == nil || replayed.Allocation.ReconciliationRoot != m.Root() {
		t.Fatalf("replayed v2 checkpoint missing: %v", err)
	}
}

func TestLPoDV2FundingRequiresMatchingExistingValidatorPool(t *testing.T) {
	db, _, m, priv, address := trustedLPoDV2FundingFixture(t)
	defer db.Close()
	if err := db.StoreStakingPoolRemaining(m.ValidatorRemaining - 1); err != nil {
		t.Fatal(err)
	}
	parent, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	request := &LPoDSettlement{
		Parent: parent, Migration: m, PositionProtocol: true,
		Proposer: priv.Public().Hex(), Leader: address,
		Stake: map[string]LPoDValidatorStake{priv.Public().Hex(): {Amount: 100_000 * lpod.Unit, Active: true}},
	}
	if _, _, err := db.PreviewLPoD(height+1, request); err == nil {
		t.Fatal("v2 migration funded without the matching preexisting validator pool")
	}
	if checkpoint, err := db.LoadLPoDCheckpoint(); err != nil || checkpoint != nil {
		t.Fatalf("failed v2 funding wrote an allocation checkpoint: %v", err)
	}
}

func TestLPoDCheckpointRejectsOverflowingAllocationBounds(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tests := []struct {
		name      string
		validator uint64
		issued    uint64
		sale      uint64
	}{
		{"validator underflow", ^uint64(0), 0, lpod.InitialNAPRO},
		{"issued underflow", 1_000_000_000 * lpod.Unit, ^uint64(0), lpod.InitialNAPRO},
		{"sale underflow", 1_000_000_000 * lpod.Unit, 1_000_000_000 * lpod.Unit, ^uint64(0)},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash := crypto.HashBytes([]byte{byte(i)})
			cp := LPoDCheckpoint{
				State: lpod.State{
					FundingDebit: lpod.InitialNAPRO,
					Balance:      lpod.InitialNAPRO,
					LastHeight:   LPoDV2ActivationHeight,
				},
				Allocation: &LPoDAllocation{
					Version: 2, PositionLifecycleVersion: 1,
					Genesis:                   crypto.HashBytes([]byte("genesis")),
					FundingHeight:             LPoDV2ActivationHeight,
					FundingBlock:              hash,
					InitialValidatorRemaining: tt.validator,
					ValidatorRemaining:        tt.validator,
					HistoricalIssued:          tt.issued,
					DeclaredSaleRemaining:     tt.sale,
					Remaining:                 tt.sale,
					SnapshotRoot:              crypto.HashBytes([]byte("snapshot")),
					FundingParent:             crypto.HashBytes([]byte("parent")),
					TrustAssumption:           LPoDV2TrustAssumption,
				},
			}
			raw, err := json.Marshal(cp)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.put(lpodKey(hash), raw); err != nil {
				t.Fatal(err)
			}
			if _, err := db.lpodCheckpoint(hash); err == nil {
				t.Fatal("overflowing checkpoint allocation accepted")
			}
		})
	}
}
