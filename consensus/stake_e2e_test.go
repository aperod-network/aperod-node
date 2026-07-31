package consensus_test

// End-to-end integration test for the v2 stake deposit path through the
// consensus engine.  Verifies the complete flow:
//
//   1. Build a structurally-valid v2 stake deposit payload (locally signed).
//   2. Add the UTXO and the stake tx to a UTXOSet + Mempool.
//   3. Let the engine produce a block (proposer = our key).
//   4. Confirm the stake tx appears in the produced block (not dropped).
//   5. Confirm the registry records the new validator as Pending.
//   6. Confirm the burned UTXO is no longer in the active set.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// TestStakeDeposit_E2E_ProducedBlock proves that a v2 stake deposit submitted
// via mempool.Add survives produceBlock without being silently dropped, is
// included in the produced block, and triggers registry + UTXO state updates.
func TestStakeDeposit_E2E_ProducedBlock(t *testing.T) {
	// ── 1. Keys ───────────────────────────────────────────────────────────────
	// proposerPriv / proposerPub: the block-producing validator (genesis node).
	// stakerPriv  / stakerPub:   a NEW validator registering stake.
	proposerPriv, proposerPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey (proposer):", err)
	}
	stakerPriv, stakerPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey (staker):", err)
	}

	// ── 2. UTXO that the staker will burn as proof ────────────────────────────
	const stakeAmountNAPR uint64 = 10_000_000_000_000 // 100 000 APRO (MinStakeNAPR)

	// Derive the deterministic blind: same formula the CLI uses.
	var spendPub crypto.Point32
	copy(spendPub[:], []byte(stakerPub))
	burnBlind, err := crypto.DeterministicMintBlind(spendPub, stakeAmountNAPR)
	if err != nil {
		t.Fatal("DeterministicMintBlind:", err)
	}
	commit, err := crypto.Commit(stakeAmountNAPR, burnBlind)
	if err != nil {
		t.Fatal("Commit:", err)
	}

	// Use a deterministic UTXO key: txhash = 0xCC...CC, outIdx = 0.
	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0xCC
	}
	const burnOutIdx uint32 = 0

	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       burnTxHash,
		OutputIndex:  burnOutIdx,
		OneTimePub:   spendPub,
		AmountCommit: commit,
	})

	// ── 3. Validator registry wired to the UTXO set ───────────────────────────
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	// Seed the proposer as an active genesis validator.
	registry.InitFromGenesis([]crypto.ValidatorPubKey{proposerPub}, 10_000_000_000_000)

	// ── 4. Build the v2 stake deposit transaction ─────────────────────────────
	msg := core.StakeSignMsgV2(core.StakeDeposit, stakerPub, stakeAmountNAPR, burnTxHash, burnOutIdx)
	sig, err := stakerPriv.Sign(msg)
	if err != nil {
		t.Fatal("Sign:", err)
	}
	extra, err := core.EncodeStakeExtraV2(
		core.StakeDeposit, stakerPub, stakeAmountNAPR, sig,
		burnTxHash, burnOutIdx, burnBlind,
	)
	if err != nil {
		t.Fatal("EncodeStakeExtraV2:", err)
	}
	stakeTx := core.Transaction{
		Version: core.TxVersionStake,
		Extra:   extra,
	}

	// ── 5. Mempool ────────────────────────────────────────────────────────────
	mp := core.NewMempool(core.DefaultMempoolConfig())
	if err := mp.Add(stakeTx); err != nil {
		t.Fatalf("mempool.Add stake tx: %v", err)
	}

	// ── 6. Chain with genesis block ───────────────────────────────────────────
	chain := makeChainWithGenesis(t, proposerPriv, proposerPub)

	// ── 7. Consensus engine ───────────────────────────────────────────────────
	lk, err := crypto.NewLockedValidatorKey(proposerPriv.Bytes(), nil)
	if err != nil {
		t.Fatal("NewLockedValidatorKey:", err)
	}
	defer lk.Destroy()

	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{proposerPub},
		MyKey:        lk,
		Registry:     registry,
	}, chain, mp, newNopLogger())

	// Wire the UTXO set (needed for ApplyBlock in the self-produced path).
	// TxVerifier is nil — self-produced blocks skip VerifyBlock.
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	// ── 8. Run and wait for the produced block ────────────────────────────────
	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	var producedBlock *core.Block
	select {
	case producedBlock = <-eng.ProducedCh():
		t.Logf("block produced: height=%d txs=%d", producedBlock.Header.Height, len(producedBlock.Txs))
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: no block produced in 2s")
	}

	// ── 9. The stake tx must be in the block ──────────────────────────────────
	stakeHash := stakeTx.Hash()
	found := false
	for _, tx := range producedBlock.Txs {
		if tx.Hash() == stakeHash {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("stake tx %x not found in produced block (txs=%d): deposit was silently dropped",
			stakeHash[:8], len(producedBlock.Txs))
	}

	// ── 10. Registry must show the staker as Pending ──────────────────────────
	entry, ok := registry.GetEntry(stakerPub)
	if !ok {
		t.Fatal("registry.GetEntry(stakerPub) not found: ProcessStakeTx was never called for the deposit")
	}
	if entry.Status != core.ValidatorPending {
		t.Errorf("validator status = %v, want Pending", entry.Status)
	}
	if entry.StakeNAPR != stakeAmountNAPR {
		t.Errorf("validator.StakeNAPR = %d, want %d", entry.StakeNAPR, stakeAmountNAPR)
	}
	t.Logf("validator registered: status=%s stake=%.0f APRO",
		entry.Status, float64(entry.StakeNAPR)/1e8)

	// ── 11. Burn UTXO must no longer be in the active set ────────────────────
	if utxos.Get(burnTxHash, burnOutIdx) != nil {
		t.Error("burn UTXO is still in the active set after staking (MarkStaked was not called)")
	}
	if !utxos.IsStaked(burnTxHash, burnOutIdx) {
		t.Error("IsStaked(burnTxHash, burnOutIdx) = false: UTXO was not recorded as staked")
	}
	t.Log("burn UTXO correctly removed from active set and recorded as staked")
}

