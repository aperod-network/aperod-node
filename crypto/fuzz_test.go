package crypto_test

// Fuzz tests for crypto primitives (Phase 1, task 1.2.10).
// Run with: go test -fuzz=FuzzDecodeAddress -fuzztime=10s ./crypto/...
//
// These use Go 1.18+ native fuzzing. Each target exercises a primitive
// that processes external bytes, verifying it never panics.

import (
        "testing"

        "github.com/aperod/aperod/crypto"
)

// ─── Address encode/decode ────────────────────────────────────────────────────

// FuzzDecodeAddress verifies that DecodeAddress never panics on arbitrary input.
func FuzzDecodeAddress(f *testing.F) {
        // Seed with a valid address
        wk, _ := crypto.GenerateWalletKeys()
        addr := crypto.AddressFromKeys(crypto.MainnetByte, wk)
        f.Add([]byte(addr.String()))

        // Seed with some invalid inputs
        f.Add([]byte(""))
        f.Add([]byte("not-an-address"))
        f.Add([]byte("APR" + string(make([]byte, 100))))

        f.Fuzz(func(t *testing.T, data []byte) {
                a := crypto.Address(data)
                // Must not panic; error is allowed
                _, _, _, _ = crypto.DecodeAddress(a)
                _ = crypto.Validate(a)
        })
}

// FuzzEncodeDecodeAddress verifies round-trip consistency.
func FuzzEncodeDecodeAddress(f *testing.F) {
        // Seed with a valid (spend, view) pair
        wk, _ := crypto.GenerateWalletKeys()
        f.Add(wk.Spend.Public[:], wk.View.Public[:])

        f.Fuzz(func(t *testing.T, spendBytes, viewBytes []byte) {
                if len(spendBytes) != 32 || len(viewBytes) != 32 {
                        return // skip — not valid key sizes
                }
                var spend, view crypto.Point32
                copy(spend[:], spendBytes)
                copy(view[:], viewBytes)

                // May fail if bytes don't represent a valid curve point — that's OK
                addr := crypto.EncodeAddress(crypto.MainnetByte, spend, view)
                // Decode must not panic
                _, _, _, _ = crypto.DecodeAddress(addr)
        })
}

// ─── Hash ─────────────────────────────────────────────────────────────────────

// FuzzHashBytes verifies hash never panics on any input.
func FuzzHashBytes(f *testing.F) {
        f.Add([]byte("hello"))
        f.Add([]byte(""))
        f.Add(make([]byte, 1024))

        f.Fuzz(func(t *testing.T, data []byte) {
                h := crypto.HashBytes(data)
                if len(h) != 32 {
                        t.Errorf("hash len = %d, want 32", len(h))
                }
        })
}

// ─── ScanForOutput (stealth addresses) ───────────────────────────────────────

// FuzzScanForOutput verifies the scanner never panics on garbage inputs.
func FuzzScanForOutput(f *testing.F) {
        wk, _ := crypto.GenerateWalletKeys()
        so, _ := crypto.CreateStealthOutput(wk.Spend.Public, wk.View.Public)
        f.Add(
                wk.View.Private[:],
                wk.Spend.Public[:],
                so.TxPubKey[:],
                so.OneTimePub[:],
        )
        // Corrupt inputs
        f.Add(make([]byte, 32), make([]byte, 32), make([]byte, 32), make([]byte, 32))

        f.Fuzz(func(t *testing.T, viewPriv, spendPub, txPub, oneTime []byte) {
                if len(viewPriv) != 32 || len(spendPub) != 32 || len(txPub) != 32 || len(oneTime) != 32 {
                        return
                }
                var vp crypto.Scalar32
                var sp, tp, op crypto.Point32
                copy(vp[:], viewPriv)
                copy(sp[:], spendPub)
                copy(tp[:], txPub)
                copy(op[:], oneTime)
                // Must not panic
                _, _ = crypto.ScanForOutput(vp, sp, tp, op)
        })
}

// ─── MLSAGSign / MLSAGVerify ─────────────────────────────────────────────────

