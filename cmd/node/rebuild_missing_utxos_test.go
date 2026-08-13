package main

// Tests for rebuildMissingUTXOs — the --repair-db path that re-populates
// the u/ (UTXO) and t/ (tx-hash index) LevelDB families after an OOM-kill +
// RecoverFile cycle silently wipes them.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// buildMintBlock writes a synthetic genesis + one reward block to a fresh DB.
// The reward block (height 1) contains a single coinbase transaction with one
// output — the mint UTXO under test.  Returns the DB, the reward block, and
// the tx hash so callers can locate the specific UTXO.
func buildMintBlock(t *testing.T, dir string) (*store.DB, *core.Block, crypto.Hash32, uint32) {
	t.Helper()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	putBlk := func(b *core.Block) {
		raw, merr := json.Marshal(b)
		if merr != nil {
			t.Fatalf("marshal h=%d: %v", b.Header.Height, merr)
		}
		h := b.Hash()
		if perr := db.PutRawBlock(h, b.Header.Height, raw); perr != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, perr)
		}
	}

	// Genesis block (height 0, no transactions).
	genesis := &core.Block{
		Header: core.BlockHeader{
			Height:     0,
			PrevHash:   crypto.Hash32{},
			Timestamp:  time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot: core.MerkleRoot(nil),
		},
	}
	if serr := genesis.Header.Sign(priv); serr != nil {
		t.Fatalf("Sign genesis: %v", serr)
	}
	putBlk(genesis)

	// Mint output — non-zero OneTimePub so the output is visually distinct.
	var otp crypto.Point32
	otp[0] = 0xCA
	otp[1] = 0xFE
	otp[31] = 0x01

	const outIdx uint32 = 0
	mintTx := core.Transaction{
		Version: core.TxVersionBase,
		Outputs: []core.Output{
			{
				OneTimePub: otp,
				TxPubKey:   otp,
			},
		},
	}
	txs := []core.Transaction{mintTx}
	hdr := core.BlockHeader{
		Height:       1,
		PrevHash:     genesis.Hash(),
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	if serr := hdr.Sign(priv); serr != nil {
		t.Fatalf("Sign h=1: %v", serr)
	}
	rewardBlk := &core.Block{Header: hdr, Txs: txs}
	putBlk(rewardBlk)

	txHash := mintTx.Hash()
	if err := db.PutTip(rewardBlk.Hash(), 1); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	return db, rewardBlk, txHash, outIdx
}

// TestRebuildMissingUTXOs_RestoresDeletedEntry is the primary regression guard:
//  1. A reward block containing a mint output is stored via PutRawBlock.
//  2. The u/ entry is written then manually deleted (simulating SST loss after
//     OOM-kill + LevelDB RecoverFile).
//  3. rebuildMissingUTXOs() is called.
//  4. GetUTXO() must return the entry again (not nil).
//  5. IsUTXOSpent() must be false (the output was never spent).
func TestRebuildMissingUTXOs_RestoresDeletedEntry(t *testing.T) {
	dir := t.TempDir()
	db, _, txHash, outIdx := buildMintBlock(t, dir)

	// Write the UTXO entry (as the node would after accepting the block).
	su := &store.StoredUTXO{
		TxHash:      txHash,
		OutputIndex: outIdx,
		BlockHeight: 1,
	}
	if err := db.PutUTXO(txHash, outIdx, su); err != nil {
		t.Fatalf("PutUTXO (setup): %v", err)
	}

	// Simulate SST loss: delete the u/ entry without marking it spent.
	if err := db.DeleteUTXO(txHash, outIdx); err != nil {
		t.Fatalf("DeleteUTXO (simulate SST loss): %v", err)
	}

	// Confirm the entry is truly gone before the repair.
	before, err := db.GetUTXO(txHash, outIdx)
	if err != nil {
		t.Fatalf("GetUTXO before repair: %v", err)
	}
	if before != nil {
		t.Fatal("expected u/ entry to be absent before repair, but found one")
	}

	// Run the repair.
	n, repairErr := rebuildMissingUTXOs(db, 1, discardLogger())
	if repairErr != nil {
		t.Fatalf("rebuildMissingUTXOs: %v", repairErr)
	}
	if n == 0 {
		t.Fatal("rebuildMissingUTXOs returned 0 restored entries, expected >= 1")
	}

	// Assert the UTXO is back.
	after, err := db.GetUTXO(txHash, outIdx)
	if err != nil {
		t.Fatalf("GetUTXO after repair: %v", err)
	}
	if after == nil {
		t.Fatal("GetUTXO returned nil after rebuildMissingUTXOs — repair did not restore the entry")
	}
	if after.TxHash != txHash {
		t.Errorf("restored UTXO TxHash mismatch: got %x, want %x", after.TxHash[:4], txHash[:4])
	}
	if after.OutputIndex != outIdx {
		t.Errorf("restored UTXO OutputIndex: got %d, want %d", after.OutputIndex, outIdx)
	}
	if after.BlockHeight != 1 {
		t.Errorf("restored UTXO BlockHeight: got %d, want 1", after.BlockHeight)
	}

	// Output must not be flagged as spent.
	if db.IsUTXOSpent(txHash, outIdx) {
		t.Fatal("IsUTXOSpent returned true after repair — entry should be unspent")
	}
}

