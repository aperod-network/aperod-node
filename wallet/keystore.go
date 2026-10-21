package wallet

// Keystore — AES-256-GCM encrypted wallet storage with scrypt key derivation.
// Compatible with the Web3/Ethereum keystore v3 spirit but APRO-specific format.
// Task 3.1.3.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// KDF parameters for PBKDF2-SHA256.
// 200000 iterations matches 2024 OWASP recommendations for PBKDF2-SHA256.
const (
	pbkdf2Iter = 200000
	keyLen     = 32 // AES-256
)

// KeystoreV1 is the JSON-serialisable encrypted wallet record. Version 1 is
// the legacy format; version 2 authenticates the HD derivation metadata.
type KeystoreV1 struct {
	Version      int            `json:"version"`
	Address      string         `json:"address"`
	AccountIndex *uint32        `json:"account_index,omitempty"`
	AddressIndex *uint32        `json:"address_index,omitempty"`
	CreatedAt    string         `json:"created_at"`
	Crypto       keystoreCrypto `json:"crypto"`
}

type keystoreCrypto struct {
	Cipher     string       `json:"cipher"`     // "aes-256-gcm"
	CipherText string       `json:"ciphertext"` // hex
	Nonce      string       `json:"nonce"`      // hex (12 bytes)
	KDF        string       `json:"kdf"`        // "pbkdf2-sha256"
	KDFParams  pbkdf2Params `json:"kdfparams"`
	MAC        string       `json:"mac"` // hex authentication MAC
}

type pbkdf2Params struct {
	Iter int    `json:"iter"`
	Salt string `json:"salt"` // hex (32 bytes)
}

// EncryptMnemonic encrypts the mnemonic phrase with password and returns a
// KeystoreV1 struct that can be marshaled to JSON for persistent storage.
func EncryptMnemonic(mnemonic, password, address string) (*KeystoreV1, error) {
	return encryptMnemonic(mnemonic, password, address, nil, nil)
}

// EncryptMnemonicWithDerivation creates a version 2 keystore that authenticates
// the exact HD indices and address. EncryptMnemonic remains available for
// reading/writing legacy version 1 keystores without derivation metadata.
func EncryptMnemonicWithDerivation(mnemonic, password, address string, account, index uint32) (*KeystoreV1, error) {
	if account >= hardenedOffset || index >= hardenedOffset {
		return nil, fmt.Errorf("keystore: account and index must be less than %d", hardenedOffset)
	}
	return encryptMnemonic(mnemonic, password, address, &account, &index)
}

func encryptMnemonic(mnemonic, password, address string, account, index *uint32) (*KeystoreV1, error) {
	if mnemonic == "" {
		return nil, errors.New("keystore: mnemonic is empty")
	}
	if password == "" {
		return nil, errors.New("keystore: password is required")
	}

	// Generate random salt and nonce
	salt := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("keystore: salt: %w", err)
	}
	nonce := make([]byte, 12) // GCM standard nonce size
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("keystore: nonce: %w", err)
	}

	// Derive AES key from password using PBKDF2-SHA256
	aesKey := pbkdf2SHA256([]byte(password), salt, pbkdf2Iter, keyLen)

	// Encrypt with AES-256-GCM
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("keystore: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keystore: gcm: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(mnemonic), nil)

	// Preserve the legacy MAC for keystores without derivation metadata.
	// New metadata-bearing keystores authenticate the address and indices
	// so an edited path cannot silently relabel the wallet.
	mac := computeMAC(aesKey, ciphertext)
	if account != nil && index != nil {
		mac = computeMACWithDerivation(aesKey, ciphertext, address, *account, *index)
	}

	version := 1
	if account != nil && index != nil {
		version = 2
	}
	return &KeystoreV1{
		Version:      version,
		Address:      address,
		AccountIndex: account,
		AddressIndex: index,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		Crypto: keystoreCrypto{
			Cipher:     "aes-256-gcm",
			CipherText: hex.EncodeToString(ciphertext),
			Nonce:      hex.EncodeToString(nonce),
			KDF:        "pbkdf2-sha256",
			KDFParams: pbkdf2Params{
				Iter: pbkdf2Iter,
				Salt: hex.EncodeToString(salt),
			},
			MAC: hex.EncodeToString(mac),
		},
	}, nil
}

