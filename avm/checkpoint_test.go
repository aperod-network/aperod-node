package avm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type checkpointTestChain struct {
	db          *store.DB
	dir         string
	privateKey  crypto.ValidatorPrivKey
	authorities []crypto.ValidatorPubKey
	activation  uint64
}

func newCheckpointTestChain(t *testing.T) *checkpointTestChain {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open DB: %v", err)
	}
	privateKey, publicKey, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("generate validator key: %v", err)
	}
	genesis := &core.Block{Header: core.BlockHeader{
		Height:     0,
		Timestamp:  1,
		MerkleRoot: core.MerkleRoot(nil),
	}}
	genesisRaw, err := json.Marshal(genesis)
	if err != nil {
		t.Fatalf("marshal genesis: %v", err)
	}
	if err := db.PutRawBlock(genesis.Hash(), 0, genesisRaw); err != nil {
		t.Fatalf("store genesis: %v", err)
	}
	if err := db.PutTip(genesis.Hash(), 0); err != nil {
		t.Fatalf("store genesis tip: %v", err)
	}
	chain := &checkpointTestChain{
		db:          db,
		dir:         dir,
		privateKey:  privateKey,
		authorities: []crypto.ValidatorPubKey{publicKey},
		activation:  1,
	}
	t.Cleanup(func() { _ = chain.db.Close() })
	return chain
}

func (c *checkpointTestChain) appendBlock(t *testing.T, height uint64, previous crypto.Hash32, txs []core.Transaction) crypto.Hash32 {
	t.Helper()
	block := &core.Block{
		Header: core.BlockHeader{
			Height:       height,
			PrevHash:     previous,
			Timestamp:    int64(height + 1),
			Round:        uint32(height),
			ValidatorPub: c.privateKey.Public(),
			MerkleRoot:   core.MerkleRoot(txs),
		},
		Txs: txs,
	}
	if err := block.Header.Sign(c.privateKey); err != nil {
		t.Fatalf("sign block %d: %v", height, err)
	}
	prepared, err := NewBlockExecutor(LevelStore{DB: c.db}).PrepareBlock(context.Background(), block)
	if err != nil {
		t.Fatalf("prepare block %d: %v", height, err)
	}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal block %d: %v", height, err)
	}
	if err := CommitCanonicalBlock(c.db, block, raw, prepared); err != nil {
		t.Fatalf("commit block %d: %v", height, err)
	}
	return block.Hash()
}

func (c *checkpointTestChain) appendDeployAndEmptyBlocks(t *testing.T, through uint64, checkpointAt uint64, checkpointPath string) {
	t.Helper()
	prev, _, err := c.db.GetTip()
	if err != nil {
		t.Fatalf("read initial tip: %v", err)
	}
	for height := uint64(1); height <= through; height++ {
		var txs []core.Transaction
		if height == 1 {
			txs = []core.Transaction{
				signedAVMTransaction(t, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(false)),
			}
		}
		prev = c.appendBlock(t, height, prev, txs)
		if height == checkpointAt {
			checkpoint, err := CreateCheckpointFromCanonicalState(
				c.db, c.activation, height, prev, c.privateKey, c.authorities,
			)
			if err != nil {
				t.Fatalf("create checkpoint at %d: %v", height, err)
			}
			if err := WriteCheckpointFile(checkpointPath, checkpoint); err != nil {
				t.Fatalf("write checkpoint at %d: %v", height, err)
			}
		}
	}
}

func TestCheckpointStateCommitmentIsOrderBoundAndDeterministic(t *testing.T) {
	entries := []CheckpointEntry{
		{Key: []byte("a"), Value: []byte("one")},
		{Key: []byte("b"), Value: []byte("two")},
	}
	first, err := StateSnapshotCommitment(entries)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StateSnapshotCommitment(entries)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("identical AVM snapshots produced different commitments")
	}
	if _, err := StateSnapshotCommitment([]CheckpointEntry{
		{Key: []byte("b"), Value: []byte("two")},
		{Key: []byte("a"), Value: []byte("one")},
	}); err == nil {
		t.Fatal("unordered snapshot was accepted")
	}
	if _, err := StateSnapshotCommitment([]CheckpointEntry{
		{Key: []byte("a"), Value: []byte("one")},
		{Key: []byte("a"), Value: []byte("two")},
	}); err == nil {
		t.Fatal("duplicate state key was accepted")
	}
}

