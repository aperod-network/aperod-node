package core_test

// Integration tests for the RingCT transaction builder.
// Covers the full Alice → Bob privacy transaction cycle:
//   1. Alice scans the chain and discovers her UTXOs.
//   2. Alice builds a signed RingCT transaction to Bob.
//   3. The transaction passes structural + cryptographic verification.
//   4. Bob scans and finds his received output; change goes back to Alice.

import (
        "testing"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// makeGenesisWithCoinbase creates a minimal single-block chain where Alice
// owns one coinbase output worth `supply` base units.
func makeGenesisWithCoinbase(t *testing.T, aliceKeys *crypto.WalletKeyPair, supply uint64) (*core.Chain, *core.Block, crypto.BlindFactor) {
        t.Helper()

        validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
        if err != nil {
                t.Fatal(err)
        }

        net := crypto.TestnetByte
        aliceAddr := crypto.AddressFromKeys(net, aliceKeys)

        _, aliceSpend, aliceView, err := crypto.DecodeAddress(aliceAddr)
        if err != nil {
                t.Fatal(err)
        }

        // Create a stealth output for Alice
        so, err := crypto.CreateStealthOutput(aliceSpend, aliceView)
        if err != nil {
                t.Fatal(err)
        }

        blind, err := crypto.NewBlindFactor()
        if err != nil {
                t.Fatal(err)
        }
        commit, err := crypto.Commit(supply, blind)
        if err != nil {
                t.Fatal(err)
        }
        encAmount := core.EncryptAmount(supply, &so.HsScalar)
        proof, err := crypto.ProveRange(supply, blind)
        if err != nil {
                t.Fatal(err)
        }

        coinbase := core.Transaction{
                Version: 1,
                Outputs: []core.Output{{
                        OneTimePub:   so.OneTimePub,
                        TxPubKey:     so.TxPubKey,
                        AmountCommit: commit,
                        EncAmount:    encAmount,
                }},
                RangeProofs: []*crypto.RangeProof{proof},
        }

        txs := []core.Transaction{coinbase}
        hdr := core.BlockHeader{
                Height:       0,
                Timestamp:    1_000_000_000,
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(txs),
        }
        if err := hdr.Sign(validatorPriv); err != nil {
                t.Fatal(err)
        }

        genesis := &core.Block{Header: hdr, Txs: txs}
        chain := core.NewChain()
        if err := chain.SetGenesis(genesis); err != nil {
                t.Fatal(err)
        }
        return chain, genesis, blind
}

// TestTxBuilder_AliceToBob exercises the full RingCT transaction lifecycle.
func TestTxBuilder_AliceToBob(t *testing.T) {
        const supply = 1_000_000_000 // 1 APRO = 1_000_000_000 nAPR

        // Generate Alice and Bob wallets
        aliceKeys, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatal(err)
        }
        bobKeys, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatal(err)
        }

        net := crypto.TestnetByte
        aliceAddr := crypto.AddressFromKeys(net, aliceKeys)
        bobAddr := crypto.AddressFromKeys(net, bobKeys)

        // Create a chain with Alice's coinbase UTXO
        chain, _, inputBlind := makeGenesisWithCoinbase(t, aliceKeys, supply)

        // Alice scans her chain for owned outputs
        scanner := core.NewWalletScanner(
                aliceKeys.View.Private,
                aliceKeys.Spend.Public,
                aliceKeys.View.Public,
                net,
        )
        ownedByAlice := scanner.ScanChain(chain, 0, chain.Height())
        if len(ownedByAlice) == 0 {
                t.Fatal("Alice found no outputs in her coinbase block")
        }
        ownedByAlice[0].Blind = inputBlind
        t.Logf("Alice owns %d UTXOs, total balance: %d base units",
                len(ownedByAlice), core.Balance(ownedByAlice))

        // Verify Alice's balance
        aliceBalance := core.Balance(ownedByAlice)
        if aliceBalance != supply {
                t.Fatalf("Alice balance = %d, want %d", aliceBalance, supply)
        }

        // Build a transaction: Alice → Bob (30_000_000 base units, change back to Alice)
        sendAmount := uint64(30_000_000)
        builder := core.NewTxBuilder(
                aliceKeys.Spend.Private,
                aliceKeys.View.Private,
                aliceKeys.Spend.Public,
                ownedByAlice,
                1, // 1 nAPR/byte fee rate
        )

        result, err := builder.Build(sendAmount, bobAddr, aliceAddr)
        if err != nil {
                t.Fatalf("Build failed: %v", err)
        }

        t.Logf("Built tx: %d inputs, %d outputs, fee=%d, change=%d",
                result.InputCount, result.OutputCount, result.TotalFee, result.ChangeAmount)

        // Verify change is correct: supply − sendAmount − fee
        expectedChange := supply - sendAmount - result.TotalFee
        if result.ChangeAmount != expectedChange {
                t.Errorf("change = %d, want %d", result.ChangeAmount, expectedChange)
        }

        // ── Structural validation ─────────────────────────────────────────────────
        tx := result.Tx
        if err := tx.Validate(); err != nil {
                t.Fatalf("tx.Validate failed: %v", err)
        }
        if len(tx.Signatures) != len(tx.Inputs) {
                t.Errorf("signature count mismatch: %d sigs for %d inputs",
                        len(tx.Signatures), len(tx.Inputs))
        }
        for i, sig := range tx.Signatures {
                if sig == nil {
                        t.Errorf("nil signature at input %d", i)
                }
        }
        for i, rp := range tx.RangeProofs {
                if rp == nil {
                        t.Errorf("nil range proof at output %d", i)
                }
        }

        // ── Bob scans a hypothetical block containing Alice's tx ─────────────────
        // Wrap the tx in a block so the scanner can find Bob's output.
        _, genesisBlock := chain.Tip().Header, chain.Tip()
        _ = genesisBlock

        validatorPriv, validatorPub, _ := crypto.GenerateValidatorKey()
        prevHash := chain.Tip().Hash()
        txs := []core.Transaction{tx}
        hdr := core.BlockHeader{
                Height:       1,
                PrevHash:     prevHash,
                Timestamp:    1_000_001_000,
                ValidatorPub: validatorPub,
                MerkleRoot:   core.MerkleRoot(txs),
        }
        _ = hdr.Sign(validatorPriv)
        block1 := &core.Block{Header: hdr, Txs: txs}

        bobScanner := core.NewWalletScanner(
                bobKeys.View.Private,
                bobKeys.Spend.Public,
                bobKeys.View.Public,
                net,
        )
        bobOutputs := bobScanner.ScanBlock(block1)
        if len(bobOutputs) == 0 {
                t.Fatal("Bob found no outputs — stealth scan failed")
        }
        bobReceived := core.Balance(bobOutputs)
        if bobReceived != sendAmount {
                t.Errorf("Bob received %d base units, want %d", bobReceived, sendAmount)
        }
        t.Logf("Bob received: %d base units", bobReceived)

        // Alice should also find her change output in block1
        aliceNewOutputs := scanner.ScanBlock(block1)
        if result.ChangeAmount > 0 && len(aliceNewOutputs) == 0 {
                t.Error("Alice found no change output")
        }
        aliceChange := core.Balance(aliceNewOutputs)
        if result.ChangeAmount > 0 && aliceChange != result.ChangeAmount {
                t.Errorf("Alice change = %d, want %d", aliceChange, result.ChangeAmount)
        }
        t.Logf("Alice change: %d base units", aliceChange)
}

