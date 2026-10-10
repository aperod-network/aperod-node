package store

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func makeLPoDAuditFixture(t *testing.T, defect string) string {
	t.Helper()
	path := t.TempDir()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	genesis := &core.Block{Header: core.BlockHeader{Height: 0}}
	genesis.Header.MerkleRoot = core.MerkleRoot(genesis.Txs)
	var genesisErr error
	if defect == "missing-txs" {
		raw, err := json.Marshal(struct {
			Header core.BlockHeader
		}{Header: genesis.Header})
		if err != nil {
			t.Fatal(err)
		}
		genesisErr = db.CommitRawBlockWithAVM(genesis.Hash(), 0, raw, nil, crypto.Hash32{})
	} else {
		genesisErr = commitAuditBlock(db, genesis)
	}
	if genesisErr != nil {
		t.Fatal(genesisErr)
	}

	parent := genesis.Hash()
	if defect == "bad-parent" {
		parent[0] ^= 0xff
	}
	block1 := &core.Block{
		Header: core.BlockHeader{Height: 1, PrevHash: parent},
		Txs:    []core.Transaction{{Outputs: make([]core.Output, 2)}},
	}
	block1.Header.MerkleRoot = core.MerkleRoot(block1.Txs)
	if err := commitAuditBlock(db, block1); err != nil {
		t.Fatal(err)
	}

	block2 := &core.Block{
		Header: core.BlockHeader{Height: 2, PrevHash: block1.Hash()},
		Txs:    []core.Transaction{{Inputs: make([]core.RingInput, 1)}},
	}
	block2.Header.MerkleRoot = core.MerkleRoot(block2.Txs)
	if err := commitAuditBlock(db, block2); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(block2.Hash(), 2); err != nil {
		t.Fatal(err)
	}

	switch defect {
	case "pruned":
		if err := db.PutBlock(block1.Hash(), &StoredBlock{
			Height: 1, PrevHash: block1.Header.PrevHash, Hash: block1.Hash(), TxCount: len(block1.Txs),
		}); err != nil {
			t.Fatal(err)
		}
	case "missing":
		hash := block1.Hash()
		if err := db.db.Delete(append(append([]byte{}, prefixBlock...), hash[:]...), nil); err != nil {
			t.Fatal(err)
		}
	case "bad-merkle":
		block1.Txs[0].Outputs = append(block1.Txs[0].Outputs, core.Output{})
		raw, err := json.Marshal(block1)
		if err != nil {
			t.Fatal(err)
		}
		hash := block1.Hash()
		key := append(append([]byte{}, prefixBlock...), hash[:]...)
		if err := db.db.Put(key, raw, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func commitAuditBlock(db *DB, block *core.Block) error {
	raw, err := json.Marshal(block)
	if err != nil {
		return err
	}
	return db.CommitRawBlockWithAVM(block.Hash(), block.Header.Height, raw, nil, crypto.Hash32{})
}

func TestAuditLPoDBodyCoverageReadOnly(t *testing.T) {
	tests := []struct {
		name            string
		defect          string
		wantComplete    bool
		wantChecked     uint64
		wantOutputs     uint64
		wantIssueHeight uint64
		wantIssueKind   string
	}{
		{name: "complete", wantComplete: true, wantChecked: 3, wantOutputs: 2},
		{name: "pruned", defect: "pruned", wantChecked: 1, wantIssueHeight: 1, wantIssueKind: "pruned"},
		{name: "missing", defect: "missing", wantChecked: 1, wantIssueHeight: 1, wantIssueKind: "missing-or-pruned"},
		{name: "noncanonical", defect: "bad-merkle", wantChecked: 1, wantIssueHeight: 1, wantIssueKind: "noncanonical"},
		{name: "broken-parent-link", defect: "bad-parent", wantChecked: 1, wantIssueHeight: 1, wantIssueKind: "noncanonical"},
		{name: "missing-txs-field", defect: "missing-txs", wantChecked: 0, wantIssueHeight: 0, wantIssueKind: "noncanonical"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := makeLPoDAuditFixture(t, tt.defect)
			report, err := AuditLPoDBodyCoverageReadOnly(path, 2)
			if tt.wantComplete {
				if err != nil {
					t.Fatalf("audit failed: %v", err)
				}
			} else if err == nil {
				t.Fatal("expected coverage failure")
			}
			if report == nil {
				t.Fatal("expected report")
			}
			if report.Complete != tt.wantComplete || report.CheckedBlocks != tt.wantChecked ||
				report.CoinbaseOutputs != tt.wantOutputs {
				t.Fatalf("unexpected report: %+v", report)
			}
			if !tt.wantComplete && (report.IssueHeight == nil || *report.IssueHeight != tt.wantIssueHeight ||
				report.IssueKind != tt.wantIssueKind) {
				t.Fatalf("unexpected first issue: %+v", report)
			}
			for _, phrase := range []string{"structural presence and linkage", "signatures, proofs or full-body integrity", "LPoDBodyRootStep", "exact issuance openings"} {
				if !strings.Contains(report.Notice, phrase) {
					t.Fatalf("report notice missing %q: %s", phrase, report.Notice)
				}
			}
		})
	}
}

func TestAuditRefusesMissingLockWithoutCreatingFiles(t *testing.T) {
	path := t.TempDir()
	report, err := AuditLPoDBodyCoverageReadOnly(path, 0)
	if err == nil || !strings.Contains(err.Error(), "LOCK") {
		t.Fatalf("expected missing LOCK error, report=%+v err=%v", report, err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("missing-LOCK audit created files: %v", entries)
	}
}

func TestAuditLPoDBodyCoverageRequiresCopiedPathAndBoundedHeight(t *testing.T) {
	if _, err := AuditLPoDBodyCoverageReadOnly("", 0); err == nil {
		t.Fatal("empty copied database path accepted")
	}
	path := makeLPoDAuditFixture(t, "")
	if _, err := AuditLPoDBodyCoverageReadOnly(path, 3); err == nil {
		t.Fatal("through-height above tip accepted")
	}
}

func TestIsPrunedStoredBlockDistinguishesCoreBlockEncoding(t *testing.T) {
	fullRaw, err := json.Marshal(&core.Block{})
	if err != nil {
		t.Fatal(err)
	}
	if isPrunedStoredBlock(fullRaw) {
		t.Fatal("core.Block JSON was identified as a pruned StoredBlock")
	}
	prunedRaw, err := json.Marshal(&StoredBlock{Height: 4, TxCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !isPrunedStoredBlock(prunedRaw) {
		t.Fatal("pruned StoredBlock JSON was not identified")
	}
	if isPrunedStoredBlock([]byte(`{"height":4}`)) {
		t.Fatal("incomplete object was identified as a pruned StoredBlock")
	}
}
