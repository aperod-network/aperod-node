package main

// Tests for the relay-node rsync-bootstrap scenario described in task #1659.
//
// When a relay (non_validator) node is bootstrapped by rsyncing chain.db from a
// running peer, the DB active-UTXO metadata belongs to the donor node's save
// history — not to the relay's own saves.  With a strict tolerance (e.g. 1%)
// this caused checkSnapshotUTXOCount to log a divergence warning but still
// accept the snapshot (the function always returns true since the fix landed).
// The relay's ValidatorRegistry was then correctly populated from the snapshot,
// so incoming blocks were accepted instead of being silently rejected with
// "producer X is not the scheduled proposer 00000000".
//
// Test matrix:
//  1. 5% UTXO-count divergence with nonValidator=true and 1% configured
//     tolerance → check still passes (nonValidator floor ensures acceptance).
//  2. After checkSnapshotUTXOCount passes, RestoreFromSnapshot populates both
//     the UTXOSet (non-empty active set) and the ValidatorRegistry (non-empty
//     validator map) rather than leaving them zeroed.
//  3. A block signed by the snapshotted validator can be added to a Chain built
//     from the same genesis, confirming that "incoming valid block is accepted"
//     end-to-end.

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// ─── Test 1: 5% divergence accepted with nonValidator tolerance floor ──────────

// TestRelayBootstrap_UTXOCountDivergenceAccepted verifies that
// checkSnapshotUTXOCount returns true for a non-validator node even when the
// snapshot's active UTXO count diverges from DB metadata by ~5% — a value
// that exceeds the 1% configured tolerance but is within the 10% floor
// applied automatically in non-validator mode.
//
// This is the core regression guard for the silent-rejection bug: if the
// function incorrectly rejected the snapshot, the validator registry would
// remain zeroed and all incoming blocks would fail the "scheduled proposer"
// check.
func TestRelayBootstrap_UTXOCountDivergenceAccepted(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chain.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	tipHashHex := fmt.Sprintf("%064x", 42)

	// DB metadata says the donor node had 200 active UTXOs when it saved.
	// The rsync'd snapshot claims 190 — a 5% divergence (diff=10, larger=200 → 5%).
	const dbCount = 200
	const snapCount = 190 // 5% below dbCount

	if err := db.StoreActiveUTXOCount(tipHashHex, dbCount); err != nil {
		t.Fatalf("StoreActiveUTXOCount: %v", err)
	}

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	// Configured tolerance is 1% — well below the 5% divergence.
	// nonValidator=true must raise the effective floor to 10%, accepting the snapshot.
	const configuredTolerancePct = 1.0
	ok := checkSnapshotUTXOCount(db, snapCount, tipHashHex, configuredTolerancePct,
		false /* isRelaxed */, true /* nonValidator */, log)

	if !ok {
		t.Errorf("checkSnapshotUTXOCount returned false; nonValidator floor (10%%) "+
			"should accept a 5%% divergence even with configured tolerance %.0f%%",
			configuredTolerancePct)
		t.Logf("log:\n%s", logBuf.String())
	}

	// The function must not emit the old full-scan rejection message.
	if logContainsMsg(&logBuf, "snapshot rejected — active UTXO count diverges from last-saved count; falling back to block scan") {
		t.Error("got legacy full-scan rejection message; snapshot must be accepted for non-validator nodes")
	}
}

// ─── Test 2: RestoreFromSnapshot populates UTXOSet and ValidatorRegistry ───────

// TestRelayBootstrap_RestorePopulatesRegistryAndUTXOs verifies that after
// checkSnapshotUTXOCount returns true, calling RestoreFromSnapshot on a fresh
// UTXOSet and ValidatorRegistry correctly installs the snapshot contents — the
// registry is NOT left zeroed (empty).
//
// A zeroed registry causes every incoming block to fail the "scheduled
// proposer" check because no validator public key is known to the engine.
func TestRelayBootstrap_RestorePopulatesRegistryAndUTXOs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chain.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	// Generate a validator key to embed in the snapshot.
	_, valPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	valPubHex := fmt.Sprintf("%x", valPub[:])

	tipHeight := uint64(50)
	tipHashHex := fmt.Sprintf("%064x", 99)

	// Build a snapshot that carries:
	//   • 100 active UTXOs (simulating the donor node's active set)
	//   • one registry entry for the generated validator
	//
	// Each UTXO must have a unique TxHash so they occupy distinct UTXOKey slots
	// in the UTXOSet map (the key is {TxHash, OutputIndex}; duplicates collapse).
	activeUTXOs := make([]*core.UTXO, 100)
	for i := range activeUTXOs {
		var txHash crypto.Hash32
		txHash[0] = byte(i)
		txHash[1] = byte(i >> 8)
		activeUTXOs[i] = &core.UTXO{
			TxHash:      txHash,
			OutputIndex: 0,
			BlockHeight: uint64(i + 1),
		}
	}
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		TxTotal:    50,
		UTXOs: core.UTXOSnapshot{
			ActiveUTXOs: activeUTXOs,
		},
		Registry: core.RegistrySnapshot{
			Validators: map[string]*core.ValidatorEntry{
				valPubHex: {
					PubKey:    valPub,
					StakeNAPR: 1_000_000_000,
				},
			},
		},
	}

	// Save and reload the snapshot (round-trip through disk).
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}
	loaded, serr := loadStartupSnapshot(dir, tipHeight, tipHashHex)
	if serr != nil {
		t.Fatalf("loadStartupSnapshot: %v", serr)
	}

	// Simulate the DB metadata belonging to a donor node: 95 active UTXOs (~5% divergence).
	const donorDBCount = 95
	if err := db.StoreActiveUTXOCount(tipHashHex, donorDBCount); err != nil {
		t.Fatalf("StoreActiveUTXOCount: %v", err)
	}

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	// The count check must pass despite the 5% divergence (nonValidator floor=10%).
	ok := checkSnapshotUTXOCount(db, len(loaded.UTXOs.ActiveUTXOs), tipHashHex,
		1.0 /* configuredPct */, false /* isRelaxed */, true /* nonValidator */, log)
	if !ok {
		t.Fatalf("checkSnapshotUTXOCount returned false unexpectedly; log:\n%s",
			logBuf.String())
	}

	// Apply the snapshot to fresh in-memory structures (mirrors main.go after utxoCountOK).
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	utxos.RestoreFromSnapshot(loaded.UTXOs)
	registry.RestoreFromSnapshot(loaded.Registry)
	registry.SetUTXOSet(utxos)

	// ── Assert: UTXOSet is populated (not zeroed).
	if got := utxos.Count(); got != 100 {
		t.Errorf("UTXOSet.Count() = %d after RestoreFromSnapshot, want 100 active UTXOs", got)
	}

	// ── Assert: ValidatorRegistry is populated (not zeroed).
	snap2 := registry.TakeSnapshot()
	if len(snap2.Validators) == 0 {
		t.Errorf("ValidatorRegistry is empty after RestoreFromSnapshot; "+
			"registry must carry the donor node's validator set so that "+
			"incoming blocks pass the 'scheduled proposer' check")
	}
	if _, found := snap2.Validators[valPubHex]; !found {
		keys := make([]string, 0, len(snap2.Validators))
		for k := range snap2.Validators {
			keys = append(keys, k[:16]+"...")
		}
		t.Errorf("validator %s... not found in restored registry; got keys: %v",
			valPubHex[:16], keys)
	}
}

