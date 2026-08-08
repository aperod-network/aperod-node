package main

// Tests for replayPostSnapshotGap — the post-snapshot block-replay gap-fill
// that ensures UTXOs created between the last snapshot and an unclean shutdown
// (OOM-kill, SIGKILL) are present in the in-memory UTXO set after restart.
//
// The tests use the same raw-block persistence path as the production node
// (json.Marshal → PutRawBlock) to verify the replay reads and applies blocks
// correctly.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// silentLog returns a logger that discards all output.
func silentLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// putRawBlk serialises blk as core.Block JSON and writes it via PutRawBlock,
// matching exactly how storeBlock() persists blocks in production.
func putRawBlk(t *testing.T, db *store.DB, blk *core.Block) {
	t.Helper()
	raw, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("marshal block h=%d: %v", blk.Header.Height, err)
	}
	if err := db.PutRawBlock(blk.Hash(), blk.Header.Height, raw); err != nil {
		t.Fatalf("PutRawBlock h=%d: %v", blk.Header.Height, err)
	}
}

// makeSignedBlk returns a signed empty block at the given height.
func makeSignedBlk(t *testing.T, height uint64, prev crypto.Hash32,
	priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey) *core.Block {
	t.Helper()
	hdr := core.BlockHeader{
		Height:       height,
		PrevHash:     prev,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatalf("Sign h=%d: %v", height, err)
	}
	return &core.Block{Header: hdr}
}

// ─── TestReplayPostSnapshotGap_OutputAdded ────────────────────────────────────

// TestReplayPostSnapshotGap_OutputAdded is the primary regression test for
// task #1564.  It simulates the SIGKILL scenario:
//
//  1. Snapshot saved at height 0 (genesis).
//  2. Block 1 is accepted — contains a transparent admin-mint output — and
//     written to LevelDB via PutRawBlock (the production path).
//  3. Node restarts: snapshot restored (height 0), block 1 absent from UTXO set.
//  4. replayPostSnapshotGap replays block 1.
//  5. The admin-mint output is now in the UTXO set.
func TestReplayPostSnapshotGap_OutputAdded(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// Block 0: genesis (snapshot covers up to height 0)
	genesis := makeSignedBlk(t, 0, crypto.Hash32{}, priv, pub)
	putRawBlk(t, db, genesis)

	// Transparent admin-mint output: a distinctive OneTimePub.
	var mintPub crypto.Point32
	mintPub[0] = 0xAD // "admin" sentinel byte

	mintTx := core.Transaction{
		Outputs: []core.Output{
			{OneTimePub: mintPub},
		},
	}
	mintTxHash := mintTx.Hash()

	// Block 1: contains the admin-mint tx — written AFTER snapshot was saved.
	blk1 := makeSignedBlk(t, 1, genesis.Hash(), priv, pub)
	blk1.Txs = []core.Transaction{mintTx}
	blk1.Header.MerkleRoot = core.MerkleRoot(blk1.Txs)
	if err := blk1.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk1: %v", err)
	}
	putRawBlk(t, db, blk1)

	// Simulate post-restart state: UTXO set restored from snapshot at height 0.
	// Block 1's output is NOT in the set yet.
	utxos := core.NewUTXOSet()
	if utxos.Get(mintTxHash, 0) != nil {
		t.Fatal("pre-condition: mint UTXO should not be in UTXO set before gap-fill")
	}

	// Gap-fill: replay blocks 1..1.
	added, spent, complete := replayPostSnapshotGap(db, utxos, core.NewValidatorRegistry(), 0, 1, silentLog())

	if !complete {
		t.Error("replayPostSnapshotGap: expected complete=true, got false")
	}
	if added != 1 {
		t.Errorf("outputs added: want 1, got %d", added)
	}
	if spent != 0 {
		t.Errorf("key images spent: want 0, got %d", spent)
	}

	// The admin-mint UTXO must now be present.
	u := utxos.Get(mintTxHash, 0)
	if u == nil {
		t.Fatal("admin-mint UTXO not found in UTXO set after gap-fill")
	}
	if u.OneTimePub != mintPub {
		t.Errorf("OneTimePub: want %x, got %x", mintPub, u.OneTimePub)
	}
	if u.BlockHeight != 1 {
		t.Errorf("BlockHeight: want 1, got %d", u.BlockHeight)
	}
}

