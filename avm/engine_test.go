package avm

import (
	"context"
	"errors"
	"testing"

	wabinbinary "github.com/tetratelabs/wabin/binary"
	"github.com/tetratelabs/wabin/wasm"
)

func TestEngineCommitsStateAndReturnsOutput(t *testing.T) {
	store := NewMemoryStore()
	engine := NewEngine(store)
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	code := stateWriteModule(false)
	contractID := [32]byte{1}
	result, err := engine.Execute(context.Background(), ExecutionRequest{
		ContractID: contractID,
		Code:       code,
		Entry:      "run",
		GasLimit:   1_000,
		AccessList: []Access{{Key: []byte("key"), Write: true}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.StateWrites != 1 {
		t.Fatalf("StateWrites = %d, want 1", result.StateWrites)
	}
	if string(result.ReturnData) != "value" {
		t.Fatalf("ReturnData = %q, want value", result.ReturnData)
	}
	key := append(append(contractID[:0:0], contractID[:]...), '/')
	key = append(key, "key"...)
	value, ok, err := store.Get(key)
	if err != nil || !ok || string(value) != "value" {
		t.Fatalf("stored value = %q, ok=%v, err=%v", value, ok, err)
	}
}

func TestEngineOutOfGasRollsBackWrites(t *testing.T) {
	store := NewMemoryStore()
	engine := NewEngine(store)
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	contractID := [32]byte{2}
	result, err := engine.Execute(context.Background(), ExecutionRequest{
		ContractID: contractID,
		Code:       stateWriteModule(true),
		GasLimit:   400,
		AccessList: []Access{{Key: []byte("key"), Write: true}},
	})
	if !errors.Is(err, ErrOutOfGas) {
		t.Fatalf("Execute error = %v, want ErrOutOfGas", err)
	}
	if result.GasUsed != 400 {
		t.Fatalf("GasUsed = %d, want 400", result.GasUsed)
	}
	key := append(append(contractID[:0:0], contractID[:]...), '/')
	key = append(key, "key"...)
	if _, ok, getErr := store.Get(key); getErr != nil || ok {
		t.Fatalf("state committed after out of gas: ok=%v err=%v", ok, getErr)
	}
}

func TestEngineRejectsUndeclaredStateWrite(t *testing.T) {
	engine := NewEngine(NewMemoryStore())
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	_, err := engine.Execute(context.Background(), ExecutionRequest{
		Code:     stateWriteModule(false),
		GasLimit: 1_000,
	})
	if err == nil {
		t.Fatal("undeclared state write accepted")
	}
}

func TestValidateModuleRejectsFloatWASIAndLoops(t *testing.T) {
	floatModule := minimalModule([]wasm.ValueType{wasm.ValueTypeF32}, nil, []byte{byte(wasm.OpcodeEnd)}, nil)
	if _, err := ValidateModule(floatModule); err == nil {
		t.Fatal("floating-point module accepted")
	}

	wasiModule := minimalModule(nil, nil, []byte{byte(wasm.OpcodeEnd)}, []*wasm.Import{{
		Type: wasm.ExternTypeFunc, Module: "wasi_snapshot_preview1", Name: "fd_write", DescFunc: 0,
	}})
	if _, err := ValidateModule(wasiModule); err == nil {
		t.Fatal("WASI module accepted")
	}

	loopModule := minimalModule(nil, nil, []byte{
		byte(wasm.OpcodeLoop), 0x40, byte(wasm.OpcodeEnd), byte(wasm.OpcodeEnd),
	}, nil)
	if _, err := ValidateModule(loopModule); err == nil {
		t.Fatal("loop module accepted")
	}
}

func TestValidateModuleDoesNotTreatIntegerImmediateAsFloatOpcode(t *testing.T) {
	// i32.const 67 encodes immediate byte 0x43, which is also f32.const when
	// interpreted as an opcode. Instruction-aware parsing must accept it.
	code := minimalModule(nil, nil, []byte{
		byte(wasm.OpcodeI32Const), 0x43, byte(wasm.OpcodeDrop), byte(wasm.OpcodeEnd),
	}, nil)
	if _, err := ValidateModule(code); err != nil {
		t.Fatalf("integer immediate rejected as float opcode: %v", err)
	}
}

func stateWriteModule(outOfGas bool) []byte {
	types := []*wasm.FunctionType{
		{Params: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32, wasm.ValueTypeI32, wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}},
		{Params: []wasm.ValueType{wasm.ValueTypeI64}},
		{Params: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}},
		{},
	}
	imports := []*wasm.Import{
		{Type: wasm.ExternTypeFunc, Module: "apro", Name: "state_write", DescFunc: 0},
		{Type: wasm.ExternTypeFunc, Module: "apro", Name: "consume_gas", DescFunc: 1},
		{Type: wasm.ExternTypeFunc, Module: "apro", Name: "output_write", DescFunc: 2},
	}
	body := []byte{
		byte(wasm.OpcodeI32Const), 0x00,
		byte(wasm.OpcodeI32Const), 0x03,
		byte(wasm.OpcodeI32Const), 0x03,
		byte(wasm.OpcodeI32Const), 0x05,
		byte(wasm.OpcodeCall), 0x00,
		byte(wasm.OpcodeDrop),
	}
	if outOfGas {
		body = append(body, byte(wasm.OpcodeI64Const), 0x90, 0x4e, byte(wasm.OpcodeCall), 0x01)
	} else {
		body = append(body,
			byte(wasm.OpcodeI32Const), 0x03,
			byte(wasm.OpcodeI32Const), 0x05,
			byte(wasm.OpcodeCall), 0x02,
			byte(wasm.OpcodeDrop),
		)
	}
	body = append(body, byte(wasm.OpcodeEnd))
	module := &wasm.Module{
		TypeSection:     types,
		ImportSection:   imports,
		FunctionSection: []wasm.Index{3},
		MemorySection:   &wasm.Memory{Min: 1, Max: MaxMemoryPages, IsMaxEncoded: true},
		ExportSection: []*wasm.Export{
			{Type: wasm.ExternTypeFunc, Name: "run", Index: wasm.Index(len(imports))},
			{Type: wasm.ExternTypeMemory, Name: "memory", Index: 0},
		},
		CodeSection: []*wasm.Code{{Body: body}},
		DataSection: []*wasm.DataSegment{{
			OffsetExpression: &wasm.ConstantExpression{
				Opcode: wasm.OpcodeI32Const,
				Data:   []byte{0},
			},
			Init: []byte("keyvalue"),
		}},
	}
	return wabinbinary.EncodeModule(module)
}

func minimalModule(params, results []wasm.ValueType, body []byte, imports []*wasm.Import) []byte {
	module := &wasm.Module{
		TypeSection:     []*wasm.FunctionType{{Params: params, Results: results}},
		ImportSection:   imports,
		FunctionSection: []wasm.Index{0},
		ExportSection: []*wasm.Export{{
			Type: wasm.ExternTypeFunc, Name: "run", Index: wasm.Index(len(imports)),
		}},
		CodeSection: []*wasm.Code{{Body: body}},
	}
	return wabinbinary.EncodeModule(module)
}
