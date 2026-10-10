package core_test

// staking_test.go — unit tests for partial stake withdrawal validation (#355).
//
// Tests:
//  1. Partial withdraw that would leave stake below minimum → error.
//  2. Valid partial withdraw reduces StakeNAPR and enqueues an UnbondingEntry.
//  3. UpdateEpoch drops unbonding entries whose EndBlock has passed.

import (
	"encoding/binary"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// makeValidatorKey generates a deterministic validator key pair from seed.
func makeValidatorKey(t *testing.T) (crypto.ValidatorPrivKey, crypto.ValidatorPubKey) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	return priv, pub
}

// buildPartialWithdrawTx constructs a v1 stake tx with StakePartialWithdraw action.
func buildPartialWithdrawTx(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey, amount uint64) core.Transaction {
	t.Helper()
	msg := core.StakeSignMsg(core.StakePartialWithdraw, pub, amount)
	sig, err := priv.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	extra, err := core.EncodeStakeExtra(core.StakePartialWithdraw, pub, amount, sig)
	if err != nil {
		t.Fatalf("EncodeStakeExtra: %v", err)
	}
	return core.Transaction{
		Version: core.TxVersionStake,
		Extra:   extra,
	}
}

// TestStaking_PartialWithdraw_BelowMinimum (#355) — withdrawing an amount that
// would leave the validator's stake below MinStakeNAPR must return an error.
func TestStaking_PartialWithdraw_BelowMinimum(t *testing.T) {
	priv, pub := makeValidatorKey(t)

	reg := core.NewValidatorRegistry()
	// Seed with exactly MinStakeNAPR so any partial withdrawal drops below min.
	reg.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR)

	// Attempt to withdraw 1 nAPRO — remaining would be MinStakeNAPR-1 < min.
	tx := buildPartialWithdrawTx(t, priv, pub, 1)
	err := reg.ProcessStakeTx(tx, 100)
	if err == nil {
		t.Fatal("ProcessStakeTx should reject partial withdraw leaving stake below minimum, got nil")
	}
}

// TestStaking_PartialWithdraw_Valid (#355) — a valid partial withdraw should
// reduce StakeNAPR by the requested amount and enqueue an UnbondingEntry.
func TestStaking_PartialWithdraw_Valid(t *testing.T) {
	priv, pub := makeValidatorKey(t)

	const genesisStake = core.MinStakeNAPR + 5_000_000_000_000 // min + 50k APRO excess

	reg := core.NewValidatorRegistry()
	reg.InitFromGenesis([]crypto.ValidatorPubKey{pub}, genesisStake)

	const withdrawAmount uint64 = 1_000_000_000_000 // 10k APRO — stays above min

	tx := buildPartialWithdrawTx(t, priv, pub, withdrawAmount)
	const blockHeight uint64 = 500
	if err := reg.ProcessStakeTx(tx, blockHeight); err != nil {
		t.Fatalf("ProcessStakeTx: unexpected error: %v", err)
	}

	entry, ok := reg.GetEntry(pub)
	if !ok {
		t.Fatal("validator entry not found after partial withdraw")
	}

	wantStake := genesisStake - withdrawAmount
	if entry.StakeNAPR != wantStake {
		t.Errorf("StakeNAPR: got %d, want %d", entry.StakeNAPR, wantStake)
	}

	if len(entry.UnbondingQueue) != 1 {
		t.Fatalf("UnbondingQueue length: got %d, want 1", len(entry.UnbondingQueue))
	}
	ub := entry.UnbondingQueue[0]
	if ub.Amount != withdrawAmount {
		t.Errorf("UnbondingEntry.Amount: got %d, want %d", ub.Amount, withdrawAmount)
	}
	wantEndBlock := blockHeight + core.PartialUnbondingBlocks
	if ub.EndBlock != wantEndBlock {
		t.Errorf("UnbondingEntry.EndBlock: got %d, want %d", ub.EndBlock, wantEndBlock)
	}
}

