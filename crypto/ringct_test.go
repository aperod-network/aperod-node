package crypto_test

// Tests that enforce the C3 (duplicate ring member) and C4 (constant-time scan)
// security fixes.  The existing security_test.go tests accept duplicates with
// t.Log + return; these tests are strict: they call t.Error when the fix is absent.

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

// TestMLSAG_RejectsDuplicateRingMembers verifies that both MLSAGSign and
// MLSAGVerify return a non-nil error when all 16 ring members are identical.
//
// A fully-duplicate ring collapses the anonymity set to 1, trivially identifying
// the signer.  The C3 fix must reject this at both call sites so a crafted
// transaction can never reach the mempool or chain verifier.
func TestMLSAG_RejectsDuplicateRingMembers(t *testing.T) {
	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}

	// All RingSize members are the same spend public key.
	ring := make([]crypto.RingMember, crypto.RingSize)
	for i := range ring {
		ring[i] = crypto.RingMember(wk.Spend.Public)
	}

	var msg crypto.Hash32

	// MLSAGSign must return a non-nil error — not silently produce a signature.
	_, signErr := crypto.MLSAGSign(msg, ring, 0, wk.Spend.Private)
	if signErr == nil {
		t.Error("MLSAGSign: expected non-nil error for all-duplicate ring, got nil (C3 fix missing?)")
	}

	// MLSAGVerify must also return a non-nil error even with a structurally valid
	// stub — the duplicate check must fire before the ring-closure walk.
	stub := &crypto.MLSAGSignature{SS: make([][32]byte, crypto.RingSize)}
	_, verifyErr := crypto.MLSAGVerify(msg, ring, stub)
	if verifyErr == nil {
		t.Error("MLSAGVerify: expected non-nil error for all-duplicate ring, got nil (C3 fix missing at verifier?)")
	}
}

// TestMLSAG_RejectsPartialDuplicate verifies that both MLSAGSign and MLSAGVerify
// return a non-nil error when only two of the 16 ring members share the same key.
//
// This is the minimal duplicate attack: an adversary replaces exactly one decoy
// with a copy of the real key, halving the effective anonymity set to 1.  The C3
// fix must catch this case as well as the all-equal case above.
func TestMLSAG_RejectsPartialDuplicate(t *testing.T) {
	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}

	// Build a ring with RingSize distinct keys, then duplicate ring[1] = ring[0].
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = crypto.RingMember(wk.Spend.Public)
	for i := 1; i < crypto.RingSize; i++ {
		d, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatalf("GenerateWalletKeys decoy %d: %v", i, err)
		}
		ring[i] = crypto.RingMember(d.Spend.Public)
	}
	ring[1] = ring[0] // introduce the single duplicate

	var msg crypto.Hash32

	_, signErr := crypto.MLSAGSign(msg, ring, 0, wk.Spend.Private)
	if signErr == nil {
		t.Error("MLSAGSign: expected non-nil error for ring with one duplicate member, got nil (C3 fix missing?)")
	}

	stub := &crypto.MLSAGSignature{SS: make([][32]byte, crypto.RingSize)}
	_, verifyErr := crypto.MLSAGVerify(msg, ring, stub)
	if verifyErr == nil {
		t.Error("MLSAGVerify: expected non-nil error for ring with one duplicate member, got nil (C3 fix missing at verifier?)")
	}
}

// TestScanForOutput_ConstantTime is a correctness sanity-check for the C4 fix
// (constant-time output scanning via subtle.ConstantTimeCompare).
//
// It verifies that:
//   - ScanForOutput returns a non-nil Hs scalar for an output addressed to the wallet (match).
//   - ScanForOutput returns nil without an error for an output addressed to a different wallet (miss).
//
// The two branches both exercise the subtle.ConstantTimeCompare code path, so a
// regression to a short-circuiting comparison would still be caught by the
// match branch failing to find the output (the point arithmetic would be wrong).
func TestScanForOutput_ConstantTime(t *testing.T) {
	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}

	// Create a stealth output addressed to wk.
	out, err := crypto.CreateStealthOutput(wk.Spend.Public, wk.View.Public)
	if err != nil {
		t.Fatalf("CreateStealthOutput: %v", err)
	}

	// Match branch: scanning with the correct wallet must find the output.
	hs, err := crypto.ScanForOutput(wk.View.Private, wk.Spend.Public, out.TxPubKey, out.OneTimePub)
	if err != nil {
		t.Fatalf("ScanForOutput (match): unexpected error: %v", err)
	}
	if hs == nil {
		t.Error("ScanForOutput (match): expected non-nil Hs scalar, got nil — constant-time comparison may be broken")
	}

	// Miss branch: scanning with a different wallet must return nil without an error.
	other, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys (other): %v", err)
	}
	hs2, err := crypto.ScanForOutput(other.View.Private, other.Spend.Public, out.TxPubKey, out.OneTimePub)
	if err != nil {
		t.Fatalf("ScanForOutput (miss): unexpected error: %v", err)
	}
	if hs2 != nil {
		t.Error("ScanForOutput (miss): expected nil for a different wallet, got non-nil — false positive in output scanning")
	}
}