// TestTxBuilder_InsufficientFunds verifies that Build returns an error when
// the sender does not have enough funds.
func TestTxBuilder_InsufficientFunds(t *testing.T) {
        aliceKeys, _ := crypto.GenerateWalletKeys()
        bobKeys, _ := crypto.GenerateWalletKeys()
        net := crypto.TestnetByte
        bobAddr := crypto.AddressFromKeys(net, bobKeys)
        aliceAddr := crypto.AddressFromKeys(net, aliceKeys)

        chain, _, _ := makeGenesisWithCoinbase(t, aliceKeys, 1000)
        scanner := core.NewWalletScanner(
                aliceKeys.View.Private, aliceKeys.Spend.Public, aliceKeys.View.Public, net,
        )
        owned := scanner.ScanChain(chain, 0, chain.Height())

        builder := core.NewTxBuilder(
                aliceKeys.Spend.Private, aliceKeys.View.Private,
                aliceKeys.Spend.Public, owned, 1,
        )
        _, err := builder.Build(999_999_999, bobAddr, aliceAddr)
        if err == nil {
                t.Error("expected insufficient funds error, got nil")
        } else {
                t.Logf("Got expected error: %v", err)
        }
}

// TestFeeEstimateMatchesTransactionSize asserts that ExportedEstimateFee(1,2,r)/r
// equals the byte count returned by Transaction.Size() for a canonical
// 1-input 2-output transaction.  The test fails immediately if the constants
// in tx_builder.go (txBytesPerInput / txBytesPerOutput / txOverheadBytes)
// drift from the formula in transaction.go, which is the root cause of every
// "fee too low" rejection on mainnet.
func TestFeeEstimateMatchesTransactionSize(t *testing.T) {
	const feePerByte = 200

	// Build a canonical transaction shell.  We only care about the shapes
	// (slice lengths / ring size) — actual crypto values are zeroed out, which
	// is fine because Transaction.Size() never dereferences field contents.
	ring := make([]crypto.RingMember, crypto.RingSize)
	tx := core.Transaction{
Version: core.TxVersionCommitmentBinding,
		Inputs: []core.RingInput{
			{Ring: ring},
		},
		Outputs: []core.Output{
			{}, // payment output
			{}, // change output
		},
Signatures: []*crypto.MLSAGSignature{{
BlindSS: make([][32]byte, crypto.RingSize),
ValueSS: make([][32]byte, crypto.RingSize),
}},
		RangeProofs: []*crypto.RangeProof{{}, {}},
	}

	// ExportedEstimateFee(1, 2, feePerByte) / feePerByte gives the estimated
	// byte count.  Transaction.Size() gives the actual byte count for the same
	// shape.  They must be equal.
	wantBytes := int(core.ExportedEstimateFee(1, 2, feePerByte) / feePerByte)
	gotBytes := tx.Size()

	if gotBytes != wantBytes {
		t.Errorf(
			"constant drift detected: Transaction.Size() = %d bytes, "+
				"ExportedEstimateFee(1,2,%d)/%d = %d bytes; "+
				"update txBytesPerInput/txBytesPerOutput/txOverheadBytes in tx_builder.go "+
				"to match the Size() formula in transaction.go",
			gotBytes, feePerByte, feePerByte, wantBytes,
		)
	}
}

