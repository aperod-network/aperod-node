package main

// scan_txidx_test.go — verifies that the startup block scan (runStartupScan)
// writes t/ tx-hash index entries for every transaction in every block it
// processes, and persists the txidx_complete_height marker so a subsequent
// snapshot-fast-path restart knows those blocks are already covered.
//
// Without this behaviour, admin-minted UTXOs whose blocks fall outside the
// in-memory chain window become permanently unspendable after a node restart
// because getTransactionFromDisk (LookupTxIdx) finds no t/ entry and the
// slower u/-store / in-memory fallbacks must cover the gap instead.
//
// Two invariants are checked:
//
//  1. After runStartupScan completes, LookupTxIdx returns a non-nil entry for
//     every transaction hash that appears in the scanned blocks.
//  2. LoadTxIdxCompleteHeight returns the scan's TipHeight (the marker is
//     updated so the background backfill goroutine can resume from that point).

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// buildScanTxIdxChain writes a synthetic chain to a fresh LevelDB store.
// It stores blocks at heights 0 … blockCount using PutRawBlock (bypassing
// storeBlock so that no t/ entries are written by the builder — this
// simulates the pre-PutTxIdx legacy case that the scan must repair).
// Returns the open DB and the height of the tip block.
func buildScanTxIdxChain(
	t *testing.T,
	dir string,
	priv crypto.ValidatorPrivKey,
	pub crypto.ValidatorPubKey,
	blockCount int,
	txsPerBlock int,
) (*store.DB, uint64) {
	t.Helper()

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

	makeTx := func(seed int) core.Transaction {
		blind, berr := crypto.NewBlindFactor()
		if berr != nil {
			t.Fatalf("NewBlindFactor: %v", berr)
		}
		commit, cerr := crypto.Commit(uint64(1_000_000+seed), blind)
		if cerr != nil {
			t.Fatalf("Commit: %v", cerr)
		}
		return core.Transaction{
			Version: 1,
			Outputs: []core.Output{{AmountCommit: commit}},
			Fee:     0,
		}
	}

	// Genesis: height 0, no txs.
	genesis := &core.Block{
		Header: core.BlockHeader{
			Height:       0,
			PrevHash:     crypto.Hash32{},
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		},
	}
	if serr := genesis.Header.Sign(priv); serr != nil {
		t.Fatalf("Sign genesis: %v", serr)
	}
	putBlk(genesis)
	if err := db.PutTip(genesis.Hash(), 0); err != nil {
		t.Fatalf("PutTip genesis: %v", err)
	}

	parent := genesis
	for i := 1; i <= blockCount; i++ {
		txs := make([]core.Transaction, txsPerBlock)
		for j := range txs {
			txs[j] = makeTx(i*1000 + j)
		}
		hdr := core.BlockHeader{
			Height:       uint64(i),
			PrevHash:     parent.Hash(),
			MerkleRoot:   core.MerkleRoot(txs),
			Timestamp:    time.Now().UnixNano() + int64(i)*1_000_000,
			Round:        uint32(i),
			ValidatorPub: pub,
		}
		if serr := hdr.Sign(priv); serr != nil {
			t.Fatalf("Sign h=%d: %v", i, serr)
		}
		blk := &core.Block{Header: hdr, Txs: txs}
		putBlk(blk)
		if err := db.PutTip(blk.Hash(), uint64(i)); err != nil {
			t.Fatalf("PutTip h=%d: %v", i, err)
		}
		parent = blk
	}

	return db, uint64(blockCount)
}

// collectExpectedTxHashes scans the chain store from height 1 through tipHeight
// and returns a map of every tx hash to its (height, txIdx) pair.
func collectExpectedTxHashes(
	t *testing.T,
	db *store.DB,
	tipHeight uint64,
) map[crypto.Hash32][2]uint64 {
	t.Helper()
	expected := make(map[crypto.Hash32][2]uint64)
	for h := uint64(1); h <= tipHeight; h++ {
		raw, err := db.GetRawBlockByHeight(h)
		if err != nil || raw == nil {
			t.Fatalf("GetRawBlockByHeight(%d): %v", h, err)
		}
		var b core.Block
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("unmarshal h=%d: %v", h, err)
		}
		for i, tx := range b.Txs {
			expected[tx.Hash()] = [2]uint64{h, uint64(i)}
		}
	}
	return expected
}

