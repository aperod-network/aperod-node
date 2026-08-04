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
	"log/slog"
	"os"
	"path/filepath"
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
	// DeterministicMintBlind(spendPub, amount) mirrors what BuildMintTx wrote.
	// For mint outputs, oneTimePriv = spendPriv + height_scalar, so HsScalar = height_scalar.
	blind, err := crypto.DeterministicMintBlind(aliceSpendPub, faucetAmount)
	if err != nil {
		t.Fatalf("DeterministicMintBlind: %v", err)
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
