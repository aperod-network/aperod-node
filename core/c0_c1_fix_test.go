package core_test

// Tests for C-0 (ring-member UTXO binding) and C-1 (UTXO-backed stake deposit).

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// minimalRingCTTx builds the minimum valid-structure RingCT transaction that
// will pass tx.Validate() so TxVerifier proceeds past structural checks and
// reaches the C-0 ring-member lookup.  The ring is filled with the provided
// pub keys (must be crypto.RingSize = 16); signatures and range proofs are nil
// stubs — verification will fail there if C-0 doesn't fire first.
func minimalRingCTTx(ring []crypto.Point32, commit crypto.Commitment) core.Transaction {
	ki := crypto.KeyImage{0x01}
	sigs := make([]*crypto.MLSAGSignature, 1)
	rps := make([]*crypto.RangeProof, 1)
	var zeroBF crypto.BlindFactor
	rps[0], _ = crypto.ProveRange(1, zeroBF)
	return core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{
				Ring:         ring,
				AmountCommit: commit,
				KeyImage:     ki,
			},
		},
		Outputs: []core.Output{
			{
				OneTimePub:   crypto.Point32{0x02},
				AmountCommit: commit,
			},
		},
		Signatures:  sigs,
		RangeProofs: rps,
	}
}

// ─── C-0: ring-member UTXO presence check ─────────────────────────────────────

// TestC0_Phase1CompatModeSkipsAbsentMembers verifies that a non-strict verifier
// (Phase 1 compat mode, the default) accepts a transaction whose ring contains
// keys not present in the UTXO set.
//
// Phase 1 wallets generate random decoy keys.  Without strict mode enabled the
// C-0 check skips absent members so in-flight Phase 1 transactions are not
// orphaned during migration.  The MLSAG ring signature still proves knowledge
// of the real spending key, so no inflation is possible.
func TestC0_Phase1CompatModeSkipsAbsentMembers(t *testing.T) {
	utxos := core.NewUTXOSet()

	blind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatalf("NewBlindFactor: %v", err)
	}
	commit, err := crypto.Commit(1000, blind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Build a ring with 16 random pub keys, none present in the UTXO set
	// (mimicking txBuildRing Phase 1 behaviour).
	ring := make([]crypto.Point32, crypto.RingSize)
	for i := range ring {
		ring[i][0] = byte(i + 1)
	}

	tx := minimalRingCTTx(ring, commit)
	verifier := core.NewTxVerifier(utxos) // default: strictRingMembers = false
	err = verifier.VerifyTx(&tx)

	// Phase 1 compat mode: absent ring members must NOT trigger a C-0 error.
	// The tx may still fail at a later stage (e.g. MLSAG nil signature in this
	// test), but it must pass the ring-member UTXO existence check.
	if err != nil && strings.Contains(err.Error(), "C-0") {
		t.Fatalf("Phase 1 compat mode: absent ring member incorrectly rejected by C-0: %v", err)
	}
	t.Logf("Phase 1 compat: absent member correctly skipped (later stage: %v)", err)
}

// TestC0_AllMembersInUTXOSet_SameCommit verifies that a ring where every member
// is present in byPubKey and all have the same commitment passes C-0.
// This models Phase 2 decoys sampled from spentPubKeys (absent from byPubKey)
// combined with a real unspent member — but here all members are active for
// the convenience of testing C-0 in isolation.
func TestC0_AllMembersInUTXOSet_SameCommit(t *testing.T) {
	utxos := core.NewUTXOSet()

	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(1000, blind)

	// Populate UTXO set with all ring members sharing the same commitment.
	ring := make([]crypto.Point32, crypto.RingSize)
	for i := range ring {
		p := crypto.Point32{byte(i + 1)}
		ring[i] = p
		utxos.Add(&core.UTXO{
			TxHash:       crypto.Hash32{byte(i + 1)},
			OutputIndex:  0,
			OneTimePub:   p,
			AmountCommit: commit,
		})
	}

	tx := minimalRingCTTx(ring, commit)
	verifier := core.NewTxVerifier(utxos)
	err := verifier.VerifyTx(&tx)

	// All ring members present with matching commitment — C-0 must NOT fire.
	// The tx will still fail later (nil MLSAG sig stub), but not at C-0.
	if err != nil && strings.Contains(err.Error(), "C-0") {
		t.Fatalf("real UTXO ring incorrectly rejected by C-0: %v", err)
	}
	t.Logf("All real UTXOs with same commit accepted past C-0 (later: %v)", err)
}

