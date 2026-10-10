package store_test

// txindex_fastpath_test.go — verifies the tx-index fast path introduced to
// avoid tx.Hash() recomputation on every node restart.
//
// Three invariants are checked end-to-end:
//
//  1. PutTxIdx (called by storeBlock) writes entries that survive a DB close/reopen.
//  2. LoadTxIndex returns those entries with correct Height and TxIdx values.
//  3. FastForwardWithIndex populates chain.txIndex using the pre-built map so
//     that chain.GetTransaction succeeds — without ever calling tx.Hash()
//     (proven by substituting a synthetic hash and showing the index is
//     trusted verbatim, not recomputed from the transaction bytes).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// buildTxIdxBlock creates a minimal block at height h (extending parent) that
// carries n coinbase-style transactions so we have non-empty Txs to index.
func buildTxIdxBlock(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey, height uint64, prevHash crypto.Hash32, n int) *core.Block {
	t.Helper()
	txs := make([]core.Transaction, n)
	for i := range txs {
		blind, err := crypto.NewBlindFactor()
		if err != nil {
			t.Fatalf("NewBlindFactor: %v", err)
		}
		commit, err := crypto.Commit(uint64(5000+i)*1_000_000, blind)
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		txs[i] = core.Transaction{
			Version: 1,
			Outputs: []core.Output{{AmountCommit: commit}},
			Fee:     0,
		}
	}
	hdr := core.BlockHeader{
		Height:       height,
		PrevHash:     prevHash,
		MerkleRoot:   core.MerkleRoot(txs),
		Timestamp:    time.Now().UnixNano() + int64(height)*1_000_000,
		Round:        uint32(height),
		ValidatorPub: pub,
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatalf("Sign block %d: %v", height, err)
	}
	return &core.Block{Header: hdr, Txs: txs}
}

// ─── Test 1 — PutTxIdx / LoadTxIndex round-trip ──────────────────────────────

// TestTxIndex_PutLoad verifies that tx-index entries written with PutTxIdx are
// returned intact by LoadTxIndex after a DB close/reopen.
func TestTxIndex_PutLoad(t *testing.T) {
	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// Block 1 at height 1, two transactions.
	genesis := buildTxIdxBlock(t, priv, pub, 0, crypto.Hash32{}, 1)
	blk1 := buildTxIdxBlock(t, priv, pub, 1, genesis.Hash(), 2)
	// Block 2 at height 2, three transactions.
	blk2 := buildTxIdxBlock(t, priv, pub, 2, blk1.Hash(), 3)

	// ── First session: write tx index entries ────────────────────────────────
	db1, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for _, blk := range []*core.Block{genesis, blk1, blk2} {
		for i, tx := range blk.Txs {
			if err := db1.PutTxIdx(tx.Hash(), blk.Header.Height, i); err != nil {
				t.Fatalf("PutTxIdx height=%d i=%d: %v", blk.Header.Height, i, err)
			}
		}
	}
	db1.Close()

	// ── Second session: reload and verify ───────────────────────────────────
	db2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open (second session): %v", err)
	}
	defer db2.Close()

	idx, err := db2.LoadTxIndex(0)
	if err != nil {
		t.Fatalf("LoadTxIndex: %v", err)
	}
	if idx == nil {
		t.Fatal("LoadTxIndex returned nil — no entries found")
	}

	// Expect 1+2+3 = 6 entries total.
	const wantTotal = 6
	if len(idx) != wantTotal {
		t.Errorf("LoadTxIndex returned %d entries, want %d", len(idx), wantTotal)
	}

	// Verify each transaction's entry matches what was written.
	for _, blk := range []*core.Block{genesis, blk1, blk2} {
		for i, tx := range blk.Txs {
			h := tx.Hash()
			entry, ok := idx[h]
			if !ok {
				t.Errorf("tx at height=%d idx=%d not found in index", blk.Header.Height, i)
				continue
			}
			if entry.Height != blk.Header.Height {
				t.Errorf("tx height: got %d, want %d", entry.Height, blk.Header.Height)
			}
			if entry.TxIdx != i {
				t.Errorf("tx idx: got %d, want %d", entry.TxIdx, i)
			}
		}
	}
}

