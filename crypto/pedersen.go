package crypto

import (
        "fmt"

        "filippo.io/edwards25519"
        "golang.org/x/crypto/sha3"
)

// Commitment is a Pedersen commitment C = r·G + v·H.
// It hides the value v (amount) behind a blinding factor r.
// The commitment scheme is perfectly hiding and computationally binding.
type Commitment [32]byte

// BlindFactor is the 32-byte blinding scalar used to open a commitment.
type BlindFactor [32]byte

// hGenerator is the second generator H derived from hashing the base point G.
// SECURITY NOTE: In production, H must be derived via a verifiable hash-to-curve
// algorithm (e.g., Elligator2 or hash-to-ristretto255) so that log_G(H) is unknown.
// This implementation uses H = SHA3("Aperod/commitment/H") expanded to a scalar
// then multiplied by G, which is NOT secure (DLOG is known).
// Replace with a proper hash-to-curve before mainnet.
var hGenerator *edwards25519.Point

func init() {
        // Derive H deterministically from a domain-separated hash.
        h := sha3.New512()
        h.Write([]byte("Aperod/commitment/generator/H/v1"))
        var wide [64]byte
        copy(wide[:], h.Sum(nil))
        hs, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
        hGenerator = (&edwards25519.Point{}).ScalarBaseMult(hs)
}

// Commit creates a Pedersen commitment to value v with blinding factor r.
// C = r·G + v·H
func Commit(value uint64, blind BlindFactor) (Commitment, error) {
        rScalar, err := ScalarFromBytes(blind[:])
        if err != nil {
                // Try clamping
                clamped := clampScalar(blind)
                rScalar, err = ScalarFromBytes(clamped[:])
                if err != nil {
                        return Commitment{}, fmt.Errorf("invalid blinding factor: %w", err)
                }
        }

        vScalar := scalarFromUint64(value)

        // r·G
        rG := (&edwards25519.Point{}).ScalarBaseMult(rScalar)
        // v·H
        vH := (&edwards25519.Point{}).ScalarMult(vScalar, hGenerator)
        // C = r·G + v·H
        C := (&edwards25519.Point{}).Add(rG, vH)

        var out Commitment
        copy(out[:], C.Bytes())
        return out, nil
}

// CommitSum verifies that the sum of input commitments equals the sum of output
// commitments plus the fee commitment: ΣC_in = ΣC_out + C_fee.
// This is the fundamental privacy-preserving balance check.
func CommitSum(inputs []Commitment, outputs []Commitment, feeCommit Commitment) (bool, error) {
        sumIn := new(edwards25519.Point).Set(edwards25519.NewIdentityPoint())
        for _, c := range inputs {
                pt, err := PointFromBytes(c[:])
                if err != nil {
                        return false, fmt.Errorf("invalid input commitment: %w", err)
                }
                sumIn.Add(sumIn, pt)
        }

        sumOut := new(edwards25519.Point).Set(edwards25519.NewIdentityPoint())
        for _, c := range outputs {
                pt, err := PointFromBytes(c[:])
                if err != nil {
                        return false, fmt.Errorf("invalid output commitment: %w", err)
                }
                sumOut.Add(sumOut, pt)
        }

        feePt, err := PointFromBytes(feeCommit[:])
        if err != nil {
                return false, fmt.Errorf("invalid fee commitment: %w", err)
        }
        sumOut.Add(sumOut, feePt)

        return sumIn.Equal(sumOut) == 1, nil
}

// NewBlindFactor generates a random blinding factor.
func NewBlindFactor() (BlindFactor, error) {
        var buf [64]byte
        if _, err := randRead(buf[:]); err != nil {
                return BlindFactor{}, err
        }
        s, err := edwards25519.NewScalar().SetUniformBytes(buf[:])
        if err != nil {
                return BlindFactor{}, err
        }
        var bf BlindFactor
        copy(bf[:], s.Bytes())
        return bf, nil
}

// BlindSum computes the sum of blinding factors (for change output calculation).
// change_blind = Σin_blind - Σout_blind (mod order)
// All BlindFactor values must be canonical scalars (as produced by NewBlindFactor).
func BlindSum(in []BlindFactor, out []BlindFactor) (BlindFactor, error) {
        acc := edwards25519.NewScalar()
        for _, b := range in {
                s, err := ScalarFromBytes(b[:])
                if err != nil {
                        return BlindFactor{}, err
                }
                acc.Add(acc, s)
        }
        for _, b := range out {
                s, err := ScalarFromBytes(b[:])
                if err != nil {
                        return BlindFactor{}, err
                }
                neg := edwards25519.NewScalar().Negate(s)
                acc.Add(acc, neg)
        }
        var result BlindFactor
        copy(result[:], acc.Bytes())
        return result, nil
}

// scalarFromUint64 encodes a uint64 as a little-endian Edwards scalar.
func scalarFromUint64(v uint64) *edwards25519.Scalar {
        var b [32]byte
        b[0] = byte(v)
        b[1] = byte(v >> 8)
        b[2] = byte(v >> 16)
        b[3] = byte(v >> 24)
        b[4] = byte(v >> 32)
        b[5] = byte(v >> 40)
        b[6] = byte(v >> 48)
        b[7] = byte(v >> 56)
        s, _ := edwards25519.NewScalar().SetCanonicalBytes(b[:])
        return s
}

// clampScalar applies Ed25519 clamping to ensure a valid scalar.
func clampScalar(b BlindFactor) BlindFactor {
        out := b
        out[0] &= 248
        out[31] &= 127
        out[31] |= 64
        return out
}
