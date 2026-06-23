package crypto_test

// Additional tests targeting uncovered code paths to push coverage above 90%.

import (
        "bytes"
        "testing"

        "github.com/aperod/aperod/crypto"
)

// ─── hash.go ──────────────────────────────────────────────────────────────────

func TestHash_IsZero(t *testing.T) {
        var z crypto.Hash32
        if !z.Zero() {
                t.Error("empty Hash32.Zero() must be true")
        }
        h := crypto.HashBytes([]byte("hello"))
        if h.Zero() {
                t.Error("non-zero hash.Zero() must be false")
        }
}

func TestHash_Bytes(t *testing.T) {
        h := crypto.HashBytes([]byte("hello"))
        b := h.Bytes()
        if len(b) != 32 {
                t.Fatalf("Bytes() len = %d, want 32", len(b))
        }
        if !bytes.Equal(b, h[:]) {
                t.Error("Bytes() must equal underlying array")
        }
}

func TestHash_HashConcat(t *testing.T) {
        a := []byte("alpha")
        b := []byte("beta")
        ab := crypto.HashConcat(a, b)
        ba := crypto.HashConcat(b, a)
        if ab == ba {
                t.Error("HashConcat order must matter")
        }
        if crypto.HashConcat(a, b) != ab {
                t.Error("HashConcat must be deterministic")
        }
}

// ─── keys.go ──────────────────────────────────────────────────────────────────

func TestValidatorKey_PublicIDHex(t *testing.T) {
        priv, pub, err := crypto.GenerateValidatorKey()
        if err != nil {
                t.Fatal(err)
        }
        if priv.Public().ID() != pub.ID() {
                t.Error("priv.Public() must match generated pub")
        }
        if len(pub.Hex()) != 64 {
                t.Errorf("Hex len = %d, want 64", len(pub.Hex()))
        }
        // ID() is a short 8-char prefix for logging; Hex() is the full 64-char hex
        if len(pub.ID()) != 8 {
                t.Errorf("ID() len = %d, want 8", len(pub.ID()))
        }
        if pub.ID() != pub.Hex()[:8] {
                t.Error("ID() must equal Hex()[:8]")
        }
}

func TestValidatorKey_FromBytes_RoundTrip(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()

        priv2, err := crypto.ValidatorPrivKeyFromBytes(priv.Bytes())
        if err != nil {
                t.Fatalf("ValidatorPrivKeyFromBytes: %v", err)
        }
        if priv2.Public().ID() != pub.ID() {
                t.Error("priv roundtrip failed")
        }

        pub2, err := crypto.ValidatorPubKeyFromBytes(pub.Bytes())
        if err != nil {
                t.Fatalf("ValidatorPubKeyFromBytes: %v", err)
        }
        if pub2.ID() != pub.ID() {
                t.Error("pub roundtrip failed")
        }
}

func TestValidatorKey_Verify_BadSig(t *testing.T) {
        _, pub, _ := crypto.GenerateValidatorKey()
        msg := crypto.HashBytes([]byte("test message"))
        if pub.Verify(msg, []byte("badsig")) {
                t.Error("Verify must return false for bad signature")
        }
}

func TestValidatorKey_FromBytes_Invalid(t *testing.T) {
        if _, err := crypto.ValidatorPrivKeyFromBytes([]byte("short")); err == nil {
                t.Error("expected error for short private key bytes")
        }
        if _, err := crypto.ValidatorPubKeyFromBytes([]byte("short")); err == nil {
                t.Error("expected error for short public key bytes")
        }
}

// ─── address.go ───────────────────────────────────────────────────────────────

func TestAddress_FromKeys_StringShort(t *testing.T) {
        wk, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatal(err)
        }
        addr := crypto.AddressFromKeys(crypto.MainnetByte, wk)
        s := addr.String()
        if len(s) < 90 {
                t.Errorf("address string too short: %q", s)
        }
        sh := addr.Short()
        if len(sh) == 0 {
                t.Error("Short() must not be empty")
        }
        t.Logf("address=%s short=%s", s, sh)
}

func TestAddress_Decode_RoundTrip(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        addr := crypto.AddressFromKeys(crypto.MainnetByte, wk)

        _, spend, view, err := crypto.DecodeAddress(addr)
        if err != nil {
                t.Fatalf("DecodeAddress: %v", err)
        }
        addr2 := crypto.EncodeAddress(crypto.MainnetByte, spend, view)
        if addr2.String() != addr.String() {
                t.Error("address roundtrip mismatch")
        }
}