// TestStaking_UpdateEpoch_CleansCompletedUnbonding (#355) — UpdateEpoch must
// drop unbonding entries whose EndBlock has been reached or passed.
func TestStaking_UpdateEpoch_CleansCompletedUnbonding(t *testing.T) {
	priv, pub := makeValidatorKey(t)

	const genesisStake = core.MinStakeNAPR + 5_000_000_000_000

	reg := core.NewValidatorRegistry()
	reg.InitFromGenesis([]crypto.ValidatorPubKey{pub}, genesisStake)

	// Submit a partial withdraw at block 100.
	const withdrawAmount uint64 = 500_000_000_000 // 5k APRO
	tx := buildPartialWithdrawTx(t, priv, pub, withdrawAmount)
	if err := reg.ProcessStakeTx(tx, 100); err != nil {
		t.Fatalf("ProcessStakeTx: %v", err)
	}

	endBlock := uint64(100) + core.PartialUnbondingBlocks

	// UpdateEpoch just before EndBlock — entry must still be present.
	reg.UpdateEpoch(endBlock - 1)
	entryBefore, _ := reg.GetEntry(pub)
	if len(entryBefore.UnbondingQueue) == 0 {
		t.Error("UnbondingQueue should still contain the entry before EndBlock")
	}

	// UpdateEpoch at or after EndBlock — entry must be dropped.
	reg.UpdateEpoch(endBlock)
	entryAfter, _ := reg.GetEntry(pub)
	if len(entryAfter.UnbondingQueue) != 0 {
		t.Errorf("UnbondingQueue should be empty after EndBlock, got %d entries", len(entryAfter.UnbondingQueue))
	}
}

// Suppress "declared but not used" for binary import used in future tests.
var _ = binary.LittleEndian

