package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func saleUpgradeRecords(t *testing.T, db *DB) map[string][]byte {
	t.Helper()
	records := map[string][]byte{}
	it := db.db.NewIterator(nil, nil)
	for it.Next() {
		records[string(it.Key())] = append([]byte{}, it.Value()...)
	}
	err := it.Error()
	it.Release()
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestSaleUpgradeCopyReadOnly(t *testing.T) {
	for _, scenario := range []string{"aligned", "stale-ledger", "wrong-signature", "wrong-budget", "wrong-manifest", "missing-config", "wrong-tip-body"} {
		t.Run(scenario, func(t *testing.T) {
			g, m, key, address, records := saleStoreFixture(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "copy", "chain.db")
			db := seedSaleParent(t, path, records)
			block, settlement := saleTestBlock(t, db, m, key, address, g.Header.Height+1, g.Hash(), "upgrade")
			commitSaleFixture(t, db, block, m, settlement)
			hash, height := block.Hash(), block.Header.Height
			manifestHash := hash
			switch scenario {
			case "stale-ledger":
				next := core.Block{Header: block.Header}
				next.Header.Height++
				next.Header.PrevHash = hash
				next.Header.MerkleRoot = core.MerkleRoot(nil)
				next.Header.Timestamp++
				if err := next.Header.Sign(key); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(next)
				hash, height = next.Hash(), next.Header.Height
				manifestHash = hash
				if err := db.PutRawBlock(hash, height, raw); err != nil {
					t.Fatal(err)
				}
				var hb [8]byte
				binary.LittleEndian.PutUint64(hb[:], height)
				if err := db.db.Put([]byte("m/tip/hash"), hash[:], nil); err != nil {
					t.Fatal(err)
				}
				if err := db.db.Put([]byte("m/tip/height"), hb[:], nil); err != nil {
					t.Fatal(err)
				}
			case "wrong-signature":
				m.Checkpoint.Attestations[0].Signature[0] ^= 1
			case "wrong-budget":
				state, _ := db.LoadSaleState()
				state.Remaining--
				raw, _ := json.Marshal(state)
				if err := db.db.Put(saleStateKey, raw, nil); err != nil {
					t.Fatal(err)
				}
			case "wrong-manifest":
				manifestHash = crypto.HashBytes([]byte("not the canonical copy"))
			case "wrong-tip-body":
				if err := db.db.Put(append([]byte("b/"), hash[:]...), []byte("{}"), nil); err != nil {
					t.Fatal(err)
				}
			}
			configPath := filepath.Join(dir, "sale.json")
			raw, _ := json.Marshal(m)
			if err := os.WriteFile(configPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "missing-config" {
				configPath = ""
			}
			before := saleUpgradeRecords(t, db)
			if scenario == "aligned" {
				// Exercise the real logical exporter used by the private socket,
				// not just a filesystem copy. It must include all sale fences.
				exportPath := filepath.Join(dir, "exported", "chain.db")
				if err := os.Mkdir(filepath.Dir(exportPath), 0700); err != nil {
					t.Fatal(err)
				}
				exportHash, exportHeight, exportErr := db.exportLogicalBackup(context.Background(), exportPath,
					func(string) (uint64, error) { return 1 << 50, nil })
				if exportErr != nil {
					t.Fatal(exportErr)
				}
				exportReport, exportErr := CheckSaleUpgradeCopy(exportPath, path, configPath, exportHash, exportHeight)
				if exportErr != nil || exportReport.Mode != "active" {
					t.Fatal(exportReport, exportErr)
				}
				exported, exportErr := OpenReadOnly(exportPath)
				if exportErr != nil {
					t.Fatal(exportErr)
				}
				exportRecords := saleUpgradeRecords(t, exported)
				exported.Close()
				if len(before) != len(exportRecords) {
					t.Fatal("logical copy omitted records")
				}
				for k, v := range before {
					if !bytes.Equal(v, exportRecords[k]) {
						t.Fatalf("logical copy changed record %q", k)
					}
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			report, err := CheckSaleUpgradeCopy(path, filepath.Join(dir, "live", "chain.db"), configPath, manifestHash, height)
			if scenario == "aligned" {
				if err != nil || report.Status != "ok" || report.Mode != "active" || report.TipHeight != height {
					t.Fatalf("valid control refused: %+v %v", report, err)
				}
			} else if err == nil {
				t.Fatal("inconsistent copy accepted")
			}
			// Reopen real LevelDB, not a mock: all accounting and replay fences
			// must be byte-identical, including on refusal.
			readonly, err := OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			after := saleUpgradeRecords(t, readonly)
			readonly.Close()
			if len(before) != len(after) {
				t.Fatal("preflight changed record count")
			}
			for k, v := range before {
				if !bytes.Equal(v, after[k]) {
					t.Fatalf("preflight changed durable record %q", k)
				}
			}
			if scenario == "aligned" {
				if _, err := CheckSaleUpgradeCopy(path, path, configPath, hash, height); err == nil {
					t.Fatal("accepted live path")
				}
				alias := filepath.Join(dir, "live-alias")
				if err := os.Symlink(path, alias); err != nil {
					t.Fatal(err)
				}
				if _, err := CheckSaleUpgradeCopy(alias, path, configPath, hash, height); err == nil {
					t.Fatal("accepted live symlink alias")
				}
			}
		})
	}
}
