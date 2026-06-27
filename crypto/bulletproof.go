package crypto

// Full Bulletproofs range proof implementation.
//
// Paper: Bünz et al., "Bulletproofs: Short Proofs for Confidential
// Transactions and More", IEEE S&P 2018. https://eprint.iacr.org/2017/1066.pdf
//
// Proves 0 ≤ v < 2^64 in zero-knowledge without revealing v.
// Proof size: ~640 bytes (20 group elements + 2 scalars × 32 bytes each).
// Verify cost: O(n log n) = O(64×6) group operations ≈ sub-millisecond.
//
// Security: honest-verifier ZK and computational soundness under the
// discrete-logarithm assumption on Ed25519. All generators are derived via
// try-and-increment hash-to-point so their mutual discrete logs are unknown.

import (
        "crypto/sha256"
        "crypto/sha512"
        "encoding/binary"
        "fmt"

        "filippo.io/edwards25519"
)

// ─── Constants ────────────────────────────────────────────────────────────────

const bpBits = 64  // range [0, 2^bpBits)
const bpLog  = 6   // log₂(bpBits)

// ─── Generators ───────────────────────────────────────────────────────────────

// BpH is the value generator: V = γ·G + v·BpH.
// Shared with pedersen.go so Pedersen commitments and range proofs use the
// same H — which is required for the proof to cover the actual output amount.
var BpH *edwards25519.Point

var (
        bpU    *edwards25519.Point         // auxiliary IPA generator
        bpGvec [bpBits]*edwards25519.Point // vector generators G₀..G₆₃
        bpHvec [bpBits]*edwards25519.Point // vector generators H₀..H₆₃
)

func init() {
        BpH  = hashToPoint([]byte("Aperod/BP/H/v2"))
        bpU  = hashToPoint([]byte("Aperod/BP/U/v2"))
        for i := 0; i < bpBits; i++ {
                var idx [8]byte
                binary.LittleEndian.PutUint64(idx[:], uint64(i))
                bpGvec[i] = hashToPoint(append([]byte("Aperod/BP/Gvec/v2/"), idx[:]...))
                bpHvec[i] = hashToPoint(append([]byte("Aperod/BP/Hvec/v2/"), idx[:]...))
        }
}

// hashToPoint maps arbitrary bytes to a prime-order point on Ed25519 using
// try-and-increment. No one knows log_G(result).
func hashToPoint(seed []byte) *edwards25519.Point {
        h := sha256.New()
        for ctr := uint64(0); ; ctr++ {
                h.Reset()
                h.Write([]byte("Aperod/H2P/v1"))
                h.Write(seed)
                var cb [8]byte
                binary.LittleEndian.PutUint64(cb[:], ctr)
                h.Write(cb[:])
                digest := h.Sum(nil)

                var buf [32]byte
                copy(buf[:], digest)

                // Try positive-x compressed point
                buf[31] &= 0x7f
                if p, err := new(edwards25519.Point).SetBytes(buf[:]); err == nil {
                        r := new(edwards25519.Point).MultByCofactor(p)
                        if r.Equal(edwards25519.NewIdentityPoint()) == 0 {
                                return r
                        }
                }
                // Try negative-x
                buf[31] = digest[31] | 0x80
                if p, err := new(edwards25519.Point).SetBytes(buf[:]); err == nil {
                        r := new(edwards25519.Point).MultByCofactor(p)
                        if r.Equal(edwards25519.NewIdentityPoint()) == 0 {
                                return r
                        }
                }
        }
}

// ─── RangeProof ───────────────────────────────────────────────────────────────

// RangeProof proves that a committed value v satisfies 0 ≤ v < 2^64
// without revealing v. Fields are encoded as 32-byte compressed points/scalars.
type RangeProof struct {
        V    [32]byte         // V = γ·G + v·BpH  (value commitment)
        A    [32]byte         // A = ⟨aL,Gvec⟩ + ⟨aR,Hvec⟩ + α·G
        S    [32]byte         // S = ⟨sL,Gvec⟩ + ⟨sR,Hvec⟩ + ρ·G
        T1   [32]byte         // T₁ = τ₁·G + t₁·BpH
        T2   [32]byte         // T₂ = τ₂·G + t₂·BpH
        Taux [32]byte         // τₓ = τ₂x² + τ₁x + z²γ
        Mu   [32]byte         // μ  = α + ρx
        Tx   [32]byte         // t̂  = ⟨l,r⟩
        Ls   [bpLog][32]byte  // L commitments from inner-product argument
        Rs   [bpLog][32]byte  // R commitments from inner-product argument
        AFin [32]byte         // final scalar a from IPA
        BFin [32]byte         // final scalar b from IPA

        // ValueCommit mirrors V for backward compatibility with tx_verifier.go.
        ValueCommit Commitment
}