// TestC0_ForgedCommit_RejectedByC0 verifies that a transaction whose
// inp.AmountCommit does NOT match the on-chain commitment of a ring member in
// byPubKey is rejected with a C-0 error.
//
// Security argument: the real (unspent) spending key lives in byPubKey.  A
// malicious signer cannot claim a larger amount by forging inp.AmountCommit
// because C-0 detects the mismatch for the active UTXO.
//
// Phase 2 decoys (from spentPubKeys) are absent from byPubKey and C-0 skips
// them — identical treatment to Phase 1 random keys.  Only the real unspent
// input triggers the commitment check.
func TestC0_ForgedCommit_RejectedByC0(t *testing.T) {
	utxos := core.NewUTXOSet()

	blind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatalf("NewBlindFactor: %v", err)
	}
	realCommit, err := crypto.Commit(1000, blind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The real ring member: unspent, in byPubKey with realCommit.
	var realPub crypto.Point32
	realPub[0] = 0xCC
	utxos.Add(&core.UTXO{
		TxHash:       crypto.Hash32{0x10},
		OutputIndex:  0,
		OneTimePub:   realPub,
		AmountCommit: realCommit,
	})

	// Build a ring: realPub at index 0, remaining slots absent (random / Phase 1).
	ring := make([]crypto.Point32, crypto.RingSize)
	ring[0] = realPub
	for i := 1; i < crypto.RingSize; i++ {
		ring[i][0] = byte(i + 100) // not in UTXO set
	}

	// Build a transaction with a FORGED inp.AmountCommit.
	forgedBlind, _ := crypto.NewBlindFactor()
	forgedCommit, _ := crypto.Commit(9_999_999, forgedBlind)

	tx := minimalRingCTTx(ring, forgedCommit)
	verifier := core.NewTxVerifier(utxos)
	err = verifier.VerifyTx(&tx)

	// C-0 must fire: realPub is in byPubKey with realCommit ≠ forgedCommit.
	if err == nil {
		t.Fatal("expected C-0 rejection of forged commitment, got nil error")
	}
	if !strings.Contains(err.Error(), "C-0") {
		t.Errorf("expected C-0 error, got: %v", err)
	}
	t.Logf("Forged commit correctly rejected by C-0: %v", err)
}

// TestC0_MarkSpentAloneKeepsUTXOInByPubKey verifies that calling MarkSpent
// (key-image only) does NOT remove the UTXO from byPubKey.  The UTXO is moved
// to spentPubKeys only when ApplyBlock processes a full block that identifies
// the real spent UTXO by commitment match.  MarkSpent is used during startup
// replay where only key-image data is available.
func TestC0_MarkSpentAloneKeepsUTXOInByPubKey(t *testing.T) {
	utxos := core.NewUTXOSet()

	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(500, blind)

	var memberPub crypto.Point32
	memberPub[0] = 0xDD

	utxos.Add(&core.UTXO{
		TxHash:       crypto.Hash32{0x20},
		OutputIndex:  0,
		OneTimePub:   memberPub,
		AmountCommit: commit,
	})

	// MarkSpent records the key image but CANNOT identify which pub key was spent
	// (that requires the ring + commitment match in ApplyBlock).  The UTXO must
	// remain in byPubKey.
	utxos.MarkSpent(crypto.KeyImage{0x99})

	result := utxos.GetByPubKey(memberPub)
	if result == nil {
		t.Fatal("UTXO should remain in byPubKey after MarkSpent-only (no ring data)")
	}
	t.Log("C-0: MarkSpent alone correctly leaves UTXO in byPubKey")
}