// ─── TestReplayPostSnapshotGap_KeyImageMarkedSpent ───────────────────────────

// TestReplayPostSnapshotGap_KeyImageMarkedSpent verifies that a key image from
// a transaction in the gap window is marked spent after replay.
func TestReplayPostSnapshotGap_KeyImageMarkedSpent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	genesis := makeSignedBlk(t, 0, crypto.Hash32{}, priv, pub)
	putRawBlk(t, db, genesis)

	// A key image that will appear in block 1's transaction input.
	var ki crypto.KeyImage
	ki[0] = 0xBB

	spendTx := core.Transaction{
		Inputs: []core.RingInput{
			{KeyImage: ki},
		},
	}

	blk1 := makeSignedBlk(t, 1, genesis.Hash(), priv, pub)
	blk1.Txs = []core.Transaction{spendTx}
	blk1.Header.MerkleRoot = core.MerkleRoot(blk1.Txs)
	if err := blk1.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk1: %v", err)
	}
	putRawBlk(t, db, blk1)

	utxos := core.NewUTXOSet()

	added, spent, complete := replayPostSnapshotGap(db, utxos, core.NewValidatorRegistry(), 0, 1, silentLog())

	if !complete {
		t.Error("expected complete=true")
	}
	if added != 0 {
		t.Errorf("outputs added: want 0, got %d", added)
	}
	if spent != 1 {
		t.Errorf("key images spent: want 1, got %d", spent)
	}

	// The key image must be marked as spent in the UTXOSet.
	if !utxos.IsSpent(ki) {
		t.Error("key image not marked spent after gap-fill")
	}
}

// ─── TestReplayPostSnapshotGap_NoOp ──────────────────────────────────────────

// TestReplayPostSnapshotGap_NoOp verifies that when snap height == chain tip
// (normal clean shutdown), the function is a no-op: zero outputs added, zero
// key images marked, complete=true.
func TestReplayPostSnapshotGap_NoOp(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	utxos := core.NewUTXOSet()
	// snapTipHeight == chainTipHeight → nothing to replay.
	added, spent, complete := replayPostSnapshotGap(db, utxos, core.NewValidatorRegistry(), 5, 5, silentLog())

	if !complete {
		t.Error("expected complete=true for no-op case")
	}
	if added != 0 || spent != 0 {
		t.Errorf("expected zero work for no-op case; got added=%d spent=%d", added, spent)
	}
}

// ─── TestReplayPostSnapshotGap_MissingBlock ───────────────────────────────────

// TestReplayPostSnapshotGap_MissingBlock verifies that when a block in the gap
// is absent from LevelDB, the function sets complete=false (halts gracefully
// without panicking).
func TestReplayPostSnapshotGap_MissingBlock(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	// Gap is heights 1..3 but LevelDB is empty — all blocks are missing.
	utxos := core.NewUTXOSet()
	_, _, complete := replayPostSnapshotGap(db, utxos, core.NewValidatorRegistry(), 0, 3, silentLog())

	if complete {
		t.Error("expected complete=false when block is missing from DB")
	}
}

// ─── TestReplayPostSnapshotGap_SnapshotUTXOSpentInGap ────────────────────────

