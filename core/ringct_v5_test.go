package core_test

import (
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestRingCTV5BuilderVerifierAndState(t *testing.T) {
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
	owned[0].Blind = blind

	set := core.NewUTXOSet()
	set.Add(&core.UTXO{OneTimePub: owned[0].OneTimePub, AmountCommit: owned[0].AmountCommit})
	decoys := make([]core.DecoyUTXO, crypto.RingSize-1)
	for i := range decoys {
		keys, e := crypto.GenerateWalletKeys()
		if e != nil {
			t.Fatal(e)
		}
		b, e := crypto.NewBlindFactor()
		if e != nil {
			t.Fatal(e)
		}
		c, e := crypto.Commit(uint64(i+1), b)
		if e != nil {
			t.Fatal(e)
		}
		decoys[i] = core.DecoyUTXO{OneTimePub: keys.Spend.Public, AmountCommit: c}
		set.Add(&core.UTXO{OneTimePub: keys.Spend.Public, AmountCommit: c})
	}
	aliceAddr := crypto.AddressFromKeys(crypto.TestnetByte, alice)
	bobAddr := crypto.AddressFromKeys(crypto.TestnetByte, bob)
	result, err := core.NewTxBuilder(alice.Spend.Private, alice.View.Private, alice.Spend.Public, owned, 1).
		WithVersion(core.TxVersionCLSAG).WithDecoys(decoys).Build(1_000_000, bobAddr, aliceAddr)
	if err != nil {
		t.Fatalf("build v5: %v", err)
	}
	tx := result.Tx
	if tx.Inputs[0].RealIndex != 0 || len(tx.Signatures) != 0 || len(tx.CLSAGSignatures) != 1 {
		t.Fatal("v5 leaked a legacy signature or real index")
	}
	if err := core.NewTxVerifier(set).VerifyTx(&tx); err != nil {
		t.Fatalf("verify v5: %v", err)
	}
	data, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip core.Transaction
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if len(roundTrip.CLSAGSignatures) != 1 || len(roundTrip.Inputs[0].RingCommitments) != crypto.RingSize {
		t.Fatal("v5 JSON fields were lost")
	}
	tamperedCommit := tx
	tamperedCommit.Inputs = append([]core.RingInput(nil), tx.Inputs...)
	tamperedCommit.Inputs[0].RingCommitments = append([]crypto.Commitment(nil), tx.Inputs[0].RingCommitments...)
	tamperedCommit.Inputs[0].RingCommitments[1] = tx.Inputs[0].RingCommitments[0]
	if err := core.NewTxVerifier(set).VerifyTx(&tamperedCommit); err == nil {
		t.Fatal("tampered ring commitment accepted")
	}
	tamperedPseudo := tx
	tamperedPseudo.Inputs = append([]core.RingInput(nil), tx.Inputs...)
	tamperedPseudo.Inputs[0].PseudoOut = tx.FeeCommit
	if err := core.NewTxVerifier(set).VerifyTx(&tamperedPseudo); err == nil {
		t.Fatal("tampered pseudo-output accepted")
	}
	block := &core.Block{Header: core.BlockHeader{Height: 1}, Txs: []core.Transaction{tx}}
	if err := set.ApplyBlock(block); err != nil {
		t.Fatalf("apply v5: %v", err)
	}
	if !set.IsSpent(tx.Inputs[0].KeyImage) {
		t.Fatal("v5 key image was not marked spent")
	}
	for _, member := range tx.Inputs[0].Ring {
		if set.GetByPubKey(member) == nil {
			t.Fatal("v5 state transition removed a ring member")
		}
	}
}

func TestRingCTV5ActivationBoundary(t *testing.T) {
	v4 := &core.Transaction{Version: core.TxVersionCommitmentBinding, Inputs: []core.RingInput{{}}}
	v5 := &core.Transaction{Version: core.TxVersionCLSAG, Inputs: []core.RingInput{{}}}
	const activation = uint64(100)
	if err := core.ValidateTxVersionAtHeight(v5, activation-1, 1, activation); err == nil {
		t.Fatal("v5 accepted before activation")
	}
	if err := core.ValidateTxVersionAtHeight(v5, activation, 1, activation); err != nil {
		t.Fatalf("v5 rejected at activation: %v", err)
	}
	if err := core.ValidateTxVersionAtHeight(v4, activation-1, 1, activation); err != nil {
		t.Fatalf("v4 rejected before v5 activation: %v", err)
	}
	if err := core.ValidateTxVersionAtHeight(v4, activation, 1, activation); err == nil {
		t.Fatal("v4 accepted after v5 activation")
	}
}
