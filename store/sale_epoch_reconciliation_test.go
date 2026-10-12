package store

import (
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
)

func epochMigrationFixture(t *testing.T) (*DB, *SaleMigration, crypto.ValidatorPrivKey, crypto.Address) {
	t.Helper()
	g, m, key, address, records := saleStoreFixture(t)
	d := seedSaleParent(t, filepath.Join(t.TempDir(), "chain"), records)
	t.Cleanup(func() { _ = d.Close() })
	l, err := d.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	e := &sale.AllocationEpochBasis{FundingHeight: l.Allocation.FundingHeight,
		FundingHash: l.Allocation.FundingBlock, Burned: 13745052310704501,
		ProtectedLPoDPrincipal: l.PrincipalLocked, LegacyHoldingsUnverified: true}
	r := sale.Reconciliation{AllocationEpoch: e, Genesis: m.Checkpoint.Genesis,
		Parent: g.Hash(), LPoDCommitment: l.Digest(),
		OtherClaimsRoot: crypto.HashBytes([]byte("fixture-no-other-claims")),
		HistoricIssued:  e.Burned, ValidatorReserved: l.Allocation.ValidatorRemaining,
		GuardianReserved: l.State.Balance, LPoDReserved: l.Allocation.Remaining}
	e.Available = sale.CapNAPRO - e.Burned - r.ValidatorReserved - r.GuardianReserved - r.LPoDReserved
	_, reserved, err := r.Unassigned()
	if err != nil {
		t.Fatal(err)
	}
	m.Reconciliation = r
	m.Checkpoint.HistoricIssued, m.Checkpoint.OtherReserved = e.Burned, reserved
	m.Checkpoint.SaleRemaining = e.Available
	m.Trusted = []crypto.ValidatorPubKey{key.Public()}
	signEpochMigration(t, m, key)
	return d, m, key, address
}

func signEpochMigration(t *testing.T, m *SaleMigration, key crypto.ValidatorPrivKey) {
	t.Helper()
	m.Checkpoint.SnapshotRoot = m.Reconciliation.Digest()
	sig, err := key.Sign(m.Checkpoint.Message())
	if err != nil {
		t.Fatal(err)
	}
	m.Checkpoint.Attestations = []sale.Attestation{{Validator: key.Public(), Signature: sig}}
}

func TestOperatorEpochDebitsOnlyItsFullCurrentAvailableSource(t *testing.T) {
	d, m, key, address := epochMigrationFixture(t)
	before, err := d.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	b, settlement := saleTestBlock(t, d, m, key, address, m.Checkpoint.Height, m.Checkpoint.Parent, "paid")
	commitSaleFixture(t, d, b, m, settlement)
	state, err := d.LoadSaleState()
	const amount uint64 = 100 * 100000000
	if err != nil || state == nil || state.Remaining != m.Checkpoint.SaleRemaining-amount || state.Sold != amount {
		t.Fatal("wrong canonical allocation debit", err)
	}
	if output, err := d.GetUTXO(b.Txs[len(b.Txs)-1].Hash(), 0); err != nil || output == nil {
		t.Fatal("allocation debit without buyer UTXO", err)
	}
	after, err := d.LoadLPoDCheckpoint()
	if err != nil || after.PrincipalLocked != before.PrincipalLocked {
		t.Fatal("existing principal changed", err)
	}
}

func TestOperatorEpochRejectsResignedPolicyAndReserveMutations(t *testing.T) {
	for _, kind := range []string{"mixed", "funding", "principal", "validator", "guardian", "unassigned", "verified-claim", "available", "smaller-budget"} {
		t.Run(kind, func(t *testing.T) {
			d, m, key, address := epochMigrationFixture(t)
			e := m.Reconciliation.AllocationEpoch
			switch kind {
			case "mixed":
				m.Reconciliation.NetAllocation = &sale.NetAllocationBasis{}
			case "funding":
				e.FundingHash[0] ^= 1
			case "principal":
				e.ProtectedLPoDPrincipal++
			case "validator":
				m.Reconciliation.ValidatorReserved--
			case "guardian":
				m.Reconciliation.GuardianReserved--
			case "unassigned":
				m.Reconciliation.LPoDReserved++
			case "verified-claim":
				e.LegacyHoldingsUnverified = false
			case "available":
				e.Available++
			case "smaller-budget":
				m.Checkpoint.SaleRemaining--
			}
			signEpochMigration(t, m, key)
			if err := m.Checkpoint.Verify(m.Checkpoint.Genesis, m.Checkpoint.Parent, m.Checkpoint.Height, m.Trusted); err != nil {
				t.Fatal("mutation not genuinely signed", err)
			}
			b, _ := saleTestBlock(t, d, m, key, address, m.Checkpoint.Height, m.Checkpoint.Parent, kind)
			if _, err := d.PrepareSaleBlock(b, m); err == nil {
				t.Fatal("signed invalid opening accepted")
			}
		})
	}
}