// ─── Test 3: block signed by snapshotted validator is accepted by Chain ────────

// TestRelayBootstrap_IncomingBlockAccepted is the end-to-end guard: after the
// snapshot is restored, a block signed by the snapshotted validator can be
// added to a Chain seeded from the same genesis.  This mirrors what happens on
// a relay node after bootstrap: it restores the snapshot, builds its Chain from
// the genesis block, then accepts the first incoming block from a peer.
func TestRelayBootstrap_IncomingBlockAccepted(t *testing.T) {
	dir := t.TempDir()

	// Generate the validator that will produce and sign the incoming block.
	valPriv, valPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	// Build genesis block (height 0) signed by the validator.
	genesis := makeSignedBlk(t, 0, crypto.Hash32{}, valPriv, valPub)

	// Create the chain from genesis.
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	// Build the snapshot at height 0 (as rsync'd from the running peer).
	tipHashArr := genesis.Hash()
	tipHashHex := fmt.Sprintf("%x", tipHashArr[:])
	valPubHex := fmt.Sprintf("%x", valPub[:])

	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  0,
		TipHashHex: tipHashHex,
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{},
		Registry: core.RegistrySnapshot{
			Validators: map[string]*core.ValidatorEntry{
				valPubHex: {
					PubKey:    valPub,
					StakeNAPR: 1_000_000_000,
				},
			},
		},
	}

	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}
	loaded, serr := loadStartupSnapshot(dir, 0, tipHashHex)
	if serr != nil {
		t.Fatalf("loadStartupSnapshot: %v", serr)
	}

	// Open a DB that mimics an rsync'd chain.db — no active-UTXO metadata entry
	// (rsync copies the chain blocks but not the snapshot metadata).
	dbPath := filepath.Join(dir, "chain.db")
	db, derr := store.Open(dbPath)
	if derr != nil {
		t.Fatalf("store.Open: %v", derr)
	}
	defer db.Close()

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	// With no DB metadata entry, the check is skipped and the snapshot is
	// accepted (identical to the "first snapshot" path on the relay node).
	ok := checkSnapshotUTXOCount(db, len(loaded.UTXOs.ActiveUTXOs), tipHashHex,
		1.0 /* configuredPct */, false /* isRelaxed */, true /* nonValidator */, log)
	if !ok {
		t.Fatalf("checkSnapshotUTXOCount returned false; log:\n%s", logBuf.String())
	}
	if !logContainsMsg(&logBuf, "snapshot UTXO count check skipped — no prior active count in db (first snapshot)") {
		t.Errorf("expected 'check skipped' log when DB has no metadata; log:\n%s", logBuf.String())
	}

	// Restore snapshot into fresh in-memory structures (mirrors main.go).
	utxos := core.NewUTXOSet()
	registry := core.NewValidatorRegistry()
	utxos.RestoreFromSnapshot(loaded.UTXOs)
	registry.RestoreFromSnapshot(loaded.Registry)
	registry.SetUTXOSet(utxos)

	// Registry must be populated so the relay can validate incoming blocks.
	snap2 := registry.TakeSnapshot()
	if _, found := snap2.Validators[valPubHex]; !found {
		t.Fatalf("validator missing from restored registry; relay will reject all incoming blocks "+
			"with 'producer X is not the scheduled proposer 00000000'")
	}

	// Build and sign a block at height 1 — the first "incoming" peer block.
	incomingBlock := &core.Block{
		Header: core.BlockHeader{
			Height:       1,
			PrevHash:     tipHashArr,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: valPub,
			MerkleRoot:   core.MerkleRoot(nil),
		},
	}
	if err := incomingBlock.Header.Sign(valPriv); err != nil {
		t.Fatalf("Sign block h=1: %v", err)
	}

	// Chain.AddBlock must succeed — this is the "incoming valid block is accepted" criterion.
	if err := chain.AddBlock(incomingBlock); err != nil {
		t.Errorf("chain.AddBlock(h=1) failed: %v; "+
			"relay node rejected an incoming block from the snapshotted validator", err)
	}
}
