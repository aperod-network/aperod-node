package store

import (
	"crypto/sha512"
	"encoding/hex"
	"testing"

	"filippo.io/edwards25519"
	"github.com/aperod/aperod/crypto"
)

func TestSaleOwnerRecoverySignatureBindsCanonicalDelivery(t *testing.T) {
	seed := sha512.Sum512([]byte("owner-recovery-spend"))
	x, err := new(edwards25519.Scalar).SetUniformBytes(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	private := x.Bytes()
	pub := new(edwards25519.Point).ScalarBaseMult(x).Bytes()
	var spend, view crypto.Point32
	copy(spend[:], pub)
	copy(view[:], pub)
	addr := crypto.EncodeAddress(crypto.MainnetByte, spend, view)
	order := &SaleOrder{
		Order: crypto.HashBytes([]byte("order")), Checkpoint: crypto.HashBytes([]byte("checkpoint")),
		Amount: 100000000,
	}
	delivery := &SaleDelivery{Block: crypto.HashBytes([]byte("canonical block")), Transaction: crypto.HashBytes([]byte("canonical tx"))}
	challenge := SaleRecoveryChallenge(crypto.HashBytes([]byte("genesis")), order, delivery)
	nonceHash := sha512.New()
	nonceHash.Write([]byte("APRO_WEB_AUTH_NONCE_V1\x00"))
	nonceHash.Write(private)
	nonceHash.Write([]byte(challenge))
	r, err := new(edwards25519.Scalar).SetUniformBytes(nonceHash.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	R := new(edwards25519.Point).ScalarBaseMult(r).Bytes()
	challengeHash := sha512.New()
	challengeHash.Write([]byte("APRO_WEB_AUTH_CHALLENGE_V1\x00"))
	challengeHash.Write([]byte(challenge))
	challengeHash.Write(pub)
	challengeHash.Write(R)
	c, err := new(edwards25519.Scalar).SetUniformBytes(challengeHash.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	signature := hex.EncodeToString(append(append([]byte{}, R...), new(edwards25519.Scalar).MultiplyAdd(c, x, r).Bytes()...))
	if !validSaleRecoveryProof(addr, challenge, signature) {
		t.Fatal("owner-signed canonical recovery was rejected")
	}
	for _, other := range []string{
		challenge + ":changed", "APRO_SALE_RECOVERY_V1\x00unrelated-order",
		SaleRecoveryChallenge(crypto.HashBytes([]byte("genesis")), order, &SaleDelivery{Block: crypto.HashBytes([]byte("reorg")), Transaction: delivery.Transaction}),
	} {
		if validSaleRecoveryProof(addr, other, signature) {
			t.Fatal("a signature was reused for a different sale or chain")
		}
	}
	if validSaleRecoveryProof(addr, challenge, signature[:len(signature)-2]+"ff") {
		t.Fatal("a modified owner signature passed")
	}
}