func TestEnsureCanonicalStateUsesSignedCheckpointAndSurvivesRestart(t *testing.T) {
	chain := newCheckpointTestChain(t)
	checkpointPath := filepath.Join(t.TempDir(), "avm-checkpoint.json")
	chain.appendDeployAndEmptyBlocks(t, 4, 2, checkpointPath)

	rebuilt, err := EnsureCanonicalStateWithCheckpoint(
		context.Background(),
		chain.db,
		chain.activation,
		4,
		mustCanonicalHash(t, chain.db, 4),
		checkpointPath,
		chain.authorities,
		chain.privateKey,
		2,
	)
	if err != nil || rebuilt {
		t.Fatalf("bounded recovery rebuilt=%v err=%v", rebuilt, err)
	}
	checkpoint, found, err := ReadCheckpointFile(checkpointPath)
	if err != nil || !found || checkpoint.Height != 4 {
		t.Fatalf("refreshed checkpoint height=%v found=%v err=%v", checkpoint, found, err)
	}

	if err := chain.db.Close(); err != nil {
		t.Fatalf("close DB: %v", err)
	}
	chain.db, err = store.Open(chain.dir)
	if err != nil {
		t.Fatalf("reopen DB: %v", err)
	}
	rebuilt, err = EnsureCanonicalStateWithCheckpoint(
		context.Background(),
		chain.db,
		chain.activation,
		4,
		mustCanonicalHash(t, chain.db, 4),
		checkpointPath,
		chain.authorities,
		chain.privateKey,
		2,
	)
	if err != nil || rebuilt {
		t.Fatalf("restart recovery rebuilt=%v err=%v", rebuilt, err)
	}
}