// TestReplayPostSnapshotGap_SnapshotUTXOSpentInGap is the regression test for
// the UTXO-removal bug: if a UTXO present in the snapshot is spent by a block
// in the gap window, the old manual-MarkSpent approach would leave the UTXO in
// the active index; the correct ApplyBlock path removes it.
//
// Scenario:
//  1. Snapshot saved at height 1 with a UTXO (oneTimePub=snapshotPub).
//  2. Gap block at height 2 spends that UTXO (ring contains snapshotPub,
//     AmountCommit matches the UTXO's zero commit).
//  3. After gap-fill, the UTXO must be absent from the active index.
func TestReplayPostSnapshotGap_SnapshotUTXOSpentInGap(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// ── Block 1 with a snapshot UTXO ────────────────────────────────────────
	genesis := makeSignedBlk(t, 0, crypto.Hash32{}, priv, pub)
	putRawBlk(t, db, genesis)

	// The UTXO created at height 1 (in the snapshot) — AmountCommit is zero.
	var snapshotPub crypto.Point32
	snapshotPub[0] = 0xCC
	mintTx := core.Transaction{Outputs: []core.Output{{OneTimePub: snapshotPub}}}
	blk1 := makeSignedBlk(t, 1, genesis.Hash(), priv, pub)
	blk1.Txs = []core.Transaction{mintTx}
	blk1.Header.MerkleRoot = core.MerkleRoot(blk1.Txs)
	if err := blk1.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk1: %v", err)
	}
	putRawBlk(t, db, blk1)

	// Build snapshot at height 1 with the UTXO present in the active index.
	utxosAtSnap := core.NewUTXOSet()
	utxosAtSnap.Add(&core.UTXO{
		TxHash:      mintTx.Hash(),
		OutputIndex: 0,
		OneTimePub:  snapshotPub,
		BlockHeight: 1,
		// AmountCommit is zero — matches inp.AmountCommit below.
	})
	reg := core.NewValidatorRegistry()
	reg.SetUTXOSet(utxosAtSnap)
	blk1Hash := blk1.Hash()
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  1,
		TipHashHex: fmt.Sprintf("%x", blk1Hash[:]),
		UTXOs:      utxosAtSnap.TakeSnapshot(),
		Registry:   reg.TakeSnapshot(),
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	// ── Block 2: spends the snapshot UTXO ───────────────────────────────────
	// Generate a valid key image (ComputeKeyImage requires a valid scalar+point).
	kp, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	ki, err := crypto.ComputeKeyImage(kp.Spend.Private, kp.Spend.Public)
	if err != nil {
		t.Fatalf("ComputeKeyImage: %v", err)
	}

	spendTx := core.Transaction{
		Inputs: []core.RingInput{{
			KeyImage: ki,
			// Ring contains only the real UTXO; AmountCommit is zero to match.
			Ring: []crypto.Point32{snapshotPub},
		}},
	}
	blk2 := makeSignedBlk(t, 2, blk1Hash, priv, pub)
	blk2.Txs = []core.Transaction{spendTx}
	blk2.Header.MerkleRoot = core.MerkleRoot(blk2.Txs)
	if err := blk2.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk2: %v", err)
	}
	putRawBlk(t, db, blk2)

	// ── Simulate restart: restore snapshot, run gap-fill ────────────────────
	utxosOnRestart := core.NewUTXOSet()
	gapSnap := findLatestSnapshot(dir, 2, silentLog())
	if gapSnap == nil {
		t.Fatal("findLatestSnapshot returned nil")
	}
	utxosOnRestart.RestoreFromSnapshot(gapSnap.UTXOs)

	// Pre-condition: snapshot UTXO is present before gap-fill.
	if utxosOnRestart.Get(mintTx.Hash(), 0) == nil {
		t.Fatal("pre-condition: snapshot UTXO must be present before gap-fill")
	}

	regOnRestart := core.NewValidatorRegistry()
	regOnRestart.SetUTXOSet(utxosOnRestart)

	added, spent, complete := replayPostSnapshotGap(db, utxosOnRestart, regOnRestart, 1, 2, silentLog())

	if !complete {
		t.Errorf("gap-fill: expected complete=true, got false")
	}
	if spent != 1 {
		t.Errorf("gap-fill: key images spent: want 1, got %d", spent)
	}
	if added != 0 {
		t.Errorf("gap-fill: outputs added: want 0, got %d", added)
	}

	// The critical assertion: the snapshot UTXO must be REMOVED from the
	// active index because it was spent in the gap block.
	// Old code (manual MarkSpent+Add) would leave it as active — this test
	// would fail against that implementation.
	if utxosOnRestart.Get(mintTx.Hash(), 0) != nil {
		t.Error("snapshot UTXO still in active index after gap spend — " +
			"ApplyBlock semantics not applied (would appear in address scans and supply count)")
	}
	// Key image must be marked spent.
	if !utxosOnRestart.IsSpent(ki) {
		t.Error("key image not marked spent after gap-fill")
	}
}

