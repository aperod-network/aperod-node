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

	_, scanErr := runStartupScan(startupScanParams{
		DataDir:    dir,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		DB:         db,
		UTXOs:      utxos,
		Registry:   reg,
		Log:        silentLog(),
	})
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

	_, scanErr := runStartupScan(startupScanParams{
		DataDir:    dir,
		TipHeight:  tipHeight,
		TipHashHex: fmt.Sprintf("%x", tipHashRaw[:]),
		DB:         db,
		UTXOs:      core.NewUTXOSet(),
		Registry:   core.NewValidatorRegistry(),
		Log:        silentLog(),
	})
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
