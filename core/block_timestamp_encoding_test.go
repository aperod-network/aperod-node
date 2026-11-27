package core

import (
	"encoding/hex"
	"math"
	"testing"
)

func TestSignedHeaderEncoding_PreservesHistoricalWireBits(t *testing.T) {
	for _, tc := range []struct {
		value int64
		hex   string
	}{
		{0, "0000000000000000"},
		{1, "0100000000000000"},
		{-1, "ffffffffffffffff"},
		{math.MinInt64, "0000000000000080"},
		{math.MaxInt64, "ffffffffffffff7f"},
	} {
		if got := hex.EncodeToString(encodeInt64(tc.value)); got != tc.hex {
			t.Errorf("signed header encoding(%d)=%s, want %s", tc.value, got, tc.hex)
		}
	}
}
