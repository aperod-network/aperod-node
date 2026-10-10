// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/wallet"
)

func TestNativeLPoDLocalDepositAndPartialFullWithdrawal(t *testing.T) {
	const mnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	keys, err := wallet.DeriveFromMnemonic(mnemonic, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	hs := crypto.ScalarFromUint64(17)
	priv, err := crypto.AddScalars(hs, keys.Keys.Spend.Private)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := crypto.ScalarMulBase(priv)
	if err != nil {
		t.Fatal(err)
	}
	u := core.OwnedUTXO{UTXO: core.UTXO{TxHash: crypto.HashBytes([]byte("source")), OneTimePub: pub},
		HsScalar: hs, Amount: 10000000001, Blind: crypto.BlindFactor(crypto.ScalarFromUint64(7))}
	r := lpodSignRequest{Mnemonic: mnemonic, Action: "deposit", Genesis: strings.Repeat("11", 32), Vault: strings.Repeat("22", 32)}
	tx, err := signLPoD(r, keys, &u)
	if err != nil {
		t.Fatal(err)
	}
	a, err := tx.LPoDPositionAction()
	if err != nil || a.Amount != u.Amount {
		t.Fatalf("deposit %v", err)
	}
	body, _ := json.Marshal(a)
	if strings.Contains(string(body), mnemonic) {
		t.Fatal("mnemonic leaked")
	}
	r = lpodSignRequest{Action: "withdraw", Genesis: r.Genesis, DepositJSON: string(body), Nonce: "0",
		PositionID: hex.EncodeToString(a.PositionID[:]), Withdraw: "123"}
	partial, err := signLPoD(r, keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := partial.LPoDPositionAction()
	if err != nil || p.WithdrawAmount != 123 || p.Nonce != 1 {
		t.Fatalf("partial %v", err)
	}
	r.Nonce = "1"
	r.Withdraw = "0"
	full, err := signLPoD(r, keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := full.LPoDPositionAction()
	if err != nil || f.WithdrawAmount != 0 || f.Nonce != 2 {
		t.Fatalf("full %v", err)
	}
	r.Genesis = strings.Repeat("33", 32)
	if _, err := signLPoD(r, keys, nil); err == nil {
		t.Fatal("foreign chain accepted")
	}
	r.Genesis = strings.Repeat("11", 32)
	r.PositionID = strings.Repeat("44", 32)
	if _, err := signLPoD(r, keys, nil); err == nil {
		t.Fatal("foreign position accepted")
	}
}

// Optional public fixture used by the Node/WASM cross-runtime smoke test. It
// derives only the standard BIP39 test vector, never a real wallet credential.
func TestNativeLPoDWASMFixture(t *testing.T) {
	path := os.Getenv("LPOD_WASM_TEST_FIXTURE")
	if path == "" {
		t.Skip("cross-runtime fixture not requested")
	}
	const mnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	keys, err := wallet.DeriveFromMnemonic(mnemonic, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	out, err := core.BuildLPoDPayoutOutput(crypto.AddressFromKeys(crypto.MainnetByte, keys.Keys), 10000000001, 1,
		crypto.HashBytes([]byte("parent")), crypto.HashBytes([]byte("digest")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := core.BuildLPoDPayoutOutput(crypto.AddressFromKeys(crypto.MainnetByte, keys.Keys), 10000000001, 2,
		crypto.HashBytes([]byte("second-parent")), crypto.HashBytes([]byte("second-digest")))
	if err != nil {
		t.Fatal(err)
	}
	// Offline-only synthetic decoys exercise the ordinary local self-transfer
	// builder; they are never presented to a live node.
	decoys := make([]map[string]string, 0, 30)
	for i := 0; i < 30; i++ {
		d, err := core.BuildLPoDPayoutOutput(
			crypto.AddressFromKeys(crypto.MainnetByte, keys.Keys), uint64(200+i), uint64(2+i),
			crypto.HashBytes([]byte{byte(i), 1}), crypto.HashBytes([]byte{byte(i), 2}),
		)
		if err != nil {
			t.Fatal(err)
		}
		decoys = append(decoys, map[string]string{
			"one_time_pub": hex.EncodeToString(d.OneTimePub[:]),
			"amount_commit": hex.EncodeToString(d.AmountCommit[:]),
		})
	}
	data, err := json.Marshal(map[string]interface{}{"mnemonic": mnemonic, "genesis": strings.Repeat("11", 32), "vault": strings.Repeat("22", 32),
		"decoys": decoys,
		"second_output": map[string]interface{}{"tx_hash": strings.Repeat("44", 32), "out_idx": 0, "block_height": 2,
			"one_time_pub": hex.EncodeToString(second.OneTimePub[:]), "tx_pub_key": hex.EncodeToString(second.TxPubKey[:]),
			"amount_commit": hex.EncodeToString(second.AmountCommit[:]), "enc_amount": hex.EncodeToString(second.EncAmount[:])},
		"output": map[string]interface{}{"tx_hash": strings.Repeat("33", 32), "out_idx": 0, "block_height": 1,
			"one_time_pub": hex.EncodeToString(out.OneTimePub[:]), "tx_pub_key": hex.EncodeToString(out.TxPubKey[:]),
			"amount_commit": hex.EncodeToString(out.AmountCommit[:]), "enc_amount": hex.EncodeToString(out.EncAmount[:])}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