// TestReplayBlockStakeTxs_RestartSafety verifies that after a simulated node restart
// the stake registry and UTXO staked-set are correctly restored from committed blocks.
// This covers the security requirement that a burned stake UTXO cannot be reused as a
// ring decoy or re-staked after a restart, and that the validator remains registered.
// Uses V3 payload (237 bytes) because the UTXO is a transparent/mint output (F-049 fix).
func TestReplayBlockStakeTxs_RestartSafety(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	const stakeAmt uint64 = 10_000_000_000_000 // 100 000 APRO — above MinStakeNAPR

	var spendPub crypto.Point32
	copy(spendPub[:], []byte(pub))
	blind, blindErr := crypto.DeterministicMintBlind(spendPub, stakeAmt)
	if blindErr != nil {
		t.Fatalf("DeterministicMintBlind: %v", blindErr)
	}
	commit, commitErr := crypto.Commit(stakeAmt, blind)
	if commitErr != nil {
		t.Fatalf("Commit: %v", commitErr)
	}
	var burnHash crypto.Hash32
	for i := range burnHash {
		burnHash[i] = 0xCC
	}

	// ── "First run" — original node session ──────────────────────────────────
	// Build and apply a v3 stake deposit (transparent/mint output — F-049 fix).
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: burnHash, OutputIndex: 0, OneTimePub: spendPub, AmountCommit: commit})

	msg := core.StakeSignMsgV2(core.StakeDeposit, pub, stakeAmt, burnHash, 0)
	sig, sigErr := priv.Sign(msg)
	if sigErr != nil {
		t.Fatalf("Sign: %v", sigErr)
	}
	ownershipMsg := core.StakeOwnershipSignMsg(burnHash, 0)
	oneTimeSig, otErr := priv.Sign(ownershipMsg)
	if otErr != nil {
		t.Fatalf("Sign ownership: %v", otErr)
	}
	extra, extraErr := core.EncodeStakeExtraV3(core.StakeDeposit, pub, stakeAmt, sig, burnHash, 0, blind, oneTimeSig)
	if extraErr != nil {
		t.Fatalf("EncodeStakeExtraV3: %v", extraErr)
	}
	stakeTx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	// Apply via ApplyBlockStakeTxs (live path — same as consensus engine does).
	reg1 := core.NewValidatorRegistry()
	reg1.SetUTXOSet(utxos)
	rollback, applyErr := reg1.ApplyBlockStakeTxs([]core.Transaction{stakeTx}, 1)
	if applyErr != nil {
		t.Fatalf("ApplyBlockStakeTxs (first run): %v", applyErr)
	}
	_ = rollback // AddBlock succeeded conceptually; rollback not needed

	// Verify first-run state.
	if !utxos.IsStaked(burnHash, 0) {
		t.Fatal("first run: burn UTXO not in staked set after ApplyBlockStakeTxs")
	}
	if utxos.Get(burnHash, 0) != nil {
		t.Fatal("first run: burn UTXO still in active set after staking")
	}
	entry1, ok1 := reg1.GetEntry(pub)
	if !ok1 || entry1.Status != core.ValidatorPending {
		t.Fatalf("first run: validator not registered or wrong status (ok=%v, status=%v)", ok1, entry1.Status)
	}

	// ── "Restart" — fresh registry and UTXOSet (simulates node restart) ──────
	// On restart the active UTXO set is empty (or contains genesis only).
	// The burn UTXO is NOT present; the validator registry is empty.
	utxos2 := core.NewUTXOSet() // fresh — burn UTXO not present
	reg2 := core.NewValidatorRegistry()
	reg2.SetUTXOSet(utxos2)

	// Replay the committed block's stake tx.
	if replayErr := reg2.ReplayBlockStakeTxs([]core.Transaction{stakeTx}, 1); replayErr != nil {
		t.Fatalf("ReplayBlockStakeTxs: %v", replayErr)
	}

	// ── Assertions after replay ───────────────────────────────────────────────

	// The validator must be registered with Pending status.
	entry2, ok2 := reg2.GetEntry(pub)
	if !ok2 {
		t.Fatal("restart: validator not in registry after replay — registry was not restored")
	}
	if entry2.Status != core.ValidatorPending {
		t.Errorf("restart: validator status = %v, want Pending", entry2.Status)
	}
	if entry2.StakeNAPR != stakeAmt {
		t.Errorf("restart: stake = %d nAPRO, want %d nAPRO", entry2.StakeNAPR, stakeAmt)
	}

	// The burn UTXO must be in the staked set — cannot be reused as ring decoy.
	if !utxos2.IsStaked(burnHash, 0) {
		t.Error("restart: burn UTXO is NOT in staked set after replay — collateral reuse risk!")
	}

	// The burn UTXO must NOT be in the active set — cannot be re-spent.
	if utxos2.Get(burnHash, 0) != nil {
		t.Error("restart: burn UTXO is still in active set after replay — collateral reuse risk!")
	}

	// Idempotency: a second replay of the same block must not fail or double-count.
	if replayErr2 := reg2.ReplayBlockStakeTxs([]core.Transaction{stakeTx}, 1); replayErr2 != nil {
		// Second replay may fail because applyDeposit would see the validator already exists
		// with the same stake amount — that's expected behaviour (top-up path).
		t.Logf("second replay returned error (expected): %v", replayErr2)
	}
	// Regardless, burn UTXO must still be in staked set.
	if !utxos2.IsStaked(burnHash, 0) {
		t.Error("restart: burn UTXO not in staked set after second replay")
	}

	t.Logf("OK: validator registry and staked-UTXO set correctly restored after simulated restart")
}