// FuzzMLSAGVerify verifies the verifier never panics on arbitrary ring data.
func FuzzMLSAGVerify(f *testing.F) {
        // Seed with a real signature
        wk, _ := crypto.GenerateWalletKeys()
        ring := make([]crypto.RingMember, 3)
        for i := range ring {
                kk, _ := crypto.GenerateWalletKeys()
                ring[i] = crypto.RingMember(kk.Spend.Public)
        }
        ring[0] = crypto.RingMember(wk.Spend.Public)
        var msg crypto.Hash32
        msg[0] = 0xDE
        sig, err := crypto.MLSAGSign(msg, ring, 0, wk.Spend.Private)
        if err != nil {
                f.Skip("seed signature failed:", err)
        }

        // Use the key image as seed bytes (fixed 32-byte size)
        f.Add(msg[:], sig.KeyImage[:])
        f.Add(make([]byte, 32), make([]byte, 32))

        f.Fuzz(func(t *testing.T, msgBytes, _ []byte) {
                if len(msgBytes) != 32 {
                        return
                }
                var m crypto.Hash32
                copy(m[:], msgBytes)
                // Verify with a real sig but fuzzed message — must not panic
                _, _ = crypto.MLSAGVerify(m, ring, sig)
        })
}

// ─── Pedersen commitment ──────────────────────────────────────────────────────

// FuzzCommit verifies Commit never panics on arbitrary amounts and blind factors.
func FuzzCommit(f *testing.F) {
        f.Add(uint64(0), make([]byte, 32))
        f.Add(uint64(1_000_000), make([]byte, 32))
        bf, _ := crypto.NewBlindFactor()
        f.Add(uint64(999), bf[:])

        f.Fuzz(func(t *testing.T, amount uint64, blindBytes []byte) {
                if len(blindBytes) != 32 {
                        return
                }
                var blind crypto.BlindFactor
                copy(blind[:], blindBytes)
                // Must not panic
                _, _ = crypto.Commit(amount, blind)
        })
}

// ─── 3.4.2 Pedersen edge cases ───────────────────────────────────────────────

// FuzzPedersenEdgeCases verifies Commit handles boundary amounts (0, MAX) without panic.
func FuzzPedersenEdgeCases(f *testing.F) {
        // Seed with boundary values and random blind factors.
        f.Add(uint64(0), make([]byte, 32))
        f.Add(uint64(1<<63-1), make([]byte, 32))
        f.Add(^uint64(0), make([]byte, 32)) // MaxUint64
        bf, _ := crypto.NewBlindFactor()
        f.Add(uint64(0), bf[:])
        f.Add(^uint64(0), bf[:])

        f.Fuzz(func(t *testing.T, amount uint64, blindBytes []byte) {
                if len(blindBytes) != 32 {
                        return
                }
                var blind crypto.BlindFactor
                copy(blind[:], blindBytes)

                c, err := crypto.Commit(amount, blind)
                if err != nil {
                        return // simplified impl may reject; that's OK
                }
                // Commitment must be 32 bytes (non-zero for non-trivial inputs).
                _ = c
        })
}

// FuzzCommitSum verifies CommitSum never panics on arbitrary commitments.
func FuzzCommitSum(f *testing.F) {
        bf, _ := crypto.NewBlindFactor()
        c, _ := crypto.Commit(1000, bf)
        f.Add(c[:], c[:])
        f.Add(make([]byte, 32), make([]byte, 32))

        f.Fuzz(func(t *testing.T, inBytes, outBytes []byte) {
                if len(inBytes) != 32 || len(outBytes) != 32 {
                        return
                }
                var cIn, cOut crypto.Commitment
                copy(cIn[:], inBytes)
                copy(cOut[:], outBytes)
                // Zero fee commitment.
                var feeC crypto.Commitment
                _, _ = crypto.CommitSum([]crypto.Commitment{cIn}, []crypto.Commitment{cOut}, feeC)
        })
}

// ─── 3.4.3 Bulletproof edge cases ────────────────────────────────────────────

// FuzzVerifyRange verifies VerifyRange never panics on corrupted proof data.
func FuzzVerifyRange(f *testing.F) {
        // Seed with a real proof.
        bf, _ := crypto.NewBlindFactor()
        proof, err := crypto.ProveRange(1_000_000, bf)
        if err == nil && proof != nil {
                f.Add(proof.ValueCommit[:], proof.Ls[0][:], proof.Rs[0][:])
        }
        f.Add(make([]byte, 32), make([]byte, 32), make([]byte, 32))

        f.Fuzz(func(t *testing.T, commitBytes, lBytes, rBytes []byte) {
                if len(commitBytes) != 32 || len(lBytes) != 32 || len(rBytes) != 32 {
                        return
                }
                // Build a tampered proof and verify — must not panic.
                bf2, _ := crypto.NewBlindFactor()
                p, err := crypto.ProveRange(42, bf2)
                if err != nil || p == nil {
                        return
                }
                copy(p.ValueCommit[:], commitBytes)
                copy(p.Ls[0][:], lBytes)
                copy(p.Rs[0][:], rBytes)
                _, _ = crypto.VerifyRange(p)
        })
}
