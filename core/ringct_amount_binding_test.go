package core_test

import (
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestRingCTAmountBindingBuilderVerifierAndWalletRecovery(t *testing.T) {
	alice, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	const supply = uint64(10_000_000)
	chain, _, blind := makeGenesisWithCoinbase(t, alice, supply)
	scanner := core.NewWalletScanner(alice.View.Private, alice.Spend.Public, alice.View.Public, crypto.TestnetByte)
	owned := scanner.ScanChain(chain, 0, chain.Height())
	if len(owned) != 1 {
		t.Fatalf("genesis owned outputs=%d, want 1", len(owned))
	}
	owned[0].Blind = blind

	set := core.NewUTXOSet()
	set.Add(&owned[0].UTXO)
	decoys := make([]core.DecoyUTXO, crypto.RingSize-1)
	for i := range decoys {
		keys, keyErr := crypto.GenerateWalletKeys()
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		decoyBlind, blindErr := crypto.NewBlindFactor()
		if blindErr != nil {
			t.Fatal(blindErr)
		}
		commit, commitErr := crypto.Commit(uint64(i+1), decoyBlind)
		if commitErr != nil {
			t.Fatal(commitErr)
		}
		decoys[i] = core.DecoyUTXO{OneTimePub: keys.Spend.Public, AmountCommit: commit}
		set.Add(&core.UTXO{OneTimePub: keys.Spend.Public, AmountCommit: commit})
	}

	aliceAddr := crypto.AddressFromKeys(crypto.TestnetByte, alice)
	bobAddr := crypto.AddressFromKeys(crypto.TestnetByte, bob)
	result, err := core.NewTxBuilder(alice.Spend.Private, alice.View.Private, alice.Spend.Public, owned, 1).
		WithVersion(core.TxVersionAmountBinding).
		WithDecoys(decoys).
		Build(1_000_000, bobAddr, aliceAddr)
	if err != nil {
		t.Fatalf("build amount-bound transfer: %v", err)
	}
	if err := core.NewTxVerifier(set).VerifyTx(&result.Tx); err != nil {
		t.Fatalf("verify amount-bound transfer: %v", err)
	}
	if result.Tx.Size() < core.AmountBindingExtraBytesPerOutput*len(result.Tx.Outputs) {
		t.Fatal("transaction size estimate omitted amount-binding payload")
	}
	for i := range result.Tx.Outputs {
		if result.Tx.Outputs[i].EncAmount != ([8]byte{}) {
			t.Fatalf("bound output %d still exposes a legacy EncAmount", i)
		}
	}

	bobScanner := core.NewWalletScannerWithSpendKey(
		bob.Spend.Private, bob.View.Private, bob.Spend.Public, bob.View.Public, crypto.TestnetByte,
	)
	block := &core.Block{Header: core.BlockHeader{Height: 1}, Txs: []core.Transaction{result.Tx}}
	received, err := bobScanner.ScanBlockVerified(block)
	if err != nil {
		t.Fatalf("recipient scan: %v", err)
	}
	if len(received) != 1 || received[0].Amount != 1_000_000 {
		t.Fatalf("recipient recovered %d outputs, amount=%d; want one output of 1000000",
			len(received), func() uint64 {
				if len(received) == 0 {
					return 0
				}
				return received[0].Amount
			}())
	}

	// New-format wallets never trust the legacy XOR field for value recovery.
	tampered := result.Tx
	tampered.Outputs = append([]core.Output(nil), result.Tx.Outputs...)
	tampered.Outputs[0].EncAmount[0] ^= 0x80
	received, err = bobScanner.ScanBlockVerified(&core.Block{
		Header: core.BlockHeader{Height: 1},
		Txs:    []core.Transaction{tampered},
	})
	if err != nil {
		t.Fatalf("scan with altered legacy EncAmount: %v", err)
	}
	if len(received) != 1 || received[0].Amount != 1_000_000 {
		t.Fatal("legacy EncAmount affected bound amount recovery")
	}
	if err := core.NewTxVerifier(set).VerifyTx(&tampered); err == nil {
		t.Fatal("tampering the signed legacy EncAmount field did not invalidate the transaction")
	}

	viewOnly := core.NewWalletScanner(bob.View.Private, bob.Spend.Public, bob.View.Public, crypto.TestnetByte)
	if _, err := viewOnly.ScanBlockVerified(block); err == nil {
		t.Fatal("view-only scanner claimed a bound amount without the spend key")
	}
}