// TestC0_Phase2SpentDecoyAbsentFromByPubKey verifies that UTXOs added to
// spentPubKeys via AddSpentDecoyForTest are absent from byPubKey and therefore
// C-0 skips them — making them safe Phase 2 ring decoys.
func TestC0_Phase2SpentDecoyAbsentFromByPubKey(t *testing.T) {
	utxos := core.NewUTXOSet()

	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(200, blind)

	var spentPub crypto.Point32
	spentPub[0] = 0xEE

	spentUTXO := &core.UTXO{
		TxHash:       crypto.Hash32{0x30},
		OutputIndex:  0,
		OneTimePub:   spentPub,
		AmountCommit: commit,
	}
	// Simulate ApplyBlock moving the spent UTXO out of byPubKey.
	utxos.AddSpentDecoyForTest(spentUTXO)

	// Must be absent from byPubKey — C-0 will skip it as a ring decoy.
	if utxos.GetByPubKey(spentPub) != nil {
		t.Fatal("spent decoy should NOT be in byPubKey; C-0 would fire with commitment mismatch")
	}
	t.Log("Phase 2 spent decoy correctly absent from byPubKey — safe for ring")
}

// ─── Phase 2: UTXOSet.SampleDecoys ───────────────────────────────────────────

// TestSampleDecoys_BasicSampling verifies that SampleDecoys returns the
// requested number of decoys from spentPubKeys and excludes specified pub keys.
//
// SampleDecoys sources from spentPubKeys (spent UTXOs moved out of byPubKey by
// ApplyBlock).  Tests use AddSpentDecoyForTest to populate the pool directly.
func TestSampleDecoys_BasicSampling(t *testing.T) {
	utxos := core.NewUTXOSet()

	// Populate spentPubKeys with 20 spent UTXOs.
	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(100, blind)
	for i := 0; i < 20; i++ {
		p := crypto.Point32{byte(i + 1)}
		utxos.AddSpentDecoyForTest(&core.UTXO{
			TxHash:       crypto.Hash32{byte(i + 1)},
			OutputIndex:  0,
			OneTimePub:   p,
			AmountCommit: commit,
		})
	}

	exclude := map[crypto.Point32]bool{{0x01}: true, {0x02}: true}
	decoys := utxos.SampleDecoys(15, exclude)
	if len(decoys) != 15 {
		t.Fatalf("want 15 decoys, got %d", len(decoys))
	}
	for _, d := range decoys {
		if exclude[d.OneTimePub] {
			t.Fatalf("excluded pub key %x appeared in decoys", d.OneTimePub[:4])
		}
	}
	t.Logf("SampleDecoys: 15 unique non-excluded decoys returned")
}

// TestSampleDecoys_ShortfallReturnsAll verifies that SampleDecoys returns all
// available candidates when fewer than count are present in spentPubKeys.
func TestSampleDecoys_ShortfallReturnsAll(t *testing.T) {
	utxos := core.NewUTXOSet()

	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(50, blind)
	for i := 0; i < 5; i++ {
		p := crypto.Point32{byte(i + 1)}
		utxos.AddSpentDecoyForTest(&core.UTXO{
			TxHash:       crypto.Hash32{byte(i + 1)},
			OutputIndex:  0,
			OneTimePub:   p,
			AmountCommit: commit,
		})
	}

	decoys := utxos.SampleDecoys(15, nil)
	if len(decoys) != 5 {
		t.Fatalf("want 5 (all available), got %d", len(decoys))
	}
	t.Log("SampleDecoys: shortfall returns all available candidates")
}

// TestSampleDecoys_EmptyPoolReturnsNil verifies that SampleDecoys returns nil
// when no spent UTXOs are in the pool (new node with no transactions yet).
func TestSampleDecoys_EmptyPoolReturnsNil(t *testing.T) {
	utxos := core.NewUTXOSet()
	decoys := utxos.SampleDecoys(15, nil)
	if len(decoys) != 0 {
		t.Fatalf("want 0 decoys from empty pool, got %d", len(decoys))
	}
	t.Log("SampleDecoys: empty pool correctly returns nil (caller falls back to Phase 1)")
}