// ─── Transcript (Fiat-Shamir) ─────────────────────────────────────────────────

// bpTranscript is a running SHA3-256 chain used to derive non-interactive
// challenges. Each absorbed value updates the state; challenges are derived
// from the current state and then fed back to bind subsequent challenges.
type bpTranscript struct {
        acc [32]byte
}

func newBPTranscript(label string) *bpTranscript {
        h := sha256.New()
        h.Write([]byte("Aperod/BP/transcript/v2/"))
        h.Write([]byte(label))
        var t bpTranscript
        copy(t.acc[:], h.Sum(nil))
        return &t
}

func (t *bpTranscript) absorb(b []byte) {
        h := sha256.New()
        h.Write(t.acc[:])
        h.Write(b)
        copy(t.acc[:], h.Sum(nil))
}

func (t *bpTranscript) absorbPoint(p *edwards25519.Point)   { t.absorb(p.Bytes()) }
func (t *bpTranscript) absorbScalar(s *edwards25519.Scalar) { t.absorb(s.Bytes()) }

// challenge derives a uniform non-zero scalar from the current state,
// then advances the state so the next challenge is independent.
func (t *bpTranscript) challenge() *edwards25519.Scalar {
        h := sha512.New()
        h.Write([]byte("challenge"))
        h.Write(t.acc[:])
        var wide [64]byte
        copy(wide[:], h.Sum(nil))
        s, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
        t.absorb(s.Bytes()) // advance state
        return s
}

// ─── Scalar utilities ─────────────────────────────────────────────────────────

func scalarOne() *edwards25519.Scalar {
        var b [32]byte
        b[0] = 1
        s, _ := edwards25519.NewScalar().SetCanonicalBytes(b[:])
        return s
}

// powerVec returns [base⁰, base¹, …, baseⁿ⁻¹].
func powerVec(base *edwards25519.Scalar, n int) []*edwards25519.Scalar {
        v := make([]*edwards25519.Scalar, n)
        v[0] = scalarOne()
        for i := 1; i < n; i++ {
                v[i] = edwards25519.NewScalar().Multiply(v[i-1], base)
        }
        return v
}

// pow2Vec returns [1, 2, 4, …, 2ⁿ⁻¹].
func pow2Vec(n int) []*edwards25519.Scalar {
        two := scalarFromUint64(2)
        return powerVec(two, n)
}

// innerProduct computes ⟨a,b⟩ = Σ aᵢ·bᵢ.
func innerProduct(a, b []*edwards25519.Scalar) *edwards25519.Scalar {
        r := edwards25519.NewScalar()
        for i := range a {
                r.MultiplyAdd(a[i], b[i], r)
        }
        return r
}

func vecAddS(v []*edwards25519.Scalar, s *edwards25519.Scalar) []*edwards25519.Scalar {
        out := make([]*edwards25519.Scalar, len(v))
        for i := range v {
                out[i] = edwards25519.NewScalar().Add(v[i], s)
        }
        return out
}

func vecSubS(v []*edwards25519.Scalar, s *edwards25519.Scalar) []*edwards25519.Scalar {
        out := make([]*edwards25519.Scalar, len(v))
        for i := range v {
                out[i] = edwards25519.NewScalar().Subtract(v[i], s)
        }
        return out
}

func vecHadamard(a, b []*edwards25519.Scalar) []*edwards25519.Scalar {
        out := make([]*edwards25519.Scalar, len(a))
        for i := range a {
                out[i] = edwards25519.NewScalar().Multiply(a[i], b[i])
        }
        return out
}

