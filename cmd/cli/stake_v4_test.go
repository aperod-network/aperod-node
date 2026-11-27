package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestStakeV4NodeStatusRequiresReadyActivatedNode(t *testing.T) {
	for _, tc := range []struct {
		data string
		ok   bool
	}{
		{`{"height":41,"syncing":false,"utxo_rebuilding":false,"stake_key_image_v4_activation_height":42}`, true},
		{`{"height":40,"syncing":false,"utxo_rebuilding":false,"stake_key_image_v4_activation_height":42}`, false},
		{`{"height":41,"syncing":false,"utxo_rebuilding":false,"stake_key_image_v4_activation_height":0}`, false},
		{`{"height":41,"syncing":false,"utxo_rebuilding":false}`, false},
		{`{"height":41,"syncing":true,"utxo_rebuilding":false,"stake_key_image_v4_activation_height":42}`, false},
		{`{"height":41,"syncing":false,"utxo_rebuilding":true,"stake_key_image_v4_activation_height":42}`, false},
		{`{"height":41,"syncing":false,"stake_key_image_v4_activation_height":42}`, false},
	} {
		var status stakeV4NodeStatus
		if err := json.Unmarshal([]byte(tc.data), &status); err != nil {
			t.Fatal(err)
		}
		if err := status.check(); (err == nil) != tc.ok {
			t.Fatalf("readiness for %s: %v", tc.data, err)
		}
	}
}

func TestBuildWalletStakeV4(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	signer, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	out, err := crypto.CreateStealthOutput(keys.Spend.Public, keys.View.Public)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := crypto.ScanForOutput(keys.View.Private, keys.Spend.Public, out.TxPubKey, out.OneTimePub)
	if err != nil || hs == nil {
		t.Fatalf("scan: %v", err)
	}
	amount := uint64(core.MinStakeNAPR)
	blind, err := crypto.DeterministicPaymentBlind(*hs, amount)
	if err != nil {
		t.Fatal(err)
	}
	ref, index := crypto.Hash32{0x73}, uint32(2)
	legacy := core.EncryptAmount(amount, hs)
	for _, name := range []string{"legacy", "bound"} {
		t.Run(name, func(t *testing.T) {
			commit, err := crypto.Commit(amount, blind)
			if err != nil {
				t.Fatal(err)
			}
			source := stakeV4Source{
				TxHash: hex.EncodeToString(ref[:]), OutIdx: &index, Exists: true,
				OneTimePub: hex.EncodeToString(out.OneTimePub[:]), TxPub: hex.EncodeToString(out.TxPubKey[:]),
				EncAmount: hex.EncodeToString(legacy[:]),
			}
			if name == "bound" {
				source.Binding, commit, err = crypto.CreateAmountBinding(amount, blind, out.OneTimePub)
				if err != nil {
					t.Fatal(err)
				}
				source.Version = core.TxVersionAmountBinding
				source.EncAmount = "0000000000000000"
			}
			source.Commitment = hex.EncodeToString(commit[:])
			if _, _, err := buildWalletStakeV4(source, ref, index, amount-1, keys, signer); err == nil {
				t.Fatal("accepted a partial amount")
			}
			if _, _, err := buildWalletStakeV4(source, ref, index+1, amount, keys, signer); err == nil {
				t.Fatal("accepted a mismatched output reference")
			}
			tx, _, err := buildWalletStakeV4(source, ref, index, amount, keys, signer)
			if err != nil {
				t.Fatal(err)
			}
			utxos := core.NewUTXOSet()
			utxos.Add(&core.UTXO{TxHash: ref, OutputIndex: index, OneTimePub: out.OneTimePub, TxPubKey: out.TxPubKey, AmountCommit: commit})
			registry := core.NewValidatorRegistry()
			registry.SetUTXOSet(utxos)
			if err := registry.ConfigureStakeKeyImageV4(1); err != nil {
				t.Fatal(err)
			}
			if err := registry.ValidateBlockStakeTxs([]core.Transaction{tx}, 1); err != nil {
				t.Fatalf("registry rejected locally signed deposit: %v", err)
			}
		})
	}
}

func TestStakeV4SignerLoadsRawNodeKey(t *testing.T) {
	signer, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "validator.key")
	if err := os.WriteFile(p, signer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadStakeSigner(p)
	if err != nil || loaded.Public().Hex() != pub.Hex() {
		t.Fatalf("did not load matching node key: %v", err)
	}
	if err := os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStakeSigner(p); err == nil {
		t.Fatal("accepted publicly readable signing key")
	}
}

func TestStakeNodeURLIsLoopbackOnly(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:8545", "http://[::1]:8545/"} {
		if _, err := stakeNodeURL(raw); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://localhost:8545", "http://192.0.2.5:8545", "http://127.0.0.1:8545/admin", "http://user@127.0.0.1:8545", "http://127.0.0.1:8545/?next=other"} {
		if _, err := stakeNodeURL(raw); err == nil {
			t.Errorf("accepted unsafe URL %s", raw)
		}
	}
}
