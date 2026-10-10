package avm

import (
	"context"
	stded25519 "crypto/ed25519"
	"testing"

	"github.com/aperod/aperod/core"
)

func TestBlockExecutorDeploysWithNonceAndAtomicOverlay(t *testing.T) {
	store := NewMemoryStore()
	executor := NewBlockExecutor(store)
	tx := signedAVMTransaction(t, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(false))
	block := &core.Block{Header: core.BlockHeader{Height: 9}, Txs: []core.Transaction{tx}}

	prepared, err := executor.PrepareBlock(context.Background(), block)
	if err != nil {
		t.Fatalf("PrepareBlock: %v", err)
	}
	if len(prepared.Receipts) != 1 || len(prepared.Writes) == 0 {
		t.Fatalf("prepared receipts=%d writes=%d", len(prepared.Receipts), len(prepared.Writes))
	}
	if _, found, getErr := store.Get(contractCodeKey(tx.AVM.ContractID)); getErr != nil || found {
		t.Fatalf("prepare mutated base store: found=%v err=%v", found, getErr)
	}
	if err := store.Apply(prepared.Writes); err != nil {
		t.Fatalf("commit prepared writes: %v", err)
	}
	if code, found, getErr := store.Get(contractCodeKey(tx.AVM.ContractID)); getErr != nil || !found || len(code) == 0 {
		t.Fatalf("deployed code missing: found=%v err=%v", found, getErr)
	}
	if nonce, nonceErr := loadNonce(store, tx.AVM.Signer); nonceErr != nil || nonce != 1 {
		t.Fatalf("nonce=%d err=%v, want 1", nonce, nonceErr)
	}
}

func TestBlockExecutorRejectsReplayAndLeavesBaseUnchanged(t *testing.T) {
	store := NewMemoryStore()
	executor := NewBlockExecutor(store)
	first := signedAVMTransaction(t, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(false))
	second := first
	block := &core.Block{Header: core.BlockHeader{Height: 10}, Txs: []core.Transaction{first, second}}
	if _, err := executor.PrepareBlock(context.Background(), block); err == nil {
		t.Fatal("same-nonce replay accepted")
	}
	if len(store.data) != 0 {
		t.Fatalf("failed block mutated base state: %d entries", len(store.data))
	}
}

func TestBlockExecutorDeterministicCommitment(t *testing.T) {
	tx := signedAVMTransaction(t, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(false))
	block := &core.Block{Header: core.BlockHeader{Height: 11}, Txs: []core.Transaction{tx}}
	first, err := NewBlockExecutor(NewMemoryStore()).PrepareBlock(context.Background(), block)
	if err != nil {
		t.Fatalf("first PrepareBlock: %v", err)
	}
	second, err := NewBlockExecutor(NewMemoryStore()).PrepareBlock(context.Background(), block)
	if err != nil {
		t.Fatalf("second PrepareBlock: %v", err)
	}
	if first.WriteSetCommitment != second.WriteSetCommitment {
		t.Fatalf("write-set commitments differ: %x != %x", first.WriteSetCommitment, second.WriteSetCommitment)
	}
}

func signedAVMTransaction(t *testing.T, action core.AVMAction, contractID [32]byte, nonce uint64, code []byte) core.Transaction {
	t.Helper()
	publicKey, privateKey, err := stded25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	var signer [32]byte
	copy(signer[:], publicKey)
	payload := &core.AVMPayload{
		Action:     action,
		ContractID: contractID,
		Code:       code,
		Entry:      "run",
		GasLimit:   1_000,
		Nonce:      nonce,
		Signer:     signer,
		AccessList: []core.AVMAccess{{Key: []byte("key"), Write: true}},
	}
	if action == core.AVMDeployContract {
		payload.ContractID = core.DeriveAVMContractID(signer, nonce, code)
	}
	signingHash := payload.SigningHash()
	copy(payload.Signature[:], stded25519.Sign(privateKey, signingHash[:]))
	return core.Transaction{Version: core.TxVersionAVM, AVM: payload}
}