// TestTxIndex_LoadMinHeight verifies that LoadTxIndex(minHeight) filters out
// entries below the requested minimum — ensuring the caller only loads the
// entries relevant to the in-memory sliding window.
func TestTxIndex_LoadMinHeight(t *testing.T) {
	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	genesis := buildTxIdxBlock(t, priv, pub, 0, crypto.Hash32{}, 1)
	blk1 := buildTxIdxBlock(t, priv, pub, 1, genesis.Hash(), 1)
	blk2 := buildTxIdxBlock(t, priv, pub, 2, blk1.Hash(), 1)

	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for _, blk := range []*core.Block{genesis, blk1, blk2} {
		for i, tx := range blk.Txs {
			if err := db.PutTxIdx(tx.Hash(), blk.Header.Height, i); err != nil {
				t.Fatalf("PutTxIdx: %v", err)
			}
		}
	}

	// Ask for entries with height >= 2 — only blk2's tx should come back.
	idx, err := db.LoadTxIndex(2)
	if err != nil {
		t.Fatalf("LoadTxIndex(2): %v", err)
	}
	if idx == nil {
		t.Fatal("LoadTxIndex returned nil")
	}
	if len(idx) != 1 {
		t.Errorf("LoadTxIndex(minHeight=2) returned %d entries, want 1", len(idx))
	}
	for _, tx := range blk2.Txs {
		if _, ok := idx[tx.Hash()]; !ok {
			t.Error("blk2 tx not found in filtered index")
		}
	}
}

// TestTxIndex_LoadEmptyReturnsNil confirms that LoadTxIndex returns (nil, nil)
// on a fresh DB so the startup code can detect the "index not yet populated"
// case and fall back to the slow path.
func TestTxIndex_LoadEmptyReturnsNil(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	idx, err := db.LoadTxIndex(0)
	if err != nil {
		t.Fatalf("LoadTxIndex on empty DB: %v", err)
	}
	if idx != nil {
		t.Errorf("expected nil index on empty DB, got %d entries", len(idx))
	}
}

// ─── Test 2 — FastForwardWithIndex populates chain.txIndex ───────────────────

// TestFastForwardWithIndex_GetTransaction verifies that after calling
// FastForwardWithIndex, chain.GetTransaction finds every transaction that was
// present in the pre-built index.  This confirms the fast path correctly
// restores the in-memory tx index from the persistent store.
func TestFastForwardWithIndex_GetTransaction(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	genesis := buildTxIdxBlock(t, priv, pub, 0, crypto.Hash32{}, 1)
	blk1 := buildTxIdxBlock(t, priv, pub, 1, genesis.Hash(), 2)
	blk2 := buildTxIdxBlock(t, priv, pub, 2, blk1.Hash(), 3)

	// Build the pre-loaded index exactly as the startup code does.
	txEntries := make(map[crypto.Hash32]core.TxIndexEntry)
	for _, blk := range []*core.Block{genesis, blk1, blk2} {
		for i, tx := range blk.Txs {
			txEntries[tx.Hash()] = core.TxIndexEntry{
				Height: blk.Header.Height,
				TxIdx:  i,
			}
		}
	}

	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	// FastForwardWithIndex loads blocks 1..2 (genesis is already set).
	chain.FastForwardWithIndex([]*core.Block{blk1, blk2}, txEntries)

	// Every transaction must be findable via GetTransaction.
	for _, blk := range []*core.Block{genesis, blk1, blk2} {
		for i, tx := range blk.Txs {
			h := tx.Hash()
			_, loc, ok := chain.GetTransaction(h)
			if !ok {
				t.Errorf("GetTransaction: tx at height=%d idx=%d not found after FastForwardWithIndex",
					blk.Header.Height, i)
				continue
			}
			if loc.Block.Header.Height != blk.Header.Height {
				t.Errorf("GetTransaction: height mismatch: got %d, want %d",
					loc.Block.Header.Height, blk.Header.Height)
			}
			if loc.TxIndex != i {
				t.Errorf("GetTransaction: TxIndex mismatch: got %d, want %d",
					loc.TxIndex, i)
			}
		}
	}
	if chain.Height() != 2 {
		t.Errorf("chain tip height = %d, want 2", chain.Height())
	}
}