// TestRebuildMissingUTXOs_SkipsSpentOutput confirms that an output already
// marked as spent (su/ entry present) is NOT re-inserted into the u/ store by
// the repair — preventing phantom spendable UTXOs from appearing.
func TestRebuildMissingUTXOs_SkipsSpentOutput(t *testing.T) {
	dir := t.TempDir()
	db, _, txHash, outIdx := buildMintBlock(t, dir)

	// Mark the output as spent BEFORE the repair.
	if err := db.MarkUTXOSpent(txHash, outIdx); err != nil {
		t.Fatalf("MarkUTXOSpent: %v", err)
	}
	// Do NOT write a u/ entry — the UTXO was removed when spent.

	n, repairErr := rebuildMissingUTXOs(db, 1, discardLogger())
	if repairErr != nil {
		t.Fatalf("rebuildMissingUTXOs: %v", repairErr)
	}

	// The repair should have restored the t/ tx-hash index entry (count >= 0
	// depending on whether PutTxIdx ran) but NOT the u/ UTXO entry.
	_ = n

	after, err := db.GetUTXO(txHash, outIdx)
	if err != nil {
		t.Fatalf("GetUTXO after repair: %v", err)
	}
	if after != nil {
		t.Fatal("rebuildMissingUTXOs re-inserted a spent UTXO — it should have been skipped")
	}
}

// TestRebuildMissingUTXOs_RestoresTxHashIndex confirms that the t/ tx-hash
// index is also re-populated for a block whose t/ entry was lost alongside
// the u/ entries.
func TestRebuildMissingUTXOs_RestoresTxHashIndex(t *testing.T) {
	dir := t.TempDir()
	db, _, txHash, _ := buildMintBlock(t, dir)

	// Confirm no t/ entry exists yet (we never called PutTxIdx in setup).
	beforeIdx, err := db.LookupTxIdx(txHash)
	if err != nil {
		t.Fatalf("LookupTxIdx before repair: %v", err)
	}
	if beforeIdx != nil {
		t.Skip("t/ entry already present — repair skips existing entries (already idempotent)")
	}

	_, repairErr := rebuildMissingUTXOs(db, 1, discardLogger())
	if repairErr != nil {
		t.Fatalf("rebuildMissingUTXOs: %v", repairErr)
	}

	afterIdx, err := db.LookupTxIdx(txHash)
	if err != nil {
		t.Fatalf("LookupTxIdx after repair: %v", err)
	}
	if afterIdx == nil {
		t.Fatal("LookupTxIdx returned nil after rebuildMissingUTXOs — t/ index was not restored")
	}
	if afterIdx.Height != 1 {
		t.Errorf("restored TxIdx.Height: got %d, want 1", afterIdx.Height)
	}
}

