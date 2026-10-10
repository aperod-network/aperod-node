package crypto_test

// Second round of coverage tests targeting remaining gaps.

import (
        "testing"

        "github.com/aperod/aperod/crypto"
)

// ─── wallet.go: WalletKeysFromSeed, PointFromBytes, ScalarFromBytes ───────────

func TestWalletKeysFromSeed_Deterministic(t *testing.T) {
        seed := make([]byte, 32)
        seed[0] = 42
        kp1, err := crypto.WalletKeysFromSeed(seed)
        if err != nil {
                t.Fatalf("WalletKeysFromSeed: %v", err)
        }
        kp2, err := crypto.WalletKeysFromSeed(seed)
        if err != nil {
                t.Fatalf("WalletKeysFromSeed 2nd: %v", err)
        }
        if kp1.Spend.Public != kp2.Spend.Public {
                t.Error("WalletKeysFromSeed must be deterministic (SpendPub)")
        }
        if kp1.View.Public != kp2.View.Public {
                t.Error("WalletKeysFromSeed must be deterministic (ViewPub)")
        }
}

func TestWalletKeysFromSeed_DifferentSeeds(t *testing.T) {
        s1 := make([]byte, 32)
        s1[0] = 1
        s2 := make([]byte, 32)
        s2[0] = 2
        kp1, _ := crypto.WalletKeysFromSeed(s1)
        kp2, _ := crypto.WalletKeysFromSeed(s2)
        if kp1.Spend.Public == kp2.Spend.Public {
                t.Error("different seeds must produce different keys")
        }
}

func TestPointFromBytes_Invalid(t *testing.T) {
        _, err := crypto.PointFromBytes([]byte("not-a-valid-point"))
        if err == nil {
                t.Error("expected error for invalid point bytes")
        }
}

func TestScalarFromBytes_Invalid(t *testing.T) {
        big := make([]byte, 32)
        for i := range big {
                big[i] = 0xFF
        }
        _, err := crypto.ScalarFromBytes(big)
        if err == nil {
                t.Error("expected error for non-canonical scalar bytes")
        }
}

func TestScalarFromBytes_Valid(t *testing.T) {
        var b [32]byte
        b[0] = 1
        s, err := crypto.ScalarFromBytes(b[:])
        if err != nil {
                t.Fatalf("ScalarFromBytes([1,...0]): %v", err)
        }
        if s == nil {
                t.Error("ScalarFromBytes must return non-nil for valid input")
        }
}

// ─── pedersen.go: Commit, CommitSum ──────────────────────────────────────────

func TestCommit_Deterministic(t *testing.T) {
        bf, _ := crypto.NewBlindFactor()
        c1, err := crypto.Commit(42, bf)
        if err != nil {
                t.Fatal(err)
        }
        c2, _ := crypto.Commit(42, bf)
        if c1 != c2 {
                t.Error("Commit must be deterministic")
        }
        c3, _ := crypto.Commit(43, bf)
        if c1 == c3 {
                t.Error("different values must produce different commitments")
        }
}

func TestCommit_ZeroValue(t *testing.T) {
        bf, _ := crypto.NewBlindFactor()
        _, err := crypto.Commit(0, bf)
        if err != nil {
                t.Fatalf("Commit(0): %v", err)
        }
}

func TestCommitSum_Basic(t *testing.T) {
        bIn, _ := crypto.NewBlindFactor()
        bOut, _ := crypto.NewBlindFactor()
        bFee := crypto.BlindFactor{} // zero blind for fee

        commitIn, _ := crypto.Commit(100, bIn)
        commitOut, _ := crypto.Commit(70, bOut)
        feeCommit, _ := crypto.Commit(0, bFee)

        _, err := crypto.CommitSum(
                []crypto.Commitment{commitIn},
                []crypto.Commitment{commitOut},
                feeCommit,
        )
        if err != nil {
                t.Fatalf("CommitSum: %v", err)
        }
}

// ─── stealth.go: CreateStealthAddress, ScanForOutput ─────────────────────────

func TestCreateStealthAddress(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        sa, err := crypto.CreateStealthAddress(wk.Spend.Public, wk.View.Public)
        if err != nil {
                t.Fatal(err)
        }
        var zero crypto.Point32
        if sa.OneTimePub == zero {
                t.Error("StealthAddress.OneTimePub must not be zero")
        }
        if sa.TxPubKey == zero {
                t.Error("StealthAddress.TxPubKey must not be zero")
        }
}

func TestScanForOutput_Detects(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        sa, err := crypto.CreateStealthAddress(wk.Spend.Public, wk.View.Public)
        if err != nil {
                t.Fatal(err)
        }
        scalar, err := crypto.ScanForOutput(wk.View.Private, wk.Spend.Public, sa.TxPubKey, sa.OneTimePub)
        if err != nil {
                t.Fatal(err)
        }
        if scalar == nil {
                t.Error("ScanForOutput must return a scalar for our own output")
        }
}

func TestScanForOutput_Miss(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        wkOther, _ := crypto.GenerateWalletKeys()
        sa, _ := crypto.CreateStealthAddress(wkOther.Spend.Public, wkOther.View.Public)
        scalar, _ := crypto.ScanForOutput(wk.View.Private, wk.Spend.Public, sa.TxPubKey, sa.OneTimePub)
        if scalar != nil {
                t.Error("ScanForOutput must return nil for someone else's output")
        }
}

// ─── bulletproof.go: ProveRange / VerifyRange ─────────────────────────────────

func TestBulletproof_RoundTrip(t *testing.T) {
        bf, _ := crypto.NewBlindFactor()
        proof, err := crypto.ProveRange(100_000_000, bf)
        if err != nil {
                t.Fatalf("ProveRange: %v", err)
        }
        if proof == nil {
                t.Error("proof must not be nil")
        }
        ok, err := crypto.VerifyRange(proof)
        if err != nil {
                t.Fatalf("VerifyRange: %v", err)
        }
        if !ok {
                t.Error("VerifyRange must return true for a valid proof")
        }
}

func TestBulletproof_ZeroValue(t *testing.T) {
        bf, _ := crypto.NewBlindFactor()
        _, err := crypto.ProveRange(0, bf)
        if err != nil {
                t.Fatalf("ProveRange(0): %v", err)
        }
}

// ─── keyimage.go: ComputeKeyImage with valid point ────────────────────────────

func TestComputeKeyImage_ValidPoint(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        ki, err := crypto.ComputeKeyImage(wk.Spend.Private, wk.Spend.Public)
        if err != nil {
                t.Fatalf("ComputeKeyImage: %v", err)
        }
        var zero crypto.KeyImage
        if ki == zero {
                t.Error("KeyImage must be non-zero for valid inputs")
        }
}
