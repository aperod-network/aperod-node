package core_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
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
	scanner := core.NewWalletScanner(alice.View.Private, alice.Spend.Public, alice.View.Public, crypto.MainnetByte)
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

func TestRingCTV5RejectsStakedCollateralAcrossRestartAndRollback(t *testing.T) {
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
	scanner := core.NewWalletScanner(alice.View.Private, alice.Spend.Public, alice.View.Public, crypto.MainnetByte)
	owned := scanner.ScanChain(chain, 0, chain.Height())
	if len(owned) != 1 {
		t.Fatalf("genesis owned outputs=%d, want 1", len(owned))
	}
	owned[0].Blind = blind

	dbPath := filepath.Join(t.TempDir(), "db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	set := core.NewUTXOSetWithDB(db)
	set.SetCLSAGActivationHeight(1)
	set.Add(&owned[0].UTXO)
const stakedCollateralActivationHeight = uint64(2_880_000)
verifier := core.NewTxVerifier(set)
verifier.SetStakedCollateralRingExclusionActivationHeight(stakedCollateralActivationHeight)

	decoys := make([]core.DecoyUTXO, crypto.RingSize-1)
	decoyUTXOs := make([]*core.UTXO, crypto.RingSize-1)
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
		utxo := &core.UTXO{
			TxHash:       crypto.HashBytes([]byte{byte(i + 1)}),
			OutputIndex:  0,
			OneTimePub:   keys.Spend.Public,
			AmountCommit: commit,
			BlockHeight:  1,
		}
		set.Add(utxo)
		decoys[i] = core.DecoyUTXO{OneTimePub: utxo.OneTimePub, AmountCommit: commit}
		decoyUTXOs[i] = utxo
		if err := db.PutUTXO(utxo.TxHash, utxo.OutputIndex, &store.StoredUTXO{
			TxHash: utxo.TxHash, OutputIndex: utxo.OutputIndex, OneTimePub: utxo.OneTimePub,
			AmountCommit: utxo.AmountCommit, BlockHeight: utxo.BlockHeight,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PutUTXO(owned[0].TxHash, owned[0].OutputIndex, &store.StoredUTXO{
		TxHash: owned[0].TxHash, OutputIndex: owned[0].OutputIndex,
		OneTimePub: owned[0].OneTimePub, AmountCommit: owned[0].AmountCommit,
		BlockHeight: owned[0].BlockHeight,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}

	// Preserve the existing spent-decoy semantics: a normally spent output is
	// still resolvable and remains eligible as a CLSAG ring member.
	set.Remove(decoyUTXOs[0].TxHash, decoyUTXOs[0].OutputIndex)
	set.AddSpentDecoyForTest(decoyUTXOs[0])

	aliceAddr := crypto.AddressFromKeys(crypto.MainnetByte, alice)
	bobAddr := crypto.AddressFromKeys(crypto.MainnetByte, bob)
	result, err := core.NewTxBuilder(alice.Spend.Private, alice.View.Private, alice.Spend.Public, owned, 1).
		WithVersion(core.TxVersionCLSAG).WithDecoys(decoys).
		Build(1_000_000, bobAddr, aliceAddr)
	if err != nil {
		t.Fatalf("build v5: %v", err)
	}
if err := verifier.VerifyTxAtHeight(&result.Tx, stakedCollateralActivationHeight-1); err != nil {
		t.Fatalf("historically valid transaction rejected before stake transition: %v", err)
	}
if err := verifier.VerifyTx(&result.Tx); err == nil || !strings.Contains(err.Error(), "height-aware") {
	t.Fatalf("heightless verification did not fail closed: %v", err)
}
if err := verifier.VerifyTxAtHeight(&result.Tx, 0); err == nil || !strings.Contains(err.Error(), "nonzero candidate height") {
	t.Fatalf("zero-height verification did not fail closed: %v", err)
}

	var validatorSig, ownershipSig [64]byte
	pub := crypto.ValidatorPubKey(alice.Spend.Public[:])
	stakeExtra, err := core.EncodeStakeExtraV3(core.StakeDeposit, pub, supply, validatorSig[:],
		owned[0].TxHash, owned[0].OutputIndex, blind, ownershipSig[:])
	if err != nil {
		t.Fatal(err)
	}
	stakeTx := core.Transaction{Version: core.TxVersionStake, Extra: stakeExtra}
if err := verifier.VerifyBlock(&core.Block{
	Header: core.BlockHeader{Height: stakedCollateralActivationHeight - 1},
		Txs:    []core.Transaction{result.Tx, stakeTx},
}); err != nil {
	t.Fatalf("pre-activation block rejected under the new rule: %v", err)
}
if err := verifier.VerifyBlock(&core.Block{
	Header: core.BlockHeader{Height: stakedCollateralActivationHeight},
	Txs:    []core.Transaction{result.Tx, stakeTx},
}); err == nil || !strings.Contains(err.Error(), "staked collateral") {
	t.Fatalf("same-block spend at activation was not rejected: %v", err)
	}

	// Runtime stake transition must prevent the authentic output from being
	// selected as either the hidden real member or a decoy.
	if err := set.MarkStaked(owned[0].TxHash, owned[0].OutputIndex); err != nil {
		t.Fatalf("MarkStaked: %v", err)
	}
if err := verifier.VerifyTxAtHeight(&result.Tx, stakedCollateralActivationHeight-1); err != nil {
	t.Fatalf("pre-activation mempool transaction rejected: %v", err)
}
if err := verifier.VerifyTxAtHeight(&result.Tx, stakedCollateralActivationHeight); err == nil || !strings.Contains(err.Error(), "staked collateral") {
	t.Fatalf("CLSAG transaction using staked collateral was not rejected at activation: %v", err)
	}
preActivationPoolConfig := core.DefaultMempoolConfig()
preActivationPoolConfig.CurrentHeight = func() uint64 { return stakedCollateralActivationHeight - 2 }
preActivationPoolConfig.RingCTCLSAGActivationHeight = 1
preActivationPoolConfig.BaseFeePerByte = 1
preActivationPoolConfig.Verifier = verifier
if err := core.NewMempool(preActivationPoolConfig).Add(result.Tx); err != nil {
t.Fatalf("mempool rejected transaction before activation: %v", err)
}
activePoolConfig := preActivationPoolConfig
activePoolConfig.CurrentHeight = func() uint64 { return stakedCollateralActivationHeight - 1 }
if err := core.NewMempool(activePoolConfig).Add(result.Tx); err == nil || !strings.Contains(err.Error(), "staked collateral") {
t.Fatalf("mempool did not enforce activation-height rule: %v", err)
}
if err := verifier.VerifyBlock(&core.Block{
	Header: core.BlockHeader{Height: stakedCollateralActivationHeight},
		Txs:    []core.Transaction{result.Tx},
	}); err == nil {
		t.Fatal("canonical block validation accepted a CLSAG transaction using staked collateral")
	}
	sampledBeforeRestart := set.SampleCLSAGDecoys(crypto.RingSize-1, nil)
	if len(sampledBeforeRestart) != crypto.RingSize-1 {
		t.Fatalf("staked output reduced available CLSAG decoys: got %d, want %d",
			len(sampledBeforeRestart), crypto.RingSize-1)
	}
	for _, sampled := range sampledBeforeRestart {
		if sampled.OneTimePub == owned[0].OneTimePub {
			t.Fatal("staked collateral remained eligible for CLSAG decoy sampling")
		}
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Reconstruct the normal restart state from the persistent ring index and
	// replayed canonical stake event. The retained rm/ record remains available
	// for historical compatibility; IsStaked is the consensus exclusion.
	restarted := core.NewUTXOSetWithDB(db)
	restarted.SetCLSAGActivationHeight(1)
	restarted.MarkStakedKnown(owned[0].TxHash, owned[0].OutputIndex, &core.UTXO{
		TxHash: owned[0].TxHash, OutputIndex: owned[0].OutputIndex,
		AmountCommit: owned[0].UTXO.AmountCommit,
	})
restartedVerifier := core.NewTxVerifier(restarted)
restartedVerifier.SetStakedCollateralRingExclusionActivationHeight(stakedCollateralActivationHeight)
	if got := restarted.GetRingMember(owned[0].OneTimePub); got == nil {
		t.Fatal("staked output's canonical ring record was destructively removed")
	}
if err := restartedVerifier.VerifyTxAtHeight(&result.Tx, stakedCollateralActivationHeight); err == nil || !strings.Contains(err.Error(), "staked collateral") {
		t.Fatalf("CLSAG transaction using staked collateral was not rejected after restart replay: %v", err)
	}
	sampledAfterRestart := restarted.SampleCLSAGDecoys(crypto.RingSize-1, nil)
	if len(sampledAfterRestart) != crypto.RingSize-1 {
		t.Fatalf("replayed staked output reduced available CLSAG decoys: got %d, want %d",
			len(sampledAfterRestart), crypto.RingSize-1)
	}
	for _, sampled := range sampledAfterRestart {
		if sampled.OneTimePub == owned[0].OneTimePub {
			t.Fatal("replayed staked collateral remained eligible for CLSAG decoy sampling")
		}
	}

	// Rolling back the stake transition restores the pre-stake canonical state.
	restarted.UnmarkStaked(owned[0].TxHash, owned[0].OutputIndex)
if err := restartedVerifier.VerifyTxAtHeight(&result.Tx, stakedCollateralActivationHeight); err != nil {
		t.Fatalf("transaction rejected after stake rollback: %v", err)
	}
}

func TestRingCTV5BuilderSupportsIntentionalBurn(t *testing.T) {
	alice, err := crypto.GenerateWalletKeys()
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
		keys, keyErr := crypto.GenerateWalletKeys()
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		decoyBlind, blindErr := crypto.NewBlindFactor()
		if blindErr != nil {
			t.Fatal(blindErr)
		}
		decoyCommit, commitErr := crypto.Commit(uint64(i+1), decoyBlind)
		if commitErr != nil {
			t.Fatal(commitErr)
		}
		decoys[i] = core.DecoyUTXO{OneTimePub: keys.Spend.Public, AmountCommit: decoyCommit}
		set.Add(&core.UTXO{OneTimePub: keys.Spend.Public, AmountCommit: decoyCommit})
	}

	result, err := core.NewTxBuilder(
		alice.Spend.Private,
		alice.View.Private,
		alice.Spend.Public,
		owned,
		1,
	).WithVersion(core.TxVersionCLSAG).
		WithDecoys(decoys).
		Build(1_000_000, crypto.MainnetBurnAddress(), crypto.AddressFromKeys(crypto.MainnetByte, alice))
	if err != nil {
		t.Fatalf("build v5 burn: %v", err)
	}
	if !result.Tx.IsBurnTx() {
		t.Fatal("v5 burn marker was not preserved")
	}
	if len(result.Tx.Outputs) != 1 || result.ChangeOutIdx != 0 || result.PayOutIdx != -1 {
		t.Fatalf("v5 burn outputs=%d change_index=%d pay_index=%d; want optional change only",
			len(result.Tx.Outputs), result.ChangeOutIdx, result.PayOutIdx)
	}
	if err := core.NewTxVerifier(set).VerifyTx(&result.Tx); err != nil {
		t.Fatalf("verify v5 burn: %v", err)
	}
}
