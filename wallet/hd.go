package wallet

// HD wallet — BIP44 hierarchical deterministic key derivation.
// Path: m/44'/7777'/account'/0'/index'
// 7777 = Aperod coin type (APRO)
// Derivation follows SLIP-0010 (Ed25519): all levels are hardened.

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/aperod/aperod/crypto"
)

const (
	// AperodCoinType is the BIP44 coin type for APRO.
	AperodCoinType uint32 = 7777

	// hardenedOffset is added to hardened child indices.
	hardenedOffset uint32 = 0x80000000
)

// extKey holds a 32-byte private key and 32-byte chain code (SLIP-0010).
type extKey struct {
	key       [32]byte
	chainCode [32]byte
}

// masterFromSeed derives the root extended key from a 64-byte BIP39 seed.
// SLIP-0010: HMAC-SHA512("ed25519 seed", seed)
func masterFromSeed(seed []byte) (*extKey, error) {
	if len(seed) != 64 {
		return nil, errors.New("hd: seed must be 64 bytes")
	}
	mac := hmac.New(sha512.New, []byte("ed25519 seed"))
	mac.Write(seed)
	I := mac.Sum(nil)
	var ek extKey
	copy(ek.key[:], I[:32])
	copy(ek.chainCode[:], I[32:])
	return &ek, nil
}

// child derives a hardened child key. For Ed25519 (SLIP-0010) all children
// must be hardened; if index < hardenedOffset it is promoted automatically.
func (ek *extKey) child(index uint32) (*extKey, error) {
	if index < hardenedOffset {
		index |= hardenedOffset
	}
	// Data = 0x00 || key || index (big-endian)
	data := make([]byte, 37)
	data[0] = 0x00
	copy(data[1:33], ek.key[:])
	binary.BigEndian.PutUint32(data[33:], index)

	mac := hmac.New(sha512.New, ek.chainCode[:])
	mac.Write(data)
	I := mac.Sum(nil)

	var child extKey
	copy(child.key[:], I[:32])
	copy(child.chainCode[:], I[32:])
	return &child, nil
}

// DerivedKeys holds the Aperod spend + view key pair at a specific BIP44 path.
type DerivedKeys struct {
	Path         string       // e.g. "m/44'/7777'/0'/0'/0'"
	Keys         *crypto.WalletKeyPair
	Address      crypto.Address
	AccountIndex uint32
	AddressIndex uint32
}

// DeriveKeys derives Aperod spend+view keys from a 64-byte BIP39 seed.
// BIP44 path: m/44'/7777'/account'/0'/index'
func DeriveKeys(seed []byte, account, index uint32) (*DerivedKeys, error) {
	master, err := masterFromSeed(seed)
	if err != nil {
		return nil, err
	}

	// Traverse: purpose' / coin_type' / account' / change' / index'
	levels := []uint32{
		44 | hardenedOffset,
		AperodCoinType | hardenedOffset,
		account | hardenedOffset,
		0 | hardenedOffset,
		index | hardenedOffset,
	}
	cur := master
	for _, idx := range levels {
		cur, err = cur.child(idx)
		if err != nil {
			return nil, fmt.Errorf("hd derive: %w", err)
		}
	}

	// Use first 32 bytes as the spend seed; view seed is HMAC("view", key)
	spendSeed := cur.key[:]
	kp, err := crypto.WalletKeysFromSeed(spendSeed)
	if err != nil {
		return nil, fmt.Errorf("hd keys: %w", err)
	}

	addr := crypto.AddressFromKeys(crypto.TestnetByte, kp)
	path := fmt.Sprintf("m/44'/%d'/%d'/0'/%d'", AperodCoinType, account, index)
	return &DerivedKeys{
		Path:         path,
		Keys:         kp,
		Address:      addr,
		AccountIndex: account,
		AddressIndex: index,
	}, nil
}

// DeriveFromMnemonic is a convenience wrapper: mnemonic → seed → DeriveKeys.
func DeriveFromMnemonic(mnemonic, passphrase string, account, index uint32) (*DerivedKeys, error) {
	if _, err := ValidateMnemonicForRecovery(mnemonic); err != nil {
		return nil, fmt.Errorf("hd: invalid mnemonic: %w", err)
	}
	seed := MnemonicToSeed(mnemonic, passphrase)
	return DeriveKeys(seed, account, index)
}
