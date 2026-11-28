package crypto

import (
	"crypto/ed25519"
	"testing"
)

func TestValidatorVerifyRejectsMalformedAuthorityWithoutPanic(t *testing.T) {
	private, public, err := GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	hash := Hash32{1, 2, 3}
	signature, err := private.Sign(hash)
	if err != nil {
		t.Fatal(err)
	}
	if !public.Verify(hash, signature) {
		t.Fatal("valid signature rejected")
	}
	for _, length := range []int{0, 1, ed25519.PublicKeySize - 1, ed25519.PublicKeySize + 1, ed25519.PrivateKeySize} {
		if ValidatorPubKey(make([]byte, length)).Verify(hash, signature) {
			t.Fatalf("accepted malformed authority of length %d", length)
		}
	}
	if (ValidatorPubKey(nil)).Verify(hash, signature) {
		t.Fatal("accepted missing authority")
	}
	for _, length := range []int{0, ed25519.SignatureSize - 1, ed25519.SignatureSize + 1} {
		if public.Verify(hash, make([]byte, length)) {
			t.Fatalf("accepted malformed signature of length %d", length)
		}
	}
	signature[0] ^= 1
	if public.Verify(hash, signature) {
		t.Fatal("accepted forged signature with valid authority")
	}
}
