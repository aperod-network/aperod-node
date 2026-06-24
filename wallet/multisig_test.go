package wallet_test

// multisig_test.go — tests for block 3.3 (2-of-3 Shamir threshold multisig).
//
// Tests:
//   3.3.1 MultisigSetup: address is generated, 3 shares are produced
//   3.3.2 PartialSign: each participant produces a partial contribution
//   3.3.3 CombinePartials: any 2 of 3 partials reconstruct the spend key
//   3.3.x CombineShares: direct reconstruction (all 3 pair combinations)
//   3.3.x Error cases: wrong indices, duplicate shares

import (
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/wallet"
)

// TestMultisigSetup_GeneratesAddress verifies that MultisigSetup produces a
// valid multisig address and 3 shares with distinct keys (3.3.1).
func TestMultisigSetup_GeneratesAddress(t *testing.T) {
	shares, addr, err := wallet.MultisigSetup(crypto.TestnetByte)
	if err != nil {
		t.Fatalf("MultisigSetup: %v", err)
	}
	if len(shares) != 3 {
		t.Fatalf("expected 3 shares, got %d", len(shares))
	}
	if addr.Address == "" {
		t.Fatal("address is empty")
	}
	// Address should decode without error
	net, spend, view, err := crypto.DecodeAddress(addr.Address)
	if err != nil {
		t.Fatalf("DecodeAddress: %v", err)
	}
	if net != crypto.TestnetByte {
		t.Errorf("net = 0x%02x, want TestnetByte 0x%02x", byte(net), byte(crypto.TestnetByte))
	}
	if spend != addr.SpendPub {
		t.Error("decoded spend key != addr.SpendPub")
	}
	if view != addr.ViewPub {
		t.Error("decoded view key != addr.ViewPub")
	}
	// All 3 shares should have distinct keys
	for i := 0; i < 3; i++ {
		if shares[i].Index != i+1 {
			t.Errorf("share[%d].Index = %d, want %d", i, shares[i].Index, i+1)
		}
		if shares[i].SpendPub != addr.SpendPub {
			t.Errorf("share[%d].SpendPub != addr.SpendPub", i)
		}
		// Share keys should be distinct (astronomically unlikely to collide)
		for j := i + 1; j < 3; j++ {
			if shares[i].ShareKey == shares[j].ShareKey {
				t.Errorf("share[%d] and share[%d] have identical ShareKey", i, j)
			}
		}
	}
}

// TestMultisigSetup_EachSetupDiffers verifies that two MultisigSetup calls
// produce independent wallets (no key reuse).
func TestMultisigSetup_EachSetupDiffers(t *testing.T) {
	_, addr1, err1 := wallet.MultisigSetup(crypto.TestnetByte)
	_, addr2, err2 := wallet.MultisigSetup(crypto.TestnetByte)
	if err1 != nil || err2 != nil {
		t.Fatalf("MultisigSetup errors: %v, %v", err1, err2)
	}
	if addr1.Address == addr2.Address {
		t.Error("two MultisigSetup calls produced the same address")
	}
}

// TestCombineShares_AllPairs verifies that any 2 of 3 shares reconstruct
// the same spend key, and that the derived public key matches the address (3.3.3).
func TestCombineShares_AllPairs(t *testing.T) {
	shares, addr, err := wallet.MultisigSetup(crypto.TestnetByte)
	if err != nil {
		t.Fatalf("MultisigSetup: %v", err)
	}

	pairs := [][2]int{{0, 1}, {0, 2}, {1, 2}} // index pairs into shares slice
	reconstructed := make([]crypto.Scalar32, 3)

	for pIdx, pair := range pairs {
		twoShares := []wallet.MultisigShare{shares[pair[0]], shares[pair[1]]}
		key, err := wallet.CombineShares(twoShares)
		if err != nil {
			t.Fatalf("CombineShares pair (%d,%d): %v", pair[0]+1, pair[1]+1, err)
		}
		reconstructed[pIdx] = key

		// Derive public key and compare with the multisig address spend pub
		pub, err := wallet.SpendPublic(key)
		if err != nil {
			t.Fatalf("SpendPublic pair (%d,%d): %v", pair[0]+1, pair[1]+1, err)
		}
		if pub != addr.SpendPub {
			t.Errorf("pair (%d,%d): reconstructed pubkey != addr.SpendPub", pair[0]+1, pair[1]+1)
		}
	}

	// All 3 reconstructions must yield the same scalar
	if reconstructed[0] != reconstructed[1] {
		t.Error("pair (1,2) and pair (1,3) produced different secrets")
	}
	if reconstructed[0] != reconstructed[2] {
		t.Error("pair (1,2) and pair (2,3) produced different secrets")
	}
}

