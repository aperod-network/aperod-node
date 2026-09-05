package avm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestCommitCanonicalBlockPersistsBlockTipAndAVMAtomically(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	levelStore := LevelStore{DB: db}
	tx := signedAVMTransaction(t, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(false))
	block := &core.Block{Header: core.BlockHeader{Height: 12}, Txs: []core.Transaction{tx}}
	prepared, err := NewBlockExecutor(levelStore).PrepareBlock(context.Background(), block)
	if err != nil {
		t.Fatalf("PrepareBlock: %v", err)
	}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := CommitCanonicalBlock(db, block, raw, prepared); err != nil {
		t.Fatalf("CommitCanonicalBlock: %v", err)
	}

	hash := block.Hash()
	if stored, getErr := db.GetRawBlock(hash); getErr != nil || len(stored) == 0 {
		t.Fatalf("raw block missing: len=%d err=%v", len(stored), getErr)
	}
	tipHash, tipHeight, tipErr := db.GetTip()
	if tipErr != nil || tipHash != hash || tipHeight != 12 {
		t.Fatalf("tip=(%x,%d) err=%v", tipHash, tipHeight, tipErr)
	}
	if code, found, getErr := levelStore.Get(contractCodeKey(tx.AVM.ContractID)); getErr != nil || !found || len(code) == 0 {
		t.Fatalf("AVM code missing: found=%v err=%v", found, getErr)
	}
	markerHash, markerCommitment, found, markerErr := db.GetAVMCommitment(12)
	if markerErr != nil || !found || markerHash != hash || markerCommitment != prepared.WriteSetCommitment {
		t.Fatalf("marker mismatch: found=%v err=%v", found, markerErr)
	}
}

func TestEnsureCanonicalStateRebuildsInterruptedCommitAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx := signedAVMTransaction(t, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(false))
	block := &core.Block{Header: core.BlockHeader{Height: 1}, Txs: []core.Transaction{tx}}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	prepared, err := NewBlockExecutor(NewMemoryStore()).PrepareBlock(context.Background(), block)
	if err != nil {
		t.Fatalf("PrepareBlock: %v", err)
	}
	raw, _ := json.Marshal(block)

	// Model an old/interrupted commit that made the canonical block and tip
	// durable without its AVM state or commitment marker.
	if err := db.PutRawBlock(block.Hash(), block.Header.Height, raw); err != nil {
		t.Fatalf("PutRawBlock: %v", err)
	}
	if err := db.PutTip(block.Hash(), block.Header.Height); err != nil {
		t.Fatalf("PutTip: %v", err)
	}
	if err := db.ApplyAVMState([]store.AVMWrite{{
		Key: []byte("stale-from-interrupted-commit"), Value: []byte("stale"),
	}}); err != nil {
		t.Fatalf("seed stale AVM state: %v", err)
	}
	rebuilt, err := EnsureCanonicalState(context.Background(), db, 1, 1, block.Hash())
	if err != nil || !rebuilt {
		t.Fatalf("EnsureCanonicalState rebuilt=%v err=%v", rebuilt, err)
	}
	if code, found, getErr := db.GetAVMState(contractCodeKey(tx.AVM.ContractID)); getErr != nil || !found || len(code) == 0 {
		t.Fatalf("rebuilt code missing: found=%v err=%v", found, getErr)
	}
	if _, found, getErr := db.GetAVMState([]byte("stale-from-interrupted-commit")); getErr != nil || found {
		t.Fatalf("stale state survived rebuild: found=%v err=%v", found, getErr)
	}
	markerHash, markerCommitment, found, err := db.GetAVMCommitment(1)
	if err != nil || !found || markerHash != block.Hash() ||
		markerCommitment != prepared.WriteSetCommitment {
		t.Fatalf("rebuilt marker invalid: found=%v err=%v", found, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err = store.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	rebuilt, err = EnsureCanonicalState(context.Background(), db, 1, 1, block.Hash())
	if err != nil || rebuilt {
		t.Fatalf("restart verification rebuilt=%v err=%v", rebuilt, err)
	}
}

func TestEnsureCanonicalStateRebuildsMissingStateWithValidMarker(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx := signedAVMTransaction(t, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(false))
	block := &core.Block{Header: core.BlockHeader{Height: 3}, Txs: []core.Transaction{tx}}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	prepared, err := NewBlockExecutor(NewMemoryStore()).PrepareBlock(context.Background(), block)
	if err != nil {
		t.Fatalf("PrepareBlock: %v", err)
	}
	raw, _ := json.Marshal(block)
	if err := CommitCanonicalBlock(db, block, raw, prepared); err != nil {
		t.Fatalf("CommitCanonicalBlock: %v", err)
	}
	deletes := make([]store.AVMWrite, len(prepared.Writes))
	for i, write := range prepared.Writes {
		deletes[i] = store.AVMWrite{Key: write.Key}
	}
	if err := db.ApplyAVMState(deletes); err != nil {
		t.Fatalf("delete AVM state: %v", err)
	}
	rebuilt, err := EnsureCanonicalState(context.Background(), db, 3, 3, block.Hash())
	if err != nil || !rebuilt {
		t.Fatalf("EnsureCanonicalState rebuilt=%v err=%v", rebuilt, err)
	}
}

func TestEnsureCanonicalStateFailsClosedOnMarkerMismatch(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	block := &core.Block{Header: core.BlockHeader{Height: 2}}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	raw, _ := json.Marshal(block)
	wrong := crypto.HashBytes([]byte("wrong commitment"))
	if err := db.CommitRawBlockWithAVM(block.Hash(), 2, raw, nil, wrong); err != nil {
		t.Fatalf("CommitRawBlockWithAVM: %v", err)
	}
	if _, err := EnsureCanonicalState(context.Background(), db, 2, 2, block.Hash()); err == nil ||
		!strings.Contains(err.Error(), "marker mismatch") {
		t.Fatalf("expected marker mismatch, got %v", err)
	}
}

func TestEnsureCanonicalStateFailsClosedOnStateCorruption(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx := signedAVMTransaction(t, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(false))
	block := &core.Block{Header: core.BlockHeader{Height: 4}, Txs: []core.Transaction{tx}}
	block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
	prepared, err := NewBlockExecutor(NewMemoryStore()).PrepareBlock(context.Background(), block)
	if err != nil {
		t.Fatalf("PrepareBlock: %v", err)
	}
	raw, _ := json.Marshal(block)
	if err := CommitCanonicalBlock(db, block, raw, prepared); err != nil {
		t.Fatalf("CommitCanonicalBlock: %v", err)
	}
	if err := db.ApplyAVMState([]store.AVMWrite{{
		Key: contractCodeKey(tx.AVM.ContractID), Value: []byte("corrupt"),
	}}); err != nil {
		t.Fatalf("corrupt state: %v", err)
	}
	if _, err := EnsureCanonicalState(context.Background(), db, 4, 4, block.Hash()); err == nil ||
		!strings.Contains(err.Error(), "state corruption") {
		t.Fatalf("expected state corruption, got %v", err)
	}
}

func TestEnsureCanonicalStateActivationZeroIsDisabled(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	rebuilt, err := EnsureCanonicalState(context.Background(), db, 0, 99, crypto.Hash32{})
	if err != nil || rebuilt {
		t.Fatalf("activation zero rebuilt=%v err=%v", rebuilt, err)
	}
}
