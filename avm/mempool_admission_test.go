package avm

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
)

func TestValidateMempoolAdmission(t *testing.T) {
	code := stateWriteModule(false)
	var contractID [32]byte
	contractID[0] = 1

	t.Run("valid deploy", func(t *testing.T) {
		err := ValidateMempoolAdmission(NewMemoryStore(), &core.AVMPayload{
			Action: core.AVMDeployContract, ContractID: contractID,
			Code: code, Entry: "run", GasLimit: 1_000,
			AccessList: []core.AVMAccess{{Key: []byte("key"), Write: true}},
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("malformed module", func(t *testing.T) {
		err := ValidateMempoolAdmission(NewMemoryStore(), &core.AVMPayload{
			Action: core.AVMDeployContract, ContractID: contractID,
			Code: []byte{0, 'a', 's', 'm'}, Entry: "run", GasLimit: 1_000,
		})
		if err == nil || !strings.Contains(err.Error(), "decode Wasm") {
			t.Fatalf("got %v, want malformed module rejection", err)
		}
	})

	t.Run("intrinsic out of gas", func(t *testing.T) {
		err := ValidateMempoolAdmission(NewMemoryStore(), &core.AVMPayload{
			Action: core.AVMDeployContract, ContractID: contractID,
			Code: code, Entry: "run", GasLimit: core.AVMMinGasLimit,
			AccessList: []core.AVMAccess{{Key: []byte("key"), Write: true}},
		})
		if err == nil || !strings.Contains(err.Error(), "out of gas") {
			t.Fatalf("got %v, want out-of-gas rejection", err)
		}
	})

	t.Run("missing contract", func(t *testing.T) {
		err := ValidateMempoolAdmission(NewMemoryStore(), &core.AVMPayload{
			Action: core.AVMExecuteContract, ContractID: contractID,
			Entry: "run", GasLimit: 1_000,
		})
		if err == nil || !strings.Contains(err.Error(), "contract not found") {
			t.Fatalf("got %v, want missing contract rejection", err)
		}
	})

	t.Run("existing contract call", func(t *testing.T) {
		store := NewMemoryStore()
		if err := store.Apply([]Write{{Key: contractCodeKey(contractID), Value: code}}); err != nil {
			t.Fatal(err)
		}
		err := ValidateMempoolAdmission(store, &core.AVMPayload{
			Action: core.AVMExecuteContract, ContractID: contractID,
			Entry: "run", GasLimit: 1_000,
			AccessList: []core.AVMAccess{{Key: []byte("key"), Write: true}},
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("duplicate contract", func(t *testing.T) {
		store := NewMemoryStore()
		if err := store.Apply([]Write{{Key: contractCodeKey(contractID), Value: code}}); err != nil {
			t.Fatal(err)
		}
		err := ValidateMempoolAdmission(store, &core.AVMPayload{
			Action: core.AVMDeployContract, ContractID: contractID,
			Code: code, Entry: "run", GasLimit: 1_000,
			AccessList: []core.AVMAccess{{Key: []byte("key"), Write: true}},
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("got %v, want duplicate contract rejection", err)
		}
	})

	t.Run("dynamic out of gas", func(t *testing.T) {
		err := ValidateMempoolAdmission(NewMemoryStore(), &core.AVMPayload{
			Action: core.AVMDeployContract, ContractID: contractID,
			Code: stateWriteModule(true), Entry: "run", GasLimit: 1_000,
			AccessList: []core.AVMAccess{{Key: []byte("key"), Write: true}},
		})
		if err == nil || !strings.Contains(err.Error(), "out of gas") {
			t.Fatalf("got %v, want dynamic out-of-gas rejection", err)
		}
	})
}
