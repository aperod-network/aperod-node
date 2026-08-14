package core_test

// f049_stake_test.go — tests for the F-049 security fix:
//   1. DeterministicMintBlindV2 produces distinct output from V1 and unique per height.
//   2. V3 stake deposit (237 bytes) with valid one-time-key ownership proof is accepted
//      for transparent/mint outputs (TxPubKey==zero).
//   3. V3 stake deposit with an incorrect ownership signature is rejected.
//   4. V2 stake deposit (173 bytes) with TxPubKey==zero is still rejected (interim guard preserved).

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// buildMintUTXOFixture creates a synthetic transparent mint UTXO and returns all
// fields needed to build both V2 and V3 stake deposits for it.
func buildMintUTXOFixture(t *testing.T) (
	priv crypto.ValidatorPrivKey,
	pub crypto.ValidatorPubKey,
	spendPub crypto.Point32,
	burnTxHash crypto.Hash32,
	burnOutIdx uint32,
	burnBlind crypto.BlindFactor,
	commit crypto.Commitment,
	utxos *core.UTXOSet,
) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	copy(spendPub[:], []byte(pub))

	const amount uint64 = core.MinStakeNAPR

	burnBlind, err = crypto.DeterministicMintBlind(spendPub, amount)
	if err != nil {
		t.Fatalf("DeterministicMintBlind: %v", err)
	}
	commit, err = crypto.Commit(amount, burnBlind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	for i := range burnTxHash {
		burnTxHash[i] = 0xAA
	}
	burnOutIdx = 0

	utxos = core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       burnTxHash,
		OutputIndex:  burnOutIdx,
		OneTimePub:   spendPub, // TxPubKey not set → zero (transparent/mint output)
		AmountCommit: commit,
	})
	return
}

// TestF049_DeterministicMintBlindV2_DiffersFromV1 verifies that V2 produces a
// different blinding factor than V1 for the same (spendPub, amount) pair.
func TestF049_DeterministicMintBlindV2_DiffersFromV1(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	var spendPub crypto.Point32
	copy(spendPub[:], keys.Spend.Public[:])
	const amount uint64 = 1_000_000_000
	const height uint64 = 42

	v1, err := crypto.DeterministicMintBlind(spendPub, amount)
	if err != nil {
		t.Fatalf("DeterministicMintBlind: %v", err)
	}
	v2, err := crypto.DeterministicMintBlindV2(spendPub, amount, height)
	if err != nil {
		t.Fatalf("DeterministicMintBlindV2: %v", err)
	}
	if v1 == v2 {
		t.Error("DeterministicMintBlindV2 produced same blind as V1 — must differ")
	}
}

// TestF049_DeterministicMintBlindV2_UniquePerHeight verifies that V2 produces a
// different blinding factor for each distinct block height.
func TestF049_DeterministicMintBlindV2_UniquePerHeight(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	var spendPub crypto.Point32
	copy(spendPub[:], keys.Spend.Public[:])
	const amount uint64 = 1_000_000_000

	b1, err := crypto.DeterministicMintBlindV2(spendPub, amount, 1)
	if err != nil {
		t.Fatalf("height 1: %v", err)
	}
	b2, err := crypto.DeterministicMintBlindV2(spendPub, amount, 2)
	if err != nil {
		t.Fatalf("height 2: %v", err)
	}
	b1000, err := crypto.DeterministicMintBlindV2(spendPub, amount, 1000)
	if err != nil {
		t.Fatalf("height 1000: %v", err)
	}

	if b1 == b2 {
		t.Error("height 1 and height 2 produced the same blind — must differ")
	}
	if b1 == b1000 {
		t.Error("height 1 and height 1000 produced the same blind — must differ")
	}
	if b2 == b1000 {
		t.Error("height 2 and height 1000 produced the same blind — must differ")
	}
}

