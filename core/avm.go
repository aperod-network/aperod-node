package core

import (
	"bytes"
	stded25519 "crypto/ed25519"
	"encoding/binary"
	"fmt"
	"unicode/utf8"

	aprocrypto "github.com/aperod/aperod/crypto"
)

const (
	AVMMaxCodeSize       = 1 << 20
	AVMMaxCalldataSize   = 64 << 10
	AVMMaxEntrySize      = 64
	AVMMaxAccessListKeys = 64
	AVMMaxStateKeySize   = 64
	AVMMinGasLimit       = 100
	// AVMMaxBlockGas is a consensus-wide CPU budget. A single transaction
	// cannot reserve more gas than the complete block budget.
	AVMMaxBlockGas uint64 = 10_000_000
	AVMMaxGasLimit        = AVMMaxBlockGas
	// AVMGasPriceNAPR is the protocol price for one deterministic AVM gas unit.
	// It is intentionally a consensus constant, not a per-validator setting.
	AVMGasPriceNAPR uint64 = 10
)

type AVMAction uint8

const (
	AVMDeployContract  AVMAction = 1
	AVMExecuteContract AVMAction = 2
)

// AVMAccess declares one deterministic state dependency. Entries must be
// strictly sorted by Key and unique so all validators derive the same schedule.
type AVMAccess struct {
	Key   []byte `json:"key"`
	Write bool   `json:"write"`
}

// AVMPayload is the consensus wire payload for native Wasm contract
// deployment and execution. APRO fees are still paid by Transaction's
// RingCT/CLSAG inputs, preventing zero-input value creation.
type AVMPayload struct {
	Action     AVMAction         `json:"action"`
	ContractID aprocrypto.Hash32 `json:"contract_id"`
	Code       []byte            `json:"code,omitempty"`
	Entry      string            `json:"entry"`
	Calldata   []byte            `json:"calldata,omitempty"`
	GasLimit   uint64            `json:"gas_limit"`
	Nonce      uint64            `json:"nonce"`
	Signer     [32]byte          `json:"signer"`
	Signature  [64]byte          `json:"signature"`
	AccessList []AVMAccess       `json:"access_list,omitempty"`
}

func (p *AVMPayload) Validate() error {
	if p == nil {
		return fmt.Errorf("avm payload is required")
	}
	switch p.Action {
	case AVMDeployContract:
		if len(p.Code) == 0 || len(p.Code) > AVMMaxCodeSize {
			return fmt.Errorf("avm deploy code must be 1..%d bytes", AVMMaxCodeSize)
		}
		expected := DeriveAVMContractID(p.Signer, p.Nonce, p.Code)
		if p.ContractID != expected {
			return fmt.Errorf("avm deploy contract id does not match signer, nonce and code")
		}
	case AVMExecuteContract:
		if len(p.Code) != 0 {
			return fmt.Errorf("avm execute payload must not include code")
		}
		if p.ContractID == (aprocrypto.Hash32{}) {
			return fmt.Errorf("avm execute contract id is required")
		}
	default:
		return fmt.Errorf("unknown avm action %d", p.Action)
	}
	if len(p.Entry) == 0 || len(p.Entry) > AVMMaxEntrySize || !utf8.ValidString(p.Entry) {
		return fmt.Errorf("avm entry must be valid UTF-8 and 1..%d bytes", AVMMaxEntrySize)
	}
	if len(p.Calldata) > AVMMaxCalldataSize {
		return fmt.Errorf("avm calldata is %d bytes, max %d", len(p.Calldata), AVMMaxCalldataSize)
	}
	if p.GasLimit < AVMMinGasLimit || p.GasLimit > AVMMaxGasLimit {
		return fmt.Errorf("avm gas limit %d outside %d..%d", p.GasLimit, AVMMinGasLimit, AVMMaxGasLimit)
	}
	if p.Signer == ([32]byte{}) {
		return fmt.Errorf("avm signer is required")
	}
	if len(p.AccessList) > AVMMaxAccessListKeys {
		return fmt.Errorf("avm access list has %d entries, max %d", len(p.AccessList), AVMMaxAccessListKeys)
	}
	var previous []byte
	for i, access := range p.AccessList {
		if len(access.Key) == 0 || len(access.Key) > AVMMaxStateKeySize {
			return fmt.Errorf("avm access key %d must be 1..%d bytes", i, AVMMaxStateKeySize)
		}
		if i > 0 && bytes.Compare(previous, access.Key) >= 0 {
			return fmt.Errorf("avm access list must be strictly sorted and unique")
		}
		previous = access.Key
	}
	signingHash := p.SigningHash()
	if !stded25519.Verify(stded25519.PublicKey(p.Signer[:]), signingHash[:], p.Signature[:]) {
		return fmt.Errorf("avm payload signature is invalid")
	}
	return nil
}

func (p *AVMPayload) SigningHash() aprocrypto.Hash32 {
	if p == nil {
		return aprocrypto.Hash32{}
	}
	return aprocrypto.HashBytes([]byte("APRO/AVM/SIGN/V1"), p.canonicalBytes(false))
}

func (p *AVMPayload) canonicalBytes(includeSignature bool) []byte {
	if p == nil {
		return nil
	}
	var out bytes.Buffer
	out.WriteByte(byte(p.Action))
	out.Write(p.ContractID[:])
	writeAVMBytes(&out, p.Code)
	writeAVMBytes(&out, []byte(p.Entry))
	writeAVMBytes(&out, p.Calldata)
	var number [8]byte
	binary.LittleEndian.PutUint64(number[:], p.GasLimit)
	out.Write(number[:])
	binary.LittleEndian.PutUint64(number[:], p.Nonce)
	out.Write(number[:])
	out.Write(p.Signer[:])
	var count [2]byte
	binary.LittleEndian.PutUint16(count[:], uint16(len(p.AccessList)))
	out.Write(count[:])
	for _, access := range p.AccessList {
		writeAVMBytes(&out, access.Key)
		if access.Write {
			out.WriteByte(1)
		} else {
			out.WriteByte(0)
		}
	}
	if includeSignature {
		out.Write(p.Signature[:])
	}
	return out.Bytes()
}

func writeAVMBytes(out *bytes.Buffer, value []byte) {
	var length [4]byte
	binary.LittleEndian.PutUint32(length[:], uint32(len(value)))
	out.Write(length[:])
	out.Write(value)
}

func DeriveAVMContractID(signer [32]byte, nonce uint64, code []byte) aprocrypto.Hash32 {
	var nonceBytes [8]byte
	binary.LittleEndian.PutUint64(nonceBytes[:], nonce)
	codeHash := aprocrypto.HashBytes(code)
	return aprocrypto.HashBytes(
		[]byte("APRO/AVM/CONTRACT/V1"),
		signer[:],
		nonceBytes[:],
		codeHash[:],
	)
}

func AVMGasFee(gasLimit uint64) (uint64, error) {
	if gasLimit > ^uint64(0)/AVMGasPriceNAPR {
		return 0, fmt.Errorf("avm gas fee overflows uint64")
	}
	return gasLimit * AVMGasPriceNAPR, nil
}
