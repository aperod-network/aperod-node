package crypto_test

// Third round: target specific uncovered branches in crypto.

import (
        "testing"

        "github.com/aperod/aperod/crypto"
)

// ─── wallet.go: WalletKeysFromSeed short-seed error ───────────────────────────

func TestWalletKeysFromSeed_ShortSeed(t *testing.T) {
        _, err := crypto.WalletKeysFromSeed([]byte("short"))
        if err == nil {
                t.Error("WalletKeysFromSeed must reject seeds shorter than 32 bytes")
        }
}

// ─── keys.go: ValidatorPrivKeyFromBytes / ValidatorPubKeyFromBytes ────────────

func TestValidatorPrivKeyFromBytes_WrongLen(t *testing.T) {
        _, err := crypto.ValidatorPrivKeyFromBytes([]byte("too-short"))
        if err == nil {
                t.Error("expected error for wrong-length private key bytes")
        }
}

func TestValidatorPrivKeyFromBytes_RoundTrip(t *testing.T) {
        priv, _, err := crypto.GenerateValidatorKey()
        if err != nil {
                t.Fatal(err)
        }
        priv2, err := crypto.ValidatorPrivKeyFromBytes([]byte(priv))
        if err != nil {
                t.Fatalf("ValidatorPrivKeyFromBytes: %v", err)
        }
        if string(priv2) != string(priv) {
                t.Error("round-trip must preserve private key bytes")
        }
}

func TestValidatorPubKeyFromBytes_WrongLen(t *testing.T) {
        _, err := crypto.ValidatorPubKeyFromBytes([]byte("nope"))
        if err == nil {
                t.Error("expected error for wrong-length public key bytes")
        }
}

func TestValidatorPubKeyFromBytes_RoundTrip(t *testing.T) {
        _, pub, err := crypto.GenerateValidatorKey()
        if err != nil {
                t.Fatal(err)
        }
        pub2, err := crypto.ValidatorPubKeyFromBytes([]byte(pub))
        if err != nil {
                t.Fatalf("ValidatorPubKeyFromBytes: %v", err)
        }
        if pub2.Hex() != pub.Hex() {
                t.Error("round-trip must preserve public key bytes")
        }
}

// ─── keys.go: ID() ────────────────────────────────────────────────────────────

func TestValidatorPubKey_ID_Short(t *testing.T) {
        _, pub, _ := crypto.GenerateValidatorKey()
        id := pub.ID()
        if len(id) != 8 {
                t.Errorf("ID() length = %d, want 8", len(id))
        }
        if id != pub.Hex()[:8] {
                t.Errorf("ID() = %q, want first 8 chars of Hex()", id)
        }
}

// ─── pedersen.go: Commit with non-canonical blind triggers clampScalar ─────────

func TestCommit_NonCanonicalBlind(t *testing.T) {
        var bad crypto.BlindFactor
        for i := range bad {
                bad[i] = 0xFF
        }
        // Must not panic; may succeed (after clamping) or return error
        _, err := crypto.Commit(42, bad)
        t.Logf("Commit with 0xFF blind: err=%v", err)
}

// ─── pedersen.go: BlindSum empty-slice paths ─────────────────────────────────

func TestBlindSum_AllNegative(t *testing.T) {
        b, _ := crypto.NewBlindFactor()
        _, err := crypto.BlindSum(nil, []crypto.BlindFactor{b})
        if err != nil {
                t.Fatalf("BlindSum(nil, [b]): %v", err)
        }
}

func TestBlindSum_AllPositive(t *testing.T) {
        b1, _ := crypto.NewBlindFactor()
        b2, _ := crypto.NewBlindFactor()
        result, err := crypto.BlindSum([]crypto.BlindFactor{b1, b2}, nil)
        if err != nil {
                t.Fatalf("BlindSum([b1,b2], nil): %v", err)
        }
        var zero crypto.BlindFactor
        if result == zero {
                t.Error("BlindSum([b1,b2], nil) must not be zero (unless b1+b2=0 mod l)")
        }
}

// ─── address.go: Short() ─────────────────────────────────────────────────────

func TestAddress_Short(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)
        short := addr.Short()
        if len(short) == 0 {
                t.Error("Short() must return a non-empty string")
        }
        if len(short) >= len(addr.String()) {
                t.Error("Short() must be shorter than the full address")
        }
}

// ─── stealth.go: CreateStealthOutput ─────────────────────────────────────────

func TestCreateStealthOutput_Fields(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        out, err := crypto.CreateStealthOutput(wk.Spend.Public, wk.View.Public)
        if err != nil {
                t.Fatalf("CreateStealthOutput: %v", err)
        }
        var zeroPoint crypto.Point32
        if out.OneTimePub == zeroPoint {
                t.Error("StealthOutput.OneTimePub must not be zero")
        }
        if out.TxPubKey == zeroPoint {
                t.Error("StealthOutput.TxPubKey must not be zero")
        }
}

// ─── ringct.go: MLSAGSign / MLSAGVerify with RingMember ─────────────────────

func TestMLSAG_SignAndVerify_v2(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        ring := make([]crypto.RingMember, crypto.RingSize)
        ring[0] = wk.Spend.Public
        for i := 1; i < crypto.RingSize; i++ {
                decoy, _ := crypto.GenerateWalletKeys()
                ring[i] = decoy.Spend.Public
        }
        msg := crypto.Hash32{}
        msg[0] = 0xBC

        sig, err := crypto.MLSAGSign(msg, ring, 0, wk.Spend.Private)
        if err != nil {
                t.Fatalf("MLSAGSign: %v", err)
        }
        ok, err := crypto.MLSAGVerify(msg, ring, sig)
        if err != nil {
                t.Fatalf("MLSAGVerify: %v", err)
        }
        if !ok {
                t.Error("MLSAGVerify must return true for a valid ring signature")
        }
}

func TestMLSAG_VerifyTamperedMsg_v2(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        ring := make([]crypto.RingMember, crypto.RingSize)
        ring[0] = wk.Spend.Public
        for i := 1; i < crypto.RingSize; i++ {
                decoy, _ := crypto.GenerateWalletKeys()
                ring[i] = decoy.Spend.Public
        }
        msg := crypto.Hash32{}
        sig, _ := crypto.MLSAGSign(msg, ring, 0, wk.Spend.Private)

        msg[0] = 0xFF
        ok, _ := crypto.MLSAGVerify(msg, ring, sig)
        if ok {
                t.Error("MLSAGVerify must return false for tampered message")
        }
}

// ─── wallet.go: AddScalars ────────────────────────────────────────────────────

func TestAddScalars_SumIsValid(t *testing.T) {
        wk1, _ := crypto.GenerateWalletKeys()
        wk2, _ := crypto.GenerateWalletKeys()
        sum, err := crypto.AddScalars(wk1.Spend.Private, wk2.Spend.Private)
        if err != nil {
                t.Fatalf("AddScalars: %v", err)
        }
        var zero crypto.Scalar32
        if sum == zero {
                t.Error("AddScalars result must not be zero for random inputs")
        }
}
