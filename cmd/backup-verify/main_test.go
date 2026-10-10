package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestVerifyStageValidSmallStore(t *testing.T) {
	stage := t.TempDir()
	hash := crypto.Hash32{1, 2, 3}
	writeSmallStore(t, filepath.Join(stage, "chain.db"), hash, 7)
	writeManifest(t, stage, fmt.Sprintf(
		`{"success":true,"tip_hash":"%x","tip_height":7}`, hash[:]))

	gotHash, gotHeight, err := verifyStage(stage)
	if err != nil {
		t.Fatalf("verifyStage: %v", err)
	}
	if gotHash != fmt.Sprintf("%x", hash[:]) || gotHeight != 7 {
		t.Fatalf("got tip %s/%d, want %x/7", gotHash, gotHeight, hash[:])
	}
}

func TestVerifyStageRejectsMissingOrCorruptDB(t *testing.T) {
	t.Run("missing database", func(t *testing.T) {
		stage := t.TempDir()
		writeManifest(t, stage, `{"success":true,"tip_hash":"`+repeatHex('0', 64)+`","tip_height":0}`)
		if _, _, err := verifyStage(stage); err == nil {
			t.Fatal("verifyStage succeeded without chain.db")
		}
	})

	t.Run("corrupt database", func(t *testing.T) {
		stage := t.TempDir()
		if err := os.WriteFile(filepath.Join(stage, "chain.db"), []byte("not a database"), 0600); err != nil {
			t.Fatal(err)
		}
		writeManifest(t, stage, `{"success":true,"tip_hash":"`+repeatHex('0', 64)+`","tip_height":0}`)
		if _, _, err := verifyStage(stage); err == nil {
			t.Fatal("verifyStage succeeded with a corrupt chain.db")
		}
	})
}

func TestVerifyStageRejectsTipMismatch(t *testing.T) {
	stage := t.TempDir()
	hash := crypto.Hash32{4, 5, 6}
	writeSmallStore(t, filepath.Join(stage, "chain.db"), hash, 12)
	writeManifest(t, stage, fmt.Sprintf(
		`{"success":true,"tip_hash":"%x","tip_height":11}`, hash[:]))
	if _, _, err := verifyStage(stage); err == nil {
		t.Fatal("verifyStage succeeded with a manifest tip mismatch")
	}
}