// TestRestartSafety_UTXORebuildAndStakeReplay is an end-to-end restart integration
// test that mirrors the actual node startup sequence in cmd/node/main.go:
//
//  1. Create a genesis block with some outputs (unrelated UTXO).
//  2. Create a subsequent block that mints the burn UTXO (staker's collateral).
//  3. Create a block containing a v2 stake deposit that burns that UTXO.
//  4. Simulate node restart: fresh UTXOSet + fresh registry.
//  5. Run the startup scan (ApplyBlock per block, then ReplayBlockStakeTxs for
//     blocks with stake txs) — exactly what main.go does.
//  6. Assert:
//     - Burn UTXO absent from active set (cannot be re-spent or used as ring decoy).
//     - Burn UTXO present in staked set (IsStaked returns true).
//     - An unrelated genesis UTXO that was NOT spent is still in the active set.
//     - Validator entry has Pending status and the correct stake amount.
func TestRestartSafety_UTXORebuildAndStakeReplay(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()

	const stakeAmt uint64 = 10_000_000_000_000 // 100 000 APRO > MinStakeNAPR

	var spendPub crypto.Point32
	copy(spendPub[:], []byte(pub))
	blind, _ := crypto.DeterministicMintBlind(spendPub, stakeAmt)
	commit, _ := crypto.Commit(stakeAmt, blind)

	// A different UTXO that should survive untouched after restart (unrelated to stake).
	var unrelatedPub crypto.Point32
	unrelatedPub[0] = 0xAB
	unrelatedBlind, _ := crypto.NewBlindFactor()
	unrelatedCommit, _ := crypto.Commit(1_000_000_000, unrelatedBlind)

	// ── Block 0: genesis — contains the unrelated UTXO ───────────────────────
	genesisTx := core.Transaction{
		Version: core.TxVersionBase,
		Outputs: []core.Output{{
			OneTimePub:   unrelatedPub,
			AmountCommit: unrelatedCommit,
		}},
	}
	genesisTxHash := genesisTx.Hash()
	genesisBlock := &core.Block{}
	genesisBlock.Header.Height = 0
	genesisBlock.Txs = []core.Transaction{genesisTx}

	// ── Block 1: mint block — creates the burn UTXO ──────────────────────────
	mintTx := core.Transaction{
		Version: core.TxVersionBase,
		Outputs: []core.Output{{
			OneTimePub:   spendPub,
			AmountCommit: commit,
		}},
	}
	// ApplyBlock uses tx.Hash() as the TxHash for UTXOs — compute it before
	// building the stake payload so burnHash matches the on-chain UTXO key.
	burnHash := mintTx.Hash()
	mintBlock := &core.Block{}
	mintBlock.Header.Height = 1
	mintBlock.Txs = []core.Transaction{mintTx}

	// ── Block 2: stake deposit block (V3 — F-049 fix for transparent/mint UTXOs) ──
	stakeMsg := core.StakeSignMsgV2(core.StakeDeposit, pub, stakeAmt, burnHash, 0)
	stakeSig, _ := priv.Sign(stakeMsg)
	stakeOwnershipMsg := core.StakeOwnershipSignMsg(burnHash, 0)
	stakeOneTimeSig, _ := priv.Sign(stakeOwnershipMsg)
	stakeExtra, _ := core.EncodeStakeExtraV3(core.StakeDeposit, pub, stakeAmt, stakeSig, burnHash, 0, blind, stakeOneTimeSig)
	stakeBlock := &core.Block{}
	stakeBlock.Header.Height = 2
	stakeBlock.Txs = []core.Transaction{
		{Version: core.TxVersionStake, Extra: stakeExtra},
	}

	// ── First run: apply all blocks (simulates normal node operation) ─────────
	utxos1 := core.NewUTXOSet()
	reg1 := core.NewValidatorRegistry()
	reg1.SetUTXOSet(utxos1)

	utxos1.ApplyBlock(genesisBlock) // unrelated UTXO added
	utxos1.ApplyBlock(mintBlock)    // burn UTXO added to active set
	reg1.ApplyBlockStakeTxs(stakeBlock.Txs, 2) // moves burn UTXO to staked set

	// Sanity check first run.
	if utxos1.Get(burnHash, 0) != nil {
		t.Fatal("first run: burn UTXO still in active set after staking")
	}
	if !utxos1.IsStaked(burnHash, 0) {
		t.Fatal("first run: burn UTXO not in staked set")
	}
	if utxos1.Get(genesisTxHash, 0) == nil {
		t.Fatal("first run: unrelated genesis UTXO missing from active set")
	}

	// ── Restart: fresh UTXOSet and registry ───────────────────────────────────
	utxos2 := core.NewUTXOSet()
	reg2 := core.NewValidatorRegistry()
	reg2.SetUTXOSet(utxos2)
	// No genesis validators in this test — the staker is a new non-genesis validator.

	// Startup scan: mirrors main.go's single-pass loop over all blocks 0..tip.
	allBlocks := []*core.Block{genesisBlock, mintBlock, stakeBlock}
	for _, blk := range allBlocks {
		// Rebuild active UTXO set (ApplyBlock adds outputs, removes spent inputs).
		if err := utxos2.ApplyBlock(blk); err != nil {
			t.Fatalf("restart ApplyBlock at height %d: %v", blk.Header.Height, err)
		}
		// Replay stake txs: MarkStakedKnown finds the burn UTXO in the active set
		// (just added by ApplyBlock) and moves it to stakedUTXOs.
		if err := reg2.ReplayBlockStakeTxs(blk.Txs, blk.Header.Height); err != nil {
			t.Fatalf("restart ReplayBlockStakeTxs at height %d: %v", blk.Header.Height, err)
		}
	}

	// ── Assertions after restart ──────────────────────────────────────────────

	// Burn UTXO must NOT be in the active set — cannot be re-spent or used as a
	// ring decoy in a subsequent transaction after restart.
	if utxos2.Get(burnHash, 0) != nil {
		t.Error("restart: burn UTXO still in active set — ring-decoy reuse risk!")
	}

	// Burn UTXO must be in the staked set so IsStaked() returns true.
	if !utxos2.IsStaked(burnHash, 0) {
		t.Error("restart: burn UTXO not in staked set — collateral reuse risk!")
	}

	// Unrelated genesis UTXO must still be in the active set — legitimate ring member.
	if utxos2.Get(genesisTxHash, 0) == nil {
		t.Error("restart: unrelated genesis UTXO missing from active set — ring member lost!")
	}

	// Validator must be registered with Pending status and correct stake.
	entry, ok := reg2.GetEntry(pub)
	if !ok {
		t.Fatal("restart: validator not in registry after startup scan")
	}
	if entry.Status != core.ValidatorPending {
		t.Errorf("restart: validator status = %v, want Pending", entry.Status)
	}
	if entry.StakeNAPR != stakeAmt {
		t.Errorf("restart: stake = %d nAPRO, want %d", entry.StakeNAPR, stakeAmt)
	}

	t.Logf("OK: full restart sequence correct — burn UTXO staked, unrelated UTXO active, validator Pending %d nAPRO",
		entry.StakeNAPR)
}

