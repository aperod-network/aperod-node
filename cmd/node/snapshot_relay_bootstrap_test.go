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

	"github.com/aperod/aperod/consensus"
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

// ─── Test 3: relay consensus engine accepts block after rsync snapshot restore ──

// TestRelayBootstrap_IncomingBlockAccepted is the full end-to-end guard for the
// silent-rejection bug (task #1659).
//
// Scenario — mirrors production bootstrap via rsync:
//  1. A real validator engine produces block 1.
//  2. An operator rsyncs chain.db + snapshot from the running peer.
//     The rsync'd DB metadata shows 5% fewer active UTXOs than the snapshot
//     (the donor node's last saved count predates some transactions).
//  3. The relay node starts with non_validator=true and configured tolerance 1%.
//     checkSnapshotUTXOCount applies the 10% floor and accepts the snapshot.
//  4. RestoreFromSnapshot populates the relay's UTXOSet and ValidatorRegistry.
//  5. A relay consensus.Engine is built with Config.Validators seeded from the
//     restored registry — exactly as main.go does in the NonValidator=true path.
//  6. Block 1 arrives from the real validator via NewBlockCh().
//  7. The relay engine's handleIncomingBlock() runs isKnownValidator() and
//     proposerAt() against the restored registry — both must pass.
//  8. The relay chain advances to height 1, proving the block was accepted.
//
// Before the fix: step 3 rejected the snapshot → registry stayed zeroed →
// isKnownValidator() returned false for every incoming block → silent rejection.
func TestRelayBootstrap_IncomingBlockAccepted(t *testing.T) {
	dir := t.TempDir()

	// ── Step 1: validator engine produces block 1 ─────────────────────────────
	valPriv, valPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	genesis := makeSignedBlk(t, 0, crypto.Hash32{}, valPriv, valPub)

	validatorChain := core.NewChain()
	if err := validatorChain.SetGenesis(genesis); err != nil {
		t.Fatalf("validator SetGenesis: %v", err)
	}
	lk, err := crypto.NewLockedValidatorKey(valPriv.Bytes(), nil)
	if err != nil {
		t.Fatalf("NewLockedValidatorKey: %v", err)
	}
	defer lk.Destroy()

	validatorUTXOs := core.NewUTXOSet()
	validatorReg := core.NewValidatorRegistry()
	validatorMp := core.NewMempool(core.DefaultMempoolConfig())
	validatorEng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{valPub},
		Registry:     validatorReg,
		MyKey:        lk,
		OnCanonicalBlock: noopCanonicalPersistence,
	}, validatorChain, validatorMp, silentLog())
	validatorEng.SetTxVerifier(core.NewTxVerifier(validatorUTXOs), validatorUTXOs)

	validatorStop := make(chan struct{})
	go validatorEng.Run(validatorStop)
	defer close(validatorStop)

	var block1 *core.Block
	select {
	case block1 = <-validatorEng.ProducedCh():
		// got the produced block
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout: validator engine did not produce block 1 within 500 ms")
	}
	if block1.Header.Height != 1 {
		t.Fatalf("expected block at height 1, got %d", block1.Header.Height)
	}

	// ── Step 2: build and save a snapshot (as rsync'd from the running peer) ──
	// The snapshot is taken at genesis (height 0) and carries the validator's
	// registry entry — exactly what the relay node receives after rsync.
	tipHashArr := genesis.Hash()
	tipHashHex := fmt.Sprintf("%x", tipHashArr[:])
	valPubHex := valPub.Hex()

	// 100 UTXOs with distinct TxHash values (prevents UTXOKey collisions).
	activeUTXOs := make([]*core.UTXO, 100)
	for i := range activeUTXOs {
		var txHash crypto.Hash32
		txHash[0] = byte(i)
		txHash[1] = byte(i >> 8)
		activeUTXOs[i] = &core.UTXO{TxHash: txHash, BlockHeight: uint64(i + 1)}
	}

	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  0,
		TipHashHex: tipHashHex,
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{ActiveUTXOs: activeUTXOs},
		Registry: core.RegistrySnapshot{
			Validators: map[string]*core.ValidatorEntry{
				valPubHex: {
					PubKey:    valPub,
					StakeNAPR: core.MinStakeNAPR * 10,
					Status:    core.ValidatorActive,
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

	// ── Step 3: rsync scenario — DB metadata shows 5% fewer UTXOs than snapshot
	dbPath := filepath.Join(dir, "chain.db")
	db, derr := store.Open(dbPath)
	if derr != nil {
		t.Fatalf("store.Open: %v", derr)
	}
	defer db.Close()

	// Donor's DB metadata recorded 95 active UTXOs; snapshot has 100 — 5% divergence.
	// Configured tolerance is 1%; nonValidator floor of 10% must apply.
	const donorCount = 95
	if err := db.StoreActiveUTXOCount(tipHashHex, donorCount); err != nil {
		t.Fatalf("StoreActiveUTXOCount: %v", err)
	}

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	ok := checkSnapshotUTXOCount(db, len(loaded.UTXOs.ActiveUTXOs), tipHashHex,
		1.0 /* configuredPct */, false /* isRelaxed */, true /* nonValidator */, log)
	if !ok {
		t.Fatalf("checkSnapshotUTXOCount rejected snapshot (5%% divergence should pass "+
			"with nonValidator floor 10%%); log:\n%s", logBuf.String())
	}

	// ── Step 4: restore snapshot into fresh in-memory structures ─────────────
	relayUTXOs := core.NewUTXOSet()
	relayReg := core.NewValidatorRegistry()
	relayUTXOs.RestoreFromSnapshot(loaded.UTXOs)
	relayReg.RestoreFromSnapshot(loaded.Registry)
	relayReg.SetUTXOSet(relayUTXOs)

	// Guard: registry must be non-empty before building the engine.
	restoredVals := relayReg.GetActiveValidators()
	if len(restoredVals) == 0 {
		t.Fatalf("restored registry has no active validators; relay engine will reject " +
			"all incoming blocks with 'producer X is not the scheduled proposer 00000000'")
	}

	// ── Step 5: build relay consensus engine from restored registry ───────────
	// main.go NonValidator=true branch: Config.Validators = genesis validator pub keys,
	// Registry = restored registry.  InitFromGenesis skips keys already present.
	relayChain := core.NewChain()
	if err := relayChain.SetGenesis(genesis); err != nil {
		t.Fatalf("relay SetGenesis: %v", err)
	}
	relayMp := core.NewMempool(core.DefaultMempoolConfig())
	relayEng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   restoredVals, // seeded from restored snapshot registry
		Registry:     relayReg,
		MyKey:        nil, // non-validator: never produces blocks
		OnCanonicalBlock: noopCanonicalPersistence,
	}, relayChain, relayMp, silentLog())
	relayEng.SetTxVerifier(core.NewTxVerifier(relayUTXOs), relayUTXOs)

	relayStop := make(chan struct{})
	go relayEng.Run(relayStop)
	defer close(relayStop)

	// ── Steps 6–8: submit block 1 and assert relay chain advances ─────────────
	// handleIncomingBlock exercises isKnownValidator() and proposerAt() against
	// the relay's restored registry.  Both must pass for the chain to advance.
	relayEng.NewBlockCh() <- block1

	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Errorf("relay chain height = %d after 300 ms, want 1; "+
				"isKnownValidator() or proposerAt() rejected the block despite "+
				"registry being restored from snapshot (rsync bootstrap regression)",
				relayChain.Height())
			return
		default:
			if relayChain.Height() == 1 {
				t.Log("OK: relay engine accepted block 1 after rsync-bootstrap snapshot restore")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
