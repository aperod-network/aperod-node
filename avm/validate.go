package avm

import (
	"encoding/binary"
	"fmt"
	"io"

	wabinbinary "github.com/tetratelabs/wabin/binary"
	"github.com/tetratelabs/wabin/wasm"
)

const (
	MaxCodeSize     = 1 << 20
	MaxMemoryPages  = 512 // 32 MiB at 64 KiB per Wasm page.
	MaxInputSize    = 64 << 10
	MaxStateKeySize = 64
	MaxStateValue   = 64 << 10
)

var allowedImports = map[string]struct{}{
	"consume_gas":  {},
	"state_read":   {},
	"state_write":  {},
	"input_read":   {},
	"output_write": {},
}

// ValidationReport is consensus-safe metadata derived from a Wasm module.
type ValidationReport struct {
	InstructionCount uint64
	StaticGas        uint64
	Imports          []string
}

// ValidateModule rejects every capability not explicitly included in the AVM
// v1 ABI. In particular, v1 is integer-only and has no loops, recursion,
// indirect calls, WASI, imported memory, threads, SIMD, host pointers, time,
// filesystem, network or random-number APIs.
func ValidateModule(code []byte) (ValidationReport, error) {
	var report ValidationReport
	if len(code) == 0 {
		return report, fmt.Errorf("avm: empty Wasm module")
	}
	if len(code) > MaxCodeSize {
		return report, fmt.Errorf("avm: Wasm module is %d bytes, max %d", len(code), MaxCodeSize)
	}

	module, err := wabinbinary.DecodeModule(code, wasm.CoreFeaturesV2)
	if err != nil {
		return report, fmt.Errorf("avm: decode Wasm: %w", err)
	}
	for _, fnType := range module.TypeSection {
		if hasFloatType(fnType.Params) || hasFloatType(fnType.Results) {
			return report, fmt.Errorf("avm: floating-point function types are forbidden")
		}
	}
	for _, global := range module.GlobalSection {
		if global.Type != nil && isFloatType(global.Type.ValType) {
			return report, fmt.Errorf("avm: floating-point globals are forbidden")
		}
	}
	for _, imp := range module.ImportSection {
		if imp.Type != wasm.ExternTypeFunc {
			return report, fmt.Errorf("avm: only function imports are allowed")
		}
		if imp.Module != "apro" {
			return report, fmt.Errorf("avm: import module %q is forbidden", imp.Module)
		}
		if _, ok := allowedImports[imp.Name]; !ok {
			return report, fmt.Errorf("avm: host function apro.%s is not in the v1 ABI", imp.Name)
		}
		report.Imports = append(report.Imports, imp.Name)
	}
	if module.MemorySection != nil {
		if module.MemorySection.Min > MaxMemoryPages {
			return report, fmt.Errorf("avm: initial memory %d pages exceeds %d", module.MemorySection.Min, MaxMemoryPages)
		}
		if module.MemorySection.IsMaxEncoded && module.MemorySection.Max > MaxMemoryPages {
			return report, fmt.Errorf("avm: maximum memory %d pages exceeds %d", module.MemorySection.Max, MaxMemoryPages)
		}
	}

	importCount := module.ImportFuncCount()
	for functionIndex, body := range module.CodeSection {
		if hasFloatType(body.LocalTypes) {
			return report, fmt.Errorf("avm: function %d has floating-point locals", functionIndex)
		}
		count, parseErr := validateInstructions(body.Body, importCount)
		if parseErr != nil {
			return report, fmt.Errorf("avm: function %d: %w", functionIndex, parseErr)
		}
		report.InstructionCount += count
	}
	// v1 charges the worst-case straight-line instruction count up front.
	// Control-flow restrictions below make this exact upper-bound finite.
	report.StaticGas = 100 + report.InstructionCount
	return report, nil
}

func hasFloatType(types []wasm.ValueType) bool {
	for _, valueType := range types {
		if isFloatType(valueType) {
			return true
		}
	}
	return false
}

func isFloatType(valueType wasm.ValueType) bool {
	return valueType == wasm.ValueTypeF32 || valueType == wasm.ValueTypeF64
}