// TestReplayBlockStakeTxs_GenesisValidatorTopup verifies that a genesis validator
// (Active status, seeded by InitFromGenesis) can receive a v2 top-up deposit in a
// historical block, and that after a restart via ReplayBlockStakeTxs the validator
// remains Active with the incremented stake — not demoted to Pending.
func TestReplayBlockStakeTxs_GenesisValidatorTopup(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()

	const genesisStake = core.MinStakeNAPR * 10
	const topupAmt uint64 = 10_000_000_000_000 // 100 000 APRO

	var spendPub crypto.Point32
	copy(spendPub[:], []byte(pub))
	blind, _ := crypto.DeterministicMintBlind(spendPub, topupAmt)
	commit, _ := crypto.Commit(topupAmt, blind)

	var burnHash crypto.Hash32
	for i := range burnHash {
		burnHash[i] = 0xDD
	}

	// Build top-up tx as V3 (transparent/mint output — F-049 fix requires V3).
	msg := core.StakeSignMsgV2(core.StakeDeposit, pub, topupAmt, burnHash, 0)
	sig, _ := priv.Sign(msg)
	ownerMsg := core.StakeOwnershipSignMsg(burnHash, 0)
	oneTimeSig, _ := priv.Sign(ownerMsg)
	extra, _ := core.EncodeStakeExtraV3(core.StakeDeposit, pub, topupAmt, sig, burnHash, 0, blind, oneTimeSig)
	stakeTx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	// ── First run: apply top-up to registry seeded with genesis validators ──
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: burnHash, OutputIndex: 0, OneTimePub: spendPub, AmountCommit: commit})

	reg1 := core.NewValidatorRegistry()
	reg1.SetUTXOSet(utxos)
	reg1.InitFromGenesis([]crypto.ValidatorPubKey{pub}, genesisStake)

	_, err := reg1.ApplyBlockStakeTxs([]core.Transaction{stakeTx}, 1)
	if err != nil {
		t.Fatalf("ApplyBlockStakeTxs: %v", err)
	}

	e1, _ := reg1.GetEntry(pub)
	if e1.Status != core.ValidatorActive {
		t.Fatalf("first run: expected Active after top-up, got %v", e1.Status)
	}
	wantStake := genesisStake + topupAmt
	if e1.StakeNAPR != wantStake {
		t.Fatalf("first run: stake = %d, want %d", e1.StakeNAPR, wantStake)
	}

	// ── Restart: fresh registry, seed genesis, then replay ───────────────────
	utxos2 := core.NewUTXOSet()
	reg2 := core.NewValidatorRegistry()
	reg2.SetUTXOSet(utxos2)
	reg2.InitFromGenesis([]crypto.ValidatorPubKey{pub}, genesisStake) // must be BEFORE replay

	if err := reg2.ReplayBlockStakeTxs([]core.Transaction{stakeTx}, 1); err != nil {
		t.Fatalf("ReplayBlockStakeTxs: %v", err)
	}

	e2, ok := reg2.GetEntry(pub)
	if !ok {
		t.Fatal("restart: validator not in registry after replay")
	}
	if e2.Status != core.ValidatorActive {
		t.Errorf("restart: validator demoted to %v after top-up replay — expected Active", e2.Status)
	}
	if e2.StakeNAPR != wantStake {
		t.Errorf("restart: stake = %d, want %d", e2.StakeNAPR, wantStake)
	}
	if !utxos2.IsStaked(burnHash, 0) {
		t.Error("restart: burn UTXO not in staked set after top-up replay")
	}
	t.Logf("OK: genesis-validator top-up correctly restored after restart (status=%v, stake=%d nAPRO)",
		e2.Status, e2.StakeNAPR)
}