func vecAddVec(a, b []*edwards25519.Scalar) []*edwards25519.Scalar {
        out := make([]*edwards25519.Scalar, len(a))
        for i := range a {
                out[i] = edwards25519.NewScalar().Add(a[i], b[i])
        }
        return out
}

func vecScale(s *edwards25519.Scalar, v []*edwards25519.Scalar) []*edwards25519.Scalar {
        out := make([]*edwards25519.Scalar, len(v))
        for i := range v {
                out[i] = edwards25519.NewScalar().Multiply(s, v[i])
        }
        return out
}

// msm is a convenience alias for variable-time multi-scalar multiplication.
func msm(scalars []*edwards25519.Scalar, points []*edwards25519.Point) *edwards25519.Point {
        return new(edwards25519.Point).VarTimeMultiScalarMult(scalars, points)
}

// pointSlice converts a fixed array of *Point to a slice.
func pointSlice(arr [bpBits]*edwards25519.Point) []*edwards25519.Point {
        s := make([]*edwards25519.Point, bpBits)
        copy(s, arr[:])
        return s
}

// ─── ProveRange ────────────────────────────────────────────────────────────────

// ProveRange creates a Bulletproof range proof that value v satisfies
// 0 ≤ v < 2^64 using blinding factor blind.
func ProveRange(value uint64, blind BlindFactor) (*RangeProof, error) {
        // 1. Value commitment V = γ·G + v·BpH ────────────────────────────────
        v := scalarFromUint64(value)
        gamma, err := ScalarFromBytes(blind[:])
        if err != nil {
                clamped := clampScalar(blind)
                gamma, err = ScalarFromBytes(clamped[:])
                if err != nil {
                        return nil, fmt.Errorf("bulletproof: invalid blind: %w", err)
                }
        }
        V := new(edwards25519.Point).Add(
                new(edwards25519.Point).ScalarBaseMult(gamma),
                new(edwards25519.Point).ScalarMult(v, BpH),
        )

        // 2. Bit decomposition: aL[i] = bit i of v, aR[i] = aL[i] - 1 ────────
        aL := make([]*edwards25519.Scalar, bpBits)
        aR := make([]*edwards25519.Scalar, bpBits)
        one := scalarOne()
        for i := 0; i < bpBits; i++ {
                aL[i] = scalarFromUint64((value >> uint(i)) & 1)
                aR[i] = edwards25519.NewScalar().Subtract(aL[i], one)
        }

        // 3. Commit to aL, aR: A = ⟨aL,Gvec⟩ + ⟨aR,Hvec⟩ + α·G ─────────────
        alphaBF, _ := NewBlindFactor()
        alpha, _   := ScalarFromBytes(alphaBF[:])
        gv := pointSlice(bpGvec)
        hv := pointSlice(bpHvec)
        A  := new(edwards25519.Point).ScalarBaseMult(alpha)
        A.Add(A, msm(aL, gv))
        A.Add(A, msm(aR, hv))

        // 4. Blinding vectors sL, sR; S = ⟨sL,Gvec⟩ + ⟨sR,Hvec⟩ + ρ·G ──────
        sL := make([]*edwards25519.Scalar, bpBits)
        sR := make([]*edwards25519.Scalar, bpBits)
        for i := 0; i < bpBits; i++ {
                bf, _ := NewBlindFactor()
                sL[i], _ = ScalarFromBytes(bf[:])
                bf, _ = NewBlindFactor()
                sR[i], _ = ScalarFromBytes(bf[:])
        }
        rhoBF, _ := NewBlindFactor()
        rho, _   := ScalarFromBytes(rhoBF[:])
        S := new(edwards25519.Point).ScalarBaseMult(rho)
        S.Add(S, msm(sL, gv))
        S.Add(S, msm(sR, hv))

        // 5. Challenges y, z ───────────────────────────────────────────────────
        tr := newBPTranscript("range-proof/v2")
        tr.absorbPoint(V)
        tr.absorbPoint(A)
        tr.absorbPoint(S)
        y := tr.challenge()
        z := tr.challenge()

        // 6. Polynomial l(x), r(x) ─────────────────────────────────────────────
        // l(x) = (aL - z) + sL·x          l₀ = aL-z, l₁ = sL
        // r(x) = yn⊙(aR + z) + z²·2ⁿ + yn⊙sR·x
        //                                  r₀ = yn⊙(aR+z)+z²·2ⁿ, r₁ = yn⊙sR
        yn   := powerVec(y, bpBits)
        twon := pow2Vec(bpBits)
        z2   := edwards25519.NewScalar().Multiply(z, z)

        l0 := vecSubS(aL, z)
        l1 := sL

        r0 := vecAddVec(vecHadamard(yn, vecAddS(aR, z)), vecScale(z2, twon))
        r1 := vecHadamard(yn, sR)

        // t₁ = ⟨l₀,r₁⟩ + ⟨l₁,r₀⟩,  t₂ = ⟨l₁,r₁⟩
        t1v := edwards25519.NewScalar().Add(innerProduct(l0, r1), innerProduct(l1, r0))
        t2v := innerProduct(l1, r1)

        // 7. Commit to t₁, t₂: Tᵢ = τᵢ·G + tᵢ·BpH ────────────────────────────
        tau1BF, _ := NewBlindFactor()
        tau1, _   := ScalarFromBytes(tau1BF[:])
        tau2BF, _ := NewBlindFactor()
        tau2, _   := ScalarFromBytes(tau2BF[:])
        T1 := new(edwards25519.Point).Add(
                new(edwards25519.Point).ScalarBaseMult(tau1),
                new(edwards25519.Point).ScalarMult(t1v, BpH),
        )
        T2 := new(edwards25519.Point).Add(
                new(edwards25519.Point).ScalarBaseMult(tau2),
                new(edwards25519.Point).ScalarMult(t2v, BpH),
        )

        // 8. Challenge x ───────────────────────────────────────────────────────
        tr.absorbPoint(T1)
        tr.absorbPoint(T2)
        x  := tr.challenge()
        x2 := edwards25519.NewScalar().Multiply(x, x)

        // 9. Evaluate polynomials at x ─────────────────────────────────────────
        l    := vecAddVec(l0, vecScale(x, l1))
        r    := vecAddVec(r0, vecScale(x, r1))
        txHat := innerProduct(l, r)

        // τₓ = τ₂x² + τ₁x + z²γ
        taux := edwards25519.NewScalar()
        taux.MultiplyAdd(tau2, x2, taux)
        taux.MultiplyAdd(tau1, x, taux)
        taux.MultiplyAdd(z2, gamma, taux)

        // μ = α + ρx
        mu := edwards25519.NewScalar().MultiplyAdd(rho, x, alpha)

        // 10. IPA ──────────────────────────────────────────────────────────────
        tr.absorbScalar(taux)
        tr.absorbScalar(mu)
        tr.absorbScalar(txHat)
        w  := tr.challenge()
        Uw := new(edwards25519.Point).ScalarMult(w, bpU)

        // H' = y⁻ⁱ · Hvec[i]  (modified generators for IPA)
        yInvPow := powerVec(edwards25519.NewScalar().Invert(y), bpBits)
        hPrime  := make([]*edwards25519.Point, bpBits)
        for i := 0; i < bpBits; i++ {
                hPrime[i] = new(edwards25519.Point).ScalarMult(yInvPow[i], bpHvec[i])
        }

        Gipa := pointSlice(bpGvec)
        Ls, Rs, aFin, bFin, err := ipaProve(Gipa, hPrime, l, r, Uw, tr)
        if err != nil {
                return nil, fmt.Errorf("bulletproof IPA: %w", err)
        }

        // 11. Assemble proof ───────────────────────────────────────────────────
        proof := &RangeProof{}
        copy(proof.V[:],    V.Bytes())
        copy(proof.A[:],    A.Bytes())
        copy(proof.S[:],    S.Bytes())
        copy(proof.T1[:],   T1.Bytes())
        copy(proof.T2[:],   T2.Bytes())
        copy(proof.Taux[:], taux.Bytes())
        copy(proof.Mu[:],   mu.Bytes())
        copy(proof.Tx[:],   txHat.Bytes())
        for k := 0; k < bpLog; k++ {
                copy(proof.Ls[k][:], Ls[k].Bytes())
                copy(proof.Rs[k][:], Rs[k].Bytes())
        }
        copy(proof.AFin[:], aFin.Bytes())
        copy(proof.BFin[:], bFin.Bytes())
        copy(proof.ValueCommit[:], V.Bytes())
        return proof, nil
}