func validateInstructions(body []byte, importCount uint32) (uint64, error) {
	var count uint64
	for offset := 0; offset < len(body); {
		op := body[offset]
		offset++
		count++

		switch {
		case op == byte(wasm.OpcodeLoop), op == byte(wasm.OpcodeBr),
			op == byte(wasm.OpcodeBrIf), op == byte(wasm.OpcodeBrTable):
			return 0, fmt.Errorf("loops and branches require AVM v2 basic-block metering")
		case op == byte(wasm.OpcodeCallIndirect):
			return 0, fmt.Errorf("indirect calls are forbidden")
		case op == byte(wasm.OpcodeCall):
			index, n, err := readU32(body[offset:])
			if err != nil {
				return 0, fmt.Errorf("decode call index: %w", err)
			}
			offset += n
			if index >= importCount {
				return 0, fmt.Errorf("calls to local functions are forbidden in AVM v1")
			}
		case op == byte(wasm.OpcodeBlock), op == byte(wasm.OpcodeIf):
			n, err := skipBlockType(body[offset:])
			if err != nil {
				return 0, err
			}
			offset += n
		case op == byte(wasm.OpcodeLocalGet), op == byte(wasm.OpcodeLocalSet),
			op == byte(wasm.OpcodeLocalTee), op == byte(wasm.OpcodeGlobalGet),
			op == byte(wasm.OpcodeGlobalSet):
			_, n, err := readU32(body[offset:])
			if err != nil {
				return 0, fmt.Errorf("decode variable index: %w", err)
			}
			offset += n
		case op >= byte(wasm.OpcodeI32Load) && op <= byte(wasm.OpcodeI64Store32):
			if op == byte(wasm.OpcodeF32Load) || op == byte(wasm.OpcodeF64Load) ||
				op == byte(wasm.OpcodeF32Store) || op == byte(wasm.OpcodeF64Store) {
				return 0, fmt.Errorf("floating-point memory instructions are forbidden")
			}
			for i := 0; i < 2; i++ {
				_, n, err := readU32(body[offset:])
				if err != nil {
					return 0, fmt.Errorf("decode memory immediate: %w", err)
				}
				offset += n
			}
		case op == byte(wasm.OpcodeMemorySize), op == byte(wasm.OpcodeMemoryGrow):
			_, n, err := readU32(body[offset:])
			if err != nil {
				return 0, fmt.Errorf("decode memory index: %w", err)
			}
			offset += n
		case op == byte(wasm.OpcodeI32Const), op == byte(wasm.OpcodeI64Const):
			n, err := skipSignedLEB(body[offset:], 10)
			if err != nil {
				return 0, fmt.Errorf("decode integer constant: %w", err)
			}
			offset += n
		case isFloatingOpcode(op):
			return 0, fmt.Errorf("floating-point opcode 0x%x is forbidden", op)
		case op == 0xfc || op == 0xfd || op == 0xfe:
			return 0, fmt.Errorf("extended, SIMD and atomic opcodes are forbidden in AVM v1")
		case isNoImmediateIntegerOpcode(op):
			// No immediate to consume.
		default:
			return 0, fmt.Errorf("opcode 0x%x is not allowed in AVM v1", op)
		}
	}
	return count, nil
}

func isNoImmediateIntegerOpcode(op byte) bool {
	switch op {
	case 0x00, 0x01, 0x05, 0x0b, 0x0f, 0x1a, 0x1b:
		return true
	}
	return (op >= 0x45 && op <= 0x5a) ||
		(op >= 0x67 && op <= 0x8a) ||
		(op >= 0xa7 && op <= 0xb1) ||
		(op >= 0xc0 && op <= 0xc4)
}

func isFloatingOpcode(op byte) bool {
	return op == 0x2a || op == 0x2b || op == 0x38 || op == 0x39 ||
		op == 0x43 || op == 0x44 ||
		(op >= 0x5b && op <= 0x66) ||
		(op >= 0x8b && op <= 0xa6) ||
		(op >= 0xb2 && op <= 0xbf)
}

func readU32(data []byte) (uint32, int, error) {
	var value uint32
	for i := 0; i < len(data) && i < 5; i++ {
		b := data[i]
		value |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return value, i + 1, nil
		}
	}
	return 0, 0, io.ErrUnexpectedEOF
}

func skipSignedLEB(data []byte, maxBytes int) (int, error) {
	for i := 0; i < len(data) && i < maxBytes; i++ {
		if data[i]&0x80 == 0 {
			return i + 1, nil
		}
	}
	return 0, io.ErrUnexpectedEOF
}

func skipBlockType(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	switch data[0] {
	case 0x40, byte(wasm.ValueTypeI32), byte(wasm.ValueTypeI64):
		return 1, nil
	case byte(wasm.ValueTypeF32), byte(wasm.ValueTypeF64):
		return 0, fmt.Errorf("floating-point block type is forbidden")
	default:
		return skipSignedLEB(data, binary.MaxVarintLen64)
	}
}