func TestEnsureCanonicalStateRejectsTamperedCheckpoint(t *testing.T) {
	chain := newCheckpointTestChain(t)
	checkpointPath := filepath.Join(t.TempDir(), "avm-checkpoint.json")
	chain.appendDeployAndEmptyBlocks(t, 2, 2, checkpointPath)
	checkpoint, found, err := ReadCheckpointFile(checkpointPath)
	if err != nil || !found {
		t.Fatalf("read checkpoint found=%v err=%v", found, err)
	}
	checkpoint.Entries[0].Value = append(checkpoint.Entries[0].Value, 'x')
	checkpoint.StateCommitment, err = StateSnapshotCommitment(checkpoint.Entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCheckpointFile(checkpointPath, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCanonicalStateWithCheckpoint(
		context.Background(),
		chain.db,
		chain.activation,
		2,
		mustCanonicalHash(t, chain.db, 2),
		checkpointPath,
		chain.authorities,
		chain.privateKey,
		2,
	); err == nil || !strings.Contains(err.Error(), "invalid trusted witness signature") {
		t.Fatalf("tampered checkpoint was not rejected by signature: %v", err)
	}
}

func TestEnsureCanonicalStateRejectsCheckpointFromFork(t *testing.T) {
	chain := newCheckpointTestChain(t)
	checkpointPath := filepath.Join(t.TempDir(), "avm-checkpoint.json")
	chain.appendDeployAndEmptyBlocks(t, 3, 2, checkpointPath)
	checkpoint, found, err := ReadCheckpointFile(checkpointPath)
	if err != nil || !found {
		t.Fatalf("read checkpoint found=%v err=%v", found, err)
	}
	forkHash := crypto.HashStr("different canonical fork")
	forkCheckpoint, err := SignCheckpoint(
		checkpoint.ActivationHeight,
		checkpoint.Height,
		checkpoint.GenesisHash,
		forkHash,
		checkpoint.WriteSetCommitment,
		checkpoint.Entries,
		chain.privateKey,
		chain.authorities,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCheckpointFile(checkpointPath, forkCheckpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCanonicalStateWithCheckpoint(
		context.Background(),
		chain.db,
		chain.activation,
		3,
		mustCanonicalHash(t, chain.db, 3),
		checkpointPath,
		chain.authorities,
		chain.privateKey,
		2,
	); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("fork checkpoint was not rejected: %v", err)
	}
}

func TestEnsureCanonicalStateFailsClosedWhenCheckpointMissingAndReplayTooLong(t *testing.T) {
	chain := newCheckpointTestChain(t)
	chain.appendDeployAndEmptyBlocks(t, 3, 0, "")
	checkpointPath := filepath.Join(t.TempDir(), "missing-checkpoint.json")
	if _, err := os.Stat(checkpointPath); !os.IsNotExist(err) {
		t.Fatalf("expected checkpoint to be absent, stat err=%v", err)
	}
	if _, err := EnsureCanonicalStateWithCheckpoint(
		context.Background(),
		chain.db,
		chain.activation,
		3,
		mustCanonicalHash(t, chain.db, 3),
		checkpointPath,
		chain.authorities,
		chain.privateKey,
		2,
	); err == nil || !strings.Contains(err.Error(), "checkpoint is missing") {
		t.Fatalf("missing checkpoint did not stop over-limit replay: %v", err)
	}
}

func TestBuildVerifiedCheckpointFromCanonicalRoundTrips(t *testing.T) {
	chain := newCheckpointTestChain(t)
	chain.appendDeployAndEmptyBlocks(t, 1, 0, "")
	tipHash := mustCanonicalHash(t, chain.db, 1)
	checkpoint, err := BuildVerifiedCheckpointFromCanonical(
		context.Background(),
		chain.db,
		chain.activation,
		1,
		tipHash,
		chain.privateKey,
		chain.authorities,
	)
	if err != nil {
		t.Fatalf("build checkpoint: %v", err)
	}
	if err := VerifyCheckpoint(checkpoint, chain.activation, chain.authorities); err != nil {
		t.Fatalf("verify checkpoint: %v", err)
	}
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := WriteCheckpointFile(path, checkpoint); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	reloaded, found, err := ReadCheckpointFile(path)
	if err != nil || !found {
		t.Fatalf("read checkpoint found=%v err=%v", found, err)
	}
	if err := VerifyCheckpoint(reloaded, chain.activation, chain.authorities); err != nil {
		t.Fatalf("verify reloaded checkpoint: %v", err)
	}
	if reloaded.Height != 1 || reloaded.BlockHash != tipHash ||
		reloaded.StateCommitment != checkpoint.StateCommitment {
		t.Fatal("checkpoint round-trip changed its canonical binding")
	}
}

func TestReadCheckpointFileRejects512MiBPlusOneSparseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized-checkpoint.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxAVMCheckpointFileBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err := ReadCheckpointFile(path)
	if err == nil || found || checkpoint != nil ||
		!strings.Contains(err.Error(), "outside supported bounds") {
		t.Fatalf("oversized checkpoint not rejected before decode: found=%v checkpoint=%v err=%v", found, checkpoint, err)
	}
}

func TestOversizedCheckpointWritePreservesExistingCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "avm-checkpoint.json")
	valid, err := NewQuorumCheckpointDraft(
		1,
		10,
		crypto.HashStr("genesis"),
		crypto.HashStr("tip"),
		crypto.HashStr("write-set"),
		[]CheckpointEntry{{Key: []byte("contract/state"), Value: []byte("value")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCheckpointFile(path, valid); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCheckpointFileWithLimit(path, valid, 32); err == nil ||
		!strings.Contains(err.Error(), "encoded file exceeds 32 bytes") {
		t.Fatalf("oversized checkpoint write was not rejected: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed oversized write replaced the prior checkpoint")
	}
	checkpoint, found, err := ReadCheckpointFile(path)
	if err != nil || !found || !CheckpointPayloadEqual(checkpoint, valid) {
		t.Fatalf("previous checkpoint became unreadable: found=%v err=%v", found, err)
	}
}

func TestBuildVerifiedCheckpointFromCanonicalRejectsInvalidBlockSignature(t *testing.T) {
	chain := newCheckpointTestChain(t)
	chain.appendDeployAndEmptyBlocks(t, 1, 0, "")
	blockHash := mustCanonicalHash(t, chain.db, 1)
	raw, err := chain.db.GetRawBlock(blockHash)
	if err != nil {
		t.Fatal(err)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatal(err)
	}
	block.Header.Signature[0] ^= 0x01
	raw, err = json.Marshal(&block)
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.db.PutRawBlock(blockHash, 1, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildVerifiedCheckpointFromCanonical(
		context.Background(),
		chain.db,
		chain.activation,
		1,
		blockHash,
		chain.privateKey,
		chain.authorities,
	); err == nil || !strings.Contains(err.Error(), "invalid validator signature") {
		t.Fatalf("builder accepted invalid block signature: %v", err)
	}
}

func TestEnsureCanonicalStateRejectsFullReplayOverLimit(t *testing.T) {
	if _, err := EnsureCanonicalState(
		context.Background(),
		nil,
		1,
		MaxAVMCheckpointReplay+1,
		crypto.Hash32{},
	); err == nil || !strings.Contains(err.Error(), "authenticated checkpoint is required") {
		t.Fatalf("over-limit replay was not refused before database access: %v", err)
	}
}

func mustCanonicalHash(t *testing.T, db *store.DB, height uint64) crypto.Hash32 {
	t.Helper()
	hash, found, err := db.GetCanonicalHash(height)
	if err != nil || !found {
		t.Fatalf("canonical hash at %d: found=%v err=%v", height, found, err)
	}
	return hash
}
