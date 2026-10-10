package crypto_test

// Benchmarks for 1.9.9: tx verification < 10ms, block verification < 50ms.
// These benchmarks cover the hot paths in crypto.

import (
        "testing"

        "github.com/aperod/aperod/crypto"
)

// ─── Key generation ───────────────────────────────────────────────────────────

func BenchmarkGenerateWalletKeys(b *testing.B) {
        for i := 0; i < b.N; i++ {
                _, _ = crypto.GenerateWalletKeys()
        }
}

func BenchmarkGenerateValidatorKey(b *testing.B) {
        for i := 0; i < b.N; i++ {
                _, _, _ = crypto.GenerateValidatorKey()
        }
}

// ─── Hashing ──────────────────────────────────────────────────────────────────

func BenchmarkHashBytes_1KB(b *testing.B) {
        data := make([]byte, 1024)
        b.ResetTimer()
        for i := 0; i < b.N; i++ {
                _ = crypto.HashBytes(data)
        }
}

// ─── Stealth output ───────────────────────────────────────────────────────────

func BenchmarkCreateStealthOutput(b *testing.B) {
        wk, _ := crypto.GenerateWalletKeys()
        b.ResetTimer()
        for i := 0; i < b.N; i++ {
                _, _ = crypto.CreateStealthOutput(wk.Spend.Public, wk.View.Public)
        }
}

func BenchmarkScanForOutput(b *testing.B) {
        wk, _ := crypto.GenerateWalletKeys()
        out, _ := crypto.CreateStealthOutput(wk.Spend.Public, wk.View.Public)
        b.ResetTimer()
        for i := 0; i < b.N; i++ {
                _, _ = crypto.ScanForOutput(wk.View.Private, wk.Spend.Public, out.TxPubKey, out.OneTimePub)
        }
}

// ─── Pedersen ─────────────────────────────────────────────────────────────────

func BenchmarkNewBlindFactor(b *testing.B) {
        for i := 0; i < b.N; i++ {
                _, _ = crypto.NewBlindFactor()
        }
}

func BenchmarkCommit(b *testing.B) {
        bf, _ := crypto.NewBlindFactor()
        b.ResetTimer()
        for i := 0; i < b.N; i++ {
                _, _ = crypto.Commit(uint64(i+1), bf)
        }
}

// ─── MLSAG ring signature ─────────────────────────────────────────────────────

func BenchmarkMLSAGSign(b *testing.B) {
        wk, _ := crypto.GenerateWalletKeys()
        ring := make([]crypto.RingMember, crypto.RingSize)
        ring[0] = wk.Spend.Public
        for i := 1; i < crypto.RingSize; i++ {
                d, _ := crypto.GenerateWalletKeys()
                ring[i] = d.Spend.Public
        }
        msg := crypto.Hash32{}
        msg[0] = 0x01
        b.ResetTimer()
        for i := 0; i < b.N; i++ {
                _, _ = crypto.MLSAGSign(msg, ring, 0, wk.Spend.Private)
        }
}

func BenchmarkMLSAGVerify(b *testing.B) {
        wk, _ := crypto.GenerateWalletKeys()
        ring := make([]crypto.RingMember, crypto.RingSize)
        ring[0] = wk.Spend.Public
        for i := 1; i < crypto.RingSize; i++ {
                d, _ := crypto.GenerateWalletKeys()
                ring[i] = d.Spend.Public
        }
        msg := crypto.Hash32{}
        sig, _ := crypto.MLSAGSign(msg, ring, 0, wk.Spend.Private)
        b.ResetTimer()
        for i := 0; i < b.N; i++ {
                _, _ = crypto.MLSAGVerify(msg, ring, sig)
        }
}

// ─── Bulletproof ─────────────────────────────────────────────────────────────

func BenchmarkProveRange(b *testing.B) {
        bf, _ := crypto.NewBlindFactor()
        b.ResetTimer()
        for i := 0; i < b.N; i++ {
                _, _ = crypto.ProveRange(1_000_000, bf)
        }
}

func BenchmarkVerifyRange(b *testing.B) {
        bf, _ := crypto.NewBlindFactor()
        proof, _ := crypto.ProveRange(1_000_000, bf)
        b.ResetTimer()
        for i := 0; i < b.N; i++ {
                _, _ = crypto.VerifyRange(proof)
        }
}

// ─── Address encode/decode ────────────────────────────────────────────────────

func BenchmarkEncodeAddress(b *testing.B) {
        wk, _ := crypto.GenerateWalletKeys()
        b.ResetTimer()
        for i := 0; i < b.N; i++ {
                _ = crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)
        }
}

func BenchmarkDecodeAddress(b *testing.B) {
        wk, _ := crypto.GenerateWalletKeys()
        addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)
        b.ResetTimer()
        for i := 0; i < b.N; i++ {
                _, _, _, _ = crypto.DecodeAddress(addr)
        }
}
