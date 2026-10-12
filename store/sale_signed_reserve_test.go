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

func TestSaleFullyResignedGuardianOmissionRejectedAtStartupAndBlock(t *testing.T) {
	g, m, key, address, records := saleStoreFixture(t)
	d := seedSaleParent(t, filepath.Join(t.TempDir(), "chain"), records)
	defer d.Close()
	// Use a disposable, explicit one-authority trust root. Both controls
	// really pass its signature threshold; no signature check is mocked.
	m.Trusted = []crypto.ValidatorPubKey{key.Public()}
	resign := func() {
		m.Checkpoint.SnapshotRoot = m.Reconciliation.Digest()
		_, reserved, err := m.Reconciliation.Unassigned()
		if err != nil {
			t.Fatal(err)
		}
		m.Checkpoint.OtherReserved = reserved
		sig, err := key.Sign(m.Checkpoint.Message())
		if err != nil {
			t.Fatal(err)
		}
		m.Checkpoint.Attestations = []sale.Attestation{{Validator: key.Public(), Signature: sig}}
	}
	file := filepath.Join(t.TempDir(), "migration.json")
	write := func() {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	resign()
	write()
	if _, err := d.LoadSaleMigrationFile(file); err != nil {
		t.Fatalf("valid signed control: %v", err)
	}
	m.Reconciliation.GuardianReserved--
	resign()
	write()
	l, err := d.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	issued, err := SaleKnownIssuedFloor(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := sale.VerifyReconciledCheckpoint(m.Checkpoint, m.Reconciliation, m.Checkpoint.Genesis, g.Hash(), l.Digest(), m.Checkpoint.Height, issued, m.Trusted); err != nil {
		t.Fatalf("mutation signature/context invalid rather than reserve-invalid: %v", err)
	}
	if _, err := d.LoadSaleMigrationFile(file); err == nil || !strings.Contains(err.Error(), "Guardian") {
		t.Fatalf("startup did not enforce Guardian floor: %v", err)
	}
	b, _ := saleTestBlock(t, d, m, key, address, m.Checkpoint.Height, g.Hash(), "guardian-omission")
	if _, err := d.PrepareSaleBlock(b, m); err == nil || !strings.Contains(err.Error(), "Guardian") {
		t.Fatalf("block did not enforce Guardian floor: %v", err)
	}
	tip, height, err := d.GetTip()
	if err != nil || tip != g.Hash() || height != g.Header.Height {
		t.Fatal("failed block changed tip")
	}
	state, err := d.LoadSaleState()
	if err != nil || state != nil {
		t.Fatal("failed block initialized a sale ledger")
	}
}

func TestSaleRejectsNewGenesisReserveWithoutChangingExistingFund(t *testing.T) {
	g, _, _, _, records := saleStoreFixture(t)
	d := seedSaleParent(t, filepath.Join(t.TempDir(), "chain"), records)
	defer d.Close()
	request := &LPoDSettlement{GenesisVestingActivationHeight: g.Header.Height + 1}
	if err := d.checkSaleGenesisFundingTransition(g.Header.Height+1, request, true); err == nil {
		t.Fatal("simultaneous reservation accepted")
	}
	if err := d.checkSaleGenesisFundingTransition(g.Header.Height, request, true); err != nil {
		t.Fatal(err)
	}
	if err := d.checkSaleGenesisFundingTransition(g.Header.Height+1, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := d.db.Put(saleStateKey, []byte(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := d.checkSaleGenesisFundingTransition(g.Header.Height+1, request, false); err == nil {
		t.Fatal("new reserve accepted after sales")
	}
	if err := d.CheckGenesisVestingConfig(g.Header.Height+2, nil); err == nil || !strings.Contains(err.Error(), "shared reserve") {
		t.Fatalf("startup accepted new funding: %v", err)
	}
}
