package core

import (
	"math"
	"math/big"
	"testing"
)

func TestRequiredFinalityVotes_ExactStrictThresholdAndBounds(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 21, 1_000_000, math.MaxInt} {
		for _, ppm := range []uint32{1, 500_000, 666_667, 1_000_000} {
			reference := new(big.Int).Mul(big.NewInt(int64(n)), new(big.Int).SetUint64(uint64(ppm)))
			reference.Div(reference, big.NewInt(1_000_000)).Add(reference, big.NewInt(1))
			got, err := RequiredFinalityVotes(n, ppm)
			if reference.Cmp(big.NewInt(math.MaxInt)) > 0 {
				if err == nil {
					t.Fatalf("unrepresentable quorum accepted for %d/%d", n, ppm)
				}
			} else if err != nil || int64(got) != reference.Int64() {
				t.Fatalf("quorum(%d,%d)=%d,%v want %s", n, ppm, got, err, reference)
			}
		}
	}
	for _, tc := range []struct {
		n   int
		ppm uint32
	}{{-1, 500_000}, {1, 0}, {1, 1_000_001}} {
		if _, err := RequiredFinalityVotes(tc.n, tc.ppm); err == nil {
			t.Fatalf("invalid quorum input accepted: %+v", tc)
		}
	}
}
