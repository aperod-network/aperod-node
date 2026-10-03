package main

import (
	"encoding/json"
	"testing"
)

func TestWasmOutputAmountAcceptsExactAndLegacySafeValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want uint64
	}{
		{name: "legacy safe integer", raw: `{"amount_napr":123456}`, want: 123456},
		{name: "zero string", raw: `{"amount_napr":"0"}`, want: 0},
		{name: "large exact mint string", raw: `{"amount_napr":"500000000000000000"}`, want: 500000000000000000},
		{name: "maximum uint64 string", raw: `{"amount_napr":"18446744073709551615"}`, want: ^uint64(0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output wasmOutput
			if err := json.Unmarshal([]byte(tt.raw), &output); err != nil {
				t.Fatalf("decode output amount: %v", err)
			}
			if output.Amount != tt.want {
				t.Fatalf("decoded amount %d, want %d", output.Amount, tt.want)
			}
		})
	}
}

func TestWasmOutputAmountRejectsMalformedOrUnsafeNumbers(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "negative number", raw: `{"amount_napr":-1}`},
		{name: "fractional number", raw: `{"amount_napr":1.5}`},
		{name: "integer written as decimal", raw: `{"amount_napr":1.0}`},
		{name: "exponent number", raw: `{"amount_napr":1e2}`},
		{name: "unsafe rounded number", raw: `{"amount_napr":9007199254740992}`},
		{name: "large JS mint number", raw: `{"amount_napr":500000000000000000}`},
		{name: "negative string", raw: `{"amount_napr":"-1"}`},
		{name: "leading zero string", raw: `{"amount_napr":"01"}`},
		{name: "plus sign string", raw: `{"amount_napr":"+1"}`},
		{name: "uint64 overflow string", raw: `{"amount_napr":"18446744073709551616"}`},
		{name: "null", raw: `{"amount_napr":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output wasmOutput
			if err := json.Unmarshal([]byte(tt.raw), &output); err == nil {
				t.Fatalf("malformed amount accepted: %s", tt.raw)
			}
		})
	}
}

func TestWasmOutputAmountOmittedRemainsZero(t *testing.T) {
	var output wasmOutput
	if err := json.Unmarshal([]byte(`{"tx_hash":"fixture"}`), &output); err != nil {
		t.Fatal(err)
	}
	if output.Amount != 0 || output.TxHash != "fixture" {
		t.Fatalf("missing amount changed legacy zero default or other output fields: %+v", output)
	}
}