// buildIncomingBlockWithStake is a test helper that builds a height-1 block
// signed by proposerPriv/proposerPub, containing stakeTxs, and queues it
// through eng.NewBlockCh().  Returns the block for inspection.
func buildAndSendBlock(
	t *testing.T,
	proposerPriv crypto.ValidatorPrivKey,
	proposerPub crypto.ValidatorPubKey,
	chain *core.Chain,
	eng interface{ NewBlockCh() chan<- *core.Block },
	txs []core.Transaction,
) *core.Block {
	t.Helper()
	tip := chain.Tip()
	hdr := core.BlockHeader{
		Height:       1,
		PrevHash:     tip.Hash(),
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: proposerPub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	if err := hdr.Sign(proposerPriv); err != nil {
		t.Fatal("Sign:", err)
	}
	block := &core.Block{Header: hdr, Txs: txs}
	eng.NewBlockCh() <- block
	return block
}

// waitChainHeight polls chain.Height() until it reaches want or deadline fires.
func waitChainHeight(want uint64, chain *core.Chain, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if chain.Height() == want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestStakeDeposit_IncomingBlock_BadSig verifies that handleIncomingBlock
// rejects a block whose stake payload has an invalid Ed25519 signature.
func TestStakeDeposit_IncomingBlock_BadSig(t *testing.T) {
	proposerPriv, proposerPub, _ := crypto.GenerateValidatorKey()
	stakerPriv, stakerPub, _ := crypto.GenerateValidatorKey()
	_ = stakerPriv

	const stakeAmt uint64 = 10_000_000_000_000
	var spendPub crypto.Point32
	copy(spendPub[:], []byte(stakerPub))
	burnBlind, _ := crypto.DeterministicMintBlind(spendPub, stakeAmt)
	commit, _ := crypto.Commit(stakeAmt, burnBlind)

	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0xBB
	}
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: burnTxHash, OutputIndex: 0, OneTimePub: spendPub, AmountCommit: commit})

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{proposerPub}, 10_000_000_000_000)

	// Build a v2 payload signed by a DIFFERENT key (bad sig).
	otherPriv, _, _ := crypto.GenerateValidatorKey()
	badSig, _ := otherPriv.Sign(core.StakeSignMsgV2(core.StakeDeposit, stakerPub, stakeAmt, burnTxHash, 0))
	extra, _ := core.EncodeStakeExtraV2(core.StakeDeposit, stakerPub, stakeAmt, badSig, burnTxHash, 0, burnBlind)
	stakeTx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	chain := makeChainWithGenesis(t, proposerPriv, proposerPub)
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{proposerPub},
		Registry:     registry,
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	buildAndSendBlock(t, proposerPriv, proposerPub, chain, eng, []core.Transaction{stakeTx})
	if waitChainHeight(1, chain, 300*time.Millisecond) {
		t.Error("block with invalid stake signature was accepted — should have been rejected")
	} else {
		t.Log("OK: block with bad stake sig correctly rejected")
	}
}