func TestWriteChainProofVerifiesCanonicalGenesisAndAnchors(t *testing.T) {
	stage := t.TempDir()
	genesisHash, middleHash, tipHash, tipHeight := writeProofStore(t, filepath.Join(stage, "chain.db"))
	writeManifest(t, stage, fmt.Sprintf(
		`{"success":true,"tip_hash":"%x","tip_height":2}`, tipHash[:]))
	anchorsPath := filepath.Join(t.TempDir(), "anchors.json")
	anchorsJSON := fmt.Sprintf(
		`{"schema":1,"genesis_hash":"%x","anchors":[{"id":"checkpoint-a","height":1,"hash":"%x"},{"id":"checkpoint-b","height":2,"hash":"%x"}]}`,
		genesisHash[:], middleHash[:], tipHash[:])
	if err := os.WriteFile(anchorsPath, []byte(anchorsJSON), 0600); err != nil {
		t.Fatal(err)
	}
	tip, height, err := verifyStage(stage)
	if err != nil {
		t.Fatalf("verifyStage: %v", err)
	}
	proofPath := filepath.Join(t.TempDir(), "proof.json")
	if err := writeChainProof(filepath.Join(stage, "chain.db"), anchorsPath, proofPath, tip, height); err != nil {
		t.Fatalf("writeChainProof: %v", err)
	}
	if height != tipHeight {
		t.Fatalf("tip height = %d, want %d", height, tipHeight)
	}
	info, err := os.Stat(proofPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("proof mode = %o, want 0600", info.Mode().Perm())
	}
	var got proof
	if err := jsonFile(proofPath, &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != 1 || got.GenesisHash != fmt.Sprintf("%x", genesisHash[:]) ||
		got.TipHash != fmt.Sprintf("%x", tipHash[:]) || got.TipHeight != 2 ||
		len(got.Anchors) != 2 || got.Anchors[0].ID != "checkpoint-a" ||
		got.Anchors[1].ID != "checkpoint-b" {
		t.Fatalf("unexpected proof: %+v", got)
	}
}

func TestWriteChainProofRejectsWrongGenesisAndNoncanonicalAnchors(t *testing.T) {
	stage := t.TempDir()
	genesisHash, middleHash, tipHash, tipHeight := writeProofStore(t, filepath.Join(stage, "chain.db"))
	cases := []struct {
		name    string
		genesis string
		height  uint64
		hash    string
	}{
		{"wrong genesis", repeatHex('f', 64), 1, fmt.Sprintf("%x", middleHash[:])},
		{"wrong canonical hash", fmt.Sprintf("%x", genesisHash[:]), 1, repeatHex('a', 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			anchorsPath := filepath.Join(t.TempDir(), "anchors.json")
			data := fmt.Sprintf(`{"genesis_hash":%q,"anchors":[{"id":"anchor","height":%d,"hash":%q}]}`,
				tc.genesis, tc.height, tc.hash)
			if err := os.WriteFile(anchorsPath, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(t.TempDir(), "proof.json")
			if err := writeChainProof(filepath.Join(stage, "chain.db"), anchorsPath, output,
				fmt.Sprintf("%x", tipHash[:]), tipHeight); err == nil {
				t.Fatal("writeChainProof accepted an invalid genesis or anchor")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("proof output exists after failure: %v", err)
			}
		})
	}
}

func TestWriteChainProofRejectsDisconnectedIndividuallyValidCanonicalEntries(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chain.db")
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, Timestamp: 1, BaseFee: 1}}
	middle := &core.Block{Header: core.BlockHeader{
		Height: 1, PrevHash: genesis.Hash(), Timestamp: 2, BaseFee: 1,
	}}
	wrongParent := crypto.Hash32{77}
	tip := &core.Block{Header: core.BlockHeader{
		Height: 2, PrevHash: wrongParent, Timestamp: 3, BaseFee: 1,
	}}
	writeProofBlocks(t, dbPath, genesis, middle, tip)
	middleHash, tipHash := middle.Hash(), tip.Hash()
	anchorsPath := writeAnchorFixture(t, genesis.Hash(), []anchor{{
		ID: "cleanup", Height: 1, Hash: fmt.Sprintf("%x", middleHash[:]),
	}})
	err := writeChainProof(dbPath, anchorsPath, filepath.Join(t.TempDir(), "proof.json"),
		fmt.Sprintf("%x", tipHash[:]), 2)
	if err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("writeChainProof error = %v, want disconnected canonical ancestry", err)
	}
}

func TestWriteChainProofRejectsBrokenPrevHashWithinRequiredRange(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chain.db")
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, Timestamp: 1, BaseFee: 1}}
	wrongParent := crypto.Hash32{78}
	middle := &core.Block{Header: core.BlockHeader{
		Height: 1, PrevHash: wrongParent, Timestamp: 2, BaseFee: 1,
	}}
	tip := &core.Block{Header: core.BlockHeader{
		Height: 2, PrevHash: middle.Hash(), Timestamp: 3, BaseFee: 1,
	}}
	writeProofBlocks(t, dbPath, genesis, middle, tip)
	genesisHash, tipHash := genesis.Hash(), tip.Hash()
	anchorsPath := writeAnchorFixture(t, genesisHash, []anchor{{
		ID: "genesis-anchor", Height: 0, Hash: fmt.Sprintf("%x", genesisHash[:]),
	}})
	err := writeChainProof(dbPath, anchorsPath, filepath.Join(t.TempDir(), "proof.json"),
		fmt.Sprintf("%x", tipHash[:]), 2)
	if err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("writeChainProof error = %v, want broken PrevHash rejection", err)
	}
}