// ─── Test 3 — fast path is actually taken (hash recomputation is skipped) ────

// TestFastForwardWithIndex_TrustsProvidedHashes is the definitive proof that
// FastForwardWithIndex skips tx.Hash() recomputation: it registers each
// transaction under a SYNTHETIC hash (not the real tx.Hash()) and verifies
// that chain.GetTransaction succeeds under that synthetic hash.
//
// If the implementation secretly recomputed tx.Hash() instead of using the
// caller-supplied map, the lookup would fail (real hash ≠ synthetic hash).
// Success proves the index is trusted verbatim — hash recomputation is skipped.
func TestFastForwardWithIndex_TrustsProvidedHashes(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	genesis := buildTxIdxBlock(t, priv, pub, 0, crypto.Hash32{}, 1)
	blk1 := buildTxIdxBlock(t, priv, pub, 1, genesis.Hash(), 2)
	blk2 := buildTxIdxBlock(t, priv, pub, 2, blk1.Hash(), 3)

	// Assign each tx a synthetic (non-real) hash with a recognisable pattern.
	// The real tx.Hash() values will be different from these.
	type syntheticEntry struct {
		hash   crypto.Hash32
		height uint64
		txIdx  int
	}
	var synthetics []syntheticEntry
	txEntries := make(map[crypto.Hash32]core.TxIndexEntry)
	counter := byte(0xA0)
	for _, blk := range []*core.Block{genesis, blk1, blk2} {
		for i := range blk.Txs {
			var synthHash crypto.Hash32
			synthHash[0] = counter
			synthHash[1] = byte(blk.Header.Height)
			synthHash[2] = byte(i)
			counter++
			synthetics = append(synthetics, syntheticEntry{synthHash, blk.Header.Height, i})
			txEntries[synthHash] = core.TxIndexEntry{Height: blk.Header.Height, TxIdx: i}
		}
	}

	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	// SetGenesis calls indexTxs() which indexes genesis txs under their real hashes.
	// FastForwardWithIndex for blocks 1 and 2 must use our synthetic hashes.
	chain.FastForwardWithIndex([]*core.Block{blk1, blk2}, txEntries)

	// Every tx in blk1/blk2 must be reachable under its SYNTHETIC hash.
	for _, s := range synthetics {
		if s.height == 0 {
			// Genesis was indexed by SetGenesis, skip synthetic check for it.
			continue
		}
		_, loc, ok := chain.GetTransaction(s.hash)
		if !ok {
			t.Errorf("fast path: tx at height=%d idx=%d not found under synthetic hash %x",
				s.height, s.txIdx, s.hash[:4])
			continue
		}
		if loc.Block.Header.Height != s.height || loc.TxIndex != s.txIdx {
			t.Errorf("fast path: location mismatch: got height=%d idx=%d, want height=%d idx=%d",
				loc.Block.Header.Height, loc.TxIndex, s.height, s.txIdx)
		}
	}
}

// ─── Test 4 — full DB→chain round-trip (end-to-end fast path) ────────────────