// ─── TestReplayPostSnapshotGap_FailureAtomicity ───────────────────────────────

// TestReplayPostSnapshotGap_FailureAtomicity verifies that when the gap-fill
// fails mid-way (registry error after ApplyBlock modifies UTXO state), the
// caller can restore state from the original snapshot and recover cleanly.
//
// This exercises the rescue-seed path: rescueSnap = gapSnap → rescue path
// calls utxos.RestoreFromSnapshot(gapSnap.UTXOs) → startup scan re-applies
// the failed block from a clean state.
func TestReplayPostSnapshotGap_FailureAtomicity(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	genesis := makeSignedBlk(t, 0, crypto.Hash32{}, priv, pub)
	putRawBlk(t, db, genesis)

	// Snapshot at height 1 with a normal UTXO.
	var normalPub crypto.Point32
	normalPub[0] = 0xDD
	normalTx := core.Transaction{Outputs: []core.Output{{OneTimePub: normalPub}}}
	blk1 := makeSignedBlk(t, 1, genesis.Hash(), priv, pub)
	blk1.Txs = []core.Transaction{normalTx}
	blk1.Header.MerkleRoot = core.MerkleRoot(blk1.Txs)
	if err := blk1.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk1: %v", err)
	}
	putRawBlk(t, db, blk1)

	utxosAtSnap := core.NewUTXOSet()
	utxosAtSnap.Add(&core.UTXO{
		TxHash:      normalTx.Hash(),
		OutputIndex: 0,
		OneTimePub:  normalPub,
		BlockHeight: 1,
	})
	reg := core.NewValidatorRegistry()
	reg.SetUTXOSet(utxosAtSnap)
	blk1Hash := blk1.Hash()
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  1,
		TipHashHex: fmt.Sprintf("%x", blk1Hash[:]),
		UTXOs:      utxosAtSnap.TakeSnapshot(),
		Registry:   reg.TakeSnapshot(),
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	// Gap block at height 2: valid mint output + bad stake tx.
	// ApplyBlock will succeed (adds the output) but ReplayBlockStakeTxs will
	// fail because the stake tx has wrong Extra length.
	var mintPub2 crypto.Point32
	mintPub2[0] = 0xEE
	mintTx2 := core.Transaction{Outputs: []core.Output{{OneTimePub: mintPub2}}}
	badStakeTx := core.Transaction{
		Version: core.TxVersionStake,
		Extra:   []byte("bad-extra-not-valid-length"),
	}
	blk2 := makeSignedBlk(t, 2, blk1Hash, priv, pub)
	blk2.Txs = []core.Transaction{mintTx2, badStakeTx}
	blk2.Header.MerkleRoot = core.MerkleRoot(blk2.Txs)
	if err := blk2.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk2: %v", err)
	}
	putRawBlk(t, db, blk2)

	// Simulate restart: restore snapshot, run gap-fill.
	utxosOnRestart := core.NewUTXOSet()
	gapSnap := findLatestSnapshot(dir, 2, silentLog())
	if gapSnap == nil {
		t.Fatal("findLatestSnapshot returned nil")
	}
	snapUTXOs := gapSnap.UTXOs // save original snapshot contents for later restore
	utxosOnRestart.RestoreFromSnapshot(snapUTXOs)

	regOnRestart := core.NewValidatorRegistry()
	regOnRestart.SetUTXOSet(utxosOnRestart)
	_, _, complete := replayPostSnapshotGap(db, utxosOnRestart, regOnRestart, 1, 2, silentLog())

	// Gap-fill must fail because the stake tx is malformed.
	if complete {
		t.Error("expected complete=false when registry replay fails after ApplyBlock")
	}

	// Simulate rescue path: restore from the original snapshot.
	// This is what the production code does: rescueSnap = gapSnap →
	// utxos.RestoreFromSnapshot(rescueSnap.UTXOs).
	utxosOnRestart.RestoreFromSnapshot(snapUTXOs)

	// After restore, the original snapshot UTXO must be present.
	if utxosOnRestart.Get(normalTx.Hash(), 0) == nil {
		t.Error("snapshot UTXO not present after restore — rescue path cannot recover cleanly")
	}
	// The mint from block 2 (partially applied by ApplyBlock) must be GONE
	// after restore — not a leftover from the failed gap-fill.
	if utxosOnRestart.Get(mintTx2.Hash(), 0) != nil {
		t.Error("partially applied UTXO from failed gap block still present after restore — " +
			"rescue scan would see it as a duplicate")
	}
}

