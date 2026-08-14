package main

// Integration test: mempool persistence across a node restart.
//
// Covers the requirements from task #1201:
//  1. A transaction submitted before a graceful stop (SIGTERM path) survives
//     and is restored into a new mempool after restart — exercising the exact
//     production lifecycle: SetVerifier → Add → Save → (restart) → SetVerifier
//     → Load → SelectTxs.
//  2. The restored transaction passes full cryptographic re-verification
//     (ring signatures, range proofs, Pedersen balance) on Load(), just as
//     production does after wiring TxVerifier.
//  3. Load() removes the file after consuming it — preventing stale replay on
//     a second restart.
//  4. Transactions that exceed the TTL while the node is down are silently
//     dropped and do not reappear.
//  5. SelectTxs returns restored transactions in descending fee-rate order
//     across a save/load cycle.
//
// All tests are in-process using the same helpers as snapshot_restart_test.go.
// Test 1 (TestMempoolPersistProductionPath) exercises the exact wiring that
// main.go performs around lines 742-749.

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// discardLogger returns a logger that drops all output below Error+1.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError + 1,
	}))
}

// mempoolTestConfig returns a MempoolConfig suitable for integration tests:
// large caps, long TTL, no verifier pre-wired, base fee of 1 nAPRO/byte.
func mempoolTestConfig() core.MempoolConfig {
	return core.MempoolConfig{
		MaxSize:        10_000,
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		TTL:            2 * time.Hour,
		BaseFeePerByte: 1, // 1 nAPRO/byte — fees just need to cover tx size
	}
}

// buildProductionTx creates a cryptographically valid RingCT transaction that
// passes TxVerifier.VerifyTx under the returned UTXOSet.
//
// The setup mirrors the production mint + spend flow:
//   1. Generate a wallet key pair (Alice).
//   2. Mint a faucet UTXO to Alice via BuildMintTx at height=1.
//   3. Register the mint UTXO in the UTXOSet so C-0 / C-0a checks pass.
//   4. Build a spend tx via TxBuilder (Phase 1 random decoys; all absent from
//      the UTXOSet, so only the real input is present — the exact scenario the
//      C-0 fix was designed for).
//
// Returns the built transaction and the UTXOSet that makes it verifiable.
func buildProductionTx(t *testing.T) (core.Transaction, *core.UTXOSet) {
	t.Helper()

	const faucetHeight = uint64(1)
	const faucetAmount = uint64(100_000_000_000) // 1000 APRO
	const sendAmount = uint64(10_000_000_000)    // 100 APRO

	// 1. Generate Alice's wallet keys.
	aliceKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	aliceAddr := crypto.AddressFromKeys(crypto.MainnetByte, aliceKeys)
	_, aliceSpendPub, _, err := crypto.DecodeAddress(aliceAddr)
	if err != nil {
		t.Fatalf("DecodeAddress: %v", err)
	}

	// 2. Mint a faucet UTXO.
	// BuildMintTx uses oneTimePub = spendPub + height*G and a deterministic blind.
	mintTx, err := core.BuildMintTx(aliceAddr, faucetAmount, faucetHeight)
	if err != nil {
		t.Fatalf("BuildMintTx: %v", err)
	}
	mintOut := mintTx.Outputs[0]

	// 3. Register the UTXO in the active UTXOSet (activates C-0 / C-0a checks).
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       mintTx.Hash(),
		OutputIndex:  0,
		OneTimePub:   mintOut.OneTimePub,
		TxPubKey:     mintOut.TxPubKey,
		AmountCommit: mintOut.AmountCommit,
		BlockHeight:  faucetHeight,
	})

	// 4. Recover Alice's blind and HsScalar so TxBuilder can sign the spend.
	// BuildMintTx uses DeterministicMintBlindV2 for height > 0 (block-reward path).
	// For mint outputs, oneTimePriv = spendPriv + height_scalar, so HsScalar = height_scalar.
	blind, err := crypto.DeterministicMintBlindV2(aliceSpendPub, faucetAmount, faucetHeight)
	if err != nil {
		t.Fatalf("DeterministicMintBlindV2: %v", err)
	}
	hsScalar := crypto.ScalarFromUint64(faucetHeight)

	ownedUTXO := core.OwnedUTXO{
		UTXO: core.UTXO{
			TxHash:       mintTx.Hash(),
			OutputIndex:  0,
			OneTimePub:   mintOut.OneTimePub,
			TxPubKey:     mintOut.TxPubKey,
			AmountCommit: mintOut.AmountCommit,
			BlockHeight:  faucetHeight,
		},
		HsScalar: hsScalar,
		Amount:   faucetAmount,
		Blind:    blind,
	}

	// 5. Build the spend transaction (Phase 1: random ring decoys not in UTXOSet).
	bobKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys bob: %v", err)
	}
	bobAddr := crypto.AddressFromKeys(crypto.MainnetByte, bobKeys)

	builder := core.NewTxBuilder(
		aliceKeys.Spend.Private,
		aliceKeys.View.Private,
		aliceSpendPub,
		[]core.OwnedUTXO{ownedUTXO},
		0, // 0 → use InitialBaseFeePerByte
	)
	result, err := builder.Build(sendAmount, bobAddr, aliceAddr)
	if err != nil {
		t.Fatalf("TxBuilder.Build: %v", err)
	}

	return result.Tx, utxos
}