// TestStakeDeposit_IncomingBlock_UTXONotFound verifies that handleIncomingBlock
// rejects a block whose stake payload references a UTXO that does not exist in
// the active UTXO set.
func TestStakeDeposit_IncomingBlock_UTXONotFound(t *testing.T) {
	proposerPriv, proposerPub, _ := crypto.GenerateValidatorKey()
	stakerPriv, stakerPub, _ := crypto.GenerateValidatorKey()

	const stakeAmt uint64 = 10_000_000_000_000
	var spendPub crypto.Point32
	copy(spendPub[:], []byte(stakerPub))
	burnBlind, _ := crypto.DeterministicMintBlind(spendPub, stakeAmt)
	commit, _ := crypto.Commit(stakeAmt, burnBlind)

	// UTXO set does NOT contain the burn UTXO — simulate missing UTXO.
	utxos := core.NewUTXOSet()
	// (intentionally empty — commit is computed but UTXO not added)
	_ = commit

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{proposerPub}, 10_000_000_000_000)

	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0xAA
	}
	msg := core.StakeSignMsgV2(core.StakeDeposit, stakerPub, stakeAmt, burnTxHash, 0)
	sig, _ := stakerPriv.Sign(msg)
	extra, _ := core.EncodeStakeExtraV2(core.StakeDeposit, stakerPub, stakeAmt, sig, burnTxHash, 0, burnBlind)
	stakeTx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	chain := makeChainWithGenesis(t, proposerPriv, proposerPub)
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{proposerPub},
		Registry:     registry,
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	buildAndSendBlock(t, proposerPriv, proposerPub, chain, eng, []core.Transaction{stakeTx})
	if waitChainHeight(1, chain, 300*time.Millisecond) {
		t.Error("block referencing missing UTXO was accepted — should have been rejected")
	} else {
		t.Log("OK: block with non-existent burn UTXO correctly rejected")
	}
}

