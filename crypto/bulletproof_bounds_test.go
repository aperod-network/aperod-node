package crypto

import "testing"

func TestPowerVecRejectsInvalidLengthsBeforeAllocation(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, n := range []int{-1, 0, bpBits + 1, maxInt} {
		if vec, err := powerVec(scalarOne(), n); err == nil || vec != nil {
			t.Errorf("powerVec(%d) = (%v, %v), want nil vector and error", n, vec, err)
		}
		if vec, err := pow2Vec(n); err == nil || vec != nil {
			t.Errorf("pow2Vec(%d) = (%v, %v), want nil vector and error", n, vec, err)
		}
	}
}

func TestPowerVecAcceptsSupportedRangeWidths(t *testing.T) {
	for _, n := range []int{16, bpBits} {
		vec, err := powerVec(scalarOne(), n)
		if err != nil {
			t.Fatalf("powerVec(%d): %v", n, err)
		}
		if len(vec) != n {
			t.Fatalf("powerVec(%d) returned %d elements", n, len(vec))
		}
	}
}

func TestVerifyRangeRejectsUnsupportedWidthsBeforeAllocation(t *testing.T) {
	for _, bits := range []uint8{1, 8, 65, 255} {
		proof := &RangeProof{Bits: bits}
		if ok, err := VerifyRange(proof); err == nil || ok {
			t.Errorf("VerifyRange with width %d = (%v, %v), want false and error", bits, ok, err)
		}
	}
}