// TestRebuildMissingUTXOs_IdempotentOnPresentEntry verifies the repair is safe
// to run more than once: an already-present u/ entry must not be overwritten or
// cause an error.
func TestRebuildMissingUTXOs_IdempotentOnPresentEntry(t *testing.T) {
	dir := t.TempDir()
	db, _, txHash, outIdx := buildMintBlock(t, dir)

	// Write the u/ entry as normal startup would.
	if err := db.PutUTXO(txHash, outIdx, &store.StoredUTXO{
		TxHash:      txHash,
		OutputIndex: outIdx,
		BlockHeight: 1,
	}); err != nil {
		t.Fatalf("PutUTXO (setup): %v", err)
	}

	// First repair pass.
	n1, err := rebuildMissingUTXOs(db, 1, discardLogger())
	if err != nil {
		t.Fatalf("first rebuildMissingUTXOs: %v", err)
	}

	// Second repair pass — should not error and should report 0 new restorations
	// for the u/ entry (it already exists).
	n2, err := rebuildMissingUTXOs(db, 1, discardLogger())
	if err != nil {
		t.Fatalf("second rebuildMissingUTXOs: %v", err)
	}

	// On the first pass the u/ entry exists so 0 UTXO restorations are counted;
	// on either pass the t/ entry may be written once then skipped.
	// Main assertion: no error and the entry is still readable.
	_ = n1
	_ = n2

	final, err := db.GetUTXO(txHash, outIdx)
	if err != nil {
		t.Fatalf("GetUTXO after double repair: %v", err)
	}
	if final == nil {
		t.Fatal("GetUTXO nil after double repair — idempotency violated")
	}
}