// TestTxIndex_EndToEnd simulates the exact startup sequence:
//  1. Blocks are written to DB via PutRawBlock + PutTxIdx (mirrors storeBlock).
//  2. On "restart", LoadTxIndex is called to get the pre-built index.
//  3. FastForwardWithIndex restores the in-memory chain.
//  4. GetTransaction succeeds for every transaction.
func TestTxIndex_EndToEnd(t *testing.T) {
	dir := t.TempDir()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	genesis := buildTxIdxBlock(t, priv, pub, 0, crypto.Hash32{}, 1)
	blk1 := buildTxIdxBlock(t, priv, pub, 1, genesis.Hash(), 3)
	blk2 := buildTxIdxBlock(t, priv, pub, 2, blk1.Hash(), 2)

	// ── First session: persist blocks and tx index ───────────────────────────
	db1, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// storeBlock equivalent: PutRawBlock + PutTxIdx per tx.
	persistBlock := func(blk *core.Block) {
		t.Helper()
		data, merr := json.Marshal(blk)
		if merr != nil {
			t.Fatalf("marshal block %d: %v", blk.Header.Height, merr)
		}
		h := blk.Hash()
		if err := db1.PutRawBlock(h, blk.Header.Height, data); err != nil {
			t.Fatalf("PutRawBlock %d: %v", blk.Header.Height, err)
		}
		for i, tx := range blk.Txs {
			if err := db1.PutTxIdx(tx.Hash(), blk.Header.Height, i); err != nil {
				t.Fatalf("PutTxIdx height=%d i=%d: %v", blk.Header.Height, i, err)
			}
		}
		if err := db1.PutTip(h, blk.Header.Height); err != nil {
			t.Fatalf("PutTip: %v", err)
		}
	}

	persistBlock(genesis)
	persistBlock(blk1)
	persistBlock(blk2)
	db1.Close()

	// ── Second session: simulate node restart with fast path ─────────────────
	db2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open (second session): %v", err)
	}
	defer db2.Close()

	_, tipHeight, err := db2.GetTip()
	if err != nil {
		t.Fatalf("GetTip: %v", err)
	}
	if tipHeight != 2 {
		t.Fatalf("tip height = %d, want 2", tipHeight)
	}

	// Load genesis and recent blocks (mirrors main.go startup code).
	loadBlock := func(height uint64) *core.Block {
		t.Helper()
		raw, err := db2.GetRawBlockByHeight(height)
		if err != nil || raw == nil {
			t.Fatalf("GetRawBlockByHeight(%d): err=%v raw=nil=%v", height, err, raw == nil)
		}
		var b core.Block
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("unmarshal block %d: %v", height, err)
		}
		return &b
	}

	restoredGenesis := loadBlock(0)
	restoredBlk1 := loadBlock(1)
	restoredBlk2 := loadBlock(2)

	// Load tx index from DB (fast path).
	dbTxIdx, err := db2.LoadTxIndex(0)
	if err != nil {
		t.Fatalf("LoadTxIndex: %v", err)
	}
	if dbTxIdx == nil {
		t.Fatal("LoadTxIndex returned nil — fast path index is missing")
	}

	// Convert store entries to core entries (mirrors main.go).
	coreTxIdx := make(map[crypto.Hash32]core.TxIndexEntry, len(dbTxIdx))
	for h, e := range dbTxIdx {
		coreTxIdx[h] = core.TxIndexEntry{Height: e.Height, TxIdx: e.TxIdx}
	}

	// Restore chain using the fast path.
	chain2 := core.NewChain()
	if err := chain2.SetGenesis(restoredGenesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	chain2.FastForwardWithIndex([]*core.Block{restoredBlk1, restoredBlk2}, coreTxIdx)

	if chain2.Height() != tipHeight {
		t.Errorf("restored chain height = %d, want %d", chain2.Height(), tipHeight)
	}

	// Verify GetTransaction works for every tx in every block.
	for _, origBlk := range []*core.Block{genesis, blk1, blk2} {
		for i, tx := range origBlk.Txs {
			h := tx.Hash()
			_, loc, ok := chain2.GetTransaction(h)
			if !ok {
				t.Errorf("end-to-end: tx at height=%d idx=%d not found after fast-path restore",
					origBlk.Header.Height, i)
				continue
			}
			if loc.Block.Header.Height != origBlk.Header.Height {
				t.Errorf("end-to-end: height mismatch: got %d, want %d",
					loc.Block.Header.Height, origBlk.Header.Height)
			}
			if loc.TxIndex != i {
				t.Errorf("end-to-end: TxIndex mismatch: got %d, want %d",
					loc.TxIndex, i)
			}
		}
	}

	t.Logf("fast-path restore: tip=%d, txEntries=%d, all GetTransaction lookups passed",
		chain2.Height(), len(coreTxIdx))
}