// ─── VerifyRange ──────────────────────────────────────────────────────────────

// VerifyRange checks that the range proof is valid.
// Returns (true, nil) only for proofs created by ProveRange.
func VerifyRange(proof *RangeProof) (bool, error) {
        if proof == nil {
                return false, fmt.Errorf("nil proof")
        }

        // ValueCommit is the externally-visible commitment field; it must match V.
        // Tampering either field breaks the proof.
        if [32]byte(proof.ValueCommit) != proof.V {
                return false, nil
        }

        // Decode proof elements (all must be valid encodings)
        decPt := func(b [32]byte, name string) (*edwards25519.Point, error) {
                p, err := new(edwards25519.Point).SetBytes(b[:])
                if err != nil {
                        return nil, fmt.Errorf("decode %s: %w", name, err)
                }
                return p, nil
        }
        decSc := func(b [32]byte, name string) (*edwards25519.Scalar, error) {
                s, err := edwards25519.NewScalar().SetCanonicalBytes(b[:])
                if err != nil {
                        return nil, fmt.Errorf("decode %s: %w", name, err)
                }
                return s, nil
        }

        V,    err := decPt(proof.V,    "V");    if err != nil { return false, err }
        A,    err := decPt(proof.A,    "A");    if err != nil { return false, err }
        S,    err := decPt(proof.S,    "S");    if err != nil { return false, err }
        T1,   err := decPt(proof.T1,   "T1");   if err != nil { return false, err }
        T2,   err := decPt(proof.T2,   "T2");   if err != nil { return false, err }
        taux, err := decSc(proof.Taux, "taux"); if err != nil { return false, err }
        mu,   err := decSc(proof.Mu,   "mu");   if err != nil { return false, err }
        txHat,err := decSc(proof.Tx,   "tx");   if err != nil { return false, err }
        aFin, err := decSc(proof.AFin, "aFin"); if err != nil { return false, err }
        bFin, err := decSc(proof.BFin, "bFin"); if err != nil { return false, err }

        Ls := make([]*edwards25519.Point, bpLog)
        Rs := make([]*edwards25519.Point, bpLog)
        for k := 0; k < bpLog; k++ {
                Ls[k], err = decPt(proof.Ls[k], fmt.Sprintf("L%d", k))
                if err != nil { return false, err }
                Rs[k], err = decPt(proof.Rs[k], fmt.Sprintf("R%d", k))
                if err != nil { return false, err }
        }

        // ── Replay transcript to get challenges ───────────────────────────────
        tr := newBPTranscript("range-proof/v2")
        tr.absorbPoint(V)
        tr.absorbPoint(A)
        tr.absorbPoint(S)
        y := tr.challenge()
        z := tr.challenge()
        tr.absorbPoint(T1)
        tr.absorbPoint(T2)
        x  := tr.challenge()
        x2 := edwards25519.NewScalar().Multiply(x, x)
        tr.absorbScalar(taux)
        tr.absorbScalar(mu)
        tr.absorbScalar(txHat)
        w := tr.challenge()

        z2 := edwards25519.NewScalar().Multiply(z, z)
        z3 := edwards25519.NewScalar().Multiply(z2, z)
        yn   := powerVec(y, bpBits)
        twon := pow2Vec(bpBits)

        // ── Check 1: polynomial commitment ────────────────────────────────────
        // t̂·BpH + τₓ·G == z²·V + δ(y,z)·BpH + x·T₁ + x²·T₂
        //
        // δ(y,z) = (z - z²)·⟨1ⁿ,yⁿ⟩ - z³·⟨1ⁿ,2ⁿ⟩
        sumYn := edwards25519.NewScalar()
        for _, yi := range yn {
                sumYn.Add(sumYn, yi)
        }
        sum2n := edwards25519.NewScalar()
        for _, ti := range twon {
                sum2n.Add(sum2n, ti)
        }
        zMinusZ2 := edwards25519.NewScalar().Subtract(z, z2)
        negZ3s2n := edwards25519.NewScalar().Negate(edwards25519.NewScalar().Multiply(z3, sum2n))
        delta    := edwards25519.NewScalar().MultiplyAdd(zMinusZ2, sumYn, negZ3s2n)

        lhs := new(edwards25519.Point).Add(
                new(edwards25519.Point).ScalarMult(txHat, BpH),
                new(edwards25519.Point).ScalarBaseMult(taux),
        )
        rhs := new(edwards25519.Point).ScalarMult(z2, V)
        rhs.Add(rhs, new(edwards25519.Point).ScalarMult(delta, BpH))
        rhs.Add(rhs, new(edwards25519.Point).ScalarMult(x, T1))
        rhs.Add(rhs, new(edwards25519.Point).ScalarMult(x2, T2))
        if lhs.Equal(rhs) != 1 {
                return false, nil
        }

        // ── Reconstruct P for IPA verification ───────────────────────────────
        // P = A + x·S - z·Σ Gvec[i] + z·Σ Hvec[i] + z²·Σ 2ⁱ·H'[i] + t̂·Uw
        //
        // (derived by expanding commitments A and S; the μ·G terms cancel)
        Uw := new(edwards25519.Point).ScalarMult(w, bpU)

        yInvPow := powerVec(edwards25519.NewScalar().Invert(y), bpBits)
        hPrime  := make([]*edwards25519.Point, bpBits)
        for i := 0; i < bpBits; i++ {
                hPrime[i] = new(edwards25519.Point).ScalarMult(yInvPow[i], bpHvec[i])
        }

        // P = A + x·S - μ·G + t̂·Uw - z·⟨1ⁿ,Gvec⟩ + z·⟨1ⁿ,Hvec⟩ + z²·⟨2ⁿ,H'⟩
        //
        // Derivation:
        //   ⟨l,G⟩ + ⟨r,H'⟩ = (A+x·S-μ·G) - z·⟨1,G⟩ + z·⟨1,Hvec⟩ + z²·⟨2ⁿ,H'⟩
        //   (the z·⟨1,Hvec⟩ comes from the y·y⁻¹=1 cancellation in ⟨r,H'⟩)
        P := new(edwards25519.Point).Add(A, new(edwards25519.Point).ScalarMult(x, S))
        // subtract μ·G  (μ = α + ρx; this removes the G blinding from A+x·S)
        P.Add(P, new(edwards25519.Point).ScalarBaseMult(edwards25519.NewScalar().Negate(mu)))
        P.Add(P, new(edwards25519.Point).ScalarMult(txHat, Uw))
        negZ := edwards25519.NewScalar().Negate(z)
        for i := 0; i < bpBits; i++ {
                P.Add(P, new(edwards25519.Point).ScalarMult(negZ, bpGvec[i]))
                P.Add(P, new(edwards25519.Point).ScalarMult(z, bpHvec[i]))
                coeff := edwards25519.NewScalar().Multiply(z2, twon[i])
                P.Add(P, new(edwards25519.Point).ScalarMult(coeff, hPrime[i]))
        }

        // ── Check 2: IPA ──────────────────────────────────────────────────────
        // Replay IPA challenges and fold P: P_{k+1} = P_k + u_k·L_k + u_k⁻¹·R_k
        ipaU    := [bpLog]*edwards25519.Scalar{}
        ipaUInv := [bpLog]*edwards25519.Scalar{}
        for k := 0; k < bpLog; k++ {
                tr.absorbPoint(Ls[k])
                tr.absorbPoint(Rs[k])
                ipaU[k]    = tr.challenge()
                ipaUInv[k] = edwards25519.NewScalar().Invert(ipaU[k])
                // P' = P + u·L + u⁻¹·R  (our L/R use aR/bL split → linear u scaling)
                P.Add(P, new(edwards25519.Point).ScalarMult(ipaU[k],    Ls[k]))
                P.Add(P, new(edwards25519.Point).ScalarMult(ipaUInv[k], Rs[k]))
        }
        // P is now P_final

        // Fold generators using the same challenges
        Gfin := pointSlice(bpGvec)
        Hfin := make([]*edwards25519.Point, bpBits)
        copy(Hfin, hPrime)
        for k := 0; k < bpLog; k++ {
                half := len(Gfin) / 2
                uInv := ipaUInv[k]
                uk   := ipaU[k]
                newG := make([]*edwards25519.Point, half)
                newH := make([]*edwards25519.Point, half)
                for i := 0; i < half; i++ {
                        newG[i] = new(edwards25519.Point).Add(
                                Gfin[i],
                                new(edwards25519.Point).ScalarMult(uInv, Gfin[half+i]),
                        )
                        newH[i] = new(edwards25519.Point).Add(
                                Hfin[i],
                                new(edwards25519.Point).ScalarMult(uk, Hfin[half+i]),
                        )
                }
                Gfin = newG
                Hfin = newH
        }
        // Gfin[0], Hfin[0] are the final single generators

        // Final check: a·G_fin + b·H_fin + a·b·Uw == P_final
        ab := edwards25519.NewScalar().Multiply(aFin, bFin)
        check := new(edwards25519.Point).Add(
                new(edwards25519.Point).ScalarMult(aFin, Gfin[0]),
                new(edwards25519.Point).ScalarMult(bFin, Hfin[0]),
        )
        check.Add(check, new(edwards25519.Point).ScalarMult(ab, Uw))

        return check.Equal(P) == 1, nil
}

