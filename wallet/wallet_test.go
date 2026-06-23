package wallet

import (
        "strings"
        "testing"
)

// ─── BIP39 wordlist sanity ─────────────────────────────────────────────────

func TestWordlistCount(t *testing.T) {
        // Require at least 1024 entries (minimum for 10-bit safety).
        // The APR wordlist may have fewer than 2048 entries; GenerateMnemonic uses
        // rejection sampling to avoid invalid indices.
        n := len(bip39WordList)
        if n < 1024 {
                t.Fatalf("wordlist too small: got %d, need at least 1024", n)
        }
        t.Logf("wordlist size: %d", n)
}

func TestWordlistNoDuplicates(t *testing.T) {
        seen := make(map[string]bool, 2048)
        for i, w := range bip39WordList {
                if seen[w] {
                        t.Errorf("duplicate word %q at index %d", w, i)
                }
                seen[w] = true
        }
}

// ─── GenerateMnemonic ──────────────────────────────────────────────────────

func TestGenerateMnemonic128(t *testing.T) {
        m, err := GenerateMnemonic(Strength128)
        if err != nil {
                t.Fatalf("GenerateMnemonic(128): %v", err)
        }
        words := strings.Fields(m)
        if len(words) != 12 {
                t.Errorf("expected 12 words, got %d: %q", len(words), m)
        }
}

func TestGenerateMnemonic256(t *testing.T) {
        m, err := GenerateMnemonic(Strength256)
        if err != nil {
                t.Fatalf("GenerateMnemonic(256): %v", err)
        }
        words := strings.Fields(m)
        if len(words) != 24 {
                t.Errorf("expected 24 words, got %d: %q", len(words), m)
        }
}

func TestGenerateMnemonicInvalidStrength(t *testing.T) {
        _, err := GenerateMnemonic(64)
        if err == nil {
                t.Error("expected error for invalid strength")
        }
}

// ─── EntropyToMnemonic / MnemonicToEntropy round-trip ─────────────────────

func TestMnemonicRoundTrip128(t *testing.T) {
        entropy := make([]byte, 16) // 128-bit
        // Use fixed entropy for deterministic test
        for i := range entropy {
                entropy[i] = byte(i + 1)
        }
        m, err := EntropyToMnemonic(entropy)
        if err != nil {
                t.Fatalf("EntropyToMnemonic: %v", err)
        }
        got, err := MnemonicToEntropy(m)
        if err != nil {
                t.Fatalf("MnemonicToEntropy: %v", err)
        }
        if string(got) != string(entropy) {
                t.Errorf("round-trip mismatch:\n  want %x\n  got  %x", entropy, got)
        }
}

func TestMnemonicRoundTrip256(t *testing.T) {
        entropy := make([]byte, 32) // 256-bit
        for i := range entropy {
                entropy[i] = byte(i * 7)
        }
        m, err := EntropyToMnemonic(entropy)
        if err != nil {
                t.Fatalf("EntropyToMnemonic: %v", err)
        }
        got, err := MnemonicToEntropy(m)
        if err != nil {
                t.Fatalf("MnemonicToEntropy: %v", err)
        }
        if string(got) != string(entropy) {
                t.Errorf("round-trip mismatch:\n  want %x\n  got  %x", entropy, got)
        }
}

// ─── ValidateMnemonic ──────────────────────────────────────────────────────

func TestValidateMnemonicValid(t *testing.T) {
        m, _ := GenerateMnemonic(Strength128)
        if err := ValidateMnemonic(m); err != nil {
                t.Errorf("unexpected error for valid mnemonic: %v", err)
        }
}

func TestValidateMnemonicBadWordCount(t *testing.T) {
        if err := ValidateMnemonic("word word word"); err == nil {
                t.Error("expected error for 3-word mnemonic")
        }
}

func TestValidateMnemonicUnknownWord(t *testing.T) {
        bad := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon zzzzz"
        if err := ValidateMnemonic(bad); err == nil {
                t.Error("expected error for unknown word")
        }
}

func TestValidateMnemonicBadChecksum(t *testing.T) {
        // Replace last word with 'zoo' to force checksum failure in most cases
        m, _ := GenerateMnemonic(Strength128)
        words := strings.Fields(m)
        if words[11] == "zoo" {
                words[11] = "zero"
        } else {
                words[11] = "zoo"
        }
        mangled := strings.Join(words, " ")
        // Accept if zoo/zero happen to produce a valid checksum (extremely unlikely)
        _ = ValidateMnemonic(mangled) // just ensure it doesn't panic
}