// ─── TestReplayPostSnapshotGap_CorruptBlock ───────────────────────────────────

// TestReplayPostSnapshotGap_CorruptBlock verifies that a block stored with
// corrupt JSON bytes in LevelDB halts the replay (complete=false) instead of
// being silently skipped.  This exercises the "block unmarshal failed —
// halting replay" path added to fix the fail-open skipping bug.
func TestReplayPostSnapshotGap_CorruptBlock(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	genesis := makeSignedBlk(t, 0, crypto.Hash32{}, priv, pub)
	putRawBlk(t, db, genesis)

	// Intentionally store invalid JSON at height 1 under a valid-looking hash.
	var fakeHash crypto.Hash32
	fakeHash[0] = 0xFF
	corruptRaw := []byte(`{this is not valid json`)
	if err := db.PutRawBlock(fakeHash, 1, corruptRaw); err != nil {
		t.Fatalf("PutRawBlock corrupt: %v", err)
	}

	utxos := core.NewUTXOSet()
	_, _, complete := replayPostSnapshotGap(db, utxos, core.NewValidatorRegistry(), 0, 1, silentLog())

	if complete {
		t.Error("expected complete=false for corrupt block JSON, got true — would silently miss UTXOs")
	}
}

// ─── TestReplayPostSnapshotGap_StakeInGap ─────────────────────────────────────

// TestReplayPostSnapshotGap_StakeInGap verifies that when a stake transaction
// is present in a gap block, a registry replay error causes complete=false so
// the caller falls back to the startup scan rather than starting with a stale
// validator registry.  It uses a deliberately malformed stake tx (TxVersionStake
// but invalid Extra length) to trigger a registry error deterministically.
func TestReplayPostSnapshotGap_StakeInGap(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	genesis := makeSignedBlk(t, 0, crypto.Hash32{}, priv, pub)
	putRawBlk(t, db, genesis)

	// A stake tx with wrong Extra length — ReplayBlockStakeTxs returns an error
	// for TxVersionStake with Extra != StakePayloadSize/StakePayloadSizeV2.
	badStakeTx := core.Transaction{
		Version: core.TxVersionStake,
		Extra:   []byte("bad-extra-not-valid-length"),
	}

	blk1 := makeSignedBlk(t, 1, genesis.Hash(), priv, pub)
	blk1.Txs = []core.Transaction{badStakeTx}
	blk1.Header.MerkleRoot = core.MerkleRoot(blk1.Txs)
	if err := blk1.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk1: %v", err)
	}
	putRawBlk(t, db, blk1)

	utxos := core.NewUTXOSet()
	_, _, complete := replayPostSnapshotGap(db, utxos, core.NewValidatorRegistry(), 0, 1, silentLog())

	if complete {
		t.Error("expected complete=false when registry.ReplayBlockStakeTxs fails, got true")
	}
}

// ─── TestSnapshotGapFill_EndToEnd ────────────────────────────────────────────

