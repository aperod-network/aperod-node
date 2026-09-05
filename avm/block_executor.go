package avm

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

var (
	codePrefix    = []byte{0xff, 'a', 'v', 'm', '/', 'c', '/'}
	noncePrefix   = []byte{0xff, 'a', 'v', 'm', '/', 'n', '/'}
	receiptPrefix = []byte{0xff, 'a', 'v', 'm', '/', 'r', '/'}
)

type Receipt struct {
	TxHash      crypto.Hash32
	ContractID  crypto.Hash32
	GasUsed     uint64
	ReturnData  []byte
	StateWrites int
}

// PreparedBlock contains deterministic AVM effects that have not yet reached
// durable state. Consensus must persist Writes atomically with the block body,
// canonical height index and tip.
type PreparedBlock struct {
	Height             uint64
	BlockHash          crypto.Hash32
	Receipts           []Receipt
	Writes             []Write
	WriteSetCommitment crypto.Hash32
}

type BlockExecutor struct {
	store Store
}

func NewBlockExecutor(store Store) *BlockExecutor {
	return &BlockExecutor{store: store}
}

func (e *BlockExecutor) PrepareBlock(ctx context.Context, block *core.Block) (*PreparedBlock, error) {
	if block == nil {
		return nil, fmt.Errorf("avm: nil block")
	}
	overlay := NewOverlayStore(e.store)
	engine := NewEngine(overlay)
	defer engine.Close(ctx)
	prepared := &PreparedBlock{
		Height:    block.Header.Height,
		BlockHash: block.Hash(),
	}

	for index := range block.Txs {
		tx := &block.Txs[index]
		if !tx.IsAVM() {
			continue
		}
		if err := tx.AVM.Validate(); err != nil {
			return nil, fmt.Errorf("avm: block %d tx[%d] payload: %w", block.Header.Height, index, err)
		}
		payload := tx.AVM
		expectedNonce, err := loadNonce(overlay, payload.Signer)
		if err != nil {
			return nil, fmt.Errorf("avm: block %d tx[%d] nonce read: %w", block.Header.Height, index, err)
		}
		if payload.Nonce != expectedNonce {
			return nil, fmt.Errorf("avm: block %d tx[%d] nonce %d, expected %d",
				block.Header.Height, index, payload.Nonce, expectedNonce)
		}

		var code []byte
		switch payload.Action {
		case core.AVMDeployContract:
			code = payload.Code
			if _, exists, getErr := overlay.Get(contractCodeKey(payload.ContractID)); getErr != nil {
				return nil, fmt.Errorf("avm: block %d tx[%d] contract lookup: %w", block.Header.Height, index, getErr)
			} else if exists {
				return nil, fmt.Errorf("avm: block %d tx[%d] contract already exists", block.Header.Height, index)
			}
		case core.AVMExecuteContract:
			var exists bool
			code, exists, err = overlay.Get(contractCodeKey(payload.ContractID))
			if err != nil {
				return nil, fmt.Errorf("avm: block %d tx[%d] load code: %w", block.Header.Height, index, err)
			}
			if !exists {
				return nil, fmt.Errorf("avm: block %d tx[%d] contract not found", block.Header.Height, index)
			}
		default:
			return nil, fmt.Errorf("avm: block %d tx[%d] unknown action", block.Header.Height, index)
		}

		accessList := make([]Access, len(payload.AccessList))
		for i, access := range payload.AccessList {
			accessList[i] = Access{Key: bytes.Clone(access.Key), Write: access.Write}
		}
		result, executeErr := engine.Execute(ctx, ExecutionRequest{
			ContractID: [32]byte(payload.ContractID),
			Code:       code,
			Entry:      payload.Entry,
			Input:      payload.Calldata,
			GasLimit:   payload.GasLimit,
			AccessList: accessList,
		})
		if executeErr != nil {
			return nil, fmt.Errorf("avm: block %d tx[%d] execution: %w", block.Header.Height, index, executeErr)
		}

		metadataWrites := []Write{{Key: signerNonceKey(payload.Signer), Value: encodeNonce(payload.Nonce + 1)}}
		if payload.Action == core.AVMDeployContract {
			metadataWrites = append(metadataWrites, Write{Key: contractCodeKey(payload.ContractID), Value: bytes.Clone(payload.Code)})
		}
		txHash := tx.Hash()
		receipt := Receipt{
			TxHash:      txHash,
			ContractID:  payload.ContractID,
			GasUsed:     result.GasUsed,
			ReturnData:  bytes.Clone(result.ReturnData),
			StateWrites: result.StateWrites,
		}
		metadataWrites = append(metadataWrites, Write{Key: receiptKey(txHash), Value: encodeReceipt(receipt)})
		if err := overlay.Apply(metadataWrites); err != nil {
			return nil, fmt.Errorf("avm: block %d tx[%d] stage metadata: %w", block.Header.Height, index, err)
		}
		prepared.Receipts = append(prepared.Receipts, receipt)
	}

	prepared.Writes = overlay.Writes()
	prepared.WriteSetCommitment = hashWrites(prepared.Writes)
	return prepared, nil
}

func contractCodeKey(contractID crypto.Hash32) []byte {
	key := append([]byte{}, codePrefix...)
	return append(key, contractID[:]...)
}

func signerNonceKey(signer [32]byte) []byte {
	key := append([]byte{}, noncePrefix...)
	return append(key, signer[:]...)
}

func receiptKey(txHash crypto.Hash32) []byte {
	key := append([]byte{}, receiptPrefix...)
	return append(key, txHash[:]...)
}

func loadNonce(store Store, signer [32]byte) (uint64, error) {
	value, found, err := store.Get(signerNonceKey(signer))
	if err != nil || !found {
		return 0, err
	}
	if len(value) != 8 {
		return 0, fmt.Errorf("stored nonce has %d bytes, want 8", len(value))
	}
	return binary.LittleEndian.Uint64(value), nil
}

func encodeNonce(nonce uint64) []byte {
	var value [8]byte
	binary.LittleEndian.PutUint64(value[:], nonce)
	return value[:]
}

func encodeReceipt(receipt Receipt) []byte {
	var out bytes.Buffer
	out.Write(receipt.TxHash[:])
	out.Write(receipt.ContractID[:])
	var number [8]byte
	binary.LittleEndian.PutUint64(number[:], receipt.GasUsed)
	out.Write(number[:])
	binary.LittleEndian.PutUint64(number[:], uint64(receipt.StateWrites))
	out.Write(number[:])
	var length [4]byte
	binary.LittleEndian.PutUint32(length[:], uint32(len(receipt.ReturnData)))
	out.Write(length[:])
	out.Write(receipt.ReturnData)
	return out.Bytes()
}

func hashWrites(writes []Write) crypto.Hash32 {
	parts := make([][]byte, 0, len(writes)*2+1)
	parts = append(parts, []byte("APRO/AVM/STATE-COMMITMENT/V1"))
	for _, write := range writes {
		var length [4]byte
		binary.LittleEndian.PutUint32(length[:], uint32(len(write.Key)))
		parts = append(parts, bytes.Clone(length[:]), bytes.Clone(write.Key))
		binary.LittleEndian.PutUint32(length[:], uint32(len(write.Value)))
		parts = append(parts, bytes.Clone(length[:]), bytes.Clone(write.Value))
	}
	return crypto.HashBytes(parts...)
}