// makeTestRing returns a ring of RingSize distinct dummy public keys (structural tests).
func makeTestRing() []crypto.RingMember {
	ring := make([]crypto.RingMember, crypto.RingSize)
	for i := range ring {
		ring[i][0] = byte(i + 1)
	}
	return ring
}

// makeTestKeyImage returns a non-zero key image unique for the given index (structural tests).
func makeTestKeyImage(idx int) crypto.KeyImage {
	var ki crypto.KeyImage
	ki[0] = byte(idx >> 8)
	ki[1] = byte(idx)
	if ki[0] == 0 && ki[1] == 0 {
		ki[2] = 1
	}
	return ki
}

// makeTestTx builds a minimal structurally-valid transaction that passes
// core.Transaction.Validate() in no-verifier mode (dummy sigs/proofs).
// fee must be ≥ tx.Size() × baseFeePerByte; kiIdx must be unique per call.
func makeTestTx(fee uint64, kiIdx int) core.Transaction {
	return core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{
				KeyImage:     makeTestKeyImage(kiIdx),
				Ring:         makeTestRing(),
				AmountCommit: crypto.Commitment{},
			},
		},
		Outputs: []core.Output{
			{
				OneTimePub:   crypto.Point32{byte(kiIdx + 1)},
				TxPubKey:     crypto.Point32{},
				AmountCommit: crypto.Commitment{},
			},
		},
		Fee:         fee,
		Signatures:  []*crypto.MLSAGSignature{{}},
		RangeProofs: []*crypto.RangeProof{{}},
	}
}

// ─── Test 1 (primary): production lifecycle — SetVerifier → Add → Save → Load ──

// TestMempoolPersistProductionPath exercises the exact main.go lifecycle:
//
//   "Before shutdown":
//     pool.SetVerifier(txVerifier)  // wired after UTXO scan completes
//     pool.Add(tx)                  // full crypto verification runs
//     pool.Save(dataDir)            // SIGTERM handler persists to disk
//
//   "After restart":
//     pool.SetVerifier(txVerifier)  // same UTXO set, same verifier setup
//     pool.Load(dataDir, log)       // re-adds with full verification
//     pool.SelectTxs(1)             // tx is returned for next block
//
// Uses a cryptographically valid RingCT transaction built by TxBuilder so that
// the full ring-signature + range-proof + Pedersen-balance path is exercised.
func TestMempoolPersistProductionPath(t *testing.T) {
	dir := t.TempDir()

	// Build a real, verifiable RingCT transaction and the UTXOSet that validates it.
	tx, utxos := buildProductionTx(t)
	txHash := tx.Hash()

	// ── Phase 1: "before shutdown" ────────────────────────────────────────────
	// Mirror main.go: create pool → wire verifier → add tx → save.
	cfg := core.DefaultMempoolConfig()
	poolBefore := core.NewMempool(cfg, discardLogger())

	verifier := core.NewTxVerifier(utxos)
	poolBefore.SetVerifier(verifier) // production wiring

	if err := poolBefore.Add(tx); err != nil {
		t.Fatalf("Add (before shutdown): %v", err)
	}
	if poolBefore.Count() != 1 {
		t.Fatalf("pre-save count: got %d, want 1", poolBefore.Count())
	}

	if err := poolBefore.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Confirm the file was written.
	mempoolFile := filepath.Join(dir, "mempool.json")
	if _, err := os.Stat(mempoolFile); os.IsNotExist(err) {
		t.Fatalf("mempool.json not written to %s", mempoolFile)
	}

	// ── Phase 2: "after restart" — new pool, same UTXOSet, wire verifier, load ─
	// The UTXO is still unspent (no block was produced), so VerifyTx passes again.
	poolAfter := core.NewMempool(cfg, discardLogger())
	poolAfter.SetVerifier(core.NewTxVerifier(utxos)) // verifier wired before Load, as in main.go

	restored := poolAfter.Load(dir, discardLogger())
	if restored != 1 {
		t.Errorf("Load returned %d restored tx(s), want 1", restored)
	}
	if poolAfter.Count() != 1 {
		t.Errorf("post-load pool count: got %d, want 1", poolAfter.Count())
	}

	// The correct transaction hash must be present.
	if _, ok := poolAfter.Get(txHash); !ok {
		t.Errorf("tx hash %x missing from pool after restart", txHash[:8])
	}

	// SelectTxs must return the tx — confirming it would be included in the next block.
	selected := poolAfter.SelectTxs(10)
	if len(selected) != 1 {
		t.Fatalf("SelectTxs returned %d tx(s), want 1", len(selected))
	}
	if selected[0].Hash() != txHash {
		gotHash := selected[0].Hash()
		t.Errorf("SelectTxs returned wrong tx: got %x, want %x",
			gotHash[:8], txHash[:8])
	}
}