// TestStakeDeposit_IncomingBlock_DuplicateBurnUTXO verifies that handleIncomingBlock
// rejects a block that contains two stake txs attempting to burn the same UTXO.
// Both txs would independently pass ValidateStakeTx, but ValidateBlockStakeTxs
// detects the within-block duplicate and rejects the block before chain insertion.
func TestStakeDeposit_IncomingBlock_DuplicateBurnUTXO(t *testing.T) {
	proposerPriv, proposerPub, _ := crypto.GenerateValidatorKey()
	staker1Priv, staker1Pub, _ := crypto.GenerateValidatorKey()
	staker2Priv, staker2Pub, _ := crypto.GenerateValidatorKey()

	const stakeAmt uint64 = 10_000_000_000_000
	var spendPub crypto.Point32
	copy(spendPub[:], []byte(staker1Pub))
	burnBlind, _ := crypto.DeterministicMintBlind(spendPub, stakeAmt)
	commit, _ := crypto.Commit(stakeAmt, burnBlind)

	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0xEE
	}
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: burnTxHash, OutputIndex: 0, OneTimePub: spendPub, AmountCommit: commit})

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{proposerPub}, 10_000_000_000_000)

	// Build two valid stake txs — both reference the SAME burn UTXO.
	buildStake := func(priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey) core.Transaction {
		msg := core.StakeSignMsgV2(core.StakeDeposit, pub, stakeAmt, burnTxHash, 0)
		sig, _ := priv.Sign(msg)
		extra, _ := core.EncodeStakeExtraV2(core.StakeDeposit, pub, stakeAmt, sig, burnTxHash, 0, burnBlind)
		return core.Transaction{Version: core.TxVersionStake, Extra: extra}
	}
	tx1 := buildStake(staker1Priv, staker1Pub)
	tx2 := buildStake(staker2Priv, staker2Pub)

	chain := makeChainWithGenesis(t, proposerPriv, proposerPub)
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{proposerPub},
		Registry:     registry,
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// Both tx1 and tx2 are valid individually but share the burn UTXO.
	buildAndSendBlock(t, proposerPriv, proposerPub, chain, eng, []core.Transaction{tx1, tx2})
	if waitChainHeight(1, chain, 300*time.Millisecond) {
		t.Error("block with duplicate burn UTXO was accepted — should have been rejected")
	} else {
		t.Log("OK: block with duplicate burn UTXO correctly rejected")
	}
}

// TestStakeDeposit_IncomingBlock_BelowMinimum verifies that handleIncomingBlock
// rejects a block whose v2 stake deposit is below the minimum stake threshold.
func TestStakeDeposit_IncomingBlock_BelowMinimum(t *testing.T) {
	proposerPriv, proposerPub, _ := crypto.GenerateValidatorKey()
	stakerPriv, stakerPub, _ := crypto.GenerateValidatorKey()

	// Use 1 nAPRO (far below MinStakeNAPR = 100,000 APRO).
	const tinyStake uint64 = 1
	var spendPub crypto.Point32
	copy(spendPub[:], []byte(stakerPub))
	burnBlind, _ := crypto.DeterministicMintBlind(spendPub, tinyStake)
	commit, _ := crypto.Commit(tinyStake, burnBlind)

	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0xFF
	}
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: burnTxHash, OutputIndex: 0, OneTimePub: spendPub, AmountCommit: commit})

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{proposerPub}, 10_000_000_000_000)

	msg := core.StakeSignMsgV2(core.StakeDeposit, stakerPub, tinyStake, burnTxHash, 0)
	sig, _ := stakerPriv.Sign(msg)
	extra, _ := core.EncodeStakeExtraV2(core.StakeDeposit, stakerPub, tinyStake, sig, burnTxHash, 0, burnBlind)
	stakeTx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	chain := makeChainWithGenesis(t, proposerPriv, proposerPub)
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{proposerPub},
		Registry:     registry,
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	buildAndSendBlock(t, proposerPriv, proposerPub, chain, eng, []core.Transaction{stakeTx})
	if waitChainHeight(1, chain, 300*time.Millisecond) {
		t.Error("block with below-minimum stake was accepted — should have been rejected")
	} else {
		t.Log("OK: block with stake below minimum correctly rejected")
	}
}

