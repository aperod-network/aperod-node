package crypto

import "testing"

func TestLockedValidatorKeyPublicAfterDestroy(t *testing.T) {
	priv, pub, err := GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := key.Public(); !got.Equals(pub) {
		t.Fatal("public key changed before destroy")
	}
	key.Destroy()
	if got := key.Public(); got != nil {
		t.Fatal("destroyed key must not expose a public key")
	}
}
