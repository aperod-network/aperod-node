package core_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestTxBuilderBuildsSignedAVMCLSAGTransaction(t *testing.T) {
	alice, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	const supply = uint64(50_000_000)
	chain, _, blind := makeGenesisWithCoinbase(t, alice, supply)
	scanner := core.NewWalletScanner(
		alice.View.Private,
		alice.Spend.Public,
		alice.View.Public,
		crypto.TestnetByte,
	)
	owned := scanner.ScanChain(chain, 0, chain.Height())
	owned[0].Blind = blind

	set := core.NewUTXOSet()
	set.Add(&core.UTXO{OneTimePub: owned[0].OneTimePub, AmountCommit: owned[0].AmountCommit})
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
		commitment, commitErr := crypto.Commit(uint64(i+1), decoyBlind)
		if commitErr != nil {
			t.Fatal(commitErr)
		}
		decoys[i] = core.DecoyUTXO{OneTimePub: keys.Spend.Public, AmountCommit: commitment}
		set.Add(&core.UTXO{OneTimePub: keys.Spend.Public, AmountCommit: commitment})
	}

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := core.AVMPayload{
		Action:     core.AVMExecuteContract,
		ContractID: crypto.HashBytes([]byte("contract")),
		Entry:      "main",
		Calldata:   []byte("request"),
		GasLimit:   25_000,
		Nonce:      1,
		AccessList: []core.AVMAccess{{Key: []byte("counter"), Write: true}},
	}
	copy(payload.Signer[:], public)
	signingHash := payload.SigningHash()
	copy(payload.Signature[:], ed25519.Sign(private, signingHash[:]))

	aliceAddr := crypto.AddressFromKeys(crypto.TestnetByte, alice)
	bobAddr := crypto.AddressFromKeys(crypto.TestnetByte, bob)
	result, err := core.NewTxBuilder(
		alice.Spend.Private,
		alice.View.Private,
		alice.Spend.Public,
		owned,
		core.InitialBaseFeePerByte,
	).WithAVM(payload).WithDecoys(decoys).Build(1_000_000, bobAddr, aliceAddr)
	if err != nil {
		t.Fatalf("build AVM transaction: %v", err)
	}
	tx := result.Tx
	if tx.Version != core.TxVersionAVM || tx.AVM == nil || !tx.UsesCLSAG() {
		t.Fatalf("builder did not produce v6 AVM/CLSAG transaction")
	}
	if len(tx.Signatures) != 0 || len(tx.CLSAGSignatures) != len(tx.Inputs) {
		t.Fatalf("builder used wrong signature scheme")
	}
	gasFee, err := core.AVMGasFee(payload.GasLimit)
	if err != nil {
		t.Fatal(err)
	}
	if minimum := tx.MinFeeAt(core.InitialBaseFeePerByte) + gasFee; tx.Fee < minimum {
		t.Fatalf("fee %d does not cover minimum %d", tx.Fee, minimum)
	}
	if err := tx.Validate(); err != nil {
		t.Fatalf("built transaction is structurally invalid: %v", err)
	}
	if err := core.NewTxVerifier(set).VerifyTx(&tx); err != nil {
		t.Fatalf("built transaction fails CLSAG verification: %v", err)
	}

	tampered := tx
	tamperedPayload := *tx.AVM
	tamperedPayload.Calldata = []byte("tampered")
	tampered.AVM = &tamperedPayload
	if err := tampered.Validate(); err == nil {
		t.Fatal("tampered AVM payload signature accepted")
	}
}