// TestRebuildMissingUTXOs_MultiBlockTxIdxGap is the primary regression guard
// for the production wallet-transfer failure caused by missing t/ (tx-hash
// index) entries after an OOM-kill + LevelDB RecoverFile cycle.
//
// The OOM scenario that triggered the original bug:
//   - u/ (UTXO store) and t/ (tx-hash index) entries both live in SST files.
//   - An OOM kill mid-compaction can silently wipe an entire SST file, removing
//     both prefixes at once.
//   - After restart the node reported the UTXO as active (u/ was repaired by
//     an earlier fix) but apr_walletSend still failed with
//     "Balance temporarily unavailable" because LookupTxIdx found no t/ entry
//     and could not fetch the full TX needed for ring construction.
//
// This test simulates the loss of all t/ entries across a multi-block chain
// (by never writing them — functionally identical to post-OOM SST loss) while
// keeping the u/ store intact.  After rebuildMissingUTXOs:
//
//  1. LookupTxIdx must return a valid entry (correct height and tx position)
//     for every transaction in the chain.
//  2. GetUTXO must still return all outputs that were written before the repair
//     (the u/ store must not be disturbed).
//  3. The total count of restored entries returned by rebuildMissingUTXOs must
//     equal the number of transactions in the chain (one t/ entry per tx,
//     since all u/ entries were already present and are skipped).
func TestRebuildMissingUTXOs_MultiBlockTxIdxGap(t *testing.T) {
	t.Parallel()

	const (
		blockCount  = 3
		txsPerBlock = 2
	)

	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// Build the chain without any t/ entries (simulates post-OOM SST loss).
	// buildScanTxIdxChain uses PutRawBlock which writes b/ and h/ entries but
	// never calls PutTxIdx, exactly mirroring OOM loss of the t/ SST file.
	db, tipHeight := buildScanTxIdxChain(t, dir, priv, pub, blockCount, txsPerBlock)

	// Collect all (txHash → {height, txIdx}) pairs from the raw block store
	// so we can verify LookupTxIdx after repair.
	//
	// Simultaneously write the u/ UTXO entry for every output so the repair
	// finds u/ intact — matching the production scenario where u/ survived the
	// OOM but t/ did not.
	type utxoKey struct {
		txHash crypto.Hash32
		outIdx uint32
	}
	writtenUTXOs := make(map[utxoKey]struct{})
	expected := collectExpectedTxHashes(t, db, tipHeight)

	for h := uint64(1); h <= tipHeight; h++ {
		raw, rerr := db.GetRawBlockByHeight(h)
		if rerr != nil || raw == nil {
			t.Fatalf("GetRawBlockByHeight(%d): %v", h, rerr)
		}
		var b core.Block
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("unmarshal h=%d: %v", h, err)
		}
		for txPos, tx := range b.Txs {
			txHash := tx.Hash()
			_ = txPos
			for oi, out := range tx.Outputs {
				su := &store.StoredUTXO{
					TxHash:       txHash,
					OutputIndex:  uint32(oi),
					OneTimePub:   out.OneTimePub,
					TxPubKey:     out.TxPubKey,
					AmountCommit: out.AmountCommit,
					EncAmount:    out.EncAmount,
					BlockHeight:  h,
				}
				if perr := db.PutUTXO(txHash, uint32(oi), su); perr != nil {
					t.Fatalf("PutUTXO h=%d tx=%x oi=%d: %v", h, txHash[:4], oi, perr)
				}
				writtenUTXOs[utxoKey{txHash, uint32(oi)}] = struct{}{}
			}
		}
	}

	// Sanity: confirm no t/ entries exist before the repair.
	for txHash := range expected {
		entry, lerr := db.LookupTxIdx(txHash)
		if lerr != nil {
			t.Fatalf("LookupTxIdx pre-repair unexpected error for %x…: %v", txHash[:4], lerr)
		}
		if entry != nil {
			t.Fatalf("LookupTxIdx pre-repair returned non-nil for %x… — t/ entry should be absent", txHash[:4])
		}
	}

	// Run the repair.
	restored, repairErr := rebuildMissingUTXOs(db, tipHeight, discardLogger())
	if repairErr != nil {
		t.Fatalf("rebuildMissingUTXOs: %v", repairErr)
	}

	// The repair should have written exactly one t/ entry per transaction.
	// u/ entries were already present and are skipped (not counted).
	wantRestored := len(expected) // one t/ entry per unique tx hash
	if restored != wantRestored {
		t.Errorf("rebuildMissingUTXOs restored=%d, want %d (one t/ entry per tx, u/ entries already present)",
			restored, wantRestored)
	}

	// ── Assertion 1: every t/ entry is now resolvable ────────────────────────
	for txHash, loc := range expected {
		wantHeight := loc[0]
		wantTxIdx := loc[1]

		entry, lerr := db.LookupTxIdx(txHash)
		if lerr != nil {
			t.Errorf("LookupTxIdx(%x…) after repair: unexpected error: %v", txHash[:4], lerr)
			continue
		}
		if entry == nil {
			t.Errorf("LookupTxIdx(%x…) after repair: got nil — t/ entry not restored (height=%d txIdx=%d)",
				txHash[:4], wantHeight, wantTxIdx)
			continue
		}
		if entry.Height != wantHeight {
			t.Errorf("LookupTxIdx(%x…) Height=%d, want %d", txHash[:4], entry.Height, wantHeight)
		}
		if uint64(entry.TxIdx) != wantTxIdx {
			t.Errorf("LookupTxIdx(%x…) TxIdx=%d, want %d", txHash[:4], entry.TxIdx, wantTxIdx)
		}
	}

	// ── Assertion 2: u/ entries are undisturbed ───────────────────────────────
	// The repair must not delete or corrupt UTXO entries that were already
	// present (the "skip if existing" branch in rebuildMissingUTXOs).
	for k := range writtenUTXOs {
		u, uerr := db.GetUTXO(k.txHash, k.outIdx)
		if uerr != nil {
			t.Errorf("GetUTXO(%x… oi=%d) after repair: %v", k.txHash[:4], k.outIdx, uerr)
			continue
		}
		if u == nil {
			t.Errorf("GetUTXO(%x… oi=%d) after repair: nil — u/ entry was removed by repair",
				k.txHash[:4], k.outIdx)
			continue
		}
		if u.TxHash != k.txHash {
			t.Errorf("GetUTXO(%x… oi=%d): TxHash mismatch after repair", k.txHash[:4], k.outIdx)
		}
	}

	// ── Assertion 3: repair is idempotent ─────────────────────────────────────
	// Running rebuildMissingUTXOs a second time must not return an error and
	// must restore 0 additional entries (all t/ entries already present).
	restored2, repairErr2 := rebuildMissingUTXOs(db, tipHeight, discardLogger())
	if repairErr2 != nil {
		t.Fatalf("second rebuildMissingUTXOs: %v", repairErr2)
	}
	if restored2 != 0 {
		t.Errorf("second rebuildMissingUTXOs restored=%d, want 0 (all entries already present)", restored2)
	}

	_ = fmt.Sprintf // imported for potential future use
}

