// Package wallet implements HD wallet functionality for Aperod (Phase 3).
// BIP39 mnemonic generation and recovery.
package wallet

import (
        "crypto/rand"
        "crypto/sha256"
        "errors"
        "fmt"
        "strings"
)

// Strength in bits for mnemonic entropy. 128 = 12 words, 256 = 24 words.
const (
        Strength128 = 128 // 12-word mnemonic
        Strength256 = 256 // 24-word mnemonic
)

// GenerateMnemonic creates a BIP39-style mnemonic phrase with the given entropy
// strength in bits. strength must be 128 or 256.
//
// Uses rejection sampling: if any derived word index exceeds the wordlist size
// (possible when the list has fewer than 2048 entries), a fresh entropy block
// is generated and re-tried. In practice this converges in ≤5 attempts.
func GenerateMnemonic(strength int) (string, error) {
        if strength != Strength128 && strength != Strength256 {
                return "", errors.New("mnemonic: strength must be 128 or 256 bits")
        }
        entropy := make([]byte, strength/8)
        const maxAttempts = 64
        for i := 0; i < maxAttempts; i++ {
                if _, err := rand.Read(entropy); err != nil {
                        return "", fmt.Errorf("mnemonic: entropy: %w", err)
                }
                m, err := EntropyToMnemonic(entropy)
                if err == nil {
                        return m, nil
                }
                if err.Error() == errIndexOutOfRange.Error() {
                        continue // retry with new entropy
                }
                return "", err
        }
        return "", fmt.Errorf("mnemonic: could not generate valid mnemonic in %d attempts", maxAttempts)
}

var errIndexOutOfRange = errors.New("mnemonic: word index out of range for current wordlist")

// EntropyToMnemonic converts raw entropy bytes to a BIP39 mnemonic.
// len(entropy) must be 16 (128-bit) or 32 (256-bit).
func EntropyToMnemonic(entropy []byte) (string, error) {
        if len(entropy) != 16 && len(entropy) != 32 {
                return "", errors.New("mnemonic: entropy must be 16 or 32 bytes")
        }
        // Compute checksum: first ENT/32 bits of SHA256(entropy)
        h := sha256.Sum256(entropy)
        checksumBits := len(entropy) * 8 / 32 // 4 or 8 bits
        // Append checksum bits to entropy bits
        bits := bytesToBits(entropy)
        checksumAll := bytesToBits(h[:])
        bits = append(bits, checksumAll[:checksumBits]...)
        // Split into 11-bit groups → word indices
        wordCount := len(bits) / 11
        words := make([]string, wordCount)
        for i := 0; i < wordCount; i++ {
                idx := bitsToUint(bits[i*11 : i*11+11])
                if int(idx) >= len(bip39WordList) {
                        return "", errIndexOutOfRange
                }
                words[i] = bip39WordList[idx]
        }
        return strings.Join(words, " "), nil
}

// MnemonicToEntropy converts a BIP39 mnemonic phrase back to raw entropy bytes.
// Returns an error if the mnemonic is invalid or checksum fails.
func MnemonicToEntropy(mnemonic string) ([]byte, error) {
        words := strings.Fields(mnemonic)
        if len(words) != 12 && len(words) != 24 {
                return nil, errors.New("mnemonic: must be 12 or 24 words")
        }
        // Convert each word to its 11-bit index
        bits := make([]byte, 0, len(words)*11)
        for _, w := range words {
                idx, ok := bip39Index[w]
                if !ok {
                        return nil, fmt.Errorf("mnemonic: unknown word %q", w)
                }
                bits = append(bits, uintToBits(idx, 11)...)
        }
        // Separate entropy bits from checksum bits
        totalBits := len(bits)
        checksumBits := totalBits / 33
        entropyBits := totalBits - checksumBits
        entropy := bitsToBytes(bits[:entropyBits])
        // Verify checksum
        h := sha256.Sum256(entropy)
        expected := bytesToBits(h[:])[:checksumBits]
        actual := bits[entropyBits:]
        for i, b := range expected {
                if b != actual[i] {
                        return nil, errors.New("mnemonic: checksum mismatch")
                }
        }
        return entropy, nil
}

// MnemonicToSeed converts a BIP39 mnemonic + optional passphrase to a 64-byte
// BIP39 seed using PBKDF2-HMAC-SHA512 with 2048 iterations (BIP39 standard).
func MnemonicToSeed(mnemonic, passphrase string) []byte {
        mnemonic = strings.TrimSpace(mnemonic)
        salt := "mnemonic" + passphrase
        return pbkdf2SHA512([]byte(mnemonic), []byte(salt), 2048, 64)
}

// ValidateMnemonic returns nil if the mnemonic is a valid BIP39 phrase
// (correct word count, all words in wordlist, correct checksum).
func ValidateMnemonic(mnemonic string) error {
        _, err := MnemonicToEntropy(mnemonic)
        return err
}

// ─── bit helpers ──────────────────────────────────────────────────────────────

func bytesToBits(b []byte) []byte {
        bits := make([]byte, len(b)*8)
        for i, by := range b {
                for j := 0; j < 8; j++ {
                        if by&(1<<uint(7-j)) != 0 {
                                bits[i*8+j] = 1
                        }
                }
        }
        return bits
}

func bitsToBytes(bits []byte) []byte {
        n := len(bits) / 8
        out := make([]byte, n)
        for i := 0; i < n; i++ {
                for j := 0; j < 8; j++ {
                        if bits[i*8+j] == 1 {
                                out[i] |= 1 << uint(7-j)
                        }
                }
        }
        return out
}

func bitsToUint(bits []byte) uint32 {
        var v uint32
        for _, b := range bits {
                v = v<<1 | uint32(b)
        }
        return v
}

func uintToBits(v uint32, n int) []byte {
        bits := make([]byte, n)
        for i := n - 1; i >= 0; i-- {
                bits[i] = byte(v & 1)
                v >>= 1
        }
        return bits
}

// bip39Index is built at init from bip39WordList for O(1) word lookup.
var bip39Index map[string]uint32

func init() {
        bip39Index = make(map[string]uint32, len(bip39WordList))
        for i, w := range bip39WordList {
                bip39Index[w] = uint32(i)
        }
}