// TestScan_WritesTxIdxEntries checks that runStartupScan writes a t/ LevelDB
// entry for every transaction in every block it processes, enabling
// getTransactionFromDisk (LookupTxIdx) to resolve those txs after restart.
func TestScan_WritesTxIdxEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	const blockCount = 5
	const txsPerBlock = 3

	// Build a chain without t/ entries (simulates pre-PutTxIdx legacy).
	db, tipHeight := buildScanTxIdxChain(t, dir, priv, pub, blockCount, txsPerBlock)

	// Collect the set of expected tx hashes before the scan mutates the DB.
	expected := collectExpectedTxHashes(t, db, tipHeight)
	if len(expected) == 0 {
		t.Fatal("expected map is empty — check buildScanTxIdxChain")
	}

	// Run the startup scan.
	utxos := core.NewUTXOSet()
	reg := core.NewValidatorRegistry()
	tipHashRaw, _, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}
	tipHashHex := fmt.Sprintf("%x", tipHashRaw[:])

	var wg1 sync.WaitGroup
	_, scanErr := runStartupScan(startupScanParams{
		DataDir:    dir,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		DB:         db,
		UTXOs:      utxos,
		Registry:   reg,
		SnapshotWg: &wg1,
		Log:        silentLog(),
	})
	wg1.Wait()
	if scanErr != nil {
		t.Fatalf("runStartupScan: %v", scanErr)
	}

	// Verify that every expected tx hash now has a t/ entry.
	for txHash, loc := range expected {
		entry, lookupErr := db.LookupTxIdx(txHash)
		if lookupErr != nil {
			t.Errorf("LookupTxIdx(%x…): unexpected error: %v", txHash[:4], lookupErr)
			continue
		}
		if entry == nil {
			t.Errorf("LookupTxIdx(%x…): got nil — t/ entry missing after scan (height=%d txIdx=%d)",
				txHash[:4], loc[0], loc[1])
			continue
		}
		if entry.Height != loc[0] {
			t.Errorf("LookupTxIdx(%x…): Height=%d, want %d", txHash[:4], entry.Height, loc[0])
		}
		if uint64(entry.TxIdx) != loc[1] {
			t.Errorf("LookupTxIdx(%x…): TxIdx=%d, want %d", txHash[:4], entry.TxIdx, loc[1])
		}
	}
}

func TestStartupScanApplyBlockFailureIsFatalBeforeIndexing(t *testing.T) {
	dir := t.TempDir()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	db, tipHeight := buildScanTxIdxChain(t, dir, priv, pub, 1, 1)

	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = keys.Spend.Public
	for i := 1; i < len(ring); i++ {
		decoy, genErr := crypto.GenerateWalletKeys()
		if genErr != nil {
			t.Fatalf("GenerateWalletKeys decoy: %v", genErr)
		}
		ring[i] = decoy.Spend.Public
	}
	sig, err := crypto.MLSAGSign(crypto.Hash32{}, ring, 0, keys.Spend.Private)
	if err != nil {
		t.Fatalf("MLSAGSign: %v", err)
	}
	badTx := core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{{
			KeyImage:     sig.KeyImage,
			Ring:         ring,
			AmountCommit: crypto.Commitment{0x7f},
		}},
	}
	badBlock := &core.Block{
		Header: core.BlockHeader{
			Height:       1,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot([]core.Transaction{badTx}),
		},
		Txs: []core.Transaction{badTx},
	}
	if err := badBlock.Header.Sign(priv); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	raw, err := json.Marshal(badBlock)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := db.PutRawBlock(badBlock.Hash(), 1, raw); err != nil {
		t.Fatalf("PutRawBlock: %v", err)
	}
	if err := db.PutTip(badBlock.Hash(), 1); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	_, err = runStartupScan(startupScanParams{
		DataDir:    dir,
		TipHeight:  tipHeight,
		TipHashHex: fmt.Sprintf("%x", badBlock.Hash()),
		DB:         db,
		UTXOs:      core.NewUTXOSet(),
		Registry:   core.NewValidatorRegistry(),
		Log:        silentLog(),
	})
	if err == nil || !strings.Contains(err.Error(), "ApplyBlock failed at height 1") {
		t.Fatalf("runStartupScan error = %v, want fatal ApplyBlock failure", err)
	}
	if entry, lookupErr := db.LookupTxIdx(badTx.Hash()); lookupErr != nil {
		t.Fatalf("LookupTxIdx: %v", lookupErr)
	} else if entry != nil {
		t.Fatal("startup scan indexed transaction after ApplyBlock failure")
	}
}