// VerifyDerivation ensures a requested derivation matches the authenticated
// metadata in version 2 keystores and the stored address in legacy records.
func (ks *KeystoreV1) VerifyDerivation(account, index uint32, address string) error {
	if ks == nil {
		return errors.New("keystore: missing keystore")
	}
	if account >= hardenedOffset || index >= hardenedOffset {
		return fmt.Errorf("keystore: account and index must be less than %d", hardenedOffset)
	}
	switch ks.Version {
	case 1:
		if ks.AccountIndex != nil || ks.AddressIndex != nil {
			return errors.New("keystore: version 1 cannot contain derivation metadata")
		}
	case 2:
		if ks.AccountIndex == nil || ks.AddressIndex == nil {
			return errors.New("keystore: version 2 requires derivation metadata")
		}
	default:
		return fmt.Errorf("keystore: unsupported version %d", ks.Version)
	}
	if ks.AccountIndex != nil && *ks.AccountIndex != account {
		return errors.New("keystore: account index does not match")
	}
	if ks.AddressIndex != nil && *ks.AddressIndex != index {
		return errors.New("keystore: address index does not match")
	}
	if ks.Address == "" || address == "" || ks.Address != address {
		return errors.New("keystore: derived address does not match stored address")
	}
	return nil
}

// DecryptMnemonic decrypts a KeystoreV1 record using the provided password.
// Returns the plaintext mnemonic or an error if the password is wrong.
func DecryptMnemonic(ks *KeystoreV1, password string) (string, error) {
	if ks == nil {
		return "", errors.New("keystore: missing keystore")
	}
	if ks.Version != 1 && ks.Version != 2 {
		return "", fmt.Errorf("keystore: unsupported version %d", ks.Version)
	}
	if ks.Crypto.Cipher != "aes-256-gcm" {
		return "", fmt.Errorf("keystore: unsupported cipher %q", ks.Crypto.Cipher)
	}

	salt, err := hex.DecodeString(ks.Crypto.KDFParams.Salt)
	if err != nil {
		return "", fmt.Errorf("keystore: decode salt: %w", err)
	}
	nonce, err := hex.DecodeString(ks.Crypto.Nonce)
	if err != nil {
		return "", fmt.Errorf("keystore: decode nonce: %w", err)
	}
	ciphertext, err := hex.DecodeString(ks.Crypto.CipherText)
	if err != nil {
		return "", fmt.Errorf("keystore: decode ciphertext: %w", err)
	}
	expectedMAC, err := hex.DecodeString(ks.Crypto.MAC)
	if err != nil {
		return "", fmt.Errorf("keystore: decode mac: %w", err)
	}

	// Derive key using PBKDF2-SHA256
	iter := ks.Crypto.KDFParams.Iter
	if iter <= 0 {
		iter = pbkdf2Iter // default for old keystores
	}
	aesKey := pbkdf2SHA256([]byte(password), salt, iter, keyLen)

	// Verify the legacy or derivation-bound MAC before decryption.
	var gotMAC []byte
	switch ks.Version {
	case 1:
		if ks.AccountIndex != nil || ks.AddressIndex != nil {
			return "", errors.New("keystore: version 1 cannot contain derivation metadata")
		}
		gotMAC = computeMAC(aesKey, ciphertext)
	case 2:
		if ks.AccountIndex == nil || ks.AddressIndex == nil {
			return "", errors.New("keystore: version 2 requires derivation metadata")
		}
		gotMAC = computeMACWithDerivation(aesKey, ciphertext, ks.Address, *ks.AccountIndex, *ks.AddressIndex)
	}
	if !macEqual(gotMAC, expectedMAC) {
		return "", errors.New("keystore: wrong password or corrupted keystore")
	}

	// Decrypt
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", fmt.Errorf("keystore: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("keystore: gcm: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("keystore: decryption failed (wrong password?)")
	}
	return string(plain), nil
}

// Marshal serializes the keystore to JSON.
func (ks *KeystoreV1) Marshal() ([]byte, error) {
	return json.MarshalIndent(ks, "", "  ")
}

// UnmarshalKeystore parses a JSON-encoded KeystoreV1 record.
func UnmarshalKeystore(data []byte) (*KeystoreV1, error) {
	var ks KeystoreV1
	if err := json.Unmarshal(data, &ks); err != nil {
		return nil, fmt.Errorf("keystore: unmarshal: %w", err)
	}
	return &ks, nil
}

// ─── internal helpers ─────────────────────────────────────────────────────────

func computeMAC(key, ciphertext []byte) []byte {
	h := sha256.New()
	h.Write(key)
	h.Write(ciphertext)
	return h.Sum(nil)
}

func computeMACWithDerivation(key, ciphertext []byte, address string, account, index uint32) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("aperod/keystore-v1/hd-derivation\x00"))
	h.Write(ciphertext)
	var addressLength [4]byte
	binary.BigEndian.PutUint32(addressLength[:], uint32(len(address)))
	h.Write(addressLength[:])
	h.Write([]byte(address))
	var indices [8]byte
	binary.BigEndian.PutUint32(indices[:4], account)
	binary.BigEndian.PutUint32(indices[4:], index)
	h.Write(indices[:])
	return h.Sum(nil)
}

func macEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