func TestWriteChainProofRejectsMissingIndexAndPrunedAnchorBody(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chain.db")
	genesisHash, tipHash, tipHeight, missingHash := writeGappedProofStore(t, dbPath)
	anchorsPath := filepath.Join(t.TempDir(), "anchors.json")
	anchorsJSON := fmt.Sprintf(
		`{"genesis_hash":"%x","anchors":[{"id":"missing","height":2,"hash":"%x"}]}`,
		genesisHash[:], missingHash[:])
	if err := os.WriteFile(anchorsPath, []byte(anchorsJSON), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "proof.json")
	if err := writeChainProof(dbPath, anchorsPath, output, fmt.Sprintf("%x", tipHash[:]), tipHeight); err == nil {
		t.Fatal("writeChainProof guessed a missing canonical height index")
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RepairHeightIndex(2, missingHash); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeChainProof(dbPath, anchorsPath, output, fmt.Sprintf("%x", tipHash[:]), tipHeight); err == nil {
		t.Fatal("writeChainProof accepted a canonical height index without its pruned block body")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("proof output exists after missing/pruned anchor failure: %v", err)
	}
}

func TestWriteChainProofWithNoAnchorsChecksOnlyGenesisAndTipIdentities(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chain.db")
	genesisHash, tipHash, tipHeight, _ := writeGappedProofStore(t, dbPath)
	anchorsPath := writeAnchorFixture(t, genesisHash, []anchor{})
	output := filepath.Join(t.TempDir(), "proof.json")
	if err := writeChainProof(dbPath, anchorsPath, output, fmt.Sprintf("%x", tipHash[:]), tipHeight); err != nil {
		t.Fatalf("writeChainProof with no anchors: %v", err)
	}
	var got proof
	if err := jsonFile(output, &got); err != nil {
		t.Fatal(err)
	}
	if got.TipHeight != tipHeight || len(got.Anchors) != 0 {
		t.Fatalf("unexpected empty-anchor proof: %+v", got)
	}
}

func TestReadAnchorsRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	for _, contents := range []string{
		`{"genesis_hash":"` + repeatHex('a', 64) + `","anchors":[],"extra":true}`,
		`{"genesis_hash":"` + repeatHex('a', 64) + `","anchors":[]} {}`,
	} {
		path := filepath.Join(t.TempDir(), "anchors.json")
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readAnchors(path); err == nil {
			t.Fatalf("readAnchors accepted invalid input: %s", contents)
		}
	}
}

func TestVerifyLegacyStageValid(t *testing.T) {
	root := t.TempDir()
	testnet := filepath.Join(root, "testnet")
	if err := os.Mkdir(testnet, 0700); err != nil {
		t.Fatal(err)
	}
	hash := crypto.Hash32{7, 8, 9}
	writeSmallStore(t, filepath.Join(testnet, "chain.db"), hash, 21)
	if err := os.WriteFile(filepath.Join(root, "explorer_db.dump"), []byte("legacy dump"), 0600); err != nil {
		t.Fatal(err)
	}

	gotHash, gotHeight, err := verifyLegacyStage(root)
	if err != nil {
		t.Fatalf("verifyLegacyStage: %v", err)
	}
	if gotHash != fmt.Sprintf("%x", hash[:]) || gotHeight != 21 {
		t.Fatalf("got tip %s/%d, want %x/21", gotHash, gotHeight, hash[:])
	}
}

func TestVerifyLegacyStageRejectsUnreadableDB(t *testing.T) {
	root := makeLegacyRoot(t)
	if err := os.Mkdir(filepath.Join(root, "testnet", "chain.db"), 0700); err != nil {
		t.Fatal(err)
	}
	writeLegacyDump(t, root)
	if _, _, err := verifyLegacyStage(root); err == nil {
		t.Fatal("verifyLegacyStage succeeded with an unreadable database")
	}
}

func TestVerifyLegacyStageRejectsSymlink(t *testing.T) {
	root := makeLegacyRoot(t)
	testnet := filepath.Join(root, "testnet")
	external := filepath.Join(t.TempDir(), "chain.db")
	writeSmallStore(t, external, crypto.Hash32{2}, 1)
	if err := os.Symlink(external, filepath.Join(testnet, "chain.db")); err != nil {
		t.Skipf("cannot create test symlink: %v", err)
	}
	writeLegacyDump(t, root)
	if _, _, err := verifyLegacyStage(root); err == nil {
		t.Fatal("verifyLegacyStage accepted a symlinked chain.db")
	}
}

func TestVerifyLegacyStageRejectsMissingDump(t *testing.T) {
	root := makeLegacyRoot(t)
	writeSmallStore(t, filepath.Join(root, "testnet", "chain.db"), crypto.Hash32{3}, 2)
	if _, _, err := verifyLegacyStage(root); err == nil {
		t.Fatal("verifyLegacyStage succeeded without explorer_db.dump")
	}
}

func TestSelectModeRejectsIncompatibleMix(t *testing.T) {
	if _, err := selectMode("/new-format", "/legacy"); err == nil {
		t.Fatal("selectMode accepted both --stage and --legacy-stage")
	}
}

