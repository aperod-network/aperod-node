package core

import (
	"math"
	"math/big"
	"testing"
)

func TestFeeQuotes_IntegerBoundsAndOrdinaryParity(t *testing.T) {
	for _, tc := range []struct {
		name            string
		inputs, outputs int
		rate            uint64
	}{
		{"ordinary", 2, 2, 3},
		{"default rate", 1, 1, 0},
		{"negative inputs", -1, 1, 3},
		{"negative outputs", 1, -1, 3},
		{"input count overflow", math.MaxInt, 1, 1},
		{"output count overflow", 1, math.MaxInt, 1},
		{"fee overflow", 1, 1, math.MaxUint64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := uint64(math.MaxUint64)
			if tc.inputs >= 0 && tc.outputs >= 0 {
				size := new(big.Int).Mul(big.NewInt(int64(tc.inputs)), big.NewInt(txBytesPerInput))
				size.Add(size, new(big.Int).Mul(big.NewInt(int64(tc.outputs)), big.NewInt(txBytesPerOutput)))
				size.Add(size, big.NewInt(txOverheadBytes))
				rate := tc.rate
				if rate == 0 {
					rate = 1
				}
				fee := new(big.Int).Mul(new(big.Int).Set(size), new(big.Int).SetUint64(rate))
				if size.Cmp(big.NewInt(math.MaxInt)) <= 0 && fee.IsUint64() {
					want = fee.Uint64()
				}
			}
			if got := ExportedEstimateFee(tc.inputs, tc.outputs, tc.rate); got != want {
				t.Fatalf("quote=%d want %d", got, want)
			}
		})
	}
	for _, version := range []TxVersion{1, TxVersionCLSAG, TxVersionAmountBinding} {
		if got := ExportedEstimateFeeForVersion(version, -1, 1, 1); got != math.MaxUint64 {
			t.Fatalf("version %d accepted negative counts: %d", version, got)
		}
		if got := ExportedEstimateFeeForVersion(version, math.MaxInt, 1, 1); got != math.MaxUint64 {
			t.Fatalf("version %d wrapped a count: %d", version, got)
		}
		if got := ExportedEstimateFeeForVersion(version, 1, 1, math.MaxUint64); got != math.MaxUint64 {
			t.Fatalf("version %d wrapped a fee: %d", version, got)
		}
	}
}
