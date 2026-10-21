package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestReadWitnessKeyCopiesBeforeZeroingInput(t *testing.T) {
	privateKey, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "witness.key")
	if err := os.WriteFile(path, privateKey.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := readWitnessKey(path)
	if err != nil {
		t.Fatalf("read witness key: %v", err)
	}
	defer crypto.ZeroBytes(loaded.Bytes())
	if !loaded.Public().Equals(privateKey.Public()) {
		t.Fatal("loaded key changed after the source buffer was cleared")
	}
}

func TestReadWitnessKeyRejectsGroupReadableFile(t *testing.T) {
	privateKey, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "witness.key")
	if err := os.WriteFile(path, privateKey.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readWitnessKey(path); err == nil || !strings.Contains(err.Error(), "no group/other permissions") {
		t.Fatalf("unsafe key file was accepted: %v", err)
	}
}

func TestRunAssembleWritesQuorumCheckpoint(t *testing.T) {
	first, firstPublic, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	second, secondPublic, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	authorities := []crypto.ValidatorPubKey{firstPublic, secondPublic}
	draft, err := avm.NewQuorumCheckpointDraft(
		1,
		10,
		crypto.HashStr("genesis"),
		crypto.HashStr("tip"),
		crypto.HashStr("write-set"),
		[]avm.CheckpointEntry{{Key: []byte("contract/state"), Value: []byte("value")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	candidate := filepath.Join(dir, "candidate.json")
	if err := avm.WriteCheckpointFile(candidate, draft); err != nil {
		t.Fatal(err)
	}
	signatureFiles := make([]string, 0, 2)
	for i, signer := range []crypto.ValidatorPrivKey{first, second} {
		signature, err := avm.SignCheckpointDraft(draft, 1, signer)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "witness-"+strconv.Itoa(i+1)+".json")
		if err := writeWitnessSignature(path, signature); err != nil {
			t.Fatal(err)
		}
		signatureFiles = append(signatureFiles, path)
	}
	out := filepath.Join(dir, "assembled.json")
	authorityHex := []string{authorities[0].Hex(), authorities[1].Hex()}
	if err := runAssemble(candidate, signatureFiles, authorityHex, 2, out); err != nil {
		t.Fatalf("assemble signatures: %v", err)
	}
	checkpoint, found, err := avm.ReadCheckpointFile(out)
	if err != nil || !found {
		t.Fatalf("read assembled checkpoint found=%v err=%v", found, err)
	}
	if err := avm.VerifyCheckpointQuorum(checkpoint, 1, authorities, 2); err != nil {
		t.Fatalf("assembled checkpoint failed quorum validation: %v", err)
	}
}

func TestRunSignRequiresMatchingIndependentCanonicalReplay(t *testing.T) {
	dbPath, err := makeCheckpointTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	witnessDBPath := copyDatabaseForWitness(t, dbPath)
	dir := t.TempDir()
	candidatePath := filepath.Join(dir, "draft.json")
	if err := runBuild(dbPath, 1, candidatePath); err != nil {
		t.Fatalf("build draft: %v", err)
	}
	witnessKey, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "witness.key")
	if err := os.WriteFile(keyPath, witnessKey.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	signedPath := filepath.Join(dir, "partial.json")
	if err := runSign(witnessDBPath, 1, candidatePath, keyPath, signedPath); err != nil {
		t.Fatalf("sign independently replayed candidate: %v", err)
	}
	partial, err := readWitnessSignature(signedPath)
	if err != nil || !partial.WitnessPublicKey.Equals(witnessKey.Public()) {
		t.Fatalf("read partial signature=%+v err=%v", partial, err)
	}

	draft, found, err := avm.ReadCheckpointFile(candidatePath)
	if err != nil || !found {
		t.Fatalf("read draft found=%v err=%v", found, err)
	}
	draft.BlockHash = crypto.HashStr("not canonical")
	forgedPath := filepath.Join(dir, "forged.json")
	if err := avm.WriteCheckpointFile(forgedPath, draft); err != nil {
		t.Fatal(err)
	}
	if err := runSign(witnessDBPath, 1, forgedPath, keyPath, filepath.Join(dir, "forged-signature.json")); err == nil ||
		!strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("witness signed a draft not found in its own canonical chain: %v", err)
	}
}

func copyDatabaseForWitness(t *testing.T, source string) string {
	t.Helper()
	destination := t.TempDir()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected non-regular database fixture file %q", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copy database for witness: %v", err)
	}
	return destination
}

func makeCheckpointTestDB(t *testing.T) (string, error) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		return "", err
	}
	defer db.Close()
	producerKey, producerPublic, err := crypto.GenerateValidatorKey()
	if err != nil {
		return "", err
	}
	genesis := &core.Block{Header: core.BlockHeader{
		Height:     0,
		Timestamp:  1,
		MerkleRoot: core.MerkleRoot(nil),
	}}
	genesisRaw, err := json.Marshal(genesis)
	if err != nil {
		return "", err
	}
	if err := db.PutRawBlock(genesis.Hash(), 0, genesisRaw); err != nil {
		return "", err
	}
	if err := db.PutTip(genesis.Hash(), 0); err != nil {
		return "", err
	}
	block := &core.Block{Header: core.BlockHeader{
		Height:       1,
		PrevHash:     genesis.Hash(),
		Timestamp:    2,
		Round:        1,
		ValidatorPub: producerPublic,
		MerkleRoot:   core.MerkleRoot(nil),
	}}
	if err := block.Header.Sign(producerKey); err != nil {
		return "", err
	}
	prepared, err := avm.NewBlockExecutor(avm.LevelStore{DB: db}).PrepareBlock(context.Background(), block)
	if err != nil {
		return "", err
	}
	blockRaw, err := json.Marshal(block)
	if err != nil {
		return "", err
	}
	if err := avm.CommitCanonicalBlock(db, block, blockRaw, prepared); err != nil {
		return "", err
	}
	return dir, nil
}
