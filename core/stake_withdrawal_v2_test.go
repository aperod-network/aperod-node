// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package core

import (
	"io"
	"log/slog"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func signedWithdrawalV2(t *testing.T, priv crypto.ValidatorPrivKey, genesis crypto.Hash32, action StakeAction, amount, nonce, generation, stake, expiry uint64) Transaction {
	t.Helper()
	a := StakeWithdrawalAuthorizationV2{Action: action, PubKey: priv.Public(), Amount: amount, Genesis: genesis,
		Nonce: nonce, Generation: generation, ExpiryHeight: expiry}
	a.StakeRef = StakeReferenceV2(genesis, a.PubKey, generation, stake)
	sig, err := priv.Sign(StakeWithdrawalSignMsgV2(a))
	if err != nil {
		t.Fatal(err)
	}
	a.Signature = sig
	extra, err := EncodeStakeWithdrawalExtraV2(a)
	if err != nil {
		t.Fatal(err)
	}
	return Transaction{Version: TxVersionStake, Extra: extra}
}

func TestStakeWithdrawalV2ChainNonceExpiryRollbackAndRestart(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	genesis := crypto.HashBytes([]byte("stake-v2-chain"))
	const activation uint64 = 10
	const stake = MinStakeNAPR * 2
	const amount = MinStakeNAPR / 2
	tx := signedWithdrawalV2(t, priv, genesis, StakePartialWithdraw, amount, 1, 1, stake, 20)

	reg := NewValidatorRegistry()
	reg.InitFromGenesis([]crypto.ValidatorPubKey{pub}, stake)
	if err := reg.ConfigureStakeWithdrawalV2(genesis, activation); err != nil {
		t.Fatal(err)
	}
	if err := reg.ValidateBlockStakeTxs([]Transaction{tx}, activation); err != nil {
		t.Fatalf("valid v2 authorization rejected: %v", err)
	}
	rollback, err := reg.ApplyBlockStakeTxs([]Transaction{tx}, activation)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := reg.GetEntry(pub)
	if entry.StakeNAPR != stake-amount || entry.StakeAuthNonce != 1 ||
		len(entry.UnbondingQueue) != 1 || entry.UnbondingQueue[0].EndBlock != activation+PartialUnbondingBlocks {
		t.Fatalf("valid partial withdrawal not applied exactly: %+v", entry)
	}
	if err := reg.ValidateBlockStakeTxs([]Transaction{tx}, activation+1); err == nil {
		t.Fatal("confirmed authorization replay accepted")
	}
	rollback()
	entry, _ = reg.GetEntry(pub)
	if entry.StakeNAPR != stake || entry.StakeAuthNonce != 0 || len(entry.UnbondingQueue) != 0 {
		t.Fatalf("rollback did not restore stake authorization state: %+v", entry)
	}

	restarted := NewValidatorRegistry()
	restarted.InitFromGenesis([]crypto.ValidatorPubKey{pub}, stake)
	if err := restarted.ConfigureStakeWithdrawalV2(genesis, activation); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReplayBlockStakeTxs([]Transaction{tx}, activation); err != nil {
		t.Fatalf("restart replay rejected canonical v2 authorization: %v", err)
	}
	entry, _ = restarted.GetEntry(pub)
	if entry.StakeNAPR != stake-amount || entry.StakeAuthNonce != 1 {
		t.Fatal("restart lost nonce or partial stake transition")
	}

	expired := signedWithdrawalV2(t, priv, genesis, StakePartialWithdraw, amount, 1, 1, stake, activation-1)
	if err := reg.ValidateBlockStakeTxs([]Transaction{expired}, activation); err == nil {
		t.Fatal("expired authorization accepted")
	}
	otherGenesis := crypto.HashBytes([]byte("other-chain"))
	crossChain := signedWithdrawalV2(t, priv, otherGenesis, StakePartialWithdraw, amount, 1, 1, stake, 20)
	if err := reg.ValidateBlockStakeTxs([]Transaction{crossChain}, activation); err == nil {
		t.Fatal("cross-chain authorization accepted")
	}

	attacker, _, _ := crypto.GenerateValidatorKey()
	forged := signedWithdrawalV2(t, attacker, genesis, StakePartialWithdraw, amount, 1, 1, stake, 20)
	a, _ := DecodeStakeWithdrawalExtraV2(forged.Extra)
	a.PubKey = pub
	a.StakeRef = StakeReferenceV2(genesis, pub, 1, stake)
	forged.Extra, _ = EncodeStakeWithdrawalExtraV2(a)
	if err := reg.ValidateBlockStakeTxs([]Transaction{forged}, activation); err == nil {
		t.Fatal("forged validator authorization accepted")
	}
}

func TestStakeWithdrawalV2ActivationGenerationAndLegacyGate(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	genesis := crypto.HashBytes([]byte("stake-v2-gate"))
	const activation uint64 = 50
	const stake = MinStakeNAPR * 2
	reg := NewValidatorRegistry()
	reg.InitFromGenesis([]crypto.ValidatorPubKey{pub}, stake)
	if err := reg.ConfigureStakeWithdrawalV2(genesis, activation); err != nil {
		t.Fatal(err)
	}
	v2 := signedWithdrawalV2(t, priv, genesis, StakeWithdraw, 0, 1, 1, stake, activation+5)
	if err := reg.ValidateBlockStakeTxs([]Transaction{v2}, activation-1); err == nil {
		t.Fatal("v2 authorization accepted before activation")
	}
	msg := StakeSignMsg(StakeWithdraw, pub, 0)
	sig, _ := priv.Sign(msg)
	extra, _ := EncodeStakeExtra(StakeWithdraw, pub, 0, sig)
	legacy := Transaction{Version: TxVersionStake, Extra: extra}
	if err := reg.ValidateBlockStakeTxs([]Transaction{legacy}, activation-1); err != nil {
		t.Fatalf("legacy withdrawal rejected before activation: %v", err)
	}
	if err := reg.ValidateBlockStakeTxs([]Transaction{legacy}, activation); err == nil {
		t.Fatal("legacy withdrawal accepted after activation")
	}

	// A collateral generation change invalidates every previously signed request,
	// even when amount and nonce are unchanged.
	reg.mu.Lock()
	reg.validators[pub.Hex()].StakeGeneration++
	reg.mu.Unlock()
	if err := reg.ValidateBlockStakeTxs([]Transaction{v2}, activation); err == nil {
		t.Fatal("authorization from prior collateral generation accepted")
	}

	pool := NewMempool(DefaultMempoolConfig())
	first := signedWithdrawalV2(t, priv, genesis, StakeWithdraw, 0, 1, 2, stake, activation+5)
	second := signedWithdrawalV2(t, priv, genesis, StakeWithdraw, 0, 1, 2, stake, activation+6)
	if err := pool.Add(first); err != nil {
		t.Fatalf("valid encoded authorization rejected by mempool: %v", err)
	}
	if err := pool.Add(second); err == nil {
		t.Fatal("mempool accepted a second pending authorization for the same validator")
	}
}

func TestMempoolStakeAdmissionRejectsCanonicalReplayAndStaleRestore(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	genesis := crypto.HashBytes([]byte("stake-mempool-replay"))
	const activation uint64 = 10
	const stake = MinStakeNAPR * 2
	const amount = MinStakeNAPR / 2
	height := activation
	reg := NewValidatorRegistry()
	reg.InitFromGenesis([]crypto.ValidatorPubKey{pub}, stake)
	if err := reg.ConfigureStakeWithdrawalV2(genesis, activation); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultMempoolConfig()
	cfg.CurrentHeight = func() uint64 { return height }
	pool := NewMempool(cfg)
	pool.SetStakeAdmissionCheck(func(tx Transaction, inclusionHeight uint64) error {
		return reg.ValidateBlockStakeTxs([]Transaction{tx}, inclusionHeight)
	})
	first := signedWithdrawalV2(t, priv, genesis, StakePartialWithdraw, amount, 1, 1, stake, activation+20)
	if err := pool.Add(first); err != nil {
		t.Fatalf("valid pending withdrawal rejected: %v", err)
	}
	dir := t.TempDir()
	if err := pool.Save(dir); err != nil {
		t.Fatal(err)
	}
	height++
	if _, err := reg.ApplyBlockStakeTxs([]Transaction{first}, height); err != nil {
		t.Fatal(err)
	}
	second := signedWithdrawalV2(t, priv, genesis, StakePartialWithdraw, amount, 2, 1, stake-amount, activation+20)
	if err := pool.Add(second); err != nil {
		t.Fatalf("valid next nonce did not replace stale pending reservation: %v", err)
	}

	restarted := NewMempool(cfg)
	restarted.SetStakeAdmissionCheck(func(tx Transaction, inclusionHeight uint64) error {
		return reg.ValidateBlockStakeTxs([]Transaction{tx}, inclusionHeight)
	})
	if restored := restarted.Load(dir, slog.New(slog.NewTextHandler(io.Discard, nil))); restored != 0 {
		t.Fatalf("restored %d already-confirmed stake transactions", restored)
	}
	if err := restarted.Add(first); err == nil {
		t.Fatal("known canonical withdrawal replay entered mempool")
	}
	if err := restarted.Add(second); err != nil {
		t.Fatalf("valid nonce replacement rejected after stale replay: %v", err)
	}
}