// TestScan_MissingBlock_HighWaterStopsBeforeGap verifies that when the
// startup scan tolerates a missing block (within maxMissing), the
// txidx_complete_height marker is set to the last height before the gap
// rather than TipHeight.  Without this guard, the background backfill
// goroutine would see the marker at TipHeight and skip the missing block,
// leaving its t/ entry permanently absent.
func TestScan_MissingBlock_HighWaterStopsBeforeGap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// Build a 5-block chain but omit block 3 to simulate a missing block.
	db2, err := store.Open(fmt.Sprintf("%s/chain.db", dir))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db2.Close() })

	makeTx2 := func(seed int) core.Transaction {
		blind, _ := crypto.NewBlindFactor()
		commit, _ := crypto.Commit(uint64(1_000_000+seed), blind)
		return core.Transaction{Version: 1, Outputs: []core.Output{{AmountCommit: commit}}}
	}
	putRaw := func(b *core.Block) {
		raw, _ := json.Marshal(b)
		h := b.Hash()
		if perr := db2.PutRawBlock(h, b.Header.Height, raw); perr != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, perr)
		}
	}

	genesis := &core.Block{Header: core.BlockHeader{
		Height: 0, Timestamp: time.Now().UnixNano(), ValidatorPub: pub,
		MerkleRoot: core.MerkleRoot(nil),
	}}
	if serr := genesis.Header.Sign(priv); serr != nil {
		t.Fatalf("Sign genesis: %v", serr)
	}
	putRaw(genesis)
	if err := db2.PutTip(genesis.Hash(), 0); err != nil {
		t.Fatalf("PutTip genesis: %v", err)
	}

	const tipH = uint64(5)
	const gapH = uint64(3)
	parent := genesis
	for i := uint64(1); i <= tipH; i++ {
		txs := []core.Transaction{makeTx2(int(i))}
		hdr := core.BlockHeader{
			Height: i, PrevHash: parent.Hash(),
			MerkleRoot: core.MerkleRoot(txs),
			Timestamp:  time.Now().UnixNano() + int64(i)*1_000_000,
			Round:      uint32(i), ValidatorPub: pub,
		}
		if serr := hdr.Sign(priv); serr != nil {
			t.Fatalf("Sign h=%d: %v", i, serr)
		}
		blk := &core.Block{Header: hdr, Txs: txs}
		if i != gapH { // omit block 3
			putRaw(blk)
		}
		if i == tipH {
			if err := db2.PutTip(blk.Hash(), tipH); err != nil {
				t.Fatalf("PutTip tip: %v", err)
			}
		}
		parent = blk
	}

	tipHashRaw, _, err := db2.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	var wg sync.WaitGroup
	_, scanErr := runStartupScan(startupScanParams{
		DataDir:          dir,
		TipHeight:        tipH,
		TipHashHex:       fmt.Sprintf("%x", tipHashRaw[:]),
		DB:               db2,
		UTXOs:            core.NewUTXOSet(),
		Registry:         core.NewValidatorRegistry(),
		MaxMissingBlocks: 10, // tolerate the gap so scan completes
		SnapshotWg:       &wg,
		Log:              silentLog(),
	})
	wg.Wait() // ensure snapshot goroutine finishes before LevelDB is closed
	if scanErr != nil {
		t.Fatalf("runStartupScan: %v", scanErr)
	}

	// Marker must stop at gapH-1 (= 2), not at TipHeight (= 5).
	marker, found, loadErr := db2.LoadTxIdxCompleteHeight()
	if loadErr != nil {
		t.Fatalf("LoadTxIdxCompleteHeight: %v", loadErr)
	}
	if !found {
		t.Fatal("txidx_complete_height absent — marker should have been set to gapH-1")
	}
	if marker >= gapH {
		t.Errorf("txidx_complete_height = %d, want < %d (must stop before the gap at height %d)",
			marker, gapH, gapH)
	}
}

// TestScan_CheckpointResume_MarkerNotAdvancedWithoutPriorCoverage verifies
// that when the startup scan resumes only from a suffix (ResumeScanFrom > 0)
// and there is no prior txidx_complete_height marker covering the prefix,
// the marker is NOT advanced to TipHeight.  Advancing it would tell the
// background backfill goroutine that the prefix is already indexed when it
// is not — leaving those t/ entries permanently missing.
func TestScan_CheckpointResume_MarkerNotAdvancedWithoutPriorCoverage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// Build a 6-block chain.  No t/ entries are written (no storeBlock call).
	db, tipHeight := buildScanTxIdxChain(t, dir, priv, pub, 6, 2)

	// No prior marker — prefix (heights 1-3) has never been indexed.
	// Scan resumes only from height 4 (ResumeScanFrom=3).
	tipHashRaw, _, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	var wg2 sync.WaitGroup
	_, scanErr := runStartupScan(startupScanParams{
		DataDir:        dir,
		TipHeight:      tipHeight,
		TipHashHex:     fmt.Sprintf("%x", tipHashRaw[:]),
		DB:             db,
		UTXOs:          core.NewUTXOSet(),
		Registry:       core.NewValidatorRegistry(),
		ResumeScanFrom: 3, // scan covers 4..6 only
		SnapshotWg:     &wg2,
		Log:            silentLog(),
	})
	wg2.Wait()
	if scanErr != nil {
		t.Fatalf("runStartupScan: %v", scanErr)
	}

	// Marker must NOT be at TipHeight — the prefix 1..3 is still unindexed.
	h, found, loadErr := db.LoadTxIdxCompleteHeight()
	if loadErr != nil {
		t.Fatalf("LoadTxIdxCompleteHeight: %v", loadErr)
	}
	if found && h >= tipHeight {
		t.Errorf("txidx_complete_height = %d, want < %d: marker advanced to TipHeight despite uncovered prefix",
			h, tipHeight)
	}
}