// TestF049_V3StakeDeposit_MintUTXO_Accepted verifies that a V3 stake deposit
// (237-byte payload) for a transparent/mint output (TxPubKey==zero) is accepted
// when the one-time-key ownership proof is correct.
func TestF049_V3StakeDeposit_MintUTXO_Accepted(t *testing.T) {
	priv, pub, spendPub, burnTxHash, burnOutIdx, burnBlind, _, utxos := buildMintUTXOFixture(t)

	reg := core.NewValidatorRegistry()
	reg.SetUTXOSet(utxos)
	reg.InitFromGenesis([]crypto.ValidatorPubKey{}, 0) // no genesis validators

	// Build valid V3 payload.
	const amount = core.MinStakeNAPR
	depositMsg := core.StakeSignMsgV2(core.StakeDeposit, pub, amount, burnTxHash, burnOutIdx)
	sig, err := priv.Sign(depositMsg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	ownershipMsg := core.StakeOwnershipSignMsg(burnTxHash, burnOutIdx)
	oneTimeSig, err := priv.Sign(ownershipMsg)
	if err != nil {
		t.Fatalf("Sign (ownership): %v", err)
	}
	extra, err := core.EncodeStakeExtraV3(
		core.StakeDeposit, pub, amount, sig,
		burnTxHash, burnOutIdx, burnBlind, oneTimeSig,
	)
	if err != nil {
		t.Fatalf("EncodeStakeExtraV3: %v", err)
	}
	_ = spendPub // used indirectly via pub/priv derivation

	tx := core.Transaction{Version: core.TxVersionStake, Extra: extra}
	if err := reg.ProcessStakeTx(tx, 1); err != nil {
		t.Fatalf("ProcessStakeTx V3 with valid ownership proof rejected: %v", err)
	}

	entry, ok := reg.GetEntry(pub)
	if !ok {
		t.Fatal("validator not registered after V3 deposit")
	}
	if entry.StakeNAPR != amount {
		t.Errorf("StakeNAPR = %d, want %d", entry.StakeNAPR, amount)
	}

	// Burn UTXO must be consumed.
	if utxos.Get(burnTxHash, burnOutIdx) != nil {
		t.Error("burn UTXO still in active set after V3 deposit — MarkStaked not called")
	}
}

// TestF049_V3StakeDeposit_WrongOwnershipSig_Rejected verifies that a V3 stake
// deposit whose one-time-key ownership signature is from a different key is rejected.
func TestF049_V3StakeDeposit_WrongOwnershipSig_Rejected(t *testing.T) {
	priv, pub, _, burnTxHash, burnOutIdx, burnBlind, _, utxos := buildMintUTXOFixture(t)

	// Attacker generates their own key — cannot produce a valid ownership sig.
	attackerPriv, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey (attacker): %v", err)
	}

	reg := core.NewValidatorRegistry()
	reg.SetUTXOSet(utxos)

	const amount = core.MinStakeNAPR
	depositMsg := core.StakeSignMsgV2(core.StakeDeposit, pub, amount, burnTxHash, burnOutIdx)
	// Legitimate self-signature from the staker.
	sig, err := priv.Sign(depositMsg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Attacker's ownership signature (wrong key).
	ownershipMsg := core.StakeOwnershipSignMsg(burnTxHash, burnOutIdx)
	wrongSig, err := attackerPriv.Sign(ownershipMsg)
	if err != nil {
		t.Fatalf("Sign (attacker): %v", err)
	}
	extra, err := core.EncodeStakeExtraV3(
		core.StakeDeposit, pub, amount, sig,
		burnTxHash, burnOutIdx, burnBlind, wrongSig,
	)
	if err != nil {
		t.Fatalf("EncodeStakeExtraV3: %v", err)
	}
	tx := core.Transaction{Version: core.TxVersionStake, Extra: extra}
	err = reg.ProcessStakeTx(tx, 1)
	if err == nil {
		t.Fatal("ProcessStakeTx should reject V3 deposit with wrong ownership sig, got nil")
	}
	if !strings.Contains(err.Error(), "ownership proof") && !strings.Contains(err.Error(), "F-049") {
		t.Errorf("error should mention ownership proof or F-049, got: %v", err)
	}

	// UTXO must be untouched (not burned on failed attempt).
	if utxos.Get(burnTxHash, burnOutIdx) == nil {
		t.Error("burn UTXO was consumed despite invalid ownership sig — MarkStaked must be gated after proof")
	}
}

// TestF049_V2StakeDeposit_MintUTXO_StillRejected verifies that the interim F-008
// guard is still in effect for V2 deposits: transparent/mint outputs (TxPubKey==zero)
// must be rejected with V2 payload — callers must upgrade to V3.
func TestF049_V2StakeDeposit_MintUTXO_StillRejected(t *testing.T) {
	priv, pub, _, burnTxHash, burnOutIdx, burnBlind, _, utxos := buildMintUTXOFixture(t)

	reg := core.NewValidatorRegistry()
	reg.SetUTXOSet(utxos)

	const amount = core.MinStakeNAPR
	depositMsg := core.StakeSignMsgV2(core.StakeDeposit, pub, amount, burnTxHash, burnOutIdx)
	sig, err := priv.Sign(depositMsg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	extra, err := core.EncodeStakeExtraV2(
		core.StakeDeposit, pub, amount, sig,
		burnTxHash, burnOutIdx, burnBlind,
	)
	if err != nil {
		t.Fatalf("EncodeStakeExtraV2: %v", err)
	}
	tx := core.Transaction{Version: core.TxVersionStake, Extra: extra}
	err = reg.ProcessStakeTx(tx, 1)
	if err == nil {
		t.Fatal("ProcessStakeTx should reject V2 deposit for transparent/mint output, got nil")
	}
	if !strings.Contains(err.Error(), "v3") && !strings.Contains(err.Error(), "F-049") {
		t.Errorf("error should mention v3 or F-049, got: %v", err)
	}
}
