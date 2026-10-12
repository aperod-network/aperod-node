package store

import (
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
)

func saleStoreFixture(t *testing.T) (*core.Block, *SaleMigration, crypto.ValidatorPrivKey, crypto.Address, map[string][]byte) {
	t.Helper()
	base, _, lm, lk, la := trustedLPoDV3FundingFixture(t)
	defer base.Close()
	g, settlement, raw := lpodActivationBlock(t, base, lm, lk, la)
	if err := base.CommitRawBlockWithAVM(g.Hash(), g.Header.Height, raw, nil, crypto.Hash32{}, settlement); err != nil {
		t.Fatal(err)
	}
	authority, pub := lk, lk.Public()
	l, err := base.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	knownIssued, err := SaleKnownIssuedFloor(l)
	if err != nil {
		t.Fatal(err)
	}
	r := sale.Reconciliation{Genesis: lm.Genesis, Parent: g.Hash(), LPoDCommitment: l.Digest(),
		OtherClaimsRoot: crypto.HashBytes([]byte("fixture-disjoint-claims")), HistoricIssued: knownIssued,
		ValidatorReserved: l.Allocation.ValidatorRemaining, GuardianReserved: 1000000000 * 100000000}
	c := sale.Checkpoint{Genesis: lm.Genesis, Parent: g.Hash(), Network: crypto.MainnetByte, Height: g.Header.Height + 1, SnapshotRoot: r.Digest(),
		HistoricIssued: r.HistoricIssued, OtherReserved: r.ValidatorReserved + r.LPoDReserved + r.GuardianReserved, SaleRemaining: 100000 * 100000000, SaleAuthority: pub}
	m := &SaleMigration{Checkpoint: c, Reconciliation: r}
	for i := 0; i < 4; i++ {
		k, p, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
		m.Trusted = append(m.Trusted, p)
		if i < 3 {
			sig, err := k.Sign(c.Message())
			if err != nil {
				t.Fatal(err)
			}
			m.Checkpoint.Attestations = append(m.Checkpoint.Attestations, sale.Attestation{Validator: p, Signature: sig})
		}
	}
	records := map[string][]byte{}
	it := base.db.NewIterator(nil, nil)
	for it.Next() {
		records[string(it.Key())] = append([]byte(nil), it.Value()...)
	}
	err = it.Error()
	it.Release()
	if err != nil {
		t.Fatal(err)
	}
	return g, m, authority, la, records
}
func seedSaleParent(t *testing.T, path string, records map[string][]byte) *DB {
	t.Helper()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range records {
		if err = d.db.Put([]byte(key), value, nil); err != nil {
			t.Fatal(err)
		}
	}
	return d
}
func saleTestBlock(t *testing.T, d *DB, m *SaleMigration, key crypto.ValidatorPrivKey, address crypto.Address, height uint64, parent crypto.Hash32, id string) (*core.Block, *LPoDSettlement) {
	t.Helper()
	tx, err := core.BuildSaleTransaction(core.SaleAuthorization{Genesis: m.Checkpoint.Genesis, Height: height, Parent: parent,
		Order: crypto.HashBytes([]byte("order-" + id)), Payment: crypto.HashBytes([]byte("payment-" + id)), Amount: 100 * 100000000}, address, m.Checkpoint.Network, key)
	if err != nil {
		t.Fatal(err)
	}
	b, r, _ := lpodActivationBlock(t, d, nil, key, address)
	if b.Header.Height != height || b.Header.PrevHash != parent {
		t.Fatal("fixture clock mismatch")
	}
	b.Txs = append(b.Txs, *tx)
	r.Transactions = append([]core.Transaction(nil), b.Txs[2:]...)
	b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
	if err = b.Header.Sign(key); err != nil {
		t.Fatal(err)
	}
	return b, r
}
func commitSaleFixture(t *testing.T, d *DB, b *core.Block, m *SaleMigration, r *LPoDSettlement) {
	t.Helper()
	p, err := d.PrepareSaleBlock(b, m)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	if err = d.CommitRawBlockWithSale(b.Hash(), b.Header.Height, raw, nil, crypto.Hash32{}, p, r); err != nil {
		t.Fatal(err)
	}
}
func TestSaleDurableBatchRestartRollbackAndIndependentStores(t *testing.T) {
	g, m, key, address, records := saleStoreFixture(t)
	paths := []string{t.TempDir() + "/node-a", t.TempDir() + "/node-b"}
	a := seedSaleParent(t, paths[0], records)
	defer func() { a.Close() }()
	b := seedSaleParent(t, paths[1], records)
	defer b.Close()
	block, r := saleTestBlock(t, a, m, key, address, m.Checkpoint.Height, g.Hash(), "first")
	commitSaleFixture(t, a, block, m, r)
	commitSaleFixture(t, b, block, m, r)
	as, _ := a.LoadSaleState()
	bs, _ := b.LoadSaleState()
	if as == nil || bs == nil || *as != *bs || as.Sold != 100*100000000 {
		t.Fatal("independent canonical stores disagree")
	}
	tx := block.Txs[len(block.Txs)-1]
	auth, _ := tx.SaleAuthorization()
	u, err := a.GetUTXO(tx.Hash(), 0)
	if err != nil || u == nil || u.AmountCommit != tx.Outputs[0].AmountCommit {
		t.Fatal("durable debit without output")
	}
	used, err := a.SaleConsumed('p', auth.Payment)
	if err != nil || !used {
		t.Fatal("payment fence missing")
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := a.LoadSaleState()
	if err != nil || restarted == nil || *restarted != *as {
		t.Fatal("restart lost allocation")
	}
	replay, _ := saleTestBlock(t, a, m, key, address, block.Header.Height+1, block.Hash(), "first")
	if _, err = a.PrepareSaleBlock(replay, m); err == nil {
		t.Fatal("restart lost durable replay fences")
	}
	if err = a.PutTip(g.Hash(), g.Header.Height); err != nil {
		t.Fatal(err)
	}
	after, err := a.LoadSaleState()
	if err != nil || after != nil {
		t.Fatal("rollback did not restore pre-activation allocation")
	}
	u, err = a.GetUTXO(tx.Hash(), 0)
	if err != nil || u != nil {
		t.Fatal("rollback retained sale output")
	}
	used, err = a.SaleConsumed('p', auth.Payment)
	if err != nil || used {
		t.Fatal("rollback retained payment fence")
	}
	commitSaleFixture(t, a, block, m, r)
	reapplied, _ := a.LoadSaleState()
	if *reapplied != *as {
		t.Fatal("reapply differs from independent store")
	}
}
func TestSaleRejectedCommitLeavesTipBudgetOutputAndFencesUnchanged(t *testing.T) {
	g, m, key, address, records := saleStoreFixture(t)
	d := seedSaleParent(t, t.TempDir()+"/node", records)
	defer d.Close()
	b, r := saleTestBlock(t, d, m, key, address, m.Checkpoint.Height, g.Hash(), "rejected")
	p, err := d.PrepareSaleBlock(b, m)
	if err != nil {
		t.Fatal(err)
	}
	p.After.Remaining--
	raw, _ := json.Marshal(b)
	if err = d.CommitRawBlockWithSale(b.Hash(), b.Header.Height, raw, nil, crypto.Hash32{}, p, r); err == nil {
		t.Fatal("forged debit committed")
	}
	tip, height, err := d.GetTip()
	if err != nil || tip != g.Hash() || height != g.Header.Height {
		t.Fatal("failed commit moved tip")
	}
	s, err := d.LoadSaleState()
	if err != nil || s != nil {
		t.Fatal("failed commit persisted state")
	}
	u, err := d.GetUTXO(b.Txs[len(b.Txs)-1].Hash(), 0)
	if err != nil || u != nil {
		t.Fatal("failed commit created output")
	}
	if err = d.CommitRawBlockWithAVM(b.Hash(), b.Header.Height, raw, nil, crypto.Hash32{}); err == nil {
		t.Fatal("legacy commit bypassed sale authorization")
	}
}
func TestSaleBodyMutationAndMissingRollbackJournalFailClosed(t *testing.T) {
	g, m, key, address, records := saleStoreFixture(t)
	d := seedSaleParent(t, t.TempDir()+"/node", records)
	defer d.Close()
	b, r := saleTestBlock(t, d, m, key, address, m.Checkpoint.Height, g.Hash(), "journal")
	b.Txs[len(b.Txs)-1].Outputs[0].EncAmount[0] ^= 1
	if _, err := d.PrepareSaleBlock(b, m); err == nil {
		t.Fatal("modified body matched signed header")
	}
	b.Txs[len(b.Txs)-1].Outputs[0].EncAmount[0] ^= 1
	commitSaleFixture(t, d, b, m, r)
	before, err := d.LoadSaleState()
	if err != nil {
		t.Fatal(err)
	}
	if err = d.db.Delete(saleJournalKey(b.Hash()), nil); err != nil {
		t.Fatal(err)
	}
	if err = d.PutTip(g.Hash(), g.Header.Height); err == nil {
		t.Fatal("rollback without recovery journal succeeded")
	}
	after, err := d.LoadSaleState()
	if err != nil || *before != *after {
		t.Fatal("failed rollback changed allocation")
	}
	tip, height, err := d.GetTip()
	if err != nil || tip != b.Hash() || height != b.Header.Height {
		t.Fatal("failed rollback moved canonical tip")
	}
}