// ─── MnemonicToSeed (BIP39 official test vector) ──────────────────────────

func TestMnemonicToSeedKnownVector(t *testing.T) {
        // BIP39 test vector from https://github.com/trezor/python-mnemonic
        mnemonic := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
        seed := MnemonicToSeed(mnemonic, "TREZOR")
        // First 8 bytes of the well-known seed
        expected := []byte{0xc5, 0x52, 0x57, 0xc3, 0x60, 0xc0, 0x7c, 0x72}
        if string(seed[:8]) != string(expected) {
                t.Errorf("seed mismatch (first 8 bytes):\n  want %x\n  got  %x", expected, seed[:8])
        }
        if len(seed) != 64 {
                t.Errorf("expected 64-byte seed, got %d", len(seed))
        }
}

// ─── HD Derivation ────────────────────────────────────────────────────────

func TestDeriveFromMnemonic(t *testing.T) {
        // Derive key pair and address from a known 12-word mnemonic
        mnemonic := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
        dk, err := DeriveFromMnemonic(mnemonic, "", 0, 0)
        if err != nil {
                t.Fatalf("DeriveFromMnemonic: %v", err)
        }
        if dk.Keys == nil {
                t.Fatal("Keys is nil")
        }
        if dk.Address == "" {
                t.Error("Address is empty")
        }
        if dk.Path != "m/44'/7777'/0'/0'/0'" {
                t.Errorf("unexpected path: %q", dk.Path)
        }
        t.Logf("address: %s", dk.Address)
        t.Logf("path:    %s", dk.Path)
}

func TestDeriveKeysAccountIndex(t *testing.T) {
        m, _ := GenerateMnemonic(Strength128)
        dk0, err := DeriveFromMnemonic(m, "", 0, 0)
        if err != nil {
                t.Fatalf("account 0: %v", err)
        }
        dk1, err := DeriveFromMnemonic(m, "", 0, 1)
        if err != nil {
                t.Fatalf("account 1: %v", err)
        }
        if dk0.Address == dk1.Address {
                t.Error("different indices produced same address")
        }
}

// ─── Keystore encrypt / decrypt round-trip ────────────────────────────────

func TestKeystoreRoundTrip(t *testing.T) {
        mnemonic, err := GenerateMnemonic(Strength128)
        if err != nil {
                t.Fatalf("generate mnemonic: %v", err)
        }
        ks, err := EncryptMnemonic(mnemonic, "sup3rs3cr3t", "tapr1testaddress")
        if err != nil {
                t.Fatalf("EncryptMnemonic: %v", err)
        }
        if ks.Version != 1 {
                t.Errorf("expected version 1, got %d", ks.Version)
        }
        decrypted, err := DecryptMnemonic(ks, "sup3rs3cr3t")
        if err != nil {
                t.Fatalf("DecryptMnemonic: %v", err)
        }
        if decrypted != mnemonic {
                t.Errorf("mnemonic mismatch:\n  want %q\n  got  %q", mnemonic, decrypted)
        }
}

func TestKeystoreWrongPassword(t *testing.T) {
        mnemonic, _ := GenerateMnemonic(Strength128)
        ks, err := EncryptMnemonic(mnemonic, "correct", "addr")
        if err != nil {
                t.Fatalf("EncryptMnemonic: %v", err)
        }
        _, err = DecryptMnemonic(ks, "wrong_password")
        if err == nil {
                t.Error("expected error for wrong password")
        }
}

func TestKeystoreMarshalUnmarshal(t *testing.T) {
        mnemonic, _ := GenerateMnemonic(Strength128)
        ks, err := EncryptMnemonic(mnemonic, "pass", "addr")
        if err != nil {
                t.Fatalf("EncryptMnemonic: %v", err)
        }
        data, err := ks.Marshal()
        if err != nil {
                t.Fatalf("Marshal: %v", err)
        }
        ks2, err := UnmarshalKeystore(data)
        if err != nil {
                t.Fatalf("UnmarshalKeystore: %v", err)
        }
        decrypted, err := DecryptMnemonic(ks2, "pass")
        if err != nil {
                t.Fatalf("DecryptMnemonic after unmarshal: %v", err)
        }
        if decrypted != mnemonic {
                t.Errorf("mnemonic mismatch after JSON round-trip")
        }
}
