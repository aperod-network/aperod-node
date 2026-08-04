package core

// Internal unit tests for txBuildRing fallback-decoy behaviour.
// These tests live in package core (not core_test) so they can access
// the unexported txBuildRing function directly.

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

// TestTxBuildRing_FallbackCount_FewerDecoysThanNeeded verifies that
// txBuildRing increments fallbackCount for every ring slot that could not be
// filled with a supplied real decoy.
//
// With only 3 real decoys and RingSize-1 = 15 slots needed, the remaining
// 12 slots must fall back to randomly-generated Phase 1 keys, so
// fallbackCount must equal (RingSize-1) - 3 = 12.
func TestTxBuildRing_FallbackCount_FewerDecoysThanNeeded(t *testing.T) {
	// Generate the "real" key that will occupy the single true ring member slot.
	realWK, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	realPub := realWK.Spend.Public

	// Provide exactly 3 decoys — far fewer than RingSize-1 = 15.
	const nDecoys = 3
	decoys := make([]DecoyUTXO, nDecoys)
	for i := range decoys {
		dk, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatalf("GenerateWalletKeys decoy %d: %v", i, err)
		}
		decoys[i] = DecoyUTXO{OneTimePub: dk.Spend.Public}
	}

	ring, realIdx, fallbackCount, err := txBuildRing(realPub, decoys)
	if err != nil {
		t.Fatalf("txBuildRing: %v", err)
	}

	// Ring must have exactly RingSize members.
	if len(ring) != crypto.RingSize {
		t.Errorf("ring length = %d, want %d", len(ring), crypto.RingSize)
	}

	// The real key must sit at realIdx.
	if ring[realIdx] != realPub {
		t.Error("ring[realIdx] is not the real key")
	}

	// fallbackCount must equal (RingSize-1) - nDecoys.
	wantFallback := (crypto.RingSize - 1) - nDecoys
	if fallbackCount != wantFallback {
		t.Errorf("fallbackCount = %d, want %d (RingSize-1=%d, nDecoys=%d)",
			fallbackCount, wantFallback, crypto.RingSize-1, nDecoys)
	}
	t.Logf("fallbackCount=%d (real decoys supplied=%d, RingSize-1=%d)",
		fallbackCount, nDecoys, crypto.RingSize-1)
}

// TestTxBuildRing_FallbackCount_NoDecoys verifies Phase 1 behaviour: when no
// decoys are provided at all, every ring slot (all RingSize-1 of them) must
// fall back to randomly-generated keys.
func TestTxBuildRing_FallbackCount_NoDecoys(t *testing.T) {
	realWK, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	realPub := realWK.Spend.Public

	ring, realIdx, fallbackCount, err := txBuildRing(realPub, nil)
	if err != nil {
		t.Fatalf("txBuildRing: %v", err)
	}

	if len(ring) != crypto.RingSize {
		t.Errorf("ring length = %d, want %d", len(ring), crypto.RingSize)
	}

	if ring[realIdx] != realPub {
		t.Error("ring[realIdx] is not the real key")
	}

	// All RingSize-1 decoy slots must be fallback.
	if fallbackCount != crypto.RingSize-1 {
		t.Errorf("fallbackCount = %d, want %d (RingSize-1)",
			fallbackCount, crypto.RingSize-1)
	}
	t.Logf("Phase 1 fallbackCount=%d as expected", fallbackCount)
}

// TestTxBuildRing_FallbackCount_ZeroWhenFullDecoys verifies that fallbackCount
// is zero when exactly RingSize-1 real decoys are supplied (Phase 2 ideal path).
func TestTxBuildRing_FallbackCount_ZeroWhenFullDecoys(t *testing.T) {
	realWK, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	realPub := realWK.Spend.Public

	// Supply exactly the right number of decoys.
	nDecoys := crypto.RingSize - 1
	decoys := make([]DecoyUTXO, nDecoys)
	for i := range decoys {
		dk, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatalf("GenerateWalletKeys decoy %d: %v", i, err)
		}
		decoys[i] = DecoyUTXO{OneTimePub: dk.Spend.Public}
	}

	_, _, fallbackCount, err := txBuildRing(realPub, decoys)
	if err != nil {
		t.Fatalf("txBuildRing: %v", err)
	}

	if fallbackCount != 0 {
		t.Errorf("fallbackCount = %d, want 0 when full decoy set is supplied", fallbackCount)
	}
	t.Log("fallbackCount=0 as expected with full decoy set")
}
