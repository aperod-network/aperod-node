package wallet

// Pure-Go PBKDF2 implementation (RFC 2898 / PKCS #5 v2.0).
// Replaces golang.org/x/crypto/pbkdf2 and golang.org/x/crypto/scrypt.
//
// For BIP39 seed derivation (mnemonic.go) this uses HMAC-SHA512 with 2048
// iterations, which exactly matches the BIP39 specification.
//
// For wallet keystore encryption (keystore.go) this uses HMAC-SHA256 with
// 200000 iterations (OWASP recommended minimum for PBKDF2-SHA256 in 2024).

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"hash"
)

// pbkdf2SHA512 derives a key using PBKDF2-HMAC-SHA512.
// Used by MnemonicToSeed for BIP39 compatibility (2048 iterations, 64 bytes).
func pbkdf2SHA512(password, salt []byte, iter, keyLen int) []byte {
	return pbkdf2Key(password, salt, iter, keyLen, sha512.New)
}

// pbkdf2SHA256 derives a key using PBKDF2-HMAC-SHA256.
// Used by keystore encryption as a scrypt replacement.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	return pbkdf2Key(password, salt, iter, keyLen, sha256.New)
}

// pbkdf2Key implements PBKDF2 with an arbitrary HMAC hash function.
// Output is deterministic: same inputs always produce the same key.
func pbkdf2Key(password, salt []byte, iter, keyLen int, newHash func() hash.Hash) []byte {
	prf := hmac.New(newHash, password)
	hLen := prf.Size()
	numBlocks := (keyLen + hLen - 1) / hLen

	dk := make([]byte, 0, numBlocks*hLen)
	U := make([]byte, hLen)
	var blockNum [4]byte
	for block := 1; block <= numBlocks; block++ {
		binary.BigEndian.PutUint32(blockNum[:], uint32(block))
		prf.Reset()
		prf.Write(salt)
		prf.Write(blockNum[:])
		U = prf.Sum(U[:0])

		T := make([]byte, hLen)
		copy(T, U)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(U)
			U = prf.Sum(U[:0])
			for j := range T {
				T[j] ^= U[j]
			}
		}
		dk = append(dk, T...)
	}
	return dk[:keyLen]
}
