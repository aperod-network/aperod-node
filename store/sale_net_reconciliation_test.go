package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
)

func TestSaleNetReconciliationRequiresExplicitSignedRootAndCanonicalReserves(t *testing.T) {
	c := netSourceFixture()
	genesis := crypto.HashBytes([]byte("net-source-genesis"))
	parent := crypto.HashBytes([]byte("net-source-parent"))
	key, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic declared wallet/self-stake amount is NOT production evidence.
	net := &sale.NetAllocationBasis{Burned: 13745052310704501, WalletNet: 100010000000000,
		WalletAccountingRoot: crypto.HashBytes([]byte("synthetic outside-wallet declaration"))}
	consumed, err := net.Consumed()
	if err != nil {
		t.Fatal(err)
	}
	r := sale.Reconciliation{NetAllocation: net, Genesis: genesis, Parent: parent, LPoDCommitment: c.Digest(),
		OtherClaimsRoot: crypto.HashBytes([]byte("no-other-claims")), HistoricIssued: consumed,
		ValidatorReserved: c.Allocation.ValidatorRemaining, GuardianReserved: c.State.Balance, LPoDReserved: c.PrincipalLocked}
	budget, reserved, err := r.Unassigned()
	if err != nil {
		t.Fatal(err)
	}
	cp := sale.Checkpoint{Genesis: genesis, Parent: parent, Height: 10, Network: crypto.MainnetByte,
		SaleAuthority: pub, SnapshotRoot: r.Digest(), HistoricIssued: consumed, OtherReserved: reserved, SaleRemaining: budget}
	sign := func() {
		cp.SnapshotRoot = r.Digest()
		sig, err := key.Sign(cp.Message())
		if err != nil {
			t.Fatal(err)
		}
		cp.Attestations = []sale.Attestation{{Validator: pub, Signature: sig}}
	}
	sign()
	if err := VerifySaleReserveFloors(c, r); err != nil {
		t.Fatal(err)
	}
	if err := sale.VerifyReconciledCheckpoint(cp, r, genesis, parent, c.Digest(), 10, 623000000000000000, []crypto.ValidatorPubKey{pub}); err != nil {
		t.Fatal(err)
	}
	originalRoot := cp.SnapshotRoot
	r.NetAllocation = nil
	if r.Digest() == originalRoot {
		t.Fatal("legacy and corrected bases share signature domain")
	}
	if err := VerifySaleReserveFloors(c, r); err == nil {
		t.Fatal("unsigned implicit interpretation change accepted")
	}
	r.NetAllocation = net
	for _, kind := range []string{"wallet-root", "principal", "rewards", "validator", "wallet-consumed"} {
		t.Run(kind, func(t *testing.T) {
			m := r
			n := *net
			m.NetAllocation = &n
			switch kind {
			case "wallet-root":
				n.WalletAccountingRoot = crypto.Hash32{}
			case "principal":
				m.LPoDReserved--
			case "rewards":
				m.GuardianReserved--
			case "validator":
				m.ValidatorReserved--
			case "wallet-consumed":
				n.WalletNet++
			}
			cp.SnapshotRoot = m.Digest()
			sig, err := key.Sign(cp.Message())
			if err != nil {
				t.Fatal(err)
			}
			cp.Attestations = []sale.Attestation{{Validator: pub, Signature: sig}}
			if err := cp.Verify(genesis, parent, 10, []crypto.ValidatorPubKey{pub}); err != nil {
				t.Fatal("mutation was not genuinely signed", err)
			}
			if err := VerifySaleReserveFloors(c, m); err == nil {
				t.Fatal("signed invalid net allocation accepted")
			}
		})
	}
}

func TestSaleNetCheckpointLoadsOnlyAtExactParentAndRejectsResignedPrincipalOmission(t *testing.T) {
	g, m, key, address, records := saleStoreFixture(t)
	chainPath := filepath.Join(t.TempDir(), "chain")
	d := seedSaleParent(t, chainPath, records)
	defer func() { d.Close() }()
	l, err := d.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	net := &sale.NetAllocationBasis{Burned: 0, WalletNet: 100000000,
		WalletAccountingRoot: crypto.HashBytes([]byte("synthetic wallet inventory root"))}
	consumed, err := net.Consumed()
	if err != nil {
		t.Fatal(err)
	}
	m.Reconciliation.NetAllocation = net
	m.Reconciliation.HistoricIssued = consumed
	m.Reconciliation.ValidatorReserved = l.Allocation.ValidatorRemaining
	m.Reconciliation.GuardianReserved = l.State.Balance
	m.Reconciliation.LPoDReserved = l.PrincipalLocked + l.Allocation.Remaining
	m.Reconciliation.OtherReserved = 0
	m.Trusted = []crypto.ValidatorPubKey{key.Public()}
	path := filepath.Join(t.TempDir(), "migration.json")
	write := func() {
		budget, reserved, err := m.Reconciliation.Unassigned()
		if err != nil {
			t.Fatal(err)
		}
		m.Checkpoint.HistoricIssued = m.Reconciliation.HistoricIssued
		m.Checkpoint.OtherReserved = reserved
		m.Checkpoint.SaleRemaining = budget
		m.Checkpoint.SnapshotRoot = m.Reconciliation.Digest()
		sig, err := key.Sign(m.Checkpoint.Message())
		if err != nil {
			t.Fatal(err)
		}
		m.Checkpoint.Attestations = []sale.Attestation{{Validator: key.Public(), Signature: sig}}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := d.LoadSaleMigrationFile(path); err != nil {
		t.Fatal("genuine signed exact-parent control rejected", err)
	}
	m.Reconciliation.LPoDReserved--
	write()
	if err := m.Checkpoint.Verify(m.Checkpoint.Genesis, g.Hash(), g.Header.Height+1, m.Trusted); err != nil {
		t.Fatal("mutation not genuinely signed", err)
	}
	if _, err := d.LoadSaleMigrationFile(path); err == nil || !strings.Contains(err.Error(), "canonical reserves") {
		t.Fatal("signed omission accepted", err)
	}
	tip, height, err := d.GetTip()
	if err != nil || tip != g.Hash() || height != g.Header.Height {
		t.Fatal("read-only configuration check changed tip")
	}
	state, err := d.LoadSaleState()
	if err != nil || state != nil {
		t.Fatal("read-only configuration check initialized ledger")
	}
	m.Reconciliation.LPoDReserved++
	write()
	if _, err := d.LoadSaleMigrationFile(path); err != nil {
		t.Fatal(err)
	}
	block, settlement := saleTestBlock(t, d, m, key, address, m.Checkpoint.Height, g.Hash(), "net-first")
	commitSaleFixture(t, d, block, m, settlement)
	credited := block.Txs[len(block.Txs)-1]
	output, err := d.GetUTXO(credited.Hash(), 0)
	state, loadErr := d.LoadSaleState()
	if err != nil || output == nil || output.AmountCommit != credited.Outputs[0].AmountCommit ||
		loadErr != nil || state == nil || state.Sold != 100*100000000 ||
		state.Remaining != m.Checkpoint.SaleRemaining-state.Sold {
		t.Fatal("net source debit and canonical recipient output were not conserved")
	}
	beforeRestart := *state
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(chainPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.LoadSaleMigrationFile(path); err != nil {
		t.Fatal("net checkpoint lost on real reopen", err)
	}
	state, err = d.LoadSaleState()
	if err != nil || state == nil || *state != beforeRestart {
		t.Fatal("reopen lost net source debit")
	}
}