// TestRebuildMissingUTXOs_BothPrefixesLostAndGetTransactionSucceeds is the
// end-to-end integration test for the --repair-db scenario described in
// task #1747: both the u/ (UTXO store) and t/ (tx-hash index) LevelDB prefix
// families are wiped simultaneously by an OOM-kill + RecoverFile cycle.
//
// After rebuildMissingUTXOs restores them in a single pass, the disk-path
// GetTransaction flow — LookupTxIdx → GetRawBlockByHeight → unmarshal → index
// into Txs — must succeed and return the correct transaction.
//
// Scenario:
//  1. Write a two-block chain (genesis + reward block with one tx/one output)
//     via PutRawBlock (normal node acceptance path).
//  2. Write the u/ UTXO entry for the reward output (as acceptBlock does).
//     The t/ tx-hash index is intentionally never written — buildMintBlock
//     does not call PutTxIdx, exactly mirroring a post-OOM SST gap.
//  3. Delete the u/ entry via DeleteUTXO (simulating the other SST wipeout).
//     Now both u/ and t/ are absent for that transaction.
//  4. Confirm both are absent before the repair.
//  5. Call rebuildMissingUTXOs (the --repair-db path).
//  6. Assert u/ UTXO entry is restored with correct fields.
//  7. Assert t/ tx-hash index is restored with correct height and tx position.
//  8. Simulate the disk-path GetTransaction: LookupTxIdx to find block+pos,
//     GetRawBlockByHeight to fetch the raw block, unmarshal, and index into
//     Txs — assert the recovered transaction hash matches the original.
func TestRebuildMissingUTXOs_BothPrefixesLostAndGetTransactionSucceeds(t *testing.T) {
	dir := t.TempDir()
	db, rewardBlk, txHash, outIdx := buildMintBlock(t, dir)

	// ── Step 2: write the u/ UTXO entry as acceptBlock would ─────────────────
	// buildMintBlock never calls PutTxIdx, so t/ is absent by design.
	su := &store.StoredUTXO{
		TxHash:      txHash,
		OutputIndex: outIdx,
		BlockHeight: 1,
		OneTimePub:  rewardBlk.Txs[0].Outputs[outIdx].OneTimePub,
		TxPubKey:    rewardBlk.Txs[0].Outputs[outIdx].TxPubKey,
	}
	if err := db.PutUTXO(txHash, outIdx, su); err != nil {
		t.Fatalf("PutUTXO (setup): %v", err)
	}

	// ── Step 3: delete u/ entry to simulate concurrent SST loss ──────────────
	// t/ was never written (step 2 above); u/ is deleted here.
	// Both prefixes are now absent — exactly the post-OOM state.
	if err := db.DeleteUTXO(txHash, outIdx); err != nil {
		t.Fatalf("DeleteUTXO (simulate SST loss): %v", err)
	}

	// ── Step 4: confirm both absent ───────────────────────────────────────────
	beforeUTXO, err := db.GetUTXO(txHash, outIdx)
	if err != nil {
		t.Fatalf("GetUTXO pre-repair: %v", err)
	}
	if beforeUTXO != nil {
		t.Fatal("expected u/ entry to be absent before repair")
	}

	beforeIdx, err := db.LookupTxIdx(txHash)
	if err != nil {
		t.Fatalf("LookupTxIdx pre-repair: %v", err)
	}
	if beforeIdx != nil {
		t.Skip("t/ entry already present before repair — test precondition not met")
	}

	// ── Step 5: call rebuildMissingUTXOs (the --repair-db path) ──────────────
	restored, repairErr := rebuildMissingUTXOs(db, 1, discardLogger())
	if repairErr != nil {
		t.Fatalf("rebuildMissingUTXOs: %v", repairErr)
	}
	// We expect at least two restorations: one u/ entry + one t/ entry.
	if restored < 2 {
		t.Fatalf("rebuildMissingUTXOs restored=%d, want >= 2 (one u/ + one t/ entry)", restored)
	}

	// ── Step 6: u/ UTXO entry restored ───────────────────────────────────────
	afterUTXO, err := db.GetUTXO(txHash, outIdx)
	if err != nil {
		t.Fatalf("GetUTXO after repair: %v", err)
	}
	if afterUTXO == nil {
		t.Fatal("GetUTXO returned nil after repair — u/ entry not restored")
	}
	if afterUTXO.TxHash != txHash {
		t.Errorf("restored UTXO TxHash: got %x…, want %x…", afterUTXO.TxHash[:4], txHash[:4])
	}
	if afterUTXO.OutputIndex != outIdx {
		t.Errorf("restored UTXO OutputIndex: got %d, want %d", afterUTXO.OutputIndex, outIdx)
	}
	if afterUTXO.BlockHeight != 1 {
		t.Errorf("restored UTXO BlockHeight: got %d, want 1", afterUTXO.BlockHeight)
	}
	if db.IsUTXOSpent(txHash, outIdx) {
		t.Fatal("IsUTXOSpent returned true after repair — entry should be unspent")
	}

	// ── Step 7: t/ tx-hash index restored ────────────────────────────────────
	afterIdx, err := db.LookupTxIdx(txHash)
	if err != nil {
		t.Fatalf("LookupTxIdx after repair: %v", err)
	}
	if afterIdx == nil {
		t.Fatal("LookupTxIdx returned nil after repair — t/ index not restored")
	}
	if afterIdx.Height != 1 {
		t.Errorf("restored TxIdx.Height: got %d, want 1", afterIdx.Height)
	}
	if afterIdx.TxIdx != 0 {
		t.Errorf("restored TxIdx.TxIdx: got %d, want 0 (first tx in block)", afterIdx.TxIdx)
	}

	// ── Step 8: disk-path GetTransaction succeeds ─────────────────────────────
	// This mirrors the production flow in getTransactionFromDisk:
	//   LookupTxIdx → GetRawBlockByHeight → unmarshal → Txs[txIdx]
	raw, err := db.GetRawBlockByHeight(afterIdx.Height)
	if err != nil {
		t.Fatalf("GetRawBlockByHeight(%d): %v", afterIdx.Height, err)
	}
	if raw == nil {
		t.Fatalf("GetRawBlockByHeight(%d): nil — block not found", afterIdx.Height)
	}
	var blk core.Block
	if err := json.Unmarshal(raw, &blk); err != nil {
		t.Fatalf("unmarshal block at height %d: %v", afterIdx.Height, err)
	}
	if afterIdx.TxIdx < 0 || afterIdx.TxIdx >= len(blk.Txs) {
		t.Fatalf("TxIdx %d out of range for block with %d txs", afterIdx.TxIdx, len(blk.Txs))
	}
	recoveredTx := blk.Txs[afterIdx.TxIdx]
	recoveredHash := recoveredTx.Hash()
	if recoveredHash != txHash {
		t.Errorf("disk-path GetTransaction: recovered tx hash %x…, want %x…",
			recoveredHash[:4], txHash[:4])
	}
	t.Logf("disk-path GetTransaction succeeded: txHash=%x… at height=%d txIdx=%d",
		txHash[:4], afterIdx.Height, afterIdx.TxIdx)
}
