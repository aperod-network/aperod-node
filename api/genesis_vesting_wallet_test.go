// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api_test

import (
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// A controlled-key positive control proves that the same native output format
// supports receipt and an actual CLSAG spend of the FULL annual-size value.
// This is not a claim to possess or test the production recipient's keys.
func TestGenesisVestingAnnualSizeNativeReceiptAndSpend(t *testing.T) {
	_, amount, _ := store.GenesisVestingTranche(0)
	testNativeReserveReceiptAndSpend(t, amount)
}

func TestGenesisVestingLiquiditySizeNativeReceiptAndSpend(t *testing.T) {
	testNativeReserveReceiptAndSpend(t, store.GenesisLiquidityAmount)
}

func testNativeReserveReceiptAndSpend(t *testing.T, amount uint64) {
	owner, err := crypto.GenerateWalletKeys()
	if err != nil { t.Fatal(err) }
	address := crypto.AddressFromKeys(crypto.MainnetByte, owner)
	parent := crypto.HashBytes([]byte("annual-output-control-parent"))
	digest := crypto.HashBytes([]byte("annual-output-control-checkpoint"))
	utxos := core.NewUTXOSet()
	var controlled core.OwnedUTXO
	for i := uint64(1); i <= 20; i++ {
		to := address
		if i > 1 {
			other, err := crypto.GenerateWalletKeys()
			if err != nil { t.Fatal(err) }
			to = crypto.AddressFromKeys(crypto.MainnetByte, other)
		}
		out, err := core.BuildLPoDPayoutOutput(to, amount, i, parent, digest)
		if err != nil { t.Fatal(err) }
		tx := core.Transaction{Version: core.TxVersionLPoDPayout}
		tx.Outputs = append(tx.Outputs, out)
		b := &core.Block{Header: core.BlockHeader{Height: i}, Txs: []core.Transaction{tx}}
		utxos.ApplyBlock(b)
		if i == 1 {
			u := utxos.Get(tx.Hash(), 0)
			hs, err := crypto.ScanForOutput(owner.View.Private, owner.Spend.Public, out.TxPubKey, out.OneTimePub)
			if err != nil || hs == nil || u == nil || u.ProtocolLocked || core.DecryptAmount(out.EncAmount, hs) != amount {
				t.Fatal("annual-size output is not owned, exact and spendable")
			}
			blind, err := crypto.DeterministicPaymentBlind(*hs, amount)
			if err != nil { t.Fatal(err) }
			controlled = core.OwnedUTXO{UTXO: *u, Amount: amount, Blind: blind, HsScalar: *hs}
		}
	}
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil { t.Fatal(err) }
	spend, err := core.NewTxBuilder(owner.Spend.Private, owner.View.Private, owner.Spend.Public,
		[]core.OwnedUTXO{controlled}, core.InitialBaseFeePerByte).
		WithVersion(core.TxVersionCLSAG).WithDecoySet(utxos).
		Build(100_000_000, crypto.AddressFromKeys(crypto.MainnetByte, recipient), address)
	if err != nil { t.Fatal(err) }
	if err := core.NewTxVerifier(utxos).VerifyTx(&spend.Tx); err != nil {
		t.Fatalf("annual-size native output cannot be spent: %v", err)
	}
}