// TestTxBuilder_ZeroAmount verifies that Build rejects a zero-amount transaction.
func TestTxBuilder_ZeroAmount(t *testing.T) {
        aliceKeys, _ := crypto.GenerateWalletKeys()
        bobKeys, _ := crypto.GenerateWalletKeys()
        net := crypto.TestnetByte
        bobAddr := crypto.AddressFromKeys(net, bobKeys)
        aliceAddr := crypto.AddressFromKeys(net, aliceKeys)

        builder := core.NewTxBuilder(
                aliceKeys.Spend.Private, aliceKeys.View.Private,
                aliceKeys.Spend.Public, nil, 1,
        )
        _, err := builder.Build(0, bobAddr, aliceAddr)
        if err == nil {
                t.Error("expected error for zero amount")
        }
}

// TestTxBuilder_FallbackDecoyCount_SparseUTXOSet verifies that when a live
// UTXOSet is attached via WithDecoySet but it contains fewer spent decoys than
// RingSize-1, BuildResult.FallbackDecoyCount is positive and
// BuildResult.RealDecoyCount equals the number of available real decoys.
//
// Setup: Alice has one coinbase UTXO worth 1 APRO.  A UTXOSet is pre-loaded
// with 3 spent-decoy entries (via AddSpentDecoyForTest).  Alice sends 30_000_000
// base units to Bob.  Because only 1 input is selected and it needs RingSize-1 = 15
// decoy slots, exactly 15-3 = 12 slots must fall back to Phase 1 random keys.
func TestTxBuilder_FallbackDecoyCount_SparseUTXOSet(t *testing.T) {
        const supply = 1_000_000_000 // 1 APRO

        aliceKeys, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatal(err)
        }
        bobKeys, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatal(err)
        }
        net := crypto.TestnetByte
        aliceAddr := crypto.AddressFromKeys(net, aliceKeys)
        bobAddr := crypto.AddressFromKeys(net, bobKeys)

        // Create a single-block chain with Alice's coinbase UTXO.
        chain, _, inputBlind := makeGenesisWithCoinbase(t, aliceKeys, supply)
        scanner := core.NewWalletScanner(
                aliceKeys.View.Private, aliceKeys.Spend.Public, aliceKeys.View.Public, net,
        )
        ownedByAlice := scanner.ScanChain(chain, 0, chain.Height())
        if len(ownedByAlice) == 0 {
                t.Fatal("Alice found no UTXOs")
        }
        ownedByAlice[0].Blind = inputBlind

        // Build a UTXOSet with only 3 spent decoys — far fewer than RingSize-1 = 15.
        const nSpentDecoys = 3
        utxos := core.NewUTXOSet()
        for i := 0; i < nSpentDecoys; i++ {
                dk, err := crypto.GenerateWalletKeys()
                if err != nil {
                        t.Fatalf("GenerateWalletKeys decoy %d: %v", i, err)
                }
                utxos.AddSpentDecoyForTest(&core.UTXO{OneTimePub: dk.Spend.Public})
        }

        builder := core.NewTxBuilder(
                aliceKeys.Spend.Private, aliceKeys.View.Private,
                aliceKeys.Spend.Public, ownedByAlice, 1,
        ).WithDecoySet(utxos)

        result, err := builder.Build(30_000_000, bobAddr, aliceAddr)
        if err != nil {
                t.Fatalf("Build failed: %v", err)
        }

        // 1 input × (RingSize-1) = 15 decoy slots total.
        // Only nSpentDecoys=3 could be filled with real on-chain UTXOs.
        wantRealDecoys := nSpentDecoys
        wantFallback := (crypto.RingSize - 1) - nSpentDecoys

        if result.RealDecoyCount != wantRealDecoys {
                t.Errorf("RealDecoyCount = %d, want %d", result.RealDecoyCount, wantRealDecoys)
        }
        if result.FallbackDecoyCount != wantFallback {
                t.Errorf("FallbackDecoyCount = %d, want %d", result.FallbackDecoyCount, wantFallback)
        }
        if result.FallbackDecoyCount == 0 {
                t.Error("FallbackDecoyCount must be > 0 when the decoy pool is sparse")
        }
        t.Logf("RealDecoyCount=%d FallbackDecoyCount=%d (RingSize-1=%d)",
                result.RealDecoyCount, result.FallbackDecoyCount, crypto.RingSize-1)
}