// TestSnapshotGapFill_EndToEnd is the end-to-end regression test for the
// admin-mint UTXO loss bug (task #1564).
//
// Scenario simulated (unclean shutdown):
//  1. Chain runs to height 1; a periodic snapshot is saved at height 1.
//  2. An admin-mint transaction is written into block 2 (added to LevelDB).
//  3. Node is SIGKILL'd — no SIGTERM snapshot at height 2 is written.
//  4. Node restarts: tryLoadStartupSnapshot(tipHeight=2) fails because the
//     only snapshot on disk is at height 1 (hash mismatch).
//  5. findLatestSnapshot(dataDir, tipHeight=2) discovers the height-1 snapshot.
//  6. replayPostSnapshotGap replays block 2 from raw LevelDB bytes.
//  7. The admin-mint UTXO is now present in the in-memory UTXOSet so address
//     scans and the circulating supply calculation are correct after restart.
func TestSnapshotGapFill_EndToEnd(t *testing.T) {
	dir := t.TempDir()

	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// ── Build block 1 ───────────────────────────────────────────────────────
	// Block 1 contains a normal (non-mint) output; the snapshot is saved here.
	genesis := makeSignedBlk(t, 0, crypto.Hash32{}, priv, pub)
	putRawBlk(t, db, genesis)

	var normalPub crypto.Point32
	normalPub[0] = 0x01
	normalTx := core.Transaction{Outputs: []core.Output{{OneTimePub: normalPub}}}

	blk1 := makeSignedBlk(t, 1, genesis.Hash(), priv, pub)
	blk1.Txs = []core.Transaction{normalTx}
	blk1.Header.MerkleRoot = core.MerkleRoot(blk1.Txs)
	if err := blk1.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk1: %v", err)
	}
	putRawBlk(t, db, blk1)

	// ── Save snapshot at height 1 ───────────────────────────────────────────
	// Simulate the periodic checkpoint saved after block 1 is accepted.
	// The UTXOSet at this point contains only the normalTx output.
	utxosAtSnap := core.NewUTXOSet()
	utxosAtSnap.Add(&core.UTXO{
		TxHash:      normalTx.Hash(),
		OutputIndex: 0,
		OneTimePub:  normalPub,
		BlockHeight: 1,
	})
	reg := core.NewValidatorRegistry()
	reg.SetUTXOSet(utxosAtSnap)

	blk1Hash := blk1.Hash()
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  1,
		TipHashHex: fmt.Sprintf("%x", blk1Hash[:]),
		UTXOs:      utxosAtSnap.TakeSnapshot(),
		Registry:   reg.TakeSnapshot(),
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	// ── Add admin-mint block at height 2 (post-snapshot) ───────────────────
	// A transparent admin-mint uses the bare spendPub as OneTimePub so the
	// address scan can find it without a stealth derivation.
	var mintPub crypto.Point32
	mintPub[0] = 0xAD // "admin" sentinel byte

	mintTx := core.Transaction{
		Outputs: []core.Output{
			{OneTimePub: mintPub},
		},
	}
	mintTxHash := mintTx.Hash()

	blk2 := makeSignedBlk(t, 2, blk1Hash, priv, pub)
	blk2.Txs = []core.Transaction{mintTx}
	blk2.Header.MerkleRoot = core.MerkleRoot(blk2.Txs)
	if err := blk2.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk2: %v", err)
	}
	putRawBlk(t, db, blk2)

	// ── Simulate restart ────────────────────────────────────────────────────
	// The chain tip is now height 2.  tryLoadStartupSnapshot(2) would fail
	// because the only snapshot on disk is at height 1 (different hash).
	// The production code then calls findLatestSnapshot(dataDir, tipHeight=2)
	// which discovers the height-1 snapshot, and then replayPostSnapshotGap
	// replays block 2.
	//
	// We test that sub-path directly here.
	utxosOnRestart := core.NewUTXOSet()

	// findLatestSnapshot finds the highest snapshot below tipHeight=2.
	// It must return the height-1 snapshot saved above.
	gapSnap := findLatestSnapshot(dir, 2, silentLog())
	if gapSnap == nil {
		t.Fatal("findLatestSnapshot returned nil — height-1 snapshot not found")
	}
	if gapSnap.TipHeight != 1 {
		t.Fatalf("findLatestSnapshot: expected TipHeight=1, got %d", gapSnap.TipHeight)
	}

	// Restore snapshot state.
	utxosOnRestart.RestoreFromSnapshot(gapSnap.UTXOs)

	// The normal output from block 1 is in the snapshot and must be visible.
	if utxosOnRestart.Get(normalTx.Hash(), 0) == nil {
		t.Error("normal UTXO from block 1 not restored from snapshot")
	}
	// The admin-mint from block 2 is NOT yet in the UTXOSet (it post-dates the snapshot).
	if utxosOnRestart.Get(mintTxHash, 0) != nil {
		t.Fatal("pre-condition: admin-mint UTXO should not be present before gap-fill")
	}

	// Gap-fill: replay block 2 from raw LevelDB bytes.
	added, spent, complete := replayPostSnapshotGap(db, utxosOnRestart, core.NewValidatorRegistry(), 1, 2, silentLog())

	if !complete {
		t.Error("gap-fill: expected complete=true, got false")
	}
	if added != 1 {
		t.Errorf("gap-fill: outputs added: want 1, got %d", added)
	}
	if spent != 0 {
		t.Errorf("gap-fill: key images spent: want 0, got %d", spent)
	}

	// The admin-mint UTXO must now be in the UTXOSet.
	u := utxosOnRestart.Get(mintTxHash, 0)
	if u == nil {
		t.Fatal("admin-mint UTXO not found after gap-fill — restart would show 0 in circulation")
	}
	if u.OneTimePub != mintPub {
		t.Errorf("OneTimePub: want %x, got %x", mintPub, u.OneTimePub)
	}
	if u.BlockHeight != 2 {
		t.Errorf("BlockHeight: want 2, got %d", u.BlockHeight)
	}

	// The normal UTXO from block 1 must still be present.
	if utxosOnRestart.Get(normalTx.Hash(), 0) == nil {
		t.Error("normal UTXO from block 1 disappeared after gap-fill")
	}
}

