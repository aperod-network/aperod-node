package avm

import (
	"strings"
	"testing"
)

func TestValidateModuleRejectsOversizedSectionVectorBeforeDecode(t *testing.T) {
	// A type-section vector claims 2^32-1 entries but has no element bytes.
	// This malformed 15-byte module must be rejected before the Wasm decoder
	// allocates a slice from the claimed count.
	module := []byte{
		0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00,
		0x01, 0x05, 0xff, 0xff, 0xff, 0xff, 0x0f,
	}

	_, err := ValidateModule(module)
	if err == nil || !strings.Contains(err.Error(), "section 1 vector count") {
		t.Fatalf("ValidateModule error = %v, want bounded type-section vector rejection", err)
	}
}

func TestValidateModuleRejectsOversizedTableVectorBeforeDecode(t *testing.T) {
	// This is the reported AVM OOM shape: a 15-byte module declares 2^32-1
	// tables with no table bodies. Reject it in preflight, before wabin can
	// allocate from the untrusted vector count.
	module := []byte{
		0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00,
		0x04, 0x05, 0xff, 0xff, 0xff, 0xff, 0x0f,
	}

	_, err := ValidateModule(module)
	if err == nil || !strings.Contains(err.Error(), "section 4 vector count") {
		t.Fatalf("ValidateModule error = %v, want bounded table-section vector rejection", err)
	}
}

func TestValidateWasmSectionVectorsLeavesInvalidHeaderToDecoder(t *testing.T) {
	if err := validateWasmSectionVectors([]byte("not wasm")); err != nil {
		t.Fatalf("preflight rejected non-Wasm header before canonical decoder: %v", err)
	}
}
