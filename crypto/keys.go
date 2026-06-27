package crypto

import (
        "crypto/ed25519"
        "crypto/rand"
        "encoding/hex"
        "fmt"
)

// ValidatorPrivKey is an ED25519 private key used by PoA validators to sign blocks.
type ValidatorPrivKey ed25519.PrivateKey

// ValidatorPubKey is an ED25519 public key used to verify validator block signatures.
type ValidatorPubKey ed25519.PublicKey

// GenerateValidatorKey creates a new random ED25519 key pair for a validator.
func GenerateValidatorKey() (ValidatorPrivKey, ValidatorPubKey, error) {
        pub, priv, err := ed25519.GenerateKey(rand.Reader)
        if err != nil {
                return nil, nil, fmt.Errorf("generate validator key: %w", err)
        }
        return ValidatorPrivKey(priv), ValidatorPubKey(pub), nil
}

// ValidatorPrivKeyFromBytes restores a private key from raw bytes.
// Accepts either a 32-byte seed (expanded via ed25519.NewKeyFromSeed)
// or a 64-byte private key (seed || public).
func ValidatorPrivKeyFromBytes(b []byte) (ValidatorPrivKey, error) {
        switch len(b) {
        case ed25519.SeedSize: // 32 bytes — expand seed to full private key
                return ValidatorPrivKey(ed25519.NewKeyFromSeed(b)), nil
        case ed25519.PrivateKeySize: // 64 bytes — use directly
                return ValidatorPrivKey(b), nil
        default:
                return nil, fmt.Errorf("invalid validator private key length: %d (want 32 or 64)", len(b))
        }
}

// ValidatorPubKeyFromBytes restores a public key from 32 raw bytes.
func ValidatorPubKeyFromBytes(b []byte) (ValidatorPubKey, error) {
        if len(b) != ed25519.PublicKeySize {
                return nil, fmt.Errorf("invalid validator public key length: %d", len(b))
        }
        return ValidatorPubKey(b), nil
}

// Public returns the corresponding public key.
func (k ValidatorPrivKey) Public() ValidatorPubKey {
        return ValidatorPubKey(ed25519.PrivateKey(k).Public().(ed25519.PublicKey))
}

// Sign signs a 32-byte hash with the validator's private key.
func (k ValidatorPrivKey) Sign(hash Hash32) ([]byte, error) {
        sig := ed25519.Sign(ed25519.PrivateKey(k), hash[:])
        return sig, nil
}

// Bytes returns the raw private key bytes (seed + public).
func (k ValidatorPrivKey) Bytes() []byte { return []byte(k) }

// Bytes returns the raw public key bytes.
func (k ValidatorPubKey) Bytes() []byte { return []byte(k) }

// Hex returns the public key as a lowercase hex string.
func (k ValidatorPubKey) Hex() string { return hex.EncodeToString(k) }

// ID returns a short 8-char hex prefix for logging.
func (k ValidatorPubKey) ID() string {
        h := hex.EncodeToString(k)
        if len(h) < 8 {
                return h
        }
        return h[:8]
}

// Verify checks an ED25519 signature against a hash.
func (k ValidatorPubKey) Verify(hash Hash32, sig []byte) bool {
        if len(sig) != ed25519.SignatureSize {
                return false
        }
        return ed25519.Verify(ed25519.PublicKey(k), hash[:], sig)
}

// Equals returns true if two public keys are identical.
func (k ValidatorPubKey) Equals(other ValidatorPubKey) bool {
        return string(k) == string(other)
}