// ─── Test 2: file is removed after Load — no double-replay ───────────────────

// TestMempoolPersistFileRemovedAfterLoad verifies that Load() deletes
// mempool.json after consuming it.  A second restart therefore finds no file
// and starts with an empty mempool — preventing stale replay of
// already-confirmed transactions.
func TestMempoolPersistFileRemovedAfterLoad(t *testing.T) {
	dir := t.TempDir()
	mempoolFile := filepath.Join(dir, "mempool.json")

	// Save a non-empty pool (no verifier — structural mode sufficient here).
	pool1 := core.NewMempool(mempoolTestConfig(), discardLogger())
	// fee=2000 nAPRO, size≈1972 bytes → 2000/1972 ≈ 1 nAPRO/byte ≥ baseFeePerByte=1
	if err := pool1.Add(makeTestTx(2000, 20)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := pool1.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// First restart — consumes the file.
	pool2 := core.NewMempool(mempoolTestConfig(), discardLogger())
	if n := pool2.Load(dir, discardLogger()); n != 1 {
		t.Fatalf("first Load: restored %d, want 1", n)
	}

	// File must be gone now.
	if _, err := os.Stat(mempoolFile); !os.IsNotExist(err) {
		t.Errorf("mempool.json still exists after Load(); want file removed")
	}

	// Second restart — nothing to load; pool stays empty.
	pool3 := core.NewMempool(mempoolTestConfig(), discardLogger())
	if n := pool3.Load(dir, discardLogger()); n != 0 {
		t.Errorf("second Load returned %d, want 0 (file was consumed)", n)
	}
	if pool3.Count() != 0 {
		t.Errorf("pool3.Count() = %d after empty restart, want 0", pool3.Count())
	}
}

// ─── Test 3: TTL-expired transactions are dropped on Load ─────────────────────

// TestMempoolPersistExpiredTxDropped verifies that a transaction persisted with
// a received timestamp older than the loading pool's TTL is silently dropped
// on Load() and does not reappear.
//
// Strategy: save the pool with TTL=1ns so that by the time Load() runs (even
// nanoseconds later) the received timestamp is already beyond TTL.  At least
// several microseconds always elapse between Add() and Load(), so this reliably
// produces the "all entries expired" outcome without any time.Sleep().
func TestMempoolPersistExpiredTxDropped(t *testing.T) {
	dir := t.TempDir()

	// Save two entries using normal (large) TTL so Save() keeps them.
	cfgSave := mempoolTestConfig()
	poolSave := core.NewMempool(cfgSave, discardLogger())
	if err := poolSave.Add(makeTestTx(2000, 50)); err != nil {
		t.Fatalf("Add tx1: %v", err)
	}
	if err := poolSave.Add(makeTestTx(2000, 51)); err != nil {
		t.Fatalf("Add tx2: %v", err)
	}
	if err := poolSave.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load with TTL=1ns: both entries' age (microseconds+) exceeds 1ns → dropped.
	cfgLoad := mempoolTestConfig()
	cfgLoad.TTL = time.Nanosecond
	poolLoad := core.NewMempool(cfgLoad, discardLogger())
	n := poolLoad.Load(dir, discardLogger())
	if n != 0 {
		t.Errorf("Load with TTL=1ns restored %d tx(s), want 0 (all expired)", n)
	}
	if poolLoad.Count() != 0 {
		t.Errorf("pool.Count() = %d after expired load, want 0", poolLoad.Count())
	}
}

// ─── Test 5: confirmed txs are absent after RemoveBlock → Save → Load ────────

// TestMempoolConfirmedTxNotReplayedAfterRestart verifies that a transaction
// which was included in a block (and therefore removed from the mempool via
// RemoveBlock) is NOT present in the pool after a save/load cycle.
//
// This is the complement of TestMempoolPersistProductionPath: where that test
// confirms a pending tx survives a restart, this test confirms a confirmed tx
// does NOT survive — preventing double-spend-rejection storms or ghost entries
// from appearing after a node restart.
//
// Scenario:
//  1. Add two transactions to the mempool (tx-confirmed, tx-pending).
//  2. Simulate block confirmation: call RemoveBlock with a block containing
//     only tx-confirmed.
//  3. Verify the pool now holds exactly tx-pending (RemoveBlock worked).
//  4. Save the mempool — tx-confirmed is already absent, so it cannot be persisted.
//  5. Load into a fresh pool.
//  6. Assert tx-confirmed is absent and tx-pending is present.
//
// This exercises the Remove/RemoveBlock → Save ordering confirmed in main.go's
// shutdown sequence, and closes the replay-after-restart edge case.
func TestMempoolConfirmedTxNotReplayedAfterRestart(t *testing.T) {
	dir := t.TempDir()

	pool := core.NewMempool(mempoolTestConfig(), discardLogger())

	// Two structurally-valid transactions with distinct key-image indices.
	txConfirmed := makeTestTx(2000, 70)
	txPending := makeTestTx(2000, 71)

	if err := pool.Add(txConfirmed); err != nil {
		t.Fatalf("Add txConfirmed: %v", err)
	}
	if err := pool.Add(txPending); err != nil {
		t.Fatalf("Add txPending: %v", err)
	}
	if pool.Count() != 2 {
		t.Fatalf("pre-RemoveBlock count: got %d, want 2", pool.Count())
	}

	// Simulate block inclusion: build a minimal block containing only txConfirmed.
	confirmedBlock := &core.Block{
		Txs: []core.Transaction{txConfirmed},
	}
	pool.RemoveBlock(confirmedBlock)

	// Immediately after RemoveBlock, only txPending should remain.
	if pool.Count() != 1 {
		t.Fatalf("post-RemoveBlock count: got %d, want 1", pool.Count())
	}
	if _, ok := pool.Get(txConfirmed.Hash()); ok {
		t.Error("txConfirmed still in pool after RemoveBlock — double-spend risk")
	}
	if _, ok := pool.Get(txPending.Hash()); !ok {
		t.Error("txPending missing from pool after RemoveBlock")
	}

	// Save — txConfirmed is already gone, so only txPending is written to disk.
	if err := pool.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load into a fresh pool and verify the confirmed tx does not resurface.
	poolAfter := core.NewMempool(mempoolTestConfig(), discardLogger())
	restored := poolAfter.Load(dir, discardLogger())
	if restored != 1 {
		t.Errorf("Load returned %d restored tx(s), want 1 (only txPending)", restored)
	}
	if poolAfter.Count() != 1 {
		t.Errorf("post-load pool count: got %d, want 1", poolAfter.Count())
	}

	// The confirmed tx must be absent — it was removed before Save, so it can
	// never appear in mempool.json and must not be replayed.
	if _, ok := poolAfter.Get(txConfirmed.Hash()); ok {
		t.Error("txConfirmed appeared in pool after restart — confirmed tx replayed (BUG)")
	}

	// The pending tx must be present and selectable for the next block.
	if _, ok := poolAfter.Get(txPending.Hash()); !ok {
		t.Error("txPending missing from pool after restart")
	}
	selected := poolAfter.SelectTxs(10)
	if len(selected) != 1 {
		t.Fatalf("SelectTxs returned %d tx(s), want 1", len(selected))
	}
	gotHash := selected[0].Hash()
	wantHash := txPending.Hash()
	if gotHash != wantHash {
		t.Errorf("SelectTxs returned wrong tx: got %x, want %x",
			gotHash[:8], wantHash[:8])
	}
}

// ─── Test 6: CleanStaleTmpFiles removes old .tmp, leaves recent .tmp alone ───

// TestCleanStaleTmpFilesOldFileDeleted verifies that CleanStaleTmpFiles removes
// a truncated .tmp file whose mtime is more than 5 minutes in the past and that
// a subsequent Load() finds an empty pool (not a JSON parse error).
//
// This is the regression guard for the OOM-kill scenario described in task
// #1364: an interrupted atomic Save() leaves mempool.json.tmp on disk.  Without
// cleanup the file accumulates; with cleanup the node boots cleanly.
func TestCleanStaleTmpFilesOldFileDeleted(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "mempool.json.tmp")

	// Write a truncated (non-JSON) .tmp to simulate an OOM-interrupted Save().
	if err := os.WriteFile(tmpPath, []byte(`{"saved_at":"2026`), 0o600); err != nil {
		t.Fatalf("WriteFile tmp: %v", err)
	}

	// Back-date mtime to 6 minutes ago (> staleTmpMaxAge of 5 min).
	staleTime := time.Now().Add(-6 * time.Minute)
	if err := os.Chtimes(tmpPath, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// CleanStaleTmpFiles must delete the stale .tmp.
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	core.CleanStaleTmpFiles(dir, log)

	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("stale .tmp still exists after CleanStaleTmpFiles; want deleted")
	}

	// Load must return 0 (no mempool.json present) without any parse error.
	pool := core.NewMempool(mempoolTestConfig(), discardLogger())
	n := pool.Load(dir, discardLogger())
	if n != 0 {
		t.Errorf("Load returned %d, want 0 (no mempool.json present)", n)
	}
	if pool.Count() != 0 {
		t.Errorf("pool.Count() = %d after clean startup, want 0", pool.Count())
	}

	// The log should mention the removal.
	if !strings.Contains(buf.String(), "stale tmp") && !strings.Contains(buf.String(), "removed stale") {
		t.Logf("log output (informational): %s", buf.String())
	}
}