// ─── TestReplayPostSnapshotGap_MultiBlock ────────────────────────────────────

// TestReplayPostSnapshotGap_MultiBlock verifies that a multi-block gap is
// replayed correctly: outputs and key images from all gap blocks are included.
func TestReplayPostSnapshotGap_MultiBlock(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// Build a 3-block chain: genesis + blk1 + blk2.
	genesis := makeSignedBlk(t, 0, crypto.Hash32{}, priv, pub)
	putRawBlk(t, db, genesis)

	var pub1 crypto.Point32
	pub1[0] = 0x01
	tx1 := core.Transaction{Outputs: []core.Output{{OneTimePub: pub1}}}

	blk1 := makeSignedBlk(t, 1, genesis.Hash(), priv, pub)
	blk1.Txs = []core.Transaction{tx1}
	blk1.Header.MerkleRoot = core.MerkleRoot(blk1.Txs)
	if err := blk1.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk1: %v", err)
	}
	putRawBlk(t, db, blk1)

	var pub2 crypto.Point32
	pub2[0] = 0x02
	tx2 := core.Transaction{Outputs: []core.Output{{OneTimePub: pub2}}}

	blk2 := makeSignedBlk(t, 2, blk1.Hash(), priv, pub)
	blk2.Txs = []core.Transaction{tx2}
	blk2.Header.MerkleRoot = core.MerkleRoot(blk2.Txs)
	if err := blk2.Header.Sign(priv); err != nil {
		t.Fatalf("re-sign blk2: %v", err)
	}
	putRawBlk(t, db, blk2)

	// Snapshot at height 0; gap covers blocks 1 and 2.
	utxos := core.NewUTXOSet()
	added, _, complete := replayPostSnapshotGap(db, utxos, core.NewValidatorRegistry(), 0, 2, silentLog())

	if !complete {
		t.Error("expected complete=true")
	}
	if added != 2 {
		t.Errorf("outputs added: want 2, got %d", added)
	}
	if utxos.Get(tx1.Hash(), 0) == nil {
		t.Error("UTXO from blk1 missing after gap-fill")
	}
	if utxos.Get(tx2.Hash(), 0) == nil {
		t.Error("UTXO from blk2 missing after gap-fill")
	}
}
