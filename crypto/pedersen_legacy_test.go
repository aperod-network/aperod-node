package crypto

import (
	"crypto/sha512"
	"encoding/binary"
	"math"
	"testing"

	"filippo.io/edwards25519"
)

func TestLegacyMintBlindPreservesHistoricalAlgorithmAndCompatibility(t *testing.T) {
	var spendPub Point32
	copy(spendPub[:], edwards25519.NewGeneratorPoint().Bytes())
	for _, amount := range []uint64{0, 1, 100_000_000, math.MaxUint64} {
		legacy, err := LegacyMintBlind(spendPub, amount)
		if err != nil {
			t.Fatal(err)
		}
		compatibility, err := DeterministicMintBlind(spendPub, amount)
		if err != nil {
			t.Fatal(err)
		}
		preimage := append([]byte("aperod-mint-blind-v1"), spendPub[:]...)
		preimage = binary.LittleEndian.AppendUint64(preimage, amount)
		digest := sha512.Sum512(preimage)
		scalar, err := edwards25519.NewScalar().SetUniformBytes(digest[:])
		if err != nil {
			t.Fatal(err)
		}
		var expected BlindFactor
		copy(expected[:], scalar.Bytes())
		if legacy != expected || compatibility != expected {
			t.Fatalf("historical blind bytes changed for amount %d", amount)
		}
		v2, err := DeterministicMintBlindV2(spendPub, amount, 42)
		if err != nil {
			t.Fatal(err)
		}
		if legacy == v2 {
			t.Fatal("legacy derivation unexpectedly replaced the height-bound algorithm")
		}
	}
}