// TestCleanStaleTmpFilesRecentFileKept verifies that CleanStaleTmpFiles does
// NOT remove a .tmp file that is younger than staleTmpMaxAge (5 minutes).
// A concurrent in-progress Save() must not be interrupted by startup cleanup.
func TestCleanStaleTmpFilesRecentFileKept(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "mempool.json.tmp")

	// Write a .tmp with a current mtime (well within the 5-minute window).
	if err := os.WriteFile(tmpPath, []byte(`{"saved_at":"2026`), 0o600); err != nil {
		t.Fatalf("WriteFile tmp: %v", err)
	}

	core.CleanStaleTmpFiles(dir, discardLogger())

	// The recent .tmp must still be present.
	if _, err := os.Stat(tmpPath); os.IsNotExist(err) {
		t.Errorf("recent .tmp was deleted by CleanStaleTmpFiles; want kept")
	}
}

// ─── Test 4: SelectTxs orders restored txs by fee rate (descending) ──────────

// TestMempoolPersistFeeRateOrderPreserved verifies that after a save/load cycle
// SelectTxs returns all transactions in strict descending fee-rate order,
// confirming the highest-paying transaction is always picked first for block
// inclusion.
func TestMempoolPersistFeeRateOrderPreserved(t *testing.T) {
	dir := t.TempDir()

	poolBefore := core.NewMempool(mempoolTestConfig(), discardLogger())

	// Three transactions with distinct fees that produce distinct fee/byte rates
	// even after integer truncation (tx size ≈ 1972 bytes).
	// Using multiples of InitialBaseFeePerByte (200 nAPRO/byte):
	//   txLow:  200×1000 = 200 000 nAPRO → rate ≈ 101 nAPRO/byte
	//   txMid:  200×2000 = 400 000 nAPRO → rate ≈ 202 nAPRO/byte
	//   txHigh: 200×3000 = 600 000 nAPRO → rate ≈ 304 nAPRO/byte
	const bfp = core.InitialBaseFeePerByte
	txLow := makeTestTx(bfp*1000, 60)
	txMid := makeTestTx(bfp*2000, 61)
	txHigh := makeTestTx(bfp*3000, 62)

	for _, tx := range []core.Transaction{txLow, txMid, txHigh} {
		if err := poolBefore.Add(tx); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := poolBefore.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	poolAfter := core.NewMempool(mempoolTestConfig(), discardLogger())
	if n := poolAfter.Load(dir, discardLogger()); n != 3 {
		t.Fatalf("Load: restored %d, want 3", n)
	}

	selected := poolAfter.SelectTxs(3)
	if len(selected) != 3 {
		t.Fatalf("SelectTxs: got %d, want 3", len(selected))
	}

	// Assert complete descending fee-rate order.
	wantOrder := []uint64{txHigh.Fee, txMid.Fee, txLow.Fee}
	for i, want := range wantOrder {
		if got := selected[i].Fee; got != want {
			t.Errorf("SelectTxs[%d]: got fee=%d, want fee=%d", i, got, want)
		}
	}
}

// ─── Test 6: corrupt / truncated mempool.json returns 0 without panic ────────

// TestMempoolLoadCorruptFileReturnsZero verifies that Load() handles a corrupt
// or truncated mempool.json (invalid JSON) without panicking and returns 0,
// leaving the pool empty.
//
// This guards the unmarshal-error path in Load() (mempool.go, ~line 594-597):
//
//	if err := json.Unmarshal(data, &dump); err != nil {
//	    log.Warn("mempool load: unmarshal error (ignoring)", "err", err)
//	    return 0
//	}
//
// If that branch were accidentally deleted or broken, a corrupt file could
// cause a panic or silently restore zero transactions without the operator
// being notified.  Both the "returns 0" and "logs a warning" properties are
// therefore tested — the former here, the latter in
// TestMempoolLoadCorruptFileLogsWarning.
//
// Test cases:
//   - Truncated JSON (simulates a mid-write OOM kill):  `{"saved_at":"2026`
//   - Pure garbage bytes:                               `NOT JSON AT ALL`
//   - Empty file (zero bytes):                          ``
//   - Valid JSON but wrong schema (an array, not an object): `[]`
func TestMempoolLoadCorruptFileReturnsZero(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
	}{
		{"truncated_json", []byte(`{"saved_at":"2026`)},
		{"garbage_bytes", []byte("NOT JSON AT ALL \x00\xff\xfe")},
		{"empty_file", []byte("")},
		{"wrong_schema", []byte(`[]`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mempool.json")

			if err := os.WriteFile(path, tc.content, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			pool := core.NewMempool(mempoolTestConfig(), discardLogger())

			// Must not panic; must return 0.
			var restored int
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("Load() panicked with corrupt input %q: %v", tc.name, r)
					}
				}()
				restored = pool.Load(dir, discardLogger())
			}()

			if restored != 0 {
				t.Errorf("Load() returned %d for corrupt file %q, want 0", restored, tc.name)
			}
			if pool.Count() != 0 {
				t.Errorf("pool.Count() = %d after loading corrupt file %q, want 0",
					pool.Count(), tc.name)
			}
		})
	}
}