// TestPartialSign_CombinePartials verifies the two-step signing flow (3.3.2 + 3.3.3).
func TestPartialSign_CombinePartials(t *testing.T) {
	shares, addr, err := wallet.MultisigSetup(crypto.TestnetByte)
	if err != nil {
		t.Fatalf("MultisigSetup: %v", err)
	}

	// Participant 1 and participant 2 co-sign
	p1, err := wallet.PartialSign(shares[0], 2)
	if err != nil {
		t.Fatalf("PartialSign(1,co=2): %v", err)
	}
	p2, err := wallet.PartialSign(shares[1], 1)
	if err != nil {
		t.Fatalf("PartialSign(2,co=1): %v", err)
	}

	combined, err := wallet.CombinePartials([]wallet.PartialSig{p1, p2})
	if err != nil {
		t.Fatalf("CombinePartials: %v", err)
	}

	// Derived public key must match the multisig address
	pub, err := wallet.SpendPublic(combined)
	if err != nil {
		t.Fatalf("SpendPublic: %v", err)
	}
	if pub != addr.SpendPub {
		t.Error("PartialSign + CombinePartials: reconstructed pubkey != addr.SpendPub")
	}
}

// TestPartialSign_AllPairs verifies that all 3 co-signer pair combinations work.
func TestPartialSign_AllPairs(t *testing.T) {
	shares, addr, err := wallet.MultisigSetup(crypto.TestnetByte)
	if err != nil {
		t.Fatalf("MultisigSetup: %v", err)
	}

	type pair struct{ i, j int }
	pairs := []pair{{1, 2}, {1, 3}, {2, 3}}

	for _, p := range pairs {
		pi, err := wallet.PartialSign(shares[p.i-1], p.j)
		if err != nil {
			t.Fatalf("PartialSign(%d, co=%d): %v", p.i, p.j, err)
		}
		pj, err := wallet.PartialSign(shares[p.j-1], p.i)
		if err != nil {
			t.Fatalf("PartialSign(%d, co=%d): %v", p.j, p.i, err)
		}
		combined, err := wallet.CombinePartials([]wallet.PartialSig{pi, pj})
		if err != nil {
			t.Fatalf("CombinePartials pair (%d,%d): %v", p.i, p.j, err)
		}
		pub, err := wallet.SpendPublic(combined)
		if err != nil {
			t.Fatalf("SpendPublic pair (%d,%d): %v", p.i, p.j, err)
		}
		if pub != addr.SpendPub {
			t.Errorf("pair (%d,%d): reconstructed pubkey != addr.SpendPub", p.i, p.j)
		}
	}
}

// TestCombineShares_ErrorCases verifies error handling.
func TestCombineShares_ErrorCases(t *testing.T) {
	shares, _, err := wallet.MultisigSetup(crypto.TestnetByte)
	if err != nil {
		t.Fatalf("MultisigSetup: %v", err)
	}

	// Too few shares
	_, err = wallet.CombineShares([]wallet.MultisigShare{shares[0]})
	if err == nil {
		t.Error("CombineShares(1 share) should return error")
	}

	// Too many shares
	_, err = wallet.CombineShares(shares)
	if err == nil {
		t.Error("CombineShares(3 shares) should return error")
	}

	// Duplicate indices
	dup := wallet.MultisigShare{Index: 1, ShareKey: shares[0].ShareKey, SpendPub: shares[0].SpendPub}
	_, err = wallet.CombineShares([]wallet.MultisigShare{shares[0], dup})
	if err == nil {
		t.Error("CombineShares with duplicate indices should return error")
	}
}

// TestPartialSign_ErrorCases verifies error handling in PartialSign.
func TestPartialSign_ErrorCases(t *testing.T) {
	shares, _, err := wallet.MultisigSetup(crypto.TestnetByte)
	if err != nil {
		t.Fatalf("MultisigSetup: %v", err)
	}

	// Same index as co-signer
	_, err = wallet.PartialSign(shares[0], 1)
	if err == nil {
		t.Error("PartialSign with own index as co-signer should error")
	}

	// Out-of-range co-signer index
	_, err = wallet.PartialSign(shares[0], 5)
	if err == nil {
		t.Error("PartialSign with co-signer index 5 should error")
	}
}

// TestCombinePartials_ErrorCases verifies error handling in CombinePartials.
func TestCombinePartials_ErrorCases(t *testing.T) {
	// Wrong count
	_, err := wallet.CombinePartials([]wallet.PartialSig{{Index: 1}})
	if err == nil {
		t.Error("CombinePartials(1 partial) should error")
	}

	// Duplicate indices
	_, err = wallet.CombinePartials([]wallet.PartialSig{
		{Index: 2},
		{Index: 2},
	})
	if err == nil {
		t.Error("CombinePartials with duplicate indices should error")
	}
}
