package avm

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	wabinbinary "github.com/tetratelabs/wabin/binary"
	"github.com/tetratelabs/wabin/wasm"
)

func TestInstructionAdmissionDepthBound(t *testing.T) {
	for _, depth := range []int{1, MaxAdmissionNestingDepth, MaxAdmissionNestingDepth + 1} {
		body := make([]byte, 0, depth*3+1)
		for i := 0; i < depth; i++ {
			body = append(body, 0x02, 0x40)
		}
		for i := 0; i <= depth; i++ {
			body = append(body, 0x0b)
		}
		_, err := validateInstructionsBounded(body, 0, MaxAdmissionNestingDepth)
		if depth <= MaxAdmissionNestingDepth && err != nil {
			t.Fatal(err)
		}
		if depth > MaxAdmissionNestingDepth && (err == nil || !strings.Contains(err.Error(), "nesting")) {
			t.Fatalf("depth %d: %v", depth, err)
		}
		// Historical instruction validation is deliberately unchanged.
		if _, err := validateInstructions(body, 0); err != nil {
			t.Fatalf("historical validator changed: %v", err)
		}
	}
}

func TestModuleAdmissionUsesDepthPolicyForDeployAndCall(t *testing.T) {
	for _, depth := range []int{MaxAdmissionNestingDepth, MaxAdmissionNestingDepth + 1} {
		module, err := wabinbinary.DecodeModule(stateWriteModule(false), wasm.CoreFeaturesV2)
		if err != nil {
			t.Fatal(err)
		}
		prefix := make([]byte, 0, depth*3)
		for i := 0; i < depth; i++ {
			prefix = append(prefix, 0x02, 0x40)
		}
		for i := 0; i < depth; i++ {
			prefix = append(prefix, 0x0b)
		}
		module.CodeSection[0].Body = append(prefix, module.CodeSection[0].Body...)
		code := wabinbinary.EncodeModule(module)
		if _, err := ValidateModule(code); err != nil {
			t.Fatal(err)
		}
		var id [32]byte
		id[0] = 1
		for _, action := range []core.AVMAction{core.AVMDeployContract, core.AVMExecuteContract} {
			store := NewMemoryStore()
			if action == core.AVMExecuteContract {
				if err := store.Apply([]Write{{Key: contractCodeKey(id), Value: code}}); err != nil {
					t.Fatal(err)
				}
			}
			err := ValidateMempoolAdmission(store, &core.AVMPayload{
				Action: action, ContractID: id, Code: code, Entry: "run", GasLimit: 1000,
				AccessList: []core.AVMAccess{{Key: []byte("key"), Write: true}},
			})
			if depth <= MaxAdmissionNestingDepth && err != nil {
				t.Fatal(err)
			}
			if depth > MaxAdmissionNestingDepth && (err == nil || !strings.Contains(err.Error(), "nesting")) {
				t.Fatalf("action %d depth %d: %v", action, depth, err)
			}
		}
	}
}
