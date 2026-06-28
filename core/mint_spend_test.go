package core_test

import (
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// TestMintAndSpend replicates the exact real-world flow:
// 1. Admin mints APR via BuildMintTx
// 2. User spends it via TxBuilder (using DeterministicMintBlind)
// This catches the "commitment balance check failed" bug.
func TestMintAndSpend(t *testing.T) {
	const mintAmount = uint64(200_000_000_000_000) // 2,000,000 APR
	const sendAmount = uint64(20_000_000_000_000)  // 200,000 APR

	// 1. Generate Alice wallet keys
	aliceKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	aliceAddr := crypto.AddressFromKeys(crypto.MainnetByte, aliceKeys)
	t.Logf("Alice address: %s", aliceAddr)

	// 2. Admin mints APR to Alice (same as restAdminMint → BuildMintTx)
	mintTx, err := core.BuildMintTx(aliceAddr, mintAmount)
	if err != nil {
		t.Fatalf("BuildMintTx: %v", err)
	}
	t.Logf("Mint tx outputs: %d", len(mintTx.Outputs))
	t.Logf("Mint output.OneTimePub: %x", mintTx.Outputs[0].OneTimePub[:8])
	t.Logf("Mint output.AmountCommit: %x", mintTx.Outputs[0].AmountCommit[:8])

	// 3. Verify the commitment recomputes correctly (as aprWalletSend does)
	_, spendPub, _, err := crypto.DecodeAddress(aliceAddr)
	if err != nil {
		t.Fatalf("DecodeAddress: %v", err)
	}
	t.Logf("spendPub (from address): %x", spendPub[:8])

	// 3b. Also derive spendPub from private key (as aprWalletSend does)
	spendPubFromPriv, err := crypto.PublicKeyFromPrivate(aliceKeys.Spend.Private)
	if err != nil {
		t.Fatalf("PublicKeyFromPrivate: %v", err)
	}
	t.Logf("spendPub (from private): %x", spendPubFromPriv[:8])

	if spendPub != spendPubFromPriv {
		t.Errorf("MISMATCH: spendPub from address != spendPub from private key!")
	} else {
		t.Log("spendPub matches ✓")
	}

	// 4. Recompute blind (as aprWalletSend does)
	blind, err := crypto.DeterministicMintBlind(spendPubFromPriv, mintAmount)
	if err != nil {
		t.Fatalf("DeterministicMintBlind: %v", err)
	}

	// 5. Verify commitment matches on-chain commitment
	recomputedCommit, err := crypto.Commit(mintAmount, blind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if recomputedCommit != mintTx.Outputs[0].AmountCommit {
		t.Errorf("COMMITMENT MISMATCH:\n  on-chain:    %x\n  recomputed:  %x",
			mintTx.Outputs[0].AmountCommit[:], recomputedCommit[:])
	} else {
		t.Log("Commitment recomputed correctly ✓")
	}

	// 6. Build the spending transaction (as TxBuilder does)
	ownedUTXO := core.OwnedUTXO{
		UTXO: core.UTXO{
			OneTimePub:   mintTx.Outputs[0].OneTimePub,
			TxPubKey:     mintTx.Outputs[0].TxPubKey,
			AmountCommit: mintTx.Outputs[0].AmountCommit,
		},
		HsScalar: crypto.Scalar32{}, // zero for mint outputs
		Amount:   mintAmount,
		Blind:    blind,
	}

	bobKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	bobAddr := crypto.AddressFromKeys(crypto.MainnetByte, bobKeys)

	builder := core.NewTxBuilder(
		aliceKeys.Spend.Private,
		aliceKeys.View.Private,
		spendPubFromPriv,
		[]core.OwnedUTXO{ownedUTXO},
		0,
	)

	result, err := builder.Build(sendAmount, bobAddr, aliceAddr)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Logf("Built tx: %d inputs, %d outputs, fee=%d", result.InputCount, result.OutputCount, result.TotalFee)

	// 7. Run full cryptographic verification (same as aprWalletSend does)
	verifier := core.NewTxVerifier(nil)
	if err := verifier.VerifyTx(&result.Tx); err != nil {
		t.Fatalf("VerifyTx FAILED: %v", err)
	} else {
		t.Log("VerifyTx passed ✓")
	}
}
