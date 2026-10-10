package core_test

import (
        "testing"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// TestMintAndSpend replicates the exact real-world flow:
// 1. Admin mints APRO via BuildMintTx
// 2. User spends it via TxBuilder (using DeterministicMintBlind)
// This catches the "commitment balance check failed" bug.
func TestMintAndSpend(t *testing.T) {
        const mintAmount = uint64(200_000_000_000_000) // 2,000,000 APRO
        const sendAmount = uint64(20_000_000_000_000)  // 200,000 APRO

        // 1. Generate Alice wallet keys
        aliceKeys, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatal(err)
        }
        aliceAddr := crypto.AddressFromKeys(crypto.MainnetByte, aliceKeys)
        t.Logf("Alice address: %s", aliceAddr)

        // 2. Admin mints APRO to Alice (same as restAdminMint → BuildMintTx, height=0)
        mintTx, err := core.BuildMintTx(aliceAddr, mintAmount, 0)
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

// TestMintUniquePerHeight guards against the coinbase-collision bug: minting
// the identical address+amount at two different heights (as PoA does for the
// per-block validator reward) must produce distinct tx hashes, output pubkeys,
// and — after spending — distinct key images. Before the height-shift fix,
// mint_pub was always spend_pub regardless of height, so every block's reward
// tx was byte-for-byte identical, silently overwriting prior UTXOs and causing
// false double-spend rejections.
func TestMintUniquePerHeight(t *testing.T) {
        const amount = uint64(1_000_000_000) // same amount every "block", like a fixed reward

        keys, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatal(err)
        }
        addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

        txA, err := core.BuildMintTx(addr, amount, 100)
        if err != nil {
                t.Fatalf("BuildMintTx height=100: %v", err)
        }
        txB, err := core.BuildMintTx(addr, amount, 101)
        if err != nil {
                t.Fatalf("BuildMintTx height=101: %v", err)
        }

        if txA.Outputs[0].OneTimePub == txB.Outputs[0].OneTimePub {
                t.Fatal("mint outputs at different heights must have distinct OneTimePub")
        }
        if txA.Hash() == txB.Hash() {
                t.Fatal("mint txs at different heights must have distinct hashes")
        }

        // Spending both rewards must yield distinct key images (no false double-spend).
        spendPub, err := crypto.PublicKeyFromPrivate(keys.Spend.Private)
        if err != nil {
                t.Fatalf("PublicKeyFromPrivate: %v", err)
        }
        heightPubA, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(100))
        if err != nil {
                t.Fatal(err)
        }
        heightPubB, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(101))
        if err != nil {
                t.Fatal(err)
        }
        oneTimePrivA, err := crypto.AddScalars(crypto.ScalarFromUint64(100), keys.Spend.Private)
        if err != nil {
                t.Fatal(err)
        }
        oneTimePrivB, err := crypto.AddScalars(crypto.ScalarFromUint64(101), keys.Spend.Private)
        if err != nil {
                t.Fatal(err)
        }
        expectedPubA, err := crypto.AddPoints(spendPub, heightPubA)
        if err != nil {
                t.Fatal(err)
        }
        expectedPubB, err := crypto.AddPoints(spendPub, heightPubB)
        if err != nil {
                t.Fatal(err)
        }
        if expectedPubA != txA.Outputs[0].OneTimePub || expectedPubB != txB.Outputs[0].OneTimePub {
                t.Fatal("recomputed mint pub does not match BuildMintTx output")
        }

        kiA, err := crypto.ComputeKeyImage(oneTimePrivA, txA.Outputs[0].OneTimePub)
        if err != nil {
                t.Fatalf("ComputeKeyImage A: %v", err)
        }
        kiB, err := crypto.ComputeKeyImage(oneTimePrivB, txB.Outputs[0].OneTimePub)
        if err != nil {
                t.Fatalf("ComputeKeyImage B: %v", err)
        }
        if kiA == kiB {
                t.Fatal("key images for mint outputs at different heights must be distinct")
        }
}

// TestMintHeightZeroMatchesLegacy ensures height=0 (used by one-off admin
// mints) is byte-for-byte identical to the pre-fix behavior: mint_pub ==
// spend_pub directly. This must never regress or existing admin-minted
// UTXOs become unspendable.
func TestMintHeightZeroMatchesLegacy(t *testing.T) {
        keys, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatal(err)
        }
        addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)
        _, spendPub, _, err := crypto.DecodeAddress(addr)
        if err != nil {
                t.Fatal(err)
        }

        tx, err := core.BuildMintTx(addr, 42, 0)
        if err != nil {
                t.Fatal(err)
        }
        if tx.Outputs[0].OneTimePub != spendPub {
                t.Fatal("height=0 must produce mint_pub == spend_pub (legacy transparent behavior)")
        }
}
