package mainnetbootstrap

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

func TestInspectKeyImageAnchorBindsCanonicalHeightAndBytes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chain.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	block := &core.Block{Header: core.BlockHeader{
		Height:     1,
		MerkleRoot: core.MerkleRoot(nil),
	}}
	rawBlock, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	hash := block.Hash()
	if err := db.PutRawBlock(hash, 1, rawBlock); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	anchor := struct {
		Height    uint64   `json:"height"`
		BlockHash string   `json:"block_hash"`
		KeyImages [][]byte `json:"key_images"`
	}{
		Height:    1,
		BlockHash: fmt.Sprintf("%x", hash),
		KeyImages: [][]byte{},
	}
	anchorBytes, err := json.Marshal(anchor)
	if err != nil {
		t.Fatal(err)
	}
	anchorPath := filepath.Join(t.TempDir(), "key-images.json")
	if err := os.WriteFile(anchorPath, anchorBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	digest, gotHash, gotHeight, err := InspectKeyImageAnchor(dbPath, anchorPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(anchorBytes)
	if digest != fmt.Sprintf("%x", sum) || gotHash != fmt.Sprintf("%x", hash) || gotHeight != 1 {
		t.Fatalf("unexpected anchor evidence: digest=%s hash=%s height=%d", digest, gotHash, gotHeight)
	}

	statement := validStatement()
	statement.CheckpointHeight = 2
	statement.KeyImageAnchorSHA256 = digest
	statement.KeyImageAnchorBlockHash = gotHash
	statement.KeyImageAnchorHeight = gotHeight
	if err := VerifyKeyImageAnchor(dbPath, statement, anchorPath); err != nil {
		t.Fatalf("matching signed key-image anchor rejected: %v", err)
	}
	if err := VerifyKeyImageAnchor(dbPath, statement, ""); err == nil {
		t.Fatal("signed key-image anchor accepted without its source file")
	}
	statement.KeyImageAnchorSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("different anchor")))
	if err := VerifyKeyImageAnchor(dbPath, statement, anchorPath); err == nil {
		t.Fatal("key-image anchor with a different signed digest accepted")
	}
}

func TestInspectKeyImageAnchorRejectsDuplicateFields(t *testing.T) {
	raw := []byte(`{"height":1,"height":1,"block_hash":"` + fmt.Sprintf("%064x", 1) + `","key_images":[]}`)
	if _, err := decodeKeyImageAnchor(raw); err == nil {
		t.Fatal("duplicate key-image anchor field accepted")
	}
}