func TestReadManifestStrictValidation(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"unknown field", `{"success":true,"tip_hash":"` + repeatHex('a', 64) + `","tip_height":0,"extra":1}`},
		{"duplicate field", `{"success":true,"success":true,"tip_hash":"` + repeatHex('a', 64) + `","tip_height":0}`},
		{"negative height", `{"success":true,"tip_hash":"` + repeatHex('a', 64) + `","tip_height":-1}`},
		{"fractional height", `{"success":true,"tip_hash":"` + repeatHex('a', 64) + `","tip_height":1.0}`},
		{"trailing JSON", `{"success":true,"tip_hash":"` + repeatHex('a', 64) + `","tip_height":0} {}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, []byte(tc.json), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readManifest(path); err == nil {
				t.Fatal("readManifest accepted invalid JSON")
			}
		})
	}
}

func writeSmallStore(t *testing.T, path string, hash crypto.Hash32, height uint64) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("create test chain.db: %v", err)
	}
	if err := db.PutRawBlock(hash, height, []byte(`{"test":"block"}`)); err != nil {
		_ = db.Close()
		t.Fatalf("write test block: %v", err)
	}
	if err := db.PutTip(hash, height); err != nil {
		_ = db.Close()
		t.Fatalf("write test tip: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close test chain.db: %v", err)
	}
}

func writeProofStore(t *testing.T, path string) (crypto.Hash32, crypto.Hash32, crypto.Hash32, uint64) {
	t.Helper()
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, Timestamp: 1, BaseFee: 1}}
	middle := &core.Block{Header: core.BlockHeader{
		Height: 1, PrevHash: genesis.Hash(), Timestamp: 2, BaseFee: 1,
	}}
	tip := &core.Block{Header: core.BlockHeader{
		Height: 2, PrevHash: middle.Hash(), Timestamp: 3, BaseFee: 1,
	}}
	writeProofBlocks(t, path, genesis, middle, tip)
	return genesis.Hash(), middle.Hash(), tip.Hash(), 2
}

func writeProofBlocks(t *testing.T, path string, blocks ...*core.Block) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("create proof chain.db: %v", err)
	}
	for _, item := range blocks {
		raw, err := json.Marshal(item)
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if err := db.PutRawBlock(item.Hash(), item.Header.Height, raw); err != nil {
			_ = db.Close()
			t.Fatalf("write proof block: %v", err)
		}
	}
	tip := blocks[len(blocks)-1]
	if err := db.PutTip(tip.Hash(), tip.Header.Height); err != nil {
		_ = db.Close()
		t.Fatalf("write proof tip: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close proof chain.db: %v", err)
	}
}

func writeGappedProofStore(t *testing.T, path string) (crypto.Hash32, crypto.Hash32, uint64, crypto.Hash32) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("create gapped proof chain.db: %v", err)
	}
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, Timestamp: 1, BaseFee: 1}}
	block1 := &core.Block{Header: core.BlockHeader{
		Height: 1, PrevHash: genesis.Hash(), Timestamp: 2, BaseFee: 1,
	}}
	missingHash := crypto.Hash32{99}
	block3 := &core.Block{Header: core.BlockHeader{
		Height: 3, PrevHash: missingHash, Timestamp: 3, BaseFee: 1,
	}}
	for _, item := range []*core.Block{genesis, block1, block3} {
		raw, err := json.Marshal(item)
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if err := db.PutRawBlock(item.Hash(), item.Header.Height, raw); err != nil {
			_ = db.Close()
			t.Fatalf("write gapped proof block: %v", err)
		}
	}
	if err := db.PutTip(block3.Hash(), 3); err != nil {
		_ = db.Close()
		t.Fatalf("write gapped proof tip: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close gapped proof chain.db: %v", err)
	}
	return genesis.Hash(), block3.Hash(), 3, missingHash
}

func writeAnchorFixture(t *testing.T, genesis crypto.Hash32, anchors []anchor) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "anchors.json")
	data, err := json.Marshal(anchorsFile{
		Schema: 1, GenesisHash: fmt.Sprintf("%x", genesis[:]), Anchors: anchors,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func jsonFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeManifest(t *testing.T, stage, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), []byte(contents), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func makeLegacyRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "testnet"), 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeLegacyDump(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "explorer_db.dump"), []byte("legacy dump"), 0600); err != nil {
		t.Fatal(err)
	}
}

func repeatHex(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
