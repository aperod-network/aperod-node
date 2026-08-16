package core_test

// Tests for TxBuilder.MaxSpendable — the send-max quote must replay Build's
// exact coin selection (dedup by OneTimePub, largest-first greedy, ≤8 inputs,
// fee = size × feePerByte) so that Build(MaxAmount) always succeeds on the
// first try.  Regression for the "Отправить всё" insufficient-funds bug:
// height-0 mints to one address share one OneTimePub (and thus one key
// image), so only the largest of them is ever spendable — a naive balance
// that sums them all overstates the sendable amount.

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// makeOwnedUTXO builds a synthetic OwnedUTXO with the given one-time pub and
// amount.  Blind/commitment validity does not matter for selection tests.
func makeOwnedUTXO(t *testing.T, pub crypto.Point32, amount uint64, idx byte) core.OwnedUTXO {
	t.Helper()
	blind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := crypto.Commit(amount, blind)
	if err != nil {
		t.Fatal(err)
	}
	var txHash crypto.Hash32
	txHash[0] = idx
	return core.OwnedUTXO{
		UTXO: core.UTXO{
			TxHash:       txHash,
			OutputIndex:  0,
			OneTimePub:   pub,
			AmountCommit: commit,
		},
		Amount: amount,
		Blind:  blind,
	}
}

func newPub(t *testing.T, seed uint64) crypto.Point32 {
	t.Helper()
	p, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(seed))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMaxSpendable_DedupSharedOneTimePub: three height-0 style mints share
// one OneTimePub — only the largest may count toward the spendable amount.
func TestMaxSpendable_DedupSharedOneTimePub(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	shared := newPub(t, 42)

	const feePerByte = 1
	utxos := []core.OwnedUTXO{
		makeOwnedUTXO(t, shared, 50_000_000_000, 1), // largest — kept
		makeOwnedUTXO(t, shared, 30_000_000_000, 2), // duplicate KI — dropped
		makeOwnedUTXO(t, shared, 20_000_000_000, 3), // duplicate KI — dropped
	}
	b := core.NewTxBuilder(keys.Spend.Private, keys.View.Private, keys.Spend.Public, utxos, feePerByte)
	res := b.MaxSpendable()

	if res.UTXOCount != 1 {
		t.Fatalf("UTXOCount = %d, want 1 (dedup by OneTimePub)", res.UTXOCount)
	}
	if res.SpendableTotal != 50_000_000_000 {
		t.Fatalf("SpendableTotal = %d, want 50_000_000_000 (largest only)", res.SpendableTotal)
	}
	wantFee := core.ExportedEstimateFee(1, 2, feePerByte)
	if res.Fee != wantFee {
		t.Fatalf("Fee = %d, want %d", res.Fee, wantFee)
	}
	if res.MaxAmount != 50_000_000_000-wantFee {
		t.Fatalf("MaxAmount = %d, want %d", res.MaxAmount, 50_000_000_000-wantFee)
	}
	if res.InputCount != 1 {
		t.Fatalf("InputCount = %d, want 1", res.InputCount)
	}
}

// TestMaxSpendable_BuildSucceedsFirstTry: Build(MaxAmount) must never return
// insufficient funds for an address with multiple UTXOs, including several
// that share one OneTimePub (height-0 mints).
func TestMaxSpendable_BuildSucceedsFirstTry(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	bobKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	net := crypto.TestnetByte
	aliceAddr := crypto.AddressFromKeys(net, keys)
	bobAddr := crypto.AddressFromKeys(net, bobKeys)

	shared := newPub(t, 7)
	utxos := []core.OwnedUTXO{
		makeOwnedUTXO(t, shared, 148_858_315_00000000/100, 1), // height-0 mint (on-chain truth)
		makeOwnedUTXO(t, shared, 148_858_320_00000000/100, 2), // shares OneTimePub — only one spendable
		makeOwnedUTXO(t, newPub(t, 8), 3_000_000_000, 3),
		makeOwnedUTXO(t, newPub(t, 9), 1_500_000_000, 4),
	}
	b := core.NewTxBuilder(keys.Spend.Private, keys.View.Private, keys.Spend.Public, utxos, 0)
	res := b.MaxSpendable()
	if res.MaxAmount == 0 {
		t.Fatal("MaxAmount = 0, want > 0")
	}
	// UTXOCount: shared pair collapses to one entry.
	if res.UTXOCount != 3 {
		t.Fatalf("UTXOCount = %d, want 3", res.UTXOCount)
	}

	// Build with exactly the quoted amount must succeed (Phase 1 decoys).
	if _, err := b.Build(res.MaxAmount, bobAddr, aliceAddr); err != nil {
		t.Fatalf("Build(MaxAmount=%d) failed: %v", res.MaxAmount, err)
	}
	// One nAPRO more must fail with insufficient funds — MaxAmount is tight.
	if _, err := b.Build(res.MaxAmount+1, bobAddr, aliceAddr); err == nil {
		t.Fatalf("Build(MaxAmount+1) unexpectedly succeeded — quote is not maximal")
	} else if !strings.Contains(err.Error(), "insufficient funds") {
		t.Fatalf("Build(MaxAmount+1) failed with unexpected error: %v", err)
	}
}

// TestMaxSpendable_DustInputsNotWorthTheirFee: an extra dust input whose
// amount is below its own fee weight must not inflate the quote.
func TestMaxSpendable_DustInputsNotWorthTheirFee(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	const feePerByte = 200 // production-like rate: 1152 bytes/input → 230_400 nAPRO per extra input
	utxos := []core.OwnedUTXO{
		makeOwnedUTXO(t, newPub(t, 11), 10_000_000_000, 1),
		makeOwnedUTXO(t, newPub(t, 12), 100_000, 2), // dust: below the 230_400 input fee cost
	}
	b := core.NewTxBuilder(keys.Spend.Private, keys.View.Private, keys.Spend.Public, utxos, feePerByte)
	res := b.MaxSpendable()

	wantFee := core.ExportedEstimateFee(1, 2, feePerByte)
	want := 10_000_000_000 - wantFee
	if res.MaxAmount != want {
		t.Fatalf("MaxAmount = %d, want %d (dust input must be excluded)", res.MaxAmount, want)
	}
	if res.InputCount != 1 {
		t.Fatalf("InputCount = %d, want 1", res.InputCount)
	}
}

// TestMaxSpendable_Empty: no UTXOs → zero result, no panic.
func TestMaxSpendable_Empty(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	b := core.NewTxBuilder(keys.Spend.Private, keys.View.Private, keys.Spend.Public, nil, 0)
	res := b.MaxSpendable()
	if res.MaxAmount != 0 || res.Fee != 0 || res.InputCount != 0 || res.UTXOCount != 0 {
		t.Fatalf("empty builder: got %+v, want zero result", res)
	}
}