func TestAddress_Decode_Invalid(t *testing.T) {
        if _, _, _, err := crypto.DecodeAddress("not-valid"); err == nil {
                t.Error("expected error for invalid address")
        }
        if _, _, _, err := crypto.DecodeAddress(""); err == nil {
                t.Error("expected error for empty address")
        }
}

// ─── keyimage.go ──────────────────────────────────────────────────────────────

func TestKeyImageSet_AddContains(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        ki, err := crypto.ComputeKeyImage(wk.Spend.Private, wk.Spend.Public)
        if err != nil {
                t.Fatal(err)
        }
        set := crypto.NewKeyImageSet()
        if set.Contains(ki) {
                t.Error("fresh set must not contain key image")
        }
        set.Add(ki)
        if !set.Contains(ki) {
                t.Error("set must contain key image after Add")
        }
}

func TestKeyImage_Equal(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        ki1, err := crypto.ComputeKeyImage(wk.Spend.Private, wk.Spend.Public)
        if err != nil {
                t.Fatal(err)
        }
        ki2, _ := crypto.ComputeKeyImage(wk.Spend.Private, wk.Spend.Public)
        if !ki1.Equal(ki2) {
                t.Error("same inputs must produce equal key images")
        }
        wk2, _ := crypto.GenerateWalletKeys()
        ki3, _ := crypto.ComputeKeyImage(wk2.Spend.Private, wk2.Spend.Public)
        if ki1.Equal(ki3) {
                t.Error("different keys must produce different key images")
        }
}

// ─── wallet.go ────────────────────────────────────────────────────────────────

func TestScalarMulBase(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        pub, err := crypto.ScalarMulBase(wk.Spend.Private)
        if err != nil {
                t.Fatal(err)
        }
        if pub != wk.Spend.Public {
                t.Error("ScalarMulBase(spendPriv) must equal SpendPub")
        }
}

func TestAddScalars(t *testing.T) {
        var a, b crypto.Scalar32
        a[0] = 5
        b[0] = 3
        sum, err := crypto.AddScalars(a, b)
        if err != nil {
                t.Fatal(err)
        }
        sum2, _ := crypto.AddScalars(b, a)
        if sum != sum2 {
                t.Error("AddScalars must be commutative")
        }
        var zero crypto.Scalar32
        if sum == zero {
                t.Error("AddScalars(5,3) must be non-zero")
        }
}

// ─── stealth.go ───────────────────────────────────────────────────────────────

func TestCreateStealthOutput(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        so, err := crypto.CreateStealthOutput(wk.Spend.Public, wk.View.Public)
        if err != nil {
                t.Fatal(err)
        }
        var zeroP crypto.Point32
        if so.OneTimePub == zeroP {
                t.Error("StealthOutput.OneTimePub must not be zero")
        }
        if so.TxPubKey == zeroP {
                t.Error("StealthOutput.TxPubKey must not be zero")
        }
        var zeroS crypto.Scalar32
        if so.HsScalar == zeroS {
                t.Error("StealthOutput.HsScalar must not be zero")
        }
}

// ─── pedersen.go ─────────────────────────────────────────────────────────────

func TestBlindSum_AddAndSubtract(t *testing.T) {
        // Use NewBlindFactor for valid random scalars (raw [32]byte values may be
        // out of the Ed25519 group order and cause "invalid scalar" errors).
        b1, err := crypto.NewBlindFactor()
        if err != nil {
                t.Fatal(err)
        }
        b2, err := crypto.NewBlindFactor()
        if err != nil {
                t.Fatal(err)
        }

        sum, err := crypto.BlindSum([]crypto.BlindFactor{b1, b2}, []crypto.BlindFactor{})
        if err != nil {
                t.Fatalf("BlindSum: %v", err)
        }
        var zero crypto.BlindFactor
        if sum == zero {
                t.Error("BlindSum of two random blinds must be non-zero (astronomically unlikely)")
        }

        // sum of {b1,b2} - b2 should equal b1 (mod l)
        sum2, err := crypto.BlindSum([]crypto.BlindFactor{b1, b2}, []crypto.BlindFactor{b2})
        if err != nil {
                t.Fatalf("BlindSum with sub: %v", err)
        }
        if sum2 != b1 {
                t.Error("BlindSum(add={b1,b2}, sub={b2}) must equal b1")
        }
}