// TestSpentDecoyPool_SurvivesRestartReplay verifies the restart lifecycle:
//
//  1. Start: add active UTXOs, apply a block that spends one.
//     → spentPubKeys has the spent UTXO; SampleDecoys returns it.
//
//  2. Simulate restart: create a new UTXOSet and restore only active UTXOs
//     (as restoreChain does via IterUTXOs+Add).
//     → spentPubKeys is empty; SampleDecoys returns nothing.
//
//  3. Replay the spending block via ApplyBlockForSpentDecoys.
//     → spentPubKeys is rebuilt; SampleDecoys returns the spent UTXO again.
//
// This mirrors what restoreChain does at node startup and proves that the
// decoy pool is available to wallet sends immediately after a restart.
func TestSpentDecoyPool_SurvivesRestartReplay(t *testing.T) {
	// ── Phase A: runtime (new block committed) ────────────────────────────────
	utxosRuntime := core.NewUTXOSet()

	blind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatalf("NewBlindFactor: %v", err)
	}
	realCommit, err := crypto.Commit(1000, blind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Add 16 UTXOs (the real input and 15 decoys).
	ring := make([]crypto.Point32, crypto.RingSize)
	for i := 0; i < crypto.RingSize; i++ {
		p := crypto.Point32{byte(i + 1)}
		ring[i] = p
		utxosRuntime.Add(&core.UTXO{
			TxHash:       crypto.Hash32{byte(i + 1)},
			OutputIndex:  0,
			OneTimePub:   ring[i],
			AmountCommit: realCommit,
		})
	}

	// Build a minimal spending block (no outputs needed for this test).
	spendingBlock := &core.Block{
		Header: core.BlockHeader{Height: 1},
		Txs: []core.Transaction{
			{
				Version: core.TxVersionBase,
				Inputs: []core.RingInput{
					{Ring: ring, AmountCommit: realCommit, KeyImage: crypto.KeyImage{0xFF}},
				},
			},
		},
	}

	// Apply the block at runtime — real UTXO (ring[0]) is moved to spentPubKeys.
	if err := utxosRuntime.ApplyBlock(spendingBlock); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	if utxosRuntime.SpentDecoyCount() == 0 {
		t.Fatal("runtime: spentPubKeys must be non-empty after ApplyBlock")
	}
	runtimeDecoys := utxosRuntime.SampleDecoys(1, nil)
	if len(runtimeDecoys) == 0 {
		t.Fatal("runtime: SampleDecoys must return spent decoys")
	}
	t.Logf("runtime: %d decoy(s) available", len(runtimeDecoys))

	// ── Phase B: simulate restart — restore only active UTXOs ─────────────────
	utxosAfterRestart := core.NewUTXOSet()
	// restoreChain calls IterUTXOs → Add for each active (unspent) UTXO.
	// ring[0] was spent, so it is absent from the persisted active set.
	for i := 1; i < crypto.RingSize; i++ {
		utxosAfterRestart.Add(&core.UTXO{
			TxHash:       crypto.Hash32{byte(i + 1)},
			OutputIndex:  0,
			OneTimePub:   ring[i],
			AmountCommit: realCommit,
		})
	}
	if utxosAfterRestart.SpentDecoyCount() != 0 {
		t.Fatal("post-restart: spentPubKeys must be empty before replay")
	}
	if decoysBeforeReplay := utxosAfterRestart.SampleDecoys(1, nil); len(decoysBeforeReplay) != 0 {
		t.Fatalf("post-restart before replay: expected 0 decoys, got %d", len(decoysBeforeReplay))
	}
	t.Log("post-restart: decoy pool correctly empty before ApplyBlockForSpentDecoys")

	// ── Phase C: replay spending block via ApplyBlockForSpentDecoys ───────────
	utxosAfterRestart.ApplyBlockForSpentDecoys(spendingBlock)
	if utxosAfterRestart.SpentDecoyCount() == 0 {
		t.Fatal("post-restart after replay: spentPubKeys must be non-empty")
	}
	replayDecoys := utxosAfterRestart.SampleDecoys(1, nil)
	if len(replayDecoys) == 0 {
		t.Fatal("post-restart after replay: SampleDecoys must return spent decoys")
	}
	// Verify the returned decoy was a member of the original ring.
	found := false
	for _, r := range ring {
		if replayDecoys[0].OneTimePub == r {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("replayed decoy pub %x was not in the original ring", replayDecoys[0].OneTimePub[:4])
	}
	t.Logf("post-restart: decoy pool correctly rebuilt — %d decoy(s) available", len(replayDecoys))
}

// ─── C-1: UTXO-backed stake deposit ──────────────────────────────────────────

// TestC1_RejectV1Deposit ensures that old 105-byte StakeDeposit txs are rejected.
func TestC1_RejectV1Deposit(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	amount := core.MinStakeNAPR
	msg := core.StakeSignMsg(core.StakeDeposit, pub, amount)
	sig, err := priv.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	extra, err := core.EncodeStakeExtra(core.StakeDeposit, pub, amount, sig)
	if err != nil {
		t.Fatalf("EncodeStakeExtra: %v", err)
	}

	tx := core.Transaction{
		Version: core.TxVersionStake,
		Extra:   extra,
	}

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(core.NewUTXOSet())

	err = registry.ProcessStakeTx(tx, 1)
	if err == nil {
		t.Fatal("expected v1 deposit to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "C-1 fix") {
		t.Fatalf("expected C-1 error, got: %v", err)
	}
	t.Logf("C-1 correctly rejected v1 deposit: %v", err)
}

// TestC1_RejectFabricatedAmount verifies that a v2 deposit where the
// claimed amount/blind do NOT open to the UTXO's AmountCommit is rejected.
func TestC1_RejectFabricatedAmount(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	realBlind, _ := crypto.NewBlindFactor()
	const realAmount uint64 = core.MinStakeNAPR
	realCommit, err := crypto.Commit(realAmount, realBlind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	burnTxHash := crypto.Hash32{0xAA}
	const burnOutIdx uint32 = 0

	utxos := core.NewUTXOSet()
	var onePub crypto.Point32
	onePub[0] = 0x11
	utxos.Add(&core.UTXO{
		TxHash:       burnTxHash,
		OutputIndex:  burnOutIdx,
		OneTimePub:   onePub,
		AmountCommit: realCommit,
	})

	// Claim a different blind/amount pair — commitment will not match.
	fabricatedBlind, _ := crypto.NewBlindFactor()
	const fabricatedAmount uint64 = core.MinStakeNAPR * 100

	msg := core.StakeSignMsgV2(core.StakeDeposit, pub, fabricatedAmount, burnTxHash, burnOutIdx)
	sig, err := priv.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	extra, err := core.EncodeStakeExtraV2(
		core.StakeDeposit, pub, fabricatedAmount, sig,
		burnTxHash, burnOutIdx, fabricatedBlind,
	)
	if err != nil {
		t.Fatalf("EncodeStakeExtraV2: %v", err)
	}

	tx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)

	err = registry.ProcessStakeTx(tx, 1)
	if err == nil {
		t.Fatal("expected fabricated-amount deposit to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "C-1 check") {
		t.Fatalf("expected C-1 check error, got: %v", err)
	}
	t.Logf("C-1 correctly rejected fabricated amount: %v", err)
}

// TestC1_AcceptValidV2Deposit verifies that a well-formed v2 deposit is accepted
// and the referenced UTXO is burned (removed from the active set).
func TestC1_AcceptValidV2Deposit(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	burnBlind, _ := crypto.NewBlindFactor()
	const stakeAmount uint64 = core.MinStakeNAPR
	burnCommit, err := crypto.Commit(stakeAmount, burnBlind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	burnTxHash := crypto.Hash32{0xBB}
	const burnOutIdx uint32 = 0

	utxos := core.NewUTXOSet()
	var onePub crypto.Point32
	onePub[0] = 0x22
	utxos.Add(&core.UTXO{
		TxHash:       burnTxHash,
		OutputIndex:  burnOutIdx,
		OneTimePub:   onePub,
		AmountCommit: burnCommit,
	})

	msg := core.StakeSignMsgV2(core.StakeDeposit, pub, stakeAmount, burnTxHash, burnOutIdx)
	sig, err := priv.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	extra, err := core.EncodeStakeExtraV2(
		core.StakeDeposit, pub, stakeAmount, sig,
		burnTxHash, burnOutIdx, burnBlind,
	)
	if err != nil {
		t.Fatalf("EncodeStakeExtraV2: %v", err)
	}

	tx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)

	if err := registry.ProcessStakeTx(tx, 1); err != nil {
		t.Fatalf("valid v2 deposit rejected: %v", err)
	}

	// UTXO must be burned — no longer returned by GetByPubKey.
	if utxos.GetByPubKey(onePub) != nil {
		t.Fatal("burn UTXO is still in the active byPubKey index after deposit")
	}
	if !utxos.IsStaked(burnTxHash, burnOutIdx) {
		t.Fatal("burn UTXO was not recorded in stakedUTXOs after deposit")
	}
	t.Log("C-1 valid v2 deposit accepted and UTXO burned correctly")
}

// TestC1_RejectDoubleStake ensures the same UTXO cannot be burned twice.
func TestC1_RejectDoubleStake(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	burnBlind, _ := crypto.NewBlindFactor()
	const stakeAmount uint64 = core.MinStakeNAPR
	burnCommit, err := crypto.Commit(stakeAmount, burnBlind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	burnTxHash := crypto.Hash32{0xCC}
	const burnOutIdx uint32 = 0

	utxos := core.NewUTXOSet()
	var onePub crypto.Point32
	onePub[0] = 0x33
	utxos.Add(&core.UTXO{
		TxHash:       burnTxHash,
		OutputIndex:  burnOutIdx,
		OneTimePub:   onePub,
		AmountCommit: burnCommit,
	})

	buildTx := func() core.Transaction {
		m := core.StakeSignMsgV2(core.StakeDeposit, pub, stakeAmount, burnTxHash, burnOutIdx)
		s, _ := priv.Sign(m)
		e, _ := core.EncodeStakeExtraV2(
			core.StakeDeposit, pub, stakeAmount, s,
			burnTxHash, burnOutIdx, burnBlind,
		)
		return core.Transaction{Version: core.TxVersionStake, Extra: e}
	}

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)

	// First deposit succeeds.
	if err := registry.ProcessStakeTx(buildTx(), 1); err != nil {
		t.Fatalf("first deposit: %v", err)
	}

	// Second deposit with the same UTXO must fail.
	if err := registry.ProcessStakeTx(buildTx(), 2); err == nil {
		t.Fatal("expected double-stake to be rejected, got nil")
	} else {
		t.Logf("C-1 double-stake correctly rejected: %v", err)
	}
}

// ─── C-0: startup replay — byPubKey populated by ApplyBlock ─────────────────

// TestC0_ApplyBlockPopulatesRingMemberIndex verifies that ApplyBlock correctly
// populates the byPubKey ring-member index so that UTXOs added via block replay
// (i.e. on node restart) are immediately visible to TxVerifier.GetByPubKey.
// This is the root fix for the post-restart rejection bug: ApplyBlock previously
// wrote to s.utxos but not to s.byPubKey, so the C-0 ring-member check always
// failed after a cold start.
func TestC0_ApplyBlockPopulatesRingMemberIndex(t *testing.T) {
	utxos := core.NewUTXOSet()

	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(1_000_000, blind)

	var pub crypto.Point32
	pub[0] = 0xAB

	// Build a block with one output — simulate what the startup scan sees.
	txHash := crypto.Hash32{0x01}
	block := &core.Block{
		Header: core.BlockHeader{Height: 1},
		Txs: []core.Transaction{
			{
				Version: core.TxVersionBase,
				Outputs: []core.Output{
					{
						OneTimePub:   pub,
						AmountCommit: commit,
					},
				},
			},
		},
	}
	// Manually set the transaction hash inside the block so ApplyBlock sees it.
	// (In real blocks the hash is derived from the serialized transaction.)
	_ = txHash // ApplyBlock uses tx.Hash() internally; pub is what we track.

	if err := utxos.ApplyBlock(block); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}

	// GetByPubKey must return the UTXO — not nil.
	got := utxos.GetByPubKey(pub)
	if got == nil {
		t.Fatal("ApplyBlock did not populate byPubKey: GetByPubKey returned nil for a UTXO added via block replay (startup restart bug)")
	}
	if got.AmountCommit != commit {
		t.Fatalf("byPubKey entry has wrong AmountCommit: got %x, want %x", got.AmountCommit[:8], commit[:8])
	}
	t.Log("ApplyBlock correctly populates byPubKey — startup replay ring-member index is consistent")
}

// TestC0_OldDecoyPassesAfterNodeRestart is the end-to-end confirmation that a
// RingCT transaction referencing a UTXO created 30+ blocks ago passes the C-0
// ring-member validation on a freshly-started node (i.e. after the UTXO set is
// rebuilt from scratch by replaying stored blocks via ApplyBlock).
//
// Scenario:
//   - Build 35 blocks.  Block 5 creates a UTXO with a known pub key that will be
//     used as a ring decoy in a later transaction.
//   - Simulate a node restart: create a brand-new UTXOSet and replay all 35 blocks
//     through ApplyBlock (exactly as cmd/node/main.go's startup scan does).
//   - Build a minimal RingCT tx whose ring contains the 30-block-old UTXO pub key
//     as one of its RingSize members.
//   - Confirm TxVerifier does NOT return a C-0 error for the old decoy.
func TestC0_OldDecoyPassesAfterNodeRestart(t *testing.T) {
	const totalBlocks = 35
	const decoyAtBlock = 5 // UTXO created here is the "old decoy"

	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(500_000, blind)

	// The one-time pub key we will track as our old decoy.
	var decoyPub crypto.Point32
	decoyPub[0] = 0xDE
	decoyPub[1] = 0xC0
	decoyPub[2] = 0x1A // "decoy" marker

	// Build totalBlocks synthetic blocks.  Block decoyAtBlock contains decoyPub.
	blocks := make([]*core.Block, totalBlocks+1) // index 0 = genesis, 1..35 = real blocks
	// Genesis (height 0) — empty.
	blocks[0] = &core.Block{Header: core.BlockHeader{Height: 0}}

	for h := uint64(1); h <= totalBlocks; h++ {
		var txs []core.Transaction
		if h == decoyAtBlock {
			// Insert a tx that creates our tracked decoy UTXO.
			txs = append(txs, core.Transaction{
				Version: core.TxVersionBase,
				Outputs: []core.Output{
					{OneTimePub: decoyPub, AmountCommit: commit},
				},
			})
		} else {
			// Other blocks just have a coin-base-like output with a unique pub.
			var p crypto.Point32
			p[0] = byte(h)
			p[1] = 0xFF
			txs = append(txs, core.Transaction{
				Version: core.TxVersionBase,
				Outputs: []core.Output{
					{OneTimePub: p, AmountCommit: commit},
				},
			})
		}
		blocks[h] = &core.Block{
			Header: core.BlockHeader{Height: h},
			Txs:    txs,
		}
	}

	// ── Simulate node restart ────────────────────────────────────────────────
	// Create a brand-new UTXOSet (empty, as it would be at process start) and
	// replay every block exactly as the startup scan in cmd/node/main.go does.
	freshUTXOs := core.NewUTXOSet()
	for _, b := range blocks {
		if err := freshUTXOs.ApplyBlock(b); err != nil {
			t.Fatalf("ApplyBlock at height %d: %v", b.Header.Height, err)
		}
	}

	// After replay, the 30-block-old decoy UTXO must be in byPubKey.
	if freshUTXOs.GetByPubKey(decoyPub) == nil {
		t.Fatal("30-block-old decoy UTXO not found in byPubKey after startup replay — node restart would reject all ring txs referencing it")
	}

	// Build a minimal RingCT tx with decoyPub as one of its ring members and
	// fill the rest with real UTXOs too (C-0 checks all members).
	ring := make([]crypto.Point32, crypto.RingSize)
	ring[0] = decoyPub
	for i := 1; i < crypto.RingSize; i++ {
		var p crypto.Point32
		p[0] = byte(i + 50)
		ring[i] = p
		// Add these extra ring members to the fresh UTXO set so C-0 passes them.
		freshUTXOs.Add(&core.UTXO{
			TxHash:       crypto.Hash32{byte(i)},
			OutputIndex:  0,
			OneTimePub:   p,
			AmountCommit: commit,
		})
	}

	// Build a range proof for the output so the tx passes Validate().
	var zeroBF crypto.BlindFactor
	rp, _ := crypto.ProveRange(1, zeroBF)
	tx := core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{
				Ring:         ring,
				AmountCommit: commit,
				KeyImage:     crypto.KeyImage{0xE2, 0xE2},
			},
		},
		Outputs: []core.Output{
			{OneTimePub: crypto.Point32{0x99}, AmountCommit: commit},
		},
		Signatures:  []*crypto.MLSAGSignature{nil}, // stub — C-0 fires before MLSAG
		RangeProofs: []*crypto.RangeProof{rp},
	}

	verifier := core.NewTxVerifier(freshUTXOs)
	err := verifier.VerifyTx(&tx)

	// The C-0 check must NOT fire.  MLSAG will fail (nil sig) — that is
	// expected and acceptable; the test only cares that the old decoy UTXO
	// is accepted by the ring-member validation stage.
	if err != nil && strings.Contains(err.Error(), "C-0") {
		t.Fatalf("C-0 incorrectly rejected 30-block-old decoy after startup replay: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "C-0") {
		t.Logf("C-0 passed for 30-block-old decoy (error at later stage is expected): %v", err)
	}
}

// TestC0_RollbackRemovesFromRingMemberIndex verifies that RollbackBlock
// removes rolled-back outputs from byPubKey so they cannot be used as ring
// decoys after an orphaned block is discarded.
func TestC0_RollbackRemovesFromRingMemberIndex(t *testing.T) {
	utxos := core.NewUTXOSet()

	blind, _ := crypto.NewBlindFactor()
	commit, _ := crypto.Commit(1_000_000, blind)

	var pub crypto.Point32
	pub[0] = 0xBB

	block := &core.Block{
		Header: core.BlockHeader{Height: 1},
		Txs: []core.Transaction{
			{
				Version: core.TxVersionBase,
				Outputs: []core.Output{
					{OneTimePub: pub, AmountCommit: commit},
				},
			},
		},
	}

	if err := utxos.ApplyBlock(block); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	if utxos.GetByPubKey(pub) == nil {
		t.Fatal("UTXO not in byPubKey after ApplyBlock")
	}

	// Roll back the block — the UTXO was never on a canonical chain.
	if err := utxos.RollbackBlock(block); err != nil {
		t.Fatalf("RollbackBlock: %v", err)
	}
	if utxos.GetByPubKey(pub) != nil {
		t.Fatal("rolled-back UTXO is still present in byPubKey — orphaned outputs must not remain as ring decoys")
	}
	t.Log("RollbackBlock correctly removes outputs from byPubKey")
}

// TestC1_RejectMissingUTXO verifies that a v2 deposit referencing a UTXO
// that does not exist in the active set is rejected.
func TestC1_RejectMissingUTXO(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	burnBlind, _ := crypto.NewBlindFactor()
	const stakeAmount uint64 = core.MinStakeNAPR

	// Reference a burn UTXO that was NEVER added to utxos.
	burnTxHash := crypto.Hash32{0xDD}
	const burnOutIdx uint32 = 0

	msg := core.StakeSignMsgV2(core.StakeDeposit, pub, stakeAmount, burnTxHash, burnOutIdx)
	sig, _ := priv.Sign(msg)
	extra, _ := core.EncodeStakeExtraV2(
		core.StakeDeposit, pub, stakeAmount, sig,
		burnTxHash, burnOutIdx, burnBlind,
	)

	tx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(core.NewUTXOSet()) // empty

	err = registry.ProcessStakeTx(tx, 1)
	if err == nil {
		t.Fatal("expected missing-UTXO deposit to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "C-1 check") {
		t.Fatalf("expected C-1 check error, got: %v", err)
	}
	t.Logf("C-1 missing UTXO correctly rejected: %v", err)
}