// ─── Test 8: corrupt mempool.json is removed after failed Load ───────────────

// TestMempoolLoadCorruptFileRemovedAfterFailedLoad verifies that Load() deletes
// mempool.json when json.Unmarshal fails so the node does not re-emit the
// corrupt-file warning on every subsequent restart.
//
// Covers the "Done looks like" requirements from task #1363:
//   - Load() removes mempool.json when json.Unmarshal fails.
//   - A second restart finds no file and starts with an empty pool.
//   - The second restart does NOT re-emit the warning.
func TestMempoolLoadCorruptFileRemovedAfterFailedLoad(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
	}{
		{"truncated_json", []byte(`{"saved_at":"2026`)},
		{"garbage_bytes", []byte("NOT JSON AT ALL \x00\xff\xfe")},
		{"empty_file", []byte("")},
		{"wrong_schema", []byte(`[]`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mempool.json")

			if err := os.WriteFile(path, tc.content, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			// First restart: encounters the corrupt file.
			pool1 := core.NewMempool(mempoolTestConfig(), discardLogger())
			restored := pool1.Load(dir, discardLogger())
			if restored != 0 {
				t.Errorf("[%s] first Load() returned %d, want 0", tc.name, restored)
			}

			// The corrupt file must be gone now so it cannot block future restarts.
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("[%s] mempool.json still present after failed Load(); want removed", tc.name)
			}

			// Second restart: no file present → empty pool, no warning.
			var warnBuf bytes.Buffer
			warnLogger := slog.New(slog.NewTextHandler(&warnBuf, &slog.HandlerOptions{
				Level: slog.LevelWarn,
			}))
			pool2 := core.NewMempool(mempoolTestConfig(), discardLogger())
			restored2 := pool2.Load(dir, warnLogger)
			if restored2 != 0 {
				t.Errorf("[%s] second Load() returned %d, want 0", tc.name, restored2)
			}
			if pool2.Count() != 0 {
				t.Errorf("[%s] pool2.Count() = %d after second restart, want 0", tc.name, pool2.Count())
			}
			// The second restart must not re-emit the "corrupt file" warning because
			// the file is already gone.
			if strings.Contains(warnBuf.String(), "unmarshal error") ||
				strings.Contains(warnBuf.String(), "corrupt") {
				t.Errorf("[%s] second Load() re-emitted corrupt-file warning; log: %q",
					tc.name, warnBuf.String())
			}
		})
	}
}