// TestScan_CheckpointResume_MarkerAdvancedWhenContiguous verifies the
// complementary case: when the existing marker establishes coverage through
// scanFrom-1, the combined coverage is contiguous and it is safe to advance
// the marker to TipHeight after the suffix scan.
func TestScan_CheckpointResume_MarkerAdvancedWhenContiguous(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	db, tipHeight := buildScanTxIdxChain(t, dir, priv, pub, 6, 2)

	// Simulate a previous full scan that covered heights 1-3 and left the
	// marker at 3.  The current run resumes from 4 (ResumeScanFrom=3).
	if err := db.StoreTxIdxCompleteHeight(3); err != nil {
		t.Fatalf("StoreTxIdxCompleteHeight: %v", err)
	}

	tipHashRaw, _, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	var wg3 sync.WaitGroup
	_, scanErr := runStartupScan(startupScanParams{
		DataDir:        dir,
		TipHeight:      tipHeight,
		TipHashHex:     fmt.Sprintf("%x", tipHashRaw[:]),
		DB:             db,
		UTXOs:          core.NewUTXOSet(),
		Registry:       core.NewValidatorRegistry(),
		ResumeScanFrom: 3, // scan covers 4..6; marker at 3 makes prefix contiguous
		SnapshotWg:     &wg3,
		Log:            silentLog(),
	})
	wg3.Wait()
	if scanErr != nil {
		t.Fatalf("runStartupScan: %v", scanErr)
	}

	// Marker must equal TipHeight now that coverage is 1..6.
	h, found, loadErr := db.LoadTxIdxCompleteHeight()
	if loadErr != nil {
		t.Fatalf("LoadTxIdxCompleteHeight: %v", loadErr)
	}
	if !found {
		t.Fatal("txidx_complete_height absent after contiguous-resume scan")
	}
	if h != tipHeight {
		t.Errorf("txidx_complete_height = %d, want %d (TipHeight)", h, tipHeight)
	}
}

