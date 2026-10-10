package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

const (
	testInputAmount   uint64 = 200_000_000_000_000
	testPaymentAmount uint64 = 20_000_000_000_000
)

func buildPaymentFixture(t *testing.T) (*core.BuildResult, crypto.Address, crypto.Hash32) {
	t.Helper()
	sender, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	senderAddress := crypto.AddressFromKeys(crypto.MainnetByte, sender)
	parent := crypto.HashBytes([]byte("synthetic canonical LPoD parent"))
	digest := crypto.HashBytes([]byte("synthetic v3 checkpoint digest"))
	payoutOutput, err := core.BuildLPoDPayoutOutput(senderAddress, testInputAmount, 20, parent, digest)
	if err != nil {
		t.Fatalf("build synthetic pure-earnings payout output: %v", err)
	}
	payoutTx := core.Transaction{Version: core.TxVersionLPoDPayout, Outputs: []core.Output{payoutOutput}}
	inputHash := payoutTx.Hash()
	hs, err := crypto.ScanForOutput(sender.View.Private, sender.Spend.Public, payoutOutput.TxPubKey, payoutOutput.OneTimePub)
	if err != nil {
		t.Fatalf("scan synthetic payout output: %v", err)
	}
	if hs == nil {
		t.Fatal("synthetic pure-earnings output was not owned by the test wallet")
	}
	inputAmount := core.DecryptAmount(payoutOutput.EncAmount, hs)
	inputBlind, err := crypto.DeterministicPaymentBlind(*hs, inputAmount)
	if err != nil {
		t.Fatal(err)
	}
	owned := core.OwnedUTXO{
		UTXO: core.UTXO{
			TxHash:       inputHash,
			OutputIndex:  0,
			OneTimePub:   payoutOutput.OneTimePub,
			TxPubKey:     payoutOutput.TxPubKey,
			AmountCommit: payoutOutput.AmountCommit,
			EncAmount:    payoutOutput.EncAmount,
			BlockHeight:  20,
		},
		HsScalar: *hs,
		Amount:   inputAmount,
		Blind:    inputBlind,
	}

	recipientKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	recipient := crypto.AddressFromKeys(crypto.MainnetByte, recipientKeys)
	decoys := make([]core.DecoyUTXO, crypto.RingSize-1)
	for i := range decoys {
		pub, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(uint64(i + 100)))
		if err != nil {
			t.Fatal(err)
		}
		blind, err := crypto.NewBlindFactor()
		if err != nil {
			t.Fatal(err)
		}
		commit, err := crypto.Commit(uint64(i+1), blind)
		if err != nil {
			t.Fatal(err)
		}
		decoys[i] = core.DecoyUTXO{OneTimePub: pub, AmountCommit: commit}
	}
	builder := core.NewTxBuilder(
		sender.Spend.Private,
		sender.View.Private,
		sender.Spend.Public,
		[]core.OwnedUTXO{owned},
		core.InitialBaseFeePerByte,
	).WithVersion(core.TxVersionCLSAG).WithDecoys(decoys).WithPaymentRecipientProof()
	built, err := builder.Build(testPaymentAmount, recipient, senderAddress)
	if err != nil {
		t.Fatalf("Build real transaction: %v", err)
	}
	return built, recipient, inputHash
}

