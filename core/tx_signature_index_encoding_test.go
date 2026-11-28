package core

import (
	"encoding/hex"
	"math"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestRingSignMessage_InputIndexEncodingIsUnchanged(t *testing.T) {
	var hash crypto.Hash32
	hash[0], hash[31] = 1, 255
	for _, tc := range []struct {
		index uint32
		bytes string
	}{
		{0, "00000000"}, {1, "00000001"}, {255, "000000ff"},
		{256, "00000100"}, {65536, "00010000"}, {math.MaxUint32, "ffffffff"},
	} {
		encoded, err := hex.DecodeString(tc.bytes)
		if err != nil {
			t.Fatal(err)
		}
		want := crypto.HashBytes([]byte("aperod/ring-sign/v1"), hash[:], encoded)
		if got := ringSignMessage(hash, tc.index); got != want {
			t.Fatalf("signature input index %d changed its canonical message", tc.index)
		}
	}
}