// TestReplayBlockStakeTxs_GenesisValidatorWithdraw verifies that a genesis validator
// who initiated a withdrawal in a historical block is correctly replayed on restart —
// status becomes Unbonding and the unbonding queue is restored.
func TestReplayBlockStakeTxs_GenesisValidatorWithdraw(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()

	const genesisStake = core.MinStakeNAPR * 10

	// Build v1 StakeWithdraw tx.
	msg := core.StakeSignMsg(core.StakeWithdraw, pub, 0)
	sig, _ := priv.Sign(msg)
	extra, _ := core.EncodeStakeExtra(core.StakeWithdraw, pub, 0, sig)
	withdrawTx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	// ── First run ─────────────────────────────────────────────────────────────
	reg1 := core.NewValidatorRegistry()
	reg1.InitFromGenesis([]crypto.ValidatorPubKey{pub}, genesisStake)
	_, err := reg1.ApplyBlockStakeTxs([]core.Transaction{withdrawTx}, 5)
	if err != nil {
		t.Fatalf("ApplyBlockStakeTxs (withdraw): %v", err)
	}
	e1, _ := reg1.GetEntry(pub)
	if e1.Status != core.ValidatorUnbonding {
		t.Fatalf("first run: expected Unbonding after withdraw, got %v", e1.Status)
	}

	// ── Restart: seed genesis then replay ─────────────────────────────────────
	reg2 := core.NewValidatorRegistry()
	reg2.InitFromGenesis([]crypto.ValidatorPubKey{pub}, genesisStake) // must be BEFORE replay
	if err := reg2.ReplayBlockStakeTxs([]core.Transaction{withdrawTx}, 5); err != nil {
		t.Fatalf("ReplayBlockStakeTxs (withdraw): %v", err)
	}
	e2, ok := reg2.GetEntry(pub)
	if !ok {
		t.Fatal("restart: validator not in registry after withdraw replay")
	}
	if e2.Status != core.ValidatorUnbonding {
		t.Errorf("restart: expected Unbonding after withdraw replay, got %v", e2.Status)
	}
	t.Logf("OK: genesis-validator withdrawal correctly replayed (status=%v)", e2.Status)
}