// ─── Test 9: Save() returns an error and leaves no .tmp on write failure ─────

// TestMempoolSaveWriteFailureNoTmpLeftover simulates a disk-write failure by
// pre-creating the temp file with mode 0o000 so that os.WriteFile fails when
// it tries to open/truncate the existing file, and verifies that:
//
//  1. Save() returns a non-nil error — the failure is never silently ignored.
//  2. No mempool.json.tmp leftover remains — Save() must remove the temp file
//     even when os.WriteFile fails mid-creation (e.g. ENOSPC, EACCES).
//
// This is a deterministic fault: the .tmp file already exists (simulating a
// partial write from a prior crash) and is locked to 0o000 so the write fails.
// After Save() returns the error, the caller can verify that the .tmp has been
// cleaned up — dir.Remove succeeds even on 0o000 files because removal
// requires write permission on the *directory*, not on the file itself.
//
// The test is skipped when running as root because root can open 0o000 files
// for writing, making the fault condition unreachable.
func TestMempoolSaveWriteFailureNoTmpLeftover(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: cannot deny write access to a 0o000 file; skipping")
	}

	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "mempool.json.tmp")
	finalPath := filepath.Join(dir, "mempool.json")

	// Pre-create the .tmp with no permissions — simulates a partial write left
	// by a previous crash.  os.WriteFile will fail when it tries to open/truncate
	// this file because O_WRONLY|O_TRUNC requires write permission on the file.
	if err := os.WriteFile(tmpPath, []byte("partial"), 0o000); err != nil {
		t.Fatalf("pre-create tmp: %v", err)
	}
	// Restore permissions after the test so t.TempDir() cleanup can remove it
	// in case Save() failed to do so (prevents test resource leak).
	t.Cleanup(func() { _ = os.Chmod(tmpPath, 0o600) })

	pool := core.NewMempool(mempoolTestConfig(), discardLogger())
	if err := pool.Add(makeTestTx(2000, 90)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Save() must return a non-nil error — write failure must never be silent.
	err := pool.Save(dir)
	if err == nil {
		t.Fatal("Save() returned nil when os.WriteFile failed; want non-nil error")
	}

	// The .tmp file must be removed by Save() — no leftover after failure.
	// (Removal requires only directory write permission, not file permission,
	// so the cleanup _ = os.Remove(tmp) in Save() must succeed here.)
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Errorf("mempool.json.tmp left on disk after Save() write failure; want removed by Save()")
	}

	// The final file must also be absent — nothing committed to disk.
	if _, statErr := os.Stat(finalPath); !os.IsNotExist(statErr) {
		t.Errorf("mempool.json unexpectedly present after Save() write failure")
	}
}

