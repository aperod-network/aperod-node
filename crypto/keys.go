package crypto

import (
        "crypto/ed25519"
        "crypto/rand"
        "encoding/hex"
        "fmt"
        "sync"
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

// ZeroBytes overwrites b with zeros to remove sensitive key material from memory.
// Call this immediately after extracting a structured key type from raw bytes,
// so that heap dumps and core files cannot expose the unprocessed secret.
func ZeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// LockedValidatorKey holds an ED25519 private key in a memory-locked buffer.
// The underlying bytes are pinned in physical RAM via mlock(2) so they cannot
// be swapped to disk or appear in a core dump.  Call Destroy when done to zero
// and unlock the buffer.
type LockedValidatorKey struct {
        mu  sync.Mutex
        buf []byte // 64-byte ed25519 private key, mlocked
}

// NewLockedValidatorKey copies keyBytes into a fresh mlocked buffer and returns
// a LockedValidatorKey.  keyBytes may be a 32-byte seed or a 64-byte private key.
// The caller should zero keyBytes with ZeroBytes after this call.
// MlockBytes failure is non-fatal: the key is still stored securely in process
// memory, just without the mlock OS guarantee.
func NewLockedValidatorKey(keyBytes []byte, warnOnMlockFail func(error)) (*LockedValidatorKey, error) {
        var priv ed25519.PrivateKey
        switch len(keyBytes) {
        case ed25519.SeedSize:
                priv = ed25519.NewKeyFromSeed(keyBytes)
        case ed25519.PrivateKeySize:
                priv = make(ed25519.PrivateKey, ed25519.PrivateKeySize)
                copy(priv, keyBytes)
        default:
                return nil, fmt.Errorf("invalid validator private key length: %d (want 32 or 64)", len(keyBytes))
        }

        buf := []byte(priv)
        if err := MlockBytes(buf); err != nil && warnOnMlockFail != nil {
                warnOnMlockFail(err)
        }
        return &LockedValidatorKey{buf: buf}, nil
}

// Public returns the corresponding validator public key.
func (lk *LockedValidatorKey) Public() ValidatorPubKey {
        lk.mu.Lock()
        defer lk.mu.Unlock()
        return ValidatorPubKey(ed25519.PrivateKey(lk.buf).Public().(ed25519.PublicKey))
}

// Sign signs a 32-byte hash directly from the locked buffer.
func (lk *LockedValidatorKey) Sign(hash Hash32) ([]byte, error) {
        lk.mu.Lock()
        defer lk.mu.Unlock()
        if lk.buf == nil {
                return nil, fmt.Errorf("locked validator key has been destroyed")
        }
        sig := ed25519.Sign(ed25519.PrivateKey(lk.buf), hash[:])
        return sig, nil
}

// PrivKey returns a ValidatorPrivKey whose backing array is the locked buffer.
// The returned value is valid only while the LockedValidatorKey has not been
// Destroy()ed.  Use it only for immediate operations (sign, write to disk).
func (lk *LockedValidatorKey) PrivKey() ValidatorPrivKey {
        lk.mu.Lock()
        defer lk.mu.Unlock()
        return ValidatorPrivKey(lk.buf)
}

// Destroy zeroes the locked buffer and releases the mlock.
// After Destroy, all methods return zero values or errors.
func (lk *LockedValidatorKey) Destroy() {
        lk.mu.Lock()
        defer lk.mu.Unlock()
        if lk.buf != nil {
                ZeroBytes(lk.buf)
                _ = MunlockBytes(lk.buf)
                lk.buf = nil
        }
}