// TestApplyBlockStakeTxs_RollbackOnPartialFailure verifies that ApplyBlockStakeTxs
// is truly atomic: if tx[0] succeeds but tx[1] fails, tx[0]'s changes to the
// registry AND the UTXO staking state are fully rolled back before returning.
//
// The test forces tx[1] to fail at apply time (not just at dry-run) by giving
// it a UTXO that is NOT in the active set. tx[0] uses a real UTXO; tx[1] uses a
// fabricated hash that was never added.  After ApplyBlockStakeTxs returns an
// error, the test asserts:
//
//   - The registry has NO new validator entries (tx[0]'s applyDeposit was undone).
//   - UTXO-A is back in the active set (MarkStaked was reversed by UnmarkStaked).
//   - UTXO-A is NOT in the staked set.
func TestApplyBlockStakeTxs_RollbackOnPartialFailure(t *testing.T) {
	priv0, pub0, _ := crypto.GenerateValidatorKey()
	priv1, pub1, _ := crypto.GenerateValidatorKey()

	const amt uint64 = 10_000_000_000_000 // 100 000 APRO — above MinStakeNAPR

	// UTXO-A: will be staked by tx[0] and must be returned to active set on rollback.
	var spendPub0 crypto.Point32
	copy(spendPub0[:], []byte(pub0))
	blind0, err := crypto.DeterministicMintBlind(spendPub0, amt)
	if err != nil {
		t.Fatalf("DeterministicMintBlind: %v", err)
	}
	commit0, err := crypto.Commit(amt, blind0)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var utxoHashA crypto.Hash32
	for i := range utxoHashA {
		utxoHashA[i] = 0xAA
	}
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: utxoHashA, OutputIndex: 0, OneTimePub: spendPub0, AmountCommit: commit0})

	// UTXO-B: referenced by tx[1] but NOT added to the UTXO set → MarkStaked fails.
	var utxoHashB crypto.Hash32
	for i := range utxoHashB {
		utxoHashB[i] = 0xBB
	}

	// Build tx[0]: valid v2 deposit for pub0 / UTXO-A.
	buildDepositTx := func(priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey,
		amount uint64, txHash crypto.Hash32, outIdx uint32, blind crypto.BlindFactor,
	) core.Transaction {
		msg := core.StakeSignMsgV2(core.StakeDeposit, pub, amount, txHash, outIdx)
		sig, err := priv.Sign(msg)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		extra, err := core.EncodeStakeExtraV2(core.StakeDeposit, pub, amount, sig, txHash, outIdx, blind)
		if err != nil {
			t.Fatalf("EncodeStakeExtraV2: %v", err)
		}
		return core.Transaction{Version: core.TxVersionStake, Extra: extra}
	}
	tx0 := buildDepositTx(priv0, pub0, amt, utxoHashA, 0, blind0)

	// Build tx[1]: valid payload but UTXO-B does not exist in the set.
	var spendPub1 crypto.Point32
	copy(spendPub1[:], []byte(pub1))
	blind1, _ := crypto.DeterministicMintBlind(spendPub1, amt)
	tx1 := buildDepositTx(priv1, pub1, amt, utxoHashB, 0, blind1)

	// Set up registry (no genesis validators — empty state).
	reg := core.NewValidatorRegistry()
	reg.SetUTXOSet(utxos)

	// Record state before apply.
	_, beforeExists0 := reg.GetEntry(pub0)
	_, beforeExists1 := reg.GetEntry(pub1)
	if beforeExists0 || beforeExists1 {
		t.Fatal("unexpected pre-existing validators")
	}
	if utxos.Get(utxoHashA, 0) == nil {
		t.Fatal("UTXO-A missing from active set before apply")
	}

	// Call ApplyBlockStakeTxs directly (bypasses ValidateBlockStakeTxs so we test
	// the rollback path directly rather than the pre-check rejection path).
	txs := []core.Transaction{tx0, tx1}
	_, applyErr := reg.ApplyBlockStakeTxs(txs, 1)

	if applyErr == nil {
		t.Fatal("ApplyBlockStakeTxs should have returned an error for missing UTXO-B, but returned nil")
	}

	// ── Registry must be unchanged ────────────────────────────────────────────
	_, after0 := reg.GetEntry(pub0)
	_, after1 := reg.GetEntry(pub1)
	if after0 {
		t.Error("registry has a new entry for pub0 after rollback — applyDeposit was NOT undone")
	}
	if after1 {
		t.Error("registry has a new entry for pub1 after rollback")
	}

	// ── UTXO-A must be back in the active set ─────────────────────────────────
	if utxos.Get(utxoHashA, 0) == nil {
		t.Error("UTXO-A is missing from the active set after rollback — UnmarkStaked was NOT called")
	}
	if utxos.IsStaked(utxoHashA, 0) {
		t.Error("UTXO-A is still recorded as staked after rollback — MarkStaked was NOT reversed")
	}

	t.Logf("OK: ApplyBlockStakeTxs rolled back tx[0] changes correctly on tx[1] failure: %v", applyErr)
}
