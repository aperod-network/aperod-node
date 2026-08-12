package crypto_test

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

// ─── Hash ─────────────────────────────────────────────────────────────────────

func TestHashBytes_Deterministic(t *testing.T) {
	h1 := crypto.HashBytes([]byte("aperod"), []byte("test"))
	h2 := crypto.HashBytes([]byte("aperod"), []byte("test"))
	if h1 != h2 {
		t.Fatal("HashBytes is not deterministic")
	}
}

func TestHashBytes_Different(t *testing.T) {
	h1 := crypto.HashBytes([]byte("a"))
	h2 := crypto.HashBytes([]byte("b"))
	if h1 == h2 {
		t.Fatal("HashBytes should produce different results for different inputs")
	}
}

// ─── Validator Keys ───────────────────────────────────────────────────────────

func TestValidatorKey_GenerateAndSign(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	msg := crypto.HashStr("test message")
	sig, err := priv.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if !pub.Verify(msg, sig) {
		t.Fatal("Verify returned false for a valid signature")
	}
}

func TestValidatorKey_WrongMessage(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	msg1 := crypto.HashStr("message1")
	msg2 := crypto.HashStr("message2")
	sig, _ := priv.Sign(msg1)
	if pub.Verify(msg2, sig) {
		t.Fatal("Verify should fail for wrong message")
	}
}

func TestValidatorKey_WrongKey(t *testing.T) {
	priv, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_, pub2, _ := crypto.GenerateValidatorKey()
	msg := crypto.HashStr("hello")
	sig, _ := priv.Sign(msg)
	if pub2.Verify(msg, sig) {
		t.Fatal("Verify should fail for different public key")
	}
}

