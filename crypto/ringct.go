package crypto

import (
        "crypto/sha512"
        "crypto/subtle"
        "fmt"

        "filippo.io/edwards25519"
)

// RingSize is the number of public keys in a ring (1 real + N-1 decoys).
// Minimum 11 for meaningful anonymity set.
const RingSize = 11

// MLSAGSignature is a Multi-Layered Linkable Spontaneous Anonymous Group signature.
// It proves that the signer holds the private key for one of the ring members
// without revealing which one.
type MLSAGSignature struct {
        // C0 is the initial challenge scalar.
        C0 [32]byte
        // SS[i] is the response scalar for ring member i.
        SS [][32]byte // length = RingSize
        // KeyImage prevents double-spending.
        KeyImage KeyImage
}

// RingMember is a public key in the ring (either the real UTXO or a decoy).
type RingMember = Point32

// MLSAGSign creates a ring signature over message (a hash) using the real key
// at position realIdx within the ring.
//
// Parameters:
//   - message: the transaction hash being signed
//   - ring: slice of RingSize public keys (spend keys of UTXOs)
//   - realIdx: index of the real key in ring (0 ≤ realIdx < RingSize)
//   - privKey: one-time private scalar for the real UTXO
func MLSAGSign(message Hash32, ring []RingMember, realIdx int, privKey Scalar32) (*MLSAGSignature, error) {
        if len(ring) != RingSize {
                return nil, fmt.Errorf("ring size must be %d, got %d", RingSize, len(ring))
        }
        if realIdx < 0 || realIdx >= RingSize {
                return nil, fmt.Errorf("realIdx %d out of range [0, %d)", realIdx, RingSize)
        }

        x, err := ScalarFromBytes(privKey[:])
        if err != nil {
                return nil, fmt.Errorf("private key: %w", err)
        }

        // Compute key image: I = x · Hp(P_real)
        ki, err := ComputeKeyImage(privKey, ring[realIdx])
        if err != nil {
                return nil, fmt.Errorf("key image: %w", err)
        }

        I, err := PointFromBytes(ki[:])
        if err != nil {
                return nil, fmt.Errorf("key image point: %w", err)
        }

        // --- MLSAG construction ---
        ss := make([]*edwards25519.Scalar, RingSize)
        cc := make([]*edwards25519.Scalar, RingSize+1)

        // Step 1: random alpha for the real index
        alpha, err := randomScalar()
        if err != nil {
                return nil, fmt.Errorf("random alpha: %w", err)
        }

        // Step 2: compute L_real = alpha·G, R_real = alpha·Hp(P_real)
        L_real := (&edwards25519.Point{}).ScalarBaseMult(alpha)
        HpReal := HashToCurvePoint(ring[realIdx][:])
        R_real := (&edwards25519.Point{}).ScalarMult(alpha, HpReal)

        // Step 3: compute challenge c_{real+1}
        cc[nextIdx(realIdx)] = ringChallenge(message, L_real, R_real)

        // Step 4: walk the ring forward from real+1
        for i := 1; i < RingSize; i++ {
                idx := (realIdx + i) % RingSize
                nextI := (realIdx + i + 1) % RingSize

                // Random response for non-real members
                ss[idx], err = randomScalar()
                if err != nil {
                        return nil, fmt.Errorf("random s[%d]: %w", idx, err)
                }

                // L_i = ss[i]·G + cc[i]·P_i
                Pi, err := PointFromBytes(ring[idx][:])
                if err != nil {
                        return nil, fmt.Errorf("ring member %d: %w", idx, err)
                }
                L_i := ringL(ss[idx], cc[(realIdx+i)%RingSize], Pi)

                // R_i = ss[i]·Hp(P_i) + cc[i]·I
                HpI := HashToCurvePoint(ring[idx][:])
                R_i := ringR(ss[idx], cc[(realIdx+i)%RingSize], HpI, I)

                cc[nextI] = ringChallenge(message, L_i, R_i)
        }

        // Step 5: close the ring — compute s_real = alpha - cc[real]·x (mod order)
        cx := edwards25519.NewScalar().Multiply(cc[realIdx], x)
        ss[realIdx] = edwards25519.NewScalar().Subtract(alpha, cx)

        // Encode
        sig := &MLSAGSignature{
                KeyImage: ki,
                SS:       make([][32]byte, RingSize),
        }
        copy(sig.C0[:], cc[0].Bytes())
        for i := range ss {
                copy(sig.SS[i][:], ss[i].Bytes())
        }
        return sig, nil
}

// MLSAGVerify verifies an MLSAG ring signature.
func MLSAGVerify(message Hash32, ring []RingMember, sig *MLSAGSignature) (bool, error) {
        if sig == nil {
                return false, fmt.Errorf("nil signature")
        }
        if len(ring) != RingSize || len(sig.SS) != RingSize {
                return false, fmt.Errorf("ring size mismatch")
        }

        I, err := PointFromBytes(sig.KeyImage[:])
        if err != nil {
                return false, fmt.Errorf("key image: %w", err)
        }

        c0, err := ScalarFromBytes(sig.C0[:])
        if err != nil {
                return false, fmt.Errorf("c0: %w", err)
        }

        cc := make([]*edwards25519.Scalar, RingSize+1)
        cc[0] = c0

        for i := 0; i < RingSize; i++ {
                s_i, err := ScalarFromBytes(sig.SS[i][:])
                if err != nil {
                        return false, fmt.Errorf("ss[%d]: %w", i, err)
                }

                Pi, err := PointFromBytes(ring[i][:])
                if err != nil {
                        return false, fmt.Errorf("ring[%d]: %w", i, err)
                }

                L_i := ringL(s_i, cc[i], Pi)
                HpI := HashToCurvePoint(ring[i][:])
                R_i := ringR(s_i, cc[i], HpI, I)

                cc[i+1] = ringChallenge(message, L_i, R_i)
        }

        // Verify that the ring closes: c_0 == cc[RingSize]
        return subtle.ConstantTimeCompare(cc[0].Bytes(), cc[RingSize].Bytes()) == 1, nil
}

// ringL computes L = s·G + c·P
func ringL(s, c *edwards25519.Scalar, P *edwards25519.Point) *edwards25519.Point {
        sG := (&edwards25519.Point{}).ScalarBaseMult(s)
        cP := (&edwards25519.Point{}).ScalarMult(c, P)
        return (&edwards25519.Point{}).Add(sG, cP)
}

// ringR computes R = s·Hp + c·I
func ringR(s, c *edwards25519.Scalar, Hp, I *edwards25519.Point) *edwards25519.Point {
        sHp := (&edwards25519.Point{}).ScalarMult(s, Hp)
        cI := (&edwards25519.Point{}).ScalarMult(c, I)
        return (&edwards25519.Point{}).Add(sHp, cI)
}

// ringChallenge computes SHA-512(message || L || R) as a scalar.
func ringChallenge(msg Hash32, L, R *edwards25519.Point) *edwards25519.Scalar {
        h := sha512.New()
        h.Write([]byte("Aperod/MLSAG/v1"))
        h.Write(msg[:])
        h.Write(L.Bytes())
        h.Write(R.Bytes())
        var wide [64]byte
        copy(wide[:], h.Sum(nil))
        s, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
        return s
}

// nextIdx wraps index within RingSize.
func nextIdx(i int) int { return (i + 1) % RingSize }