// TestStakeDeposit_SelfProduced_InvalidStakeEvicted verifies that when the
// mempool contains an invalid stake tx (e.g. UTXO not present in the active set),
// the self-produced block path rejects and evicts it rather than committing a
// canonical block with a stake tx that silently fails registry application.
func TestStakeDeposit_SelfProduced_InvalidStakeEvicted(t *testing.T) {
	proposerPriv, proposerPub, _ := crypto.GenerateValidatorKey()
	stakerPriv, stakerPub, _ := crypto.GenerateValidatorKey()

	const stakeAmt uint64 = 10_000_000_000_000
	var spendPub crypto.Point32
	copy(spendPub[:], []byte(stakerPub))
	burnBlind, _ := crypto.DeterministicMintBlind(spendPub, stakeAmt)
	// commit must match so the payload passes sig check; but UTXO won't be added.
	commit, _ := crypto.Commit(stakeAmt, burnBlind)
	_ = commit

	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0x12
	}
	// UTXOSet is empty — the UTXO is not present.
	utxos := core.NewUTXOSet()

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{proposerPub}, 10_000_000_000_000)

	msg := core.StakeSignMsgV2(core.StakeDeposit, stakerPub, stakeAmt, burnTxHash, 0)
	sig, _ := stakerPriv.Sign(msg)
	extra, _ := core.EncodeStakeExtraV2(core.StakeDeposit, stakerPub, stakeAmt, sig, burnTxHash, 0, burnBlind)
	stakeTx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	// Add the invalid stake tx to the mempool (bypasses UTXO check at admission).
	mp := core.NewMempool(core.DefaultMempoolConfig())
	if err := mp.Add(stakeTx); err != nil {
		t.Fatalf("mp.Add: %v", err)
	}

	chain := makeChainWithGenesis(t, proposerPriv, proposerPub)
	lk, _ := crypto.NewLockedValidatorKey(proposerPriv.Bytes(), nil)
	defer lk.Destroy()

	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{proposerPub},
		MyKey:        lk,
		Registry:     registry,
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// Give the engine enough time to attempt block production and evict.
	// A produced block would advance chain height; a stalled engine would not.
	// After the eviction, the engine should successfully produce a block (without
	// the invalid stake tx) — we wait a bit longer to observe that too.
	time.Sleep(150 * time.Millisecond)

	// The invalid stake tx must have been evicted from the mempool.
	stakeHash := stakeTx.Hash()
	if _, stillPresent := mp.Get(stakeHash); stillPresent {
		t.Error("invalid stake tx is still in the mempool after self-produced block validation — expected eviction")
	} else {
		t.Log("OK: invalid stake tx correctly evicted from mempool")
	}
}

// TestStakeDeposit_IncomingBlock_CommitMismatch verifies that handleIncomingBlock
// rejects a block whose stake payload's blind/amount does not open to the
// on-chain UTXO AmountCommit (C-1 check).
func TestStakeDeposit_IncomingBlock_CommitMismatch(t *testing.T) {
	proposerPriv, proposerPub, _ := crypto.GenerateValidatorKey()
	stakerPriv, stakerPub, _ := crypto.GenerateValidatorKey()

	const stakeAmt uint64 = 10_000_000_000_000
	var spendPub crypto.Point32
	copy(spendPub[:], []byte(stakerPub))

	// Real blind used for the UTXO commitment.
	realBlind, _ := crypto.DeterministicMintBlind(spendPub, stakeAmt)
	realCommit, _ := crypto.Commit(stakeAmt, realBlind)

	// Attacker claims a different blind — commitment mismatch.
	fakeBlind, _ := crypto.DeterministicMintBlind(spendPub, stakeAmt+1) // different amount → different blind

	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0xDD
	}
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: burnTxHash, OutputIndex: 0, OneTimePub: spendPub, AmountCommit: realCommit})

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{proposerPub}, 10_000_000_000_000)

	// Build payload with fakeBlind (opens to wrong commitment).
	msg := core.StakeSignMsgV2(core.StakeDeposit, stakerPub, stakeAmt, burnTxHash, 0)
	sig, _ := stakerPriv.Sign(msg)
	extra, _ := core.EncodeStakeExtraV2(core.StakeDeposit, stakerPub, stakeAmt, sig, burnTxHash, 0, fakeBlind)
	stakeTx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	chain := makeChainWithGenesis(t, proposerPriv, proposerPub)
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{proposerPub},
		Registry:     registry,
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	buildAndSendBlock(t, proposerPriv, proposerPub, chain, eng, []core.Transaction{stakeTx})
	if waitChainHeight(1, chain, 300*time.Millisecond) {
		t.Error("block with AmountCommit mismatch was accepted — should have been rejected (C-1)")
	} else {
		t.Log("OK: block with bad Pedersen commitment correctly rejected (C-1 check)")
	}
}