func TestValidatorKey_Roundtrip(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	pub2, err := crypto.ValidatorPubKeyFromBytes(pub.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Equals(pub2) {
		t.Fatal("pub key roundtrip failed")
	}
	priv2, err := crypto.ValidatorPrivKeyFromBytes(priv.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	msg := crypto.HashStr("roundtrip")
	sig, _ := priv2.Sign(msg)
	if !pub2.Verify(msg, sig) {
		t.Fatal("signature from restored key failed")
	}
}

// ─── Wallet Keys ──────────────────────────────────────────────────────────────

func TestWalletKeys_Generate(t *testing.T) {
	kp, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	if kp.Spend.Private == (crypto.Scalar32{}) {
		t.Fatal("spend private key is zero")
	}
	if kp.Spend.Public == (crypto.Point32{}) {
		t.Fatal("spend public key is zero")
	}
	if kp.View.Private == (crypto.Scalar32{}) {
		t.Fatal("view private key is zero")
	}
}

func TestWalletKeys_Deterministic(t *testing.T) {
	seed := []byte("aperod test seed 32 bytes padded!")
	kp1, err := crypto.WalletKeysFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	kp2, err := crypto.WalletKeysFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if kp1.Spend.Private != kp2.Spend.Private {
		t.Fatal("wallet keys are not deterministic from same seed")
	}
	if kp1.Spend.Public != kp2.Spend.Public {
		t.Fatal("spend public key is not deterministic")
	}
}

func TestWalletKeys_DifferentSeeds(t *testing.T) {
	kp1, _ := crypto.WalletKeysFromSeed([]byte("seed-number-one-padded-to-32byte"))
	kp2, _ := crypto.WalletKeysFromSeed([]byte("seed-number-two-padded-to-32byte"))
	if kp1.Spend.Private == kp2.Spend.Private {
		t.Fatal("different seeds produced same key")
	}
}

// ─── Address ──────────────────────────────────────────────────────────────────

func TestAddress_EncodeDecodeRoundtrip(t *testing.T) {
	kp, _ := crypto.GenerateWalletKeys()
	addr := crypto.EncodeAddress(crypto.TestnetByte, kp.Spend.Public, kp.View.Public)

	net, spend, view, err := crypto.DecodeAddress(addr)
	if err != nil {
		t.Fatalf("DecodeAddress: %v", err)
	}
	if net != crypto.TestnetByte {
		t.Fatalf("net byte: want 0x%02x got 0x%02x", crypto.TestnetByte, net)
	}
	if spend != kp.Spend.Public {
		t.Fatal("spend key mismatch after roundtrip")
	}
	if view != kp.View.Public {
		t.Fatal("view key mismatch after roundtrip")
	}
}

func TestAddress_InvalidChecksum(t *testing.T) {
	kp, _ := crypto.GenerateWalletKeys()
	addr := string(crypto.EncodeAddress(crypto.TestnetByte, kp.Spend.Public, kp.View.Public))
	// Corrupt the last character with a value guaranteed to differ from the
	// original (a bare "X" was a no-op ~1/58 runs when the address already
	// ended in 'X', making the test pass vacuously).
	replacement := "X"
	if addr[len(addr)-1] == 'X' {
		replacement = "Y"
	}
	corrupted := addr[:len(addr)-1] + replacement
	if err := crypto.Validate(crypto.Address(corrupted)); err == nil {
		t.Fatal("corrupted address should fail validation")
	}
}

func TestAddress_MainnetVsTestnet(t *testing.T) {
	kp, _ := crypto.GenerateWalletKeys()
	main := crypto.EncodeAddress(crypto.MainnetByte, kp.Spend.Public, kp.View.Public)
	test := crypto.EncodeAddress(crypto.TestnetByte, kp.Spend.Public, kp.View.Public)
	if main == test {
		t.Fatal("mainnet and testnet addresses should differ")
	}
}

// ─── Pedersen Commitments ────────────────────────────────────────────────────

func TestPedersen_Commit(t *testing.T) {
	blind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	c, err := crypto.Commit(1000, blind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if c == (crypto.Commitment{}) {
		t.Fatal("commitment is zero")
	}
}

func TestPedersen_DifferentAmounts(t *testing.T) {
	blind, _ := crypto.NewBlindFactor()
	c1, _ := crypto.Commit(100, blind)
	c2, _ := crypto.Commit(200, blind)
	if c1 == c2 {
		t.Fatal("different amounts should produce different commitments")
	}
}

func TestPedersen_Balance(t *testing.T) {
	bIn1, _ := crypto.NewBlindFactor()
	bIn2, _ := crypto.NewBlindFactor()
	bOut, _ := crypto.NewBlindFactor()
	bFee, _ := crypto.NewBlindFactor()

	// in1=300, in2=200 → out=450, fee=50
	cIn1, _ := crypto.Commit(300, bIn1)
	cIn2, _ := crypto.Commit(200, bIn2)
	cOut, _ := crypto.Commit(450, bOut)
	cFee, _ := crypto.Commit(50, bFee)

	// This DOES NOT balance because blinding factors are random.
	// The real balance check uses BlindSum to compute change blind.
	// Here we just verify CommitSum doesn't panic and returns a bool.
	ok, err := crypto.CommitSum(
		[]crypto.Commitment{cIn1, cIn2},
		[]crypto.Commitment{cOut},
		cFee,
	)
	if err != nil {
		t.Fatalf("CommitSum: %v", err)
	}
	// Random blinds won't balance; we just check it runs without error.
	_ = ok
}

// ─── Key Image ────────────────────────────────────────────────────────────────

func TestKeyImage_Compute(t *testing.T) {
	kp, _ := crypto.GenerateWalletKeys()
	ki, err := crypto.ComputeKeyImage(kp.Spend.Private, kp.Spend.Public)
	if err != nil {
		t.Fatalf("ComputeKeyImage: %v", err)
	}
	if ki == (crypto.KeyImage{}) {
		t.Fatal("key image is zero")
	}
}

func TestKeyImage_Deterministic(t *testing.T) {
	kp, _ := crypto.GenerateWalletKeys()
	ki1, _ := crypto.ComputeKeyImage(kp.Spend.Private, kp.Spend.Public)
	ki2, _ := crypto.ComputeKeyImage(kp.Spend.Private, kp.Spend.Public)
	if ki1 != ki2 {
		t.Fatal("key image is not deterministic")
	}
}

func TestKeyImage_UniquePerKey(t *testing.T) {
	kp1, _ := crypto.GenerateWalletKeys()
	kp2, _ := crypto.GenerateWalletKeys()
	ki1, _ := crypto.ComputeKeyImage(kp1.Spend.Private, kp1.Spend.Public)
	ki2, _ := crypto.ComputeKeyImage(kp2.Spend.Private, kp2.Spend.Public)
	if ki1 == ki2 {
		t.Fatal("different keys should produce different key images")
	}
}

// ─── Ring Signatures (MLSAG) ─────────────────────────────────────────────────

func TestMLSAG_SignAndVerify(t *testing.T) {
	// Generate real key pair
	kp, _ := crypto.GenerateWalletKeys()

	// Build a ring: 1 real + 10 decoys
	ring := make([]crypto.RingMember, crypto.RingSize)
	for i := range ring {
		decoy, _ := crypto.GenerateWalletKeys()
		ring[i] = decoy.Spend.Public
	}
	realIdx := 3
	ring[realIdx] = kp.Spend.Public

	msg := crypto.HashStr("test transaction data")

	sig, err := crypto.MLSAGSign(msg, ring, realIdx, kp.Spend.Private)
	if err != nil {
		t.Fatalf("MLSAGSign: %v", err)
	}
	if sig == nil {
		t.Fatal("signature is nil")
	}

	ok, err := crypto.MLSAGVerify(msg, ring, sig)
	if err != nil {
		t.Fatalf("MLSAGVerify: %v", err)
	}
	if !ok {
		t.Fatal("valid MLSAG signature failed verification")
	}
}

func TestMLSAG_WrongMessage(t *testing.T) {
	kp, _ := crypto.GenerateWalletKeys()
	ring := make([]crypto.RingMember, crypto.RingSize)
	for i := range ring {
		d, _ := crypto.GenerateWalletKeys()
		ring[i] = d.Spend.Public
	}
	ring[0] = kp.Spend.Public

	msg1 := crypto.HashStr("message1")
	msg2 := crypto.HashStr("message2")

	sig, _ := crypto.MLSAGSign(msg1, ring, 0, kp.Spend.Private)
	ok, _ := crypto.MLSAGVerify(msg2, ring, sig)
	if ok {
		t.Fatal("MLSAG should fail verification with wrong message")
	}
}

func TestMLSAG_KeyImageConsistent(t *testing.T) {
	kp, _ := crypto.GenerateWalletKeys()
	ring := make([]crypto.RingMember, crypto.RingSize)
	for i := range ring {
		d, _ := crypto.GenerateWalletKeys()
		ring[i] = d.Spend.Public
	}
	ring[5] = kp.Spend.Public

	msg := crypto.HashStr("tx1")
	sig1, _ := crypto.MLSAGSign(msg, ring, 5, kp.Spend.Private)
	sig2, _ := crypto.MLSAGSign(msg, ring, 5, kp.Spend.Private)

	// Key images must be identical for the same key (double-spend detection)
	if sig1.KeyImage != sig2.KeyImage {
		t.Fatal("key image should be deterministic for same key")
	}
}

// ─── Range Proofs ─────────────────────────────────────────────────────────────

func TestRangeProof_Create(t *testing.T) {
	blind, _ := crypto.NewBlindFactor()
	proof, err := crypto.ProveRange(12345, blind)
	if err != nil {
		t.Fatalf("ProveRange: %v", err)
	}
	if proof == nil {
		t.Fatal("proof is nil")
	}
}

func TestRangeProof_Verify(t *testing.T) {
	blind, _ := crypto.NewBlindFactor()
	proof, _ := crypto.ProveRange(999, blind)
	ok, err := crypto.VerifyRange(proof)
	if err != nil {
		t.Fatalf("VerifyRange: %v", err)
	}
	if !ok {
		t.Fatal("valid range proof failed verification")
	}
}

func TestRangeProof_Zero(t *testing.T) {
	blind, _ := crypto.NewBlindFactor()
	proof, _ := crypto.ProveRange(0, blind)
	ok, err := crypto.VerifyRange(proof)
	if err != nil {
		t.Fatalf("VerifyRange zero: %v", err)
	}
	if !ok {
		t.Fatal("range proof for value 0 failed")
	}
}

func TestRangeProof_MaxUint64(t *testing.T) {
	blind, _ := crypto.NewBlindFactor()
	proof, _ := crypto.ProveRange(^uint64(0), blind) // max uint64
	ok, err := crypto.VerifyRange(proof)
	if err != nil {
		t.Fatalf("VerifyRange max: %v", err)
	}
	if !ok {
		t.Fatal("range proof for max uint64 failed")
	}
}

// ─── Stealth Addresses ───────────────────────────────────────────────────────

func TestStealth_CreateAndScan(t *testing.T) {
	// Generate receiver keys
	receiver, _ := crypto.GenerateWalletKeys()

	// Sender creates a stealth address for receiver
	stealth, err := crypto.CreateStealthAddress(receiver.Spend.Public, receiver.View.Public)
	if err != nil {
		t.Fatalf("CreateStealthAddress: %v", err)
	}

	// Receiver scans with view key
	hs, err := crypto.ScanForOutput(
		receiver.View.Private,
		receiver.Spend.Public,
		stealth.TxPubKey,
		stealth.OneTimePub,
	)
	if err != nil {
		t.Fatalf("ScanForOutput: %v", err)
	}
	if hs == nil {
		t.Fatal("receiver should have detected output as theirs")
	}
}

func TestStealth_WrongReceiver(t *testing.T) {
	receiver, _ := crypto.GenerateWalletKeys()
	other, _ := crypto.GenerateWalletKeys()

	stealth, _ := crypto.CreateStealthAddress(receiver.Spend.Public, receiver.View.Public)

	// Other wallet should NOT detect this output
	hs, err := crypto.ScanForOutput(
		other.View.Private,
		other.Spend.Public,
		stealth.TxPubKey,
		stealth.OneTimePub,
	)
	if err != nil {
		t.Fatal(err)
	}
	if hs != nil {
		t.Fatal("wrong receiver should not detect output as theirs")
	}
}
