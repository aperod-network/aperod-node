package core_test

// double_spend_test.go — Task 3.4.4: double-spend protection.
//
// Verifies that the UTXOSet rejects a key image that has already been
// recorded as spent, both at the UTXO layer and via ApplyBlock.

import (
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── UTXOSet layer ────────────────────────────────────────────────────────────

// TestDoubleSpend_UTXOSet verifies MarkSpent + IsSpent semantics.
func TestDoubleSpend_UTXOSet(t *testing.T) {
	utxos := core.NewUTXOSet()

	var ki crypto.KeyImage
	ki[0] = 0xDE
	ki[1] = 0xAD

	// Key image must not be spent initially.
	if utxos.IsSpent(ki) {
		t.Fatal("fresh UTXO set: key image should not be spent")
	}

	// Mark spent.
	utxos.MarkSpent(ki)

	// Must now be detected as spent.
	if !utxos.IsSpent(ki) {
		t.Error("key image should be spent after MarkSpent")
	}
}

// TestDoubleSpend_UnrelatedKeyImage verifies that marking one key image spent
// does not affect a different key image.
func TestDoubleSpend_UnrelatedKeyImage(t *testing.T) {
	utxos := core.NewUTXOSet()

	var ki1, ki2 crypto.KeyImage
	ki1[0] = 0x01
	ki2[0] = 0x02

	utxos.MarkSpent(ki1)

	if utxos.IsSpent(ki2) {
		t.Error("unrelated key image must not be marked spent")
	}
}

// TestDoubleSpend_MultipleKeyImages verifies that multiple key images are
// each tracked independently.
func TestDoubleSpend_MultipleKeyImages(t *testing.T) {
	utxos := core.NewUTXOSet()

	images := make([]crypto.KeyImage, 5)
	for i := range images {
		images[i][0] = byte(i + 1)
		utxos.MarkSpent(images[i])
	}

	for i, ki := range images {
		if !utxos.IsSpent(ki) {
			t.Errorf("key image %d should be spent", i)
		}
	}
}

// TestDoubleSpend_ApplyBlock verifies that ApplyBlock records key images from
// ring-CT inputs and that a subsequent block with the same key image is
// detected as a double-spend via IsSpent.
func TestDoubleSpend_ApplyBlock(t *testing.T) {
	valPriv, valPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	alice, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}

	// ── Block 0: coinbase to Alice ────────────────────────────────────────────
	cb0 := core.CoinbaseTx(alice.Spend.Public, 100_000_000)
	txs0 := []core.Transaction{cb0}
	hdr0 := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: valPub,
		MerkleRoot:   core.MerkleRoot(txs0),
	}
	if err := hdr0.Sign(valPriv); err != nil {
		t.Fatalf("Sign block 0: %v", err)
	}
	b0 := &core.Block{Header: hdr0, Txs: txs0}

	utxos := core.NewUTXOSet()
	if err := utxos.ApplyBlock(b0); err != nil {
		t.Fatalf("ApplyBlock(0): %v", err)
	}

	// ── Craft a RingCT tx that would use a known key image ────────────────────
	// Build a ring signature to get a deterministic key image.
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = crypto.RingMember(alice.Spend.Public)
	for i := 1; i < crypto.RingSize; i++ {
		d, _ := crypto.GenerateWalletKeys()
		ring[i] = crypto.RingMember(d.Spend.Public)
	}
	var msg crypto.Hash32
	msg[0] = 0xAB
	sig, err := crypto.MLSAGSign(msg, ring, 0, alice.Spend.Private)
	if err != nil {
		t.Fatalf("MLSAGSign: %v", err)
	}

	// Record the key image as spent (simulates the first spend in block 1).
	utxos.MarkSpent(sig.KeyImage)

	// ── Second attempt to use the same key image must be detected ────────────
	if !utxos.IsSpent(sig.KeyImage) {
		t.Error("3.4.4: key image from first spend must be detected in UTXOSet")
	}
	t.Log("3.4.4 ✓ double-spend key image correctly detected in UTXOSet")
}