func TestVerifyBuiltPaymentSuccessChecksTransactionAmountFeeAndHash(t *testing.T) {
	built, recipient, sourceHash := buildPaymentFixture(t)
	encoded, err := json.Marshal(built)
	if err != nil {
		t.Fatalf("marshal build result: %v", err)
	}
	if strings.Contains(string(encoded), "paymentEphemeralSecret") || strings.Contains(string(encoded), "hasPaymentProof") {
		t.Fatal("ephemeral recipient proof material was serialized")
	}

	if built.PayOutIdx < 0 || built.PayOutIdx >= len(built.Tx.Outputs) {
		t.Fatalf("invalid payment output index %d", built.PayOutIdx)
	}
	output := built.Tx.Outputs[built.PayOutIdx]
	opened, err := crypto.Commit(testPaymentAmount, built.PayBlind)
	if err != nil {
		t.Fatalf("recompute built payment commitment: %v", err)
	}
	if opened != output.AmountCommit {
		t.Fatal("built payment commitment does not open to requested amount")
	}
	if built.Tx.Fee != built.TotalFee {
		t.Fatalf("built tx fee %d != build result fee %d", built.Tx.Fee, built.TotalFee)
	}
	if len(built.SelectedUTXOs) != 1 || built.SelectedUTXOs[0].Amount != testInputAmount ||
		built.SelectedUTXOs[0].TxHash != sourceHash || built.SelectedUTXOs[0].OutputIndex != 0 ||
		len(built.Tx.Inputs) != 1 {
		t.Fatalf("signed transaction did not spend only its selected synthetic source: selected=%d inputs=%d", len(built.SelectedUTXOs), len(built.Tx.Inputs))
	}
	if built.ChangeAmount > testInputAmount || testPaymentAmount > testInputAmount-built.ChangeAmount ||
		built.TotalFee != testInputAmount-built.ChangeAmount-testPaymentAmount {
		t.Fatalf("signed transaction does not conserve the single source: input=%d payment=%d fee=%d change=%d",
			testInputAmount, testPaymentAmount, built.TotalFee, built.ChangeAmount)
	}

	hash, err := verifyBuiltPayment(built, testPaymentAmount, recipient)
	if err != nil {
		t.Fatalf("verify built payment: %v", err)
	}
	if len(hash) != 64 {
		t.Fatalf("transaction hash was not computed from the built transaction: %q", hash)
	}
	if err := built.VerifyPaymentRecipient(recipient); err == nil {
		t.Fatal("ephemeral proof scalar remained available after verification")
	}
}

func TestVerifyBuiltPaymentRejectsTamperedValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *core.BuildResult, crypto.Address) (uint64, crypto.Address)
	}{
		{
			name: "recipient",
			mutate: func(t *testing.T, _ *core.BuildResult, _ crypto.Address) (uint64, crypto.Address) {
				other, err := crypto.GenerateWalletKeys()
				if err != nil {
					t.Fatal(err)
				}
				return testPaymentAmount, crypto.AddressFromKeys(crypto.MainnetByte, other)
			},
		},
		{
			name: "output one-time key",
			mutate: func(_ *testing.T, built *core.BuildResult, recipient crypto.Address) (uint64, crypto.Address) {
				built.Tx.Outputs[built.PayOutIdx].OneTimePub[0] ^= 1
				return testPaymentAmount, recipient
			},
		},
		{
			name: "output transaction public key",
			mutate: func(_ *testing.T, built *core.BuildResult, recipient crypto.Address) (uint64, crypto.Address) {
				built.Tx.Outputs[built.PayOutIdx].TxPubKey[0] ^= 1
				return testPaymentAmount, recipient
			},
		},
		{
			name: "amount",
			mutate: func(_ *testing.T, _ *core.BuildResult, recipient crypto.Address) (uint64, crypto.Address) {
				return testPaymentAmount + 1, recipient
			},
		},
		{
			name: "fee",
			mutate: func(_ *testing.T, built *core.BuildResult, recipient crypto.Address) (uint64, crypto.Address) {
				built.TotalFee++
				return testPaymentAmount, recipient
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			built, recipient, _ := buildPaymentFixture(t)
			amount, recipient := tt.mutate(t, built, recipient)
			if _, err := verifyBuiltPayment(built, amount, recipient); err == nil {
				t.Fatal("tampered payment was accepted")
			}
			if err := built.VerifyPaymentRecipient(recipient); err == nil {
				t.Fatal("ephemeral proof scalar was not cleared after failed verification")
			}
		})
	}
}