// TestStakeDeposit_IncomingBlock_TopupBelowMinimum verifies that a top-up
// deposit whose individual amount is below MinStakeNAPR is rejected even for
// an existing validator.  The UTXO must not be mutated (MarkStaked not called).
func TestStakeDeposit_IncomingBlock_TopupBelowMinimum(t *testing.T) {
	proposerPriv, proposerPub, _ := crypto.GenerateValidatorKey()
	stakerPriv, stakerPub, _ := crypto.GenerateValidatorKey()

	// Staker is already registered (seeded in genesis with full stake).
	const tinyTopup uint64 = 1 // 1 nAPRO — well below MinStakeNAPR

	var spendPub crypto.Point32
	copy(spendPub[:], []byte(stakerPub))
	burnBlind, _ := crypto.DeterministicMintBlind(spendPub, tinyTopup)
	commit, _ := crypto.Commit(tinyTopup, burnBlind)

	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0x33
	}
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: burnTxHash, OutputIndex: 0, OneTimePub: spendPub, AmountCommit: commit})

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	// Seed BOTH validators so the staker already exists in the registry.
	registry.InitFromGenesis([]crypto.ValidatorPubKey{proposerPub, stakerPub}, 10_000_000_000_000)

	msg := core.StakeSignMsgV2(core.StakeDeposit, stakerPub, tinyTopup, burnTxHash, 0)
	sig, _ := stakerPriv.Sign(msg)
	extra, _ := core.EncodeStakeExtraV2(core.StakeDeposit, stakerPub, tinyTopup, sig, burnTxHash, 0, burnBlind)
	stakeTx := core.Transaction{Version: core.TxVersionStake, Extra: extra}

	chain := makeChainWithGenesis(t, proposerPriv, proposerPub)
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{proposerPub},
		Registry:     registry,
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	buildAndSendBlock(t, proposerPriv, proposerPub, chain, eng, []core.Transaction{stakeTx})
	if waitChainHeight(1, chain, 300*time.Millisecond) {
		t.Error("block with below-minimum top-up was accepted — should have been rejected")
	} else {
		t.Log("OK: block with existing-validator below-minimum top-up correctly rejected")
	}

	// UTXO must NOT have been staked (MarkStaked must not have been called).
	if utxos.IsStaked(burnTxHash, 0) {
		t.Error("burn UTXO was staked despite block rejection — MarkStaked was called before block validation failed")
	}
	if utxos.Get(burnTxHash, 0) == nil {
		t.Error("burn UTXO was removed from active set despite block rejection — should still be spendable")
	}
	t.Log("burn UTXO correctly unchanged (block rejected before any state mutation)")
}