// ─── Inner Product Argument (IPA) ─────────────────────────────────────────────

// ipaProve recursively proves ⟨a,b⟩ is consistent with generators G and H.
// Returns L/R commitments per round and the final scalar pair (a,b).
func ipaProve(
        G, H []*edwards25519.Point,
        a, b []*edwards25519.Scalar,
        U *edwards25519.Point,
        tr *bpTranscript,
) (Ls [bpLog]*edwards25519.Point, Rs [bpLog]*edwards25519.Point, aFin, bFin *edwards25519.Scalar, err error) {
        n := len(a)
        for round := 0; n > 1; round++ {
                if n%2 != 0 {
                        err = fmt.Errorf("IPA: n=%d is not a power of 2", n)
                        return
                }
                half := n / 2
                aL, aR := a[:half], a[half:]
                bL, bR := b[:half], b[half:]
                GL, GR := G[:half], G[half:]
                HL, HR := H[:half], H[half:]

                // cL = ⟨aR,bL⟩,  cR = ⟨aL,bR⟩
                cL := innerProduct(aR, bL)
                cR := innerProduct(aL, bR)

                // L = ⟨aR,GL⟩ + ⟨bL,HR⟩ + cL·U
                L := msm(aR, GL)
                L.Add(L, msm(bL, HR))
                L.Add(L, new(edwards25519.Point).ScalarMult(cL, U))

                // R = ⟨aL,GR⟩ + ⟨bR,HL⟩ + cR·U
                R := msm(aL, GR)
                R.Add(R, msm(bR, HL))
                R.Add(R, new(edwards25519.Point).ScalarMult(cR, U))

                Ls[round] = L
                Rs[round] = R

                // Fiat-Shamir challenge u for this round
                tr.absorbPoint(L)
                tr.absorbPoint(R)
                u    := tr.challenge()
                uInv := edwards25519.NewScalar().Invert(u)

                // Fold: G' = GL + u⁻¹·GR,  H' = HL + u·HR
                newG := make([]*edwards25519.Point, half)
                newH := make([]*edwards25519.Point, half)
                for i := 0; i < half; i++ {
                        newG[i] = new(edwards25519.Point).Add(GL[i], new(edwards25519.Point).ScalarMult(uInv, GR[i]))
                        newH[i] = new(edwards25519.Point).Add(HL[i], new(edwards25519.Point).ScalarMult(u,    HR[i]))
                }
                // Fold: a' = aL + u·aR,  b' = bL + u⁻¹·bR
                newA := make([]*edwards25519.Scalar, half)
                newB := make([]*edwards25519.Scalar, half)
                for i := 0; i < half; i++ {
                        newA[i] = edwards25519.NewScalar().MultiplyAdd(u,    aR[i], aL[i])
                        newB[i] = edwards25519.NewScalar().MultiplyAdd(uInv, bR[i], bL[i])
                }
                G, H, a, b, n = newG, newH, newA, newB, half
        }
        return Ls, Rs, a[0], b[0], nil
}

// ─── Legacy helpers (kept for isZero32 used by old tests) ─────────────────────

func isZero32(b [32]byte) bool {
        var zero [32]byte
        for i := range b {
                if b[i] != zero[i] {
                        return false
                }
        }
        return true
}