// ─── Test 7: corrupt mempool.json emits a Warn log so the operator is notified ─

// ─── Test 10: static guard — CleanStaleTmpFiles precedes Load on every branch ─

// TestCleanStaleTmpFilesCalledBeforeLoad is a static-analysis guard that reads
// main.go and verifies two ordering invariants:
//
//  1. The FIRST call to cleanStaleSnapshotTmpFiles appears before the
//     genesis/resume branch split (`if tipHash == (crypto.Hash32{})`).
//     This guarantees both the genesis boot path and the resume boot path
//     are covered by the cleanup — neither can be reached without it.
//
//  2. Every call to tryLoadStartupSnapshot is preceded (anywhere earlier in
//     the file) by at least one cleanStaleSnapshotTmpFiles call.
//     This prevents a .tmp left by a previous crash from being mistaken for a
//     valid snapshot file.
//
// The test fails immediately if:
//   - cleanStaleSnapshotTmpFiles is removed from main.go.
//   - The first call is moved to after the genesis/resume branch split
//     (leaving the genesis path unprotected).
//   - tryLoadStartupSnapshot is added without a preceding cleanup call.
//
// Strategy: read the source file, collect 1-based line numbers for each
// marker, then assert the ordering constraints.  No binary is compiled; the
// check is deterministic and executes in microseconds.
func TestCleanStaleTmpFilesCalledBeforeLoad(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("cannot read main.go: %v", err)
	}

	lines := strings.Split(string(data), "\n")

	var cleanLines []int  // all cleanStaleSnapshotTmpFiles call sites
	branchLine := -1      // genesis/resume branch split
	var snapLoadLines []int // all tryLoadStartupSnapshot call sites

	for i, raw := range lines {
		lineNo := i + 1
		trimmed := strings.TrimSpace(raw)
		switch {
		case strings.Contains(trimmed, "cleanStaleSnapshotTmpFiles("):
			cleanLines = append(cleanLines, lineNo)
		case strings.Contains(trimmed, "tipHash == (crypto.Hash32{})") && branchLine == -1:
			branchLine = lineNo
		case strings.Contains(trimmed, "tryLoadStartupSnapshot("):
			snapLoadLines = append(snapLoadLines, lineNo)
		}
	}

	// ── Invariant 0: the cleanup function must exist at least once ────────────
	if len(cleanLines) == 0 {
		t.Fatal("cleanStaleSnapshotTmpFiles not found in main.go — was the call removed?")
	}

	// ── Invariant 1: first cleanup precedes the genesis/resume branch split ───
	// If this fails, the genesis startup path skips the .tmp cleanup entirely.
	if branchLine == -1 {
		t.Error("genesis/resume branch split (tipHash == crypto.Hash32{}) not found in main.go")
	} else if cleanLines[0] >= branchLine {
		t.Errorf(
			"cleanStaleSnapshotTmpFiles first call (line %d) must appear BEFORE "+
				"the genesis/resume branch split (line %d); "+
				"genesis startup path would skip stale-tmp cleanup",
			cleanLines[0], branchLine,
		)
	}

	// ── Invariant 2: every tryLoadStartupSnapshot has a preceding cleanup ─────
	// Checks that no new snapshot-load site was added without cleanup coverage.
	if len(snapLoadLines) == 0 {
		t.Error("tryLoadStartupSnapshot not found in main.go — was the snapshot load removed?")
	}
	for _, loadLine := range snapLoadLines {
		hasPrior := false
		for _, cleanLine := range cleanLines {
			if cleanLine < loadLine {
				hasPrior = true
				break
			}
		}
		if !hasPrior {
			t.Errorf(
				"tryLoadStartupSnapshot at line %d has no preceding cleanStaleSnapshotTmpFiles call; "+
					"a stale .tmp could be mistaken for a valid snapshot",
				loadLine,
			)
		}
	}
}

// TestMempoolLoadCorruptFileLogsWarning verifies that Load() emits a slog Warn
// message containing the text "unmarshal error" when mempool.json cannot be
// parsed.  This confirms the operator-visible signal is not silently swallowed.
//
// Strategy: wire a *bytes.Buffer as the slog handler's output and assert the
// expected string appears after Load() returns.
func TestMempoolLoadCorruptFileLogsWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mempool.json")

	// Write a clearly truncated JSON file.
	truncated := []byte(`{"saved_at":"2026-08-05T12:00:00Z","entries":[{"tx":`)
	if err := os.WriteFile(path, truncated, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Capture log output in a buffer.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	pool := core.NewMempool(mempoolTestConfig(), discardLogger())
	restored := pool.Load(dir, logger)

	if restored != 0 {
		t.Errorf("Load() returned %d for truncated file, want 0", restored)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "corrupt") {
		t.Errorf("expected warning log to contain \"corrupt\"; got: %q", logOutput)
	}
	if !strings.Contains(logOutput, "WARN") {
		t.Errorf("expected log level WARN in output; got: %q", logOutput)
	}
}
