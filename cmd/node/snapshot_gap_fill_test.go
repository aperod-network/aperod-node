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
	added, spent, complete := replayPostSnapshotGap(db, utxos, 0, 1, silentLog())

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

	added, spent, complete := replayPostSnapshotGap(db, utxos, 0, 1, silentLog())

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
	added, spent, complete := replayPostSnapshotGap(db, utxos, 5, 5, silentLog())

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
	_, _, complete := replayPostSnapshotGap(db, utxos, 0, 3, silentLog())

	if complete {
		t.Error("expected complete=false when block is missing from DB")
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
	added, _, complete := replayPostSnapshotGap(db, utxos, 0, 2, silentLog())

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