// TestStakeDeposit_BadCommit_E2E_NeverReachesChain is the full end-to-end test
// that wires a real UTXOSet, Mempool, consensus Engine, and REST Server together
// and confirms that a stake deposit with a mismatched Pedersen commitment is:
//  1. Rejected at the REST handler (422 Unprocessable Entity).
//  2. Never admitted to the mempool.
//  3. Never included in a self-produced block.
//
// This closes the gap between the unit-level handler test
// (TestREST_StakeBroadcast_CommitmentMismatch) and the incoming-block test
// (TestStakeDeposit_IncomingBlock_CommitMismatch) by exercising the full
// REST → mempool → engine path end-to-end.
func TestStakeDeposit_BadCommit_E2E_NeverReachesChain(t *testing.T) {
	// ── 1. Keys ───────────────────────────────────────────────────────────────
	proposerPriv, proposerPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey (proposer):", err)
	}
	stakerPriv, stakerPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey (staker):", err)
	}

	// ── 2. UTXO with the CORRECT commitment ───────────────────────────────────
	const stakeAmountNAPR uint64 = 10_000_000_000_000 // 100 000 APRO

	var spendPub crypto.Point32
	copy(spendPub[:], []byte(stakerPub))

	realBlind, err := crypto.DeterministicMintBlind(spendPub, stakeAmountNAPR)
	if err != nil {
		t.Fatal("DeterministicMintBlind:", err)
	}
	realCommit, err := crypto.Commit(stakeAmountNAPR, realBlind)
	if err != nil {
		t.Fatal("Commit:", err)
	}

	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0x9E
	}
	const burnOutIdx uint32 = 0

	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       burnTxHash,
		OutputIndex:  burnOutIdx,
		OneTimePub:   spendPub,
		AmountCommit: realCommit,
	})

	// ── 3. Shared Mempool and Chain ───────────────────────────────────────────
	mp := core.NewMempool(core.DefaultMempoolConfig())
	chain := makeChainWithGenesis(t, proposerPriv, proposerPub)

	// ── 4. REST Server wired to the same UTXOSet + Mempool + Chain ────────────
	srv := api.NewServer(":0", chain, mp, utxos, newNopLogger())

	// ── 5. Build a stake payload with a WRONG (all-zero) blind ───────────────
	// Commit(amount, zeroBlind) ≠ realCommit so the handler must reject it.
	var wrongBlind crypto.BlindFactor // all zeros
	msg := core.StakeSignMsgV2(core.StakeDeposit, stakerPub, stakeAmountNAPR, burnTxHash, burnOutIdx)
	sig, err := stakerPriv.Sign(msg)
	if err != nil {
		t.Fatal("Sign:", err)
	}
	extra, err := core.EncodeStakeExtraV2(
		core.StakeDeposit, stakerPub, stakeAmountNAPR, sig,
		burnTxHash, burnOutIdx, wrongBlind,
	)
	if err != nil {
		t.Fatal("EncodeStakeExtraV2:", err)
	}
	body := fmt.Sprintf(`{"tx_extra_hex":%q}`, hex.EncodeToString(extra))

	// ── 6. POST to the REST handler ────────────────────────────────────────────
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stake", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	// ── 7. Assert the handler rejected with 422 ───────────────────────────────
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("REST handler status = %d, want 422 (UnprocessableEntity); body = %s",
			rr.Code, rr.Body.String())
	} else {
		t.Log("OK: REST handler rejected bad commitment with 422")
	}
	var respJSON map[string]interface{}
	_ = json.NewDecoder(strings.NewReader(rr.Body.String())).Decode(&respJSON)
	if errMsg, _ := respJSON["error"].(string); !strings.Contains(errMsg, "commitment mismatch") {
		t.Errorf("error message = %q, want it to contain \"commitment mismatch\"", errMsg)
	}

	// ── 8. Mempool must be empty — the tx was never admitted ─────────────────
	if mp.Count() != 0 {
		t.Errorf("mempool.Count() = %d after rejected deposit, want 0", mp.Count())
	} else {
		t.Log("OK: mempool is empty after rejected deposit")
	}

	// ── 9. Wire the Engine and confirm no bad stake tx in the produced block ──
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{proposerPub}, 10_000_000_000_000)

	lk, err := crypto.NewLockedValidatorKey(proposerPriv.Bytes(), nil)
	if err != nil {
		t.Fatal("NewLockedValidatorKey:", err)
	}
	defer lk.Destroy()

	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{proposerPub},
		MyKey:        lk,
		Registry:     registry,
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	var producedBlock *core.Block
	select {
	case producedBlock = <-eng.ProducedCh():
		t.Logf("block produced: height=%d txs=%d", producedBlock.Header.Height, len(producedBlock.Txs))
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: engine did not produce a block in 2s")
	}

	// The produced block must contain no stake tx (mempool was empty).
	for _, tx := range producedBlock.Txs {
		if tx.Version == core.TxVersionStake {
			t.Errorf("produced block contains a stake tx %x — bad commitment deposit leaked into the chain",
				func() []byte { h := tx.Hash(); return h[:8] }())
		}
	}
	t.Log("OK: produced block contains no stake tx — bad commitment deposit never reached the chain")
}