// TestBackfillTxIdxRange_SkipsMissingBlockData verifies that backfillTxIdxRange
// skips a height whose raw-block data is absent from the store (GetRawBlockByHeight
// returns nil, nil) and continues indexing all subsequent blocks.
//
// This matters in production: after --repair-db some heights may have their
// height-index entry (b/ prefix) intact but the corresponding raw-block data
// (r/ prefix) absent.  The old behaviour halted permanently at such a height;
// the new behaviour skips the gap and indexes the remaining chain so the tx
// index marker eventually reaches tipHeight.
func TestBackfillTxIdxRange_SkipsMissingBlockData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// Build a 5-block chain but deliberately omit block at height 3 to
	// simulate raw-block data absent for that height.
	db, err2 := store.Open(fmt.Sprintf("%s/chain.db", dir))
	if err2 != nil {
		t.Fatalf("store.Open: %v", err2)
	}
	t.Cleanup(func() { db.Close() })

	makeTx := func(seed int) core.Transaction {
		blind, berr := crypto.NewBlindFactor()
		if berr != nil {
			t.Fatalf("NewBlindFactor: %v", berr)
		}
		commit, cerr := crypto.Commit(uint64(1_000_000+seed), blind)
		if cerr != nil {
			t.Fatalf("Commit: %v", cerr)
		}
		return core.Transaction{
			Version: 1,
			Outputs: []core.Output{{AmountCommit: commit}},
		}
	}

	genesis := &core.Block{
		Header: core.BlockHeader{
			Height:       0,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		},
	}
	if serr := genesis.Header.Sign(priv); serr != nil {
		t.Fatalf("Sign genesis: %v", serr)
	}
	storeRaw := func(b *core.Block) {
		raw, _ := json.Marshal(b)
		h := b.Hash()
		if perr := db.PutRawBlock(h, b.Header.Height, raw); perr != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, perr)
		}
	}
	storeRaw(genesis)

	parent := genesis
	const totalBlocks = 5
	const missingHeight = 3
	// txsByHeight records the transactions stored at each height for later verification.
	txsByHeight := make(map[int]core.Transaction)
	for i := 1; i <= totalBlocks; i++ {
		txs := []core.Transaction{makeTx(i)}
		hdr := core.BlockHeader{
			Height:       uint64(i),
			PrevHash:     parent.Hash(),
			MerkleRoot:   core.MerkleRoot(txs),
			Timestamp:    time.Now().UnixNano() + int64(i)*1_000_000,
			Round:        uint32(i),
			ValidatorPub: pub,
		}
		if serr := hdr.Sign(priv); serr != nil {
			t.Fatalf("Sign h=%d: %v", i, serr)
		}
		blk := &core.Block{Header: hdr, Txs: txs}
		txsByHeight[i] = txs[0]
		if i != missingHeight { // omit block 3 to simulate a raw-data gap
			storeRaw(blk)
		}
		parent = blk
	}

	// Run the backfill from 0 through 5.  Block 3 data is absent but the
	// backfill should skip it (WARN) and continue indexing 4 and 5.
	backfillTxIdxRange(0, totalBlocks, db, silentLog())

	// Marker must reach totalBlocks (backfill completed despite the gap).
	marker, found, loadErr := db.LoadTxIdxCompleteHeight()
	if loadErr != nil {
		t.Fatalf("LoadTxIdxCompleteHeight: %v", loadErr)
	}
	if !found {
		t.Fatal("txidx_complete_height absent after backfill")
	}
	if marker != uint64(totalBlocks) {
		t.Errorf("txidx_complete_height = %d, want %d (backfill must reach toH despite gap)",
			marker, totalBlocks)
	}

	// Heights 1, 2, 4, 5 must all have t/ entries (backfill continued past gap).
	for _, h := range []int{1, 2, 4, 5} {
		tx := txsByHeight[h]
		entry, lerr := db.LookupTxIdx(tx.Hash())
		if lerr != nil {
			t.Errorf("LookupTxIdx at height %d: %v", h, lerr)
			continue
		}
		if entry == nil {
			t.Errorf("height %d tx not indexed — backfill should have continued past missing block 3", h)
		}
	}

	// Height 3 was missing — its tx is not indexed (nothing to index).
	tx3 := txsByHeight[missingHeight]
	entry3, lerr3 := db.LookupTxIdx(tx3.Hash())
	if lerr3 != nil {
		t.Errorf("LookupTxIdx at height 3: %v", lerr3)
	}
	if entry3 != nil {
		t.Errorf("height 3 tx has t/ entry but the block data was absent — should not be indexed")
	}
}

// TestScan_PersiststxidxCompleteHeight checks that runStartupScan persists the
// txidx_complete_height marker at the scan's TipHeight so the background
// backfill goroutine knows that range is already covered and skips it.
func TestScan_PersiststxidxCompleteHeight(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	const blockCount = 4

	db, tipHeight := buildScanTxIdxChain(t, dir, priv, pub, blockCount, 2)

	// Marker must be absent before the scan.
	h0, found0, err0 := db.LoadTxIdxCompleteHeight()
	if err0 != nil {
		t.Fatalf("LoadTxIdxCompleteHeight before scan: %v", err0)
	}
	if found0 {
		t.Errorf("txidx_complete_height present before scan: got %d, want absent", h0)
	}

	tipHashRaw, _, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}

	var wg4 sync.WaitGroup
	_, scanErr := runStartupScan(startupScanParams{
		DataDir:    dir,
		TipHeight:  tipHeight,
		TipHashHex: fmt.Sprintf("%x", tipHashRaw[:]),
		DB:         db,
		UTXOs:      core.NewUTXOSet(),
		Registry:   core.NewValidatorRegistry(),
		SnapshotWg: &wg4,
		Log:        silentLog(),
	})
	wg4.Wait()
	if scanErr != nil {
		t.Fatalf("runStartupScan: %v", scanErr)
	}

	// Marker must equal TipHeight after the scan.
	h1, found1, err1 := db.LoadTxIdxCompleteHeight()
	if err1 != nil {
		t.Fatalf("LoadTxIdxCompleteHeight after scan: %v", err1)
	}
	if !found1 {
		t.Fatal("txidx_complete_height absent after scan — StoreTxIdxCompleteHeight was not called")
	}
	if h1 != tipHeight {
		t.Errorf("txidx_complete_height = %d, want %d", h1, tipHeight)
	}
}
