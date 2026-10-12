package store

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestSaleLedgerReconcilePreservesPaidBudgetAndFences(t *testing.T) {
	for _, scenario := range []string{"valid", "missing-body", "wrong-merkle", "wrong-parent", "wrong-baseline", "wrong-tip", "sale-in-gap", "journal-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			g, m, key, address, records := saleStoreFixture(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "chain.db")
			d := seedSaleParent(t, path, records)
			activation, p := saleTestBlock(t, d, m, key, address, g.Header.Height+1, g.Hash(), "reconcile")
			commitSaleFixture(t, d, activation, m, p) // genuinely funded canonical sale
			before, _ := d.LoadSaleState()
			old := saleUpgradeRecords(t, d)
			next := core.Block{Header: activation.Header}
			next.Header.Height++
			next.Header.PrevHash = activation.Hash()
			next.Header.Timestamp++
			next.Txs = nil
			next.Header.MerkleRoot = core.MerkleRoot(nil)
			switch scenario {
			case "wrong-parent":
				next.Header.PrevHash = crypto.HashBytes([]byte("wrong"))
			case "wrong-merkle":
				next.Header.MerkleRoot = crypto.HashBytes([]byte("wrong"))
			case "sale-in-gap":
				next.Txs = activation.Txs
				next.Header.MerkleRoot = core.MerkleRoot(next.Txs)
			}
			if err := next.Header.Sign(key); err != nil {
				t.Fatal(err)
			}
			hash, height := next.Hash(), next.Header.Height
			raw, _ := json.Marshal(next)
			if err := d.PutRawBlock(hash, height, raw); err != nil {
				t.Fatal(err)
			}
			hb := make([]byte, 8)
			binary.LittleEndian.PutUint64(hb, height)
			if err := d.db.Put([]byte("m/tip/hash"), hash[:], nil); err != nil {
				t.Fatal(err)
			}
			if err := d.db.Put([]byte("m/tip/height"), hb, nil); err != nil {
				t.Fatal(err)
			}
			if scenario == "missing-body" {
				d.db.Delete(append([]byte("b/"), hash[:]...), nil)
			}
			if scenario == "wrong-baseline" {
				m.Checkpoint.Attestations[0].Signature[0] ^= 1
			}
			if scenario == "journal-conflict" {
				d.db.Put(saleJournalKey(hash), []byte("conflict"), nil)
			}
			config := filepath.Join(dir, "sale.json")
			encoded, _ := json.Marshal(m)
			if err := os.WriteFile(config, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			snapshot := saleUpgradeRecords(t, d)
			old = snapshot
			expected := hash
			if scenario == "wrong-tip" {
				expected = crypto.HashBytes([]byte("wrong"))
			}
			_, err := d.ReconcileSaleLedger(config, expected, height, false)
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("unexpected dry-run result: %v", err)
			}
			if !reflect.DeepEqual(snapshot, saleUpgradeRecords(t, d)) {
				t.Fatal("dry-run changed data")
			}
			if scenario == "valid" {
				r, err := d.ReconcileSaleLedger(config, hash, height, true)
				if err != nil || r.Status != "reconciled" {
					t.Fatalf("apply: %v %+v", err, r)
				}
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
				d, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				current, err := d.LoadSaleState()
				if err != nil {
					t.Fatal(err)
				}
				if current.Remaining != before.Remaining || current.Sold != before.Sold || current.Checkpoint != before.Checkpoint || current.Hash != hash || current.Height != height {
					t.Fatal("budget or canonical state changed incorrectly")
				}
				after := saleUpgradeRecords(t, d)
				for k, v := range old {
					if k != string(saleStateKey) && !reflect.DeepEqual(v, after[k]) {
						t.Fatalf("original financial record changed: %q", k)
					}
				}
				if _, err := d.LoadSaleMigrationFile(config); err != nil {
					t.Fatal(err)
				}
				if _, err := d.ReconcileSaleLedger(config, hash, height, true); err != nil {
					t.Fatal("idempotent apply:", err)
				}
			} else {
				if _, err := d.ReconcileSaleLedger(config, expected, height, true); err == nil {
					t.Fatal("unsafe apply accepted")
				}
				if !reflect.DeepEqual(snapshot, saleUpgradeRecords(t, d)) {
					t.Fatal("refusal changed data")
				}
			}
			d.Close()
		})
	}
}