// TestStakeDeposit_SelfProduced_ResumesAfterDuplicateEviction verifies that
// after evicting a duplicate-burn-UTXO stake pair from the mempool, the engine
// successfully produces the next block without those stake txs.
func TestStakeDeposit_SelfProduced_ResumesAfterDuplicateEviction(t *testing.T) {
	proposerPriv, proposerPub, _ := crypto.GenerateValidatorKey()
	staker1Priv, staker1Pub, _ := crypto.GenerateValidatorKey()
	staker2Priv, staker2Pub, _ := crypto.GenerateValidatorKey()

	const stakeAmt uint64 = 10_000_000_000_000
	var spendPub crypto.Point32
	copy(spendPub[:], []byte(staker1Pub))
	burnBlind, _ := crypto.DeterministicMintBlind(spendPub, stakeAmt)
	commit, _ := crypto.Commit(stakeAmt, burnBlind)

	var burnTxHash crypto.Hash32
	for i := range burnTxHash {
		burnTxHash[i] = 0x44
	}
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: burnTxHash, OutputIndex: 0, OneTimePub: spendPub, AmountCommit: commit})

	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{proposerPub}, 10_000_000_000_000)

	// Build two valid stake txs that both claim the same burn UTXO (different validator keys).
	buildStake := func(priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey) core.Transaction {
		msg := core.StakeSignMsgV2(core.StakeDeposit, pub, stakeAmt, burnTxHash, 0)
		sig, _ := priv.Sign(msg)
		extra, _ := core.EncodeStakeExtraV2(core.StakeDeposit, pub, stakeAmt, sig, burnTxHash, 0, burnBlind)
		return core.Transaction{Version: core.TxVersionStake, Extra: extra}
	}
	tx1 := buildStake(staker1Priv, staker1Pub)
	tx2 := buildStake(staker2Priv, staker2Pub)

	mp := core.NewMempool(core.DefaultMempoolConfig())
	if err := mp.Add(tx1); err != nil {
		t.Fatalf("mp.Add tx1: %v", err)
	}
	if err := mp.Add(tx2); err != nil {
		t.Fatalf("mp.Add tx2: %v", err)
	}

	chain := makeChainWithGenesis(t, proposerPriv, proposerPub)
	lk, _ := crypto.NewLockedValidatorKey(proposerPriv.Bytes(), nil)
	defer lk.Destroy()

	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{proposerPub},
		MyKey:        lk,
		Registry:     registry,
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	defer close(stop)

	// The engine should eventually produce a block after evicting the conflicting
	// stake pair (production resumes without the duplicate-UTXO txs).
	select {
	case block := <-eng.ProducedCh():
		t.Logf("OK: block produced after eviction: height=%d txs=%d", block.Header.Height, len(block.Txs))
		// Verify the conflicting stake txs are no longer in the produced block.
		h1 := tx1.Hash()
		h2 := tx2.Hash()
		for _, tx := range block.Txs {
			h := tx.Hash()
			if h == h1 || h == h2 {
				t.Errorf("evicted stake tx %x still appeared in produced block", h[:8])
			}
		}
		// Both stake txs must have been removed from the mempool.
		if _, ok := mp.Get(h1); ok {
			t.Error("stake tx1 still in mempool after eviction")
		}
		if _, ok := mp.Get(h2); ok {
			t.Error("stake tx2 still in mempool after eviction")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: engine did not produce a block after evicting duplicate-burn-UTXO stake pair")
	}
}
