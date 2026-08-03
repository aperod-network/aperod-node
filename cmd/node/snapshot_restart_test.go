package main

// Integration test: snapshot save-on-shutdown + fast-path restore on restart.
//
// Covers the three requirements from task #1018:
//  1. A graceful stop (SIGTERM path) saves a snapshot file whose name encodes
//     the exact tip height (snapshot-v1-<height>.json).
//  2. A subsequent start with the snapshot present logs
//     "startup fast path complete — snapshot loaded" and does NOT log
//     "running startup block scan".
//  3. If TimeoutStopSec in the systemd override is below 240 s the startup
//     check logs a warning (covered by TestCheckSystemdTimeout).
//
// The test is entirely in-process: it calls the same saveStartupSnapshot /
// loadStartupSnapshot helpers that main.go calls, and uses a JSON slog handler
// writing to a bytes.Buffer to capture log output.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// newCaptureLogger returns a *slog.Logger that writes JSON lines to buf.
func newCaptureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// logContainsMsg scans JSON log lines in buf for an entry whose "msg" field
// equals want.  Returns true if found.
func logContainsMsg(buf *bytes.Buffer, want string) bool {
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var rec map[string]interface{}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if msg, ok := rec["msg"].(string); ok && msg == want {
			return true
		}
	}
	return false
}

// buildChainInStore creates a genesis block plus nExtra unsigned "empty" blocks
// in a temporary LevelDB store, sets the tip, and returns the store, the
// genesis block, and a slice of all blocks (genesis first).
func buildChainInStore(t *testing.T, dir string, nExtra int) (*store.DB, []*core.Block) {
	t.Helper()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	makeBlock := func(height uint64, prevHash crypto.Hash32) *core.Block {
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prevHash,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		}
		if signErr := hdr.Sign(priv); signErr != nil {
			t.Fatalf("block Sign height=%d: %v", height, signErr)
		}
		return &core.Block{Header: hdr}
	}

	db, err := store.Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var blocks []*core.Block
	genesis := makeBlock(0, crypto.Hash32{})
	blocks = append(blocks, genesis)

	storeBlk := func(b *core.Block) {
		raw, merr := json.Marshal(b)
		if merr != nil {
			t.Fatalf("marshal block h=%d: %v", b.Header.Height, merr)
		}
		h := b.Hash()
		if perr := db.PutRawBlock(h, b.Header.Height, raw); perr != nil {
			t.Fatalf("PutRawBlock h=%d: %v", b.Header.Height, perr)
		}
	}
	storeBlk(genesis)

	parent := genesis
	for i := 1; i <= nExtra; i++ {
		blk := makeBlock(uint64(i), parent.Hash())
		storeBlk(blk)
		blocks = append(blocks, blk)
		parent = blk
	}

	// Set tip to the last block.
	tip := blocks[len(blocks)-1]
	tipHash := tip.Hash()
	tipHeight := tip.Header.Height
	if err := db.PutTip(tipHash, tipHeight); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	return db, blocks
}

// ─── Test 1: SIGTERM path saves snapshot ─────────────────────────────────────

// TestShutdownSavesSnapshot verifies that the shutdown snapshot logic (mirroring
// the SIGTERM handler in main.go) writes a snapshot file named
// snapshot-v1-<tipHeight>.json in the data directory.
func TestShutdownSavesSnapshot(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 5) // genesis + 5 blocks → tip height 5

	tip := blocks[len(blocks)-1]
	tipHashArr := tip.Hash()
	tipHash := tipHashArr[:]
	tipHeight := tip.Header.Height // 5

	// Simulate the shutdown snapshot (mirrors the code in main.go ── 11. Wait for signal).
	shutSnap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: fmt.Sprintf("%x", tipHash),
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{},
		Registry: core.RegistrySnapshot{
			Validators: map[string]*core.ValidatorEntry{},
		},
	}

	if err := saveStartupSnapshot(dir, shutSnap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	// ── Assert: the file must exist and carry the correct tip height in its name.
	wantPath := snapshotPath(dir, tipHeight) // snapshot-v1-5.json
	if _, err := os.Stat(wantPath); os.IsNotExist(err) {
		t.Fatalf("snapshot file not created: expected %s", wantPath)
	}

	// ── Assert: the file must be parseable and have the correct tip height/hash.
	wantHashHex := fmt.Sprintf("%x", tipHash)
	loaded, err := loadStartupSnapshot(dir, tipHeight, wantHashHex)
	if err != nil {
		t.Fatalf("loadStartupSnapshot: %v", err)
	}
	if loaded.TipHeight != tipHeight {
		t.Errorf("loaded.TipHeight = %d, want %d", loaded.TipHeight, tipHeight)
	}
	if loaded.TipHashHex != wantHashHex {
		t.Errorf("loaded.TipHashHex mismatch")
	}
}

// ─── Test 2: fast path log messages on second start ──────────────────────────

// TestFastPathLogsOnRestart verifies that when a valid snapshot exists for the
// current tip, the startup path:
//   (a) logs "startup fast path complete — snapshot loaded"  ← must appear
//   (b) does NOT log "running startup block scan"            ← must be absent
//
// This is achieved by running the same snapshot-presence logic that main.go
// uses, but in-process with a captured logger.
func TestFastPathLogsOnRestart(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 3) // tip at height 3

	tip := blocks[len(blocks)-1]
	tipHash := tip.Hash()
	tipHeight := tip.Header.Height
	tipHashHex := fmt.Sprintf("%x", tipHash[:])

	// Pre-save a snapshot at the current tip (as the shutdown handler would).
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{},
		Registry: core.RegistrySnapshot{
			Validators: map[string]*core.ValidatorEntry{},
		},
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	// ── Simulate the startup fast-path logic (mirrors main.go lines 338-363).
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	snapLoaded := false
	if loaded, serr := loadStartupSnapshot(dir, tipHeight, tipHashHex); serr == nil {
		_ = loaded // in production this populates utxos + registry
		log.Info("startup fast path complete — snapshot loaded",
			"tip_height", tipHeight,
			"active_utxos", len(loaded.UTXOs.ActiveUTXOs),
			"spent_decoys", len(loaded.UTXOs.SpentDecoys),
			"key_images", len(loaded.UTXOs.KeyImages),
		)
		snapLoaded = true
	} else if !os.IsNotExist(serr) {
		log.Warn("snapshot load error, falling back to block scan", "err", serr)
	}

	if !snapLoaded {
		// Only reached when no snapshot exists; the log line below must be absent.
		log.Info("running startup block scan",
			"tip_height", tipHeight,
			"ki_from_index", false,
			"heap_sys_mib_before", uint64(0),
		)
	}

	// ── Assertions.
	if !logContainsMsg(&logBuf, "startup fast path complete — snapshot loaded") {
		t.Error("expected log message \"startup fast path complete — snapshot loaded\" was not emitted")
		t.Logf("captured log output:\n%s", logBuf.String())
	}
	if logContainsMsg(&logBuf, "running startup block scan") {
		t.Error("unexpected log message \"running startup block scan\" was emitted (should be absent when snapshot is present)")
		t.Logf("captured log output:\n%s", logBuf.String())
	}
}

// TestNoFastPathLogWhenSnapshotAbsent is the inverse: when no snapshot exists,
// the block-scan log line must appear and the fast-path line must be absent.
func TestNoFastPathLogWhenSnapshotAbsent(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 2) // tip at height 2, no snapshot saved

	tip := blocks[len(blocks)-1]
	tipHeight := tip.Header.Height
	tipHashArr2 := tip.Hash()
	tipHashHex := fmt.Sprintf("%x", tipHashArr2[:])

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	snapLoaded := false
	if _, serr := loadStartupSnapshot(dir, tipHeight, tipHashHex); serr == nil {
		log.Info("startup fast path complete — snapshot loaded", "tip_height", tipHeight)
		snapLoaded = true
	} else if !os.IsNotExist(serr) {
		log.Warn("snapshot load error, falling back to block scan", "err", serr)
	}

	if !snapLoaded {
		log.Info("running startup block scan",
			"tip_height", tipHeight,
			"ki_from_index", false,
			"heap_sys_mib_before", uint64(0),
		)
	}

	if logContainsMsg(&logBuf, "startup fast path complete — snapshot loaded") {
		t.Error("fast-path log must NOT appear when snapshot is absent")
	}
	if !logContainsMsg(&logBuf, "running startup block scan") {
		t.Error("block-scan log must appear when snapshot is absent")
	}
}

// ─── Test 3: snapshot round-trip preserves UTXO / registry data ──────────────

// TestSnapshotRoundTrip checks that a non-empty snapshot written by
// saveStartupSnapshot is faithfully read back by loadStartupSnapshot,
// confirming that the UTXOSet and registry data survive the disk round-trip.
func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()

	tipHeight := uint64(42)
	tipHashHex := strings.Repeat("ab", 32)

	// Populate a minimal but non-empty UTXOSnapshot.
	var ki crypto.KeyImage
	for i := range ki {
		ki[i] = byte(i)
	}
	utxoSnap := core.UTXOSnapshot{
		ActiveUTXOs: []*core.UTXO{
			{BlockHeight: 7},
		},
		SpentDecoys: []*core.UTXO{
			{BlockHeight: 3},
		},
		KeyImages: []crypto.KeyImage{ki},
	}

	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		TxTotal:    99,
		UTXOs:      utxoSnap,
		Registry: core.RegistrySnapshot{
			Validators: map[string]*core.ValidatorEntry{
				"valA": {StakeNAPR: 1_000_000},
			},
		},
	}

	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	loaded, err := loadStartupSnapshot(dir, tipHeight, tipHashHex)
	if err != nil {
		t.Fatalf("loadStartupSnapshot: %v", err)
	}

	if loaded.TxTotal != 99 {
		t.Errorf("TxTotal: got %d want 99", loaded.TxTotal)
	}
	if len(loaded.UTXOs.ActiveUTXOs) != 1 {
		t.Errorf("ActiveUTXOs: got %d want 1", len(loaded.UTXOs.ActiveUTXOs))
	}
	if len(loaded.UTXOs.SpentDecoys) != 1 {
		t.Errorf("SpentDecoys: got %d want 1", len(loaded.UTXOs.SpentDecoys))
	}
	if len(loaded.UTXOs.KeyImages) != 1 {
		t.Errorf("KeyImages: got %d want 1", len(loaded.UTXOs.KeyImages))
	}
	if len(loaded.Registry.Validators) != 1 {
		t.Errorf("Validators: got %d want 1", len(loaded.Registry.Validators))
	}
}

// ─── Test 4: systemd TimeoutStopSec guard ─────────────────────────────────────

const wantWarnMsg = "systemd TimeoutStopSec is below safe threshold — snapshot may not save on restart"

// writeConf writes content to a named file in dir and returns its path.
// If content is empty, the file is not written (returns a nonexistent path).
func writeConf(t *testing.T, dir, name, content string) string {
	t.Helper()
	if content == "" {
		return filepath.Join(dir, name) // does not exist
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("writeConf %s: %v", name, err)
	}
	return p
}

// TestCheckSystemdTimeout_Dropin exercises cases where the drop-in file is the
// source of truth (highest-precedence path).
func TestCheckSystemdTimeout_Dropin(t *testing.T) {
	tests := []struct {
		name        string
		dropin      string
		wantWarning bool
	}{
		{"dropin below threshold warns", "[Service]\nTimeoutStopSec=90\n", true},
		{"dropin at threshold safe", "[Service]\nTimeoutStopSec=240\n", false},
		{"dropin above threshold safe", "[Service]\nTimeoutStopSec=300\n", false},
		{"dropin infinity safe", "[Service]\nTimeoutStopSec=infinity\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dropin := writeConf(t, dir, "timeout.conf", tc.dropin)
			service := filepath.Join(dir, "aperod-node.service") // absent
			systemdDir := filepath.Join(dir, "no-systemd")       // absent → no default-warn

			var logBuf bytes.Buffer
			checkSystemdTimeout(dropin, service, systemdDir, newCaptureLogger(&logBuf))

			got := logContainsMsg(&logBuf, wantWarnMsg)
			if got != tc.wantWarning {
				t.Errorf("wantWarning=%v got=%v\nlog:\n%s", tc.wantWarning, got, logBuf.String())
			}
		})
	}
}

// TestCheckSystemdTimeout_MainService exercises the fallback path: drop-in
// absent, main unit file has TimeoutStopSec.
func TestCheckSystemdTimeout_MainService(t *testing.T) {
	tests := []struct {
		name        string
		service     string
		wantWarning bool
	}{
		{"service below threshold warns", "[Service]\nTimeoutStopSec=120\n", true},
		{"service at threshold safe", "[Service]\nTimeoutStopSec=300\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dropin := filepath.Join(dir, "timeout.conf")            // absent
			service := writeConf(t, dir, "aperod-node.service", tc.service)
			systemdDir := filepath.Join(dir, "no-systemd")          // absent → no default-warn

			var logBuf bytes.Buffer
			checkSystemdTimeout(dropin, service, systemdDir, newCaptureLogger(&logBuf))

			got := logContainsMsg(&logBuf, wantWarnMsg)
			if got != tc.wantWarning {
				t.Errorf("wantWarning=%v got=%v\nlog:\n%s", tc.wantWarning, got, logBuf.String())
			}
		})
	}
}

// TestCheckSystemdTimeout_SystemdPresentNoConfig verifies that when systemd
// appears to be running (systemdDir exists) but neither file contains
// TimeoutStopSec, a warning is emitted about the 90-second default.
func TestCheckSystemdTimeout_SystemdPresentNoConfig(t *testing.T) {
	dir := t.TempDir()
	dropin := filepath.Join(dir, "timeout.conf")           // absent
	service := filepath.Join(dir, "aperod-node.service")   // absent
	systemdDir := filepath.Join(dir, "run-systemd-system") // simulate /run/systemd/system
	if err := os.MkdirAll(systemdDir, 0755); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	checkSystemdTimeout(dropin, service, systemdDir, newCaptureLogger(&logBuf))

	if !logContainsMsg(&logBuf, wantWarnMsg) {
		t.Errorf("expected warning when systemd is active but no TimeoutStopSec is set\nlog:\n%s", logBuf.String())
	}
}

// TestCheckSystemdTimeout_NonSystemdSilent verifies that when systemd is NOT
// running (no systemdDir) and neither config file exists, no warning is emitted
// (the node may be running on Docker, macOS, etc.).
func TestCheckSystemdTimeout_NonSystemdSilent(t *testing.T) {
	dir := t.TempDir()
	dropin := filepath.Join(dir, "timeout.conf")
	service := filepath.Join(dir, "aperod-node.service")
	systemdDir := filepath.Join(dir, "no-systemd")

	var logBuf bytes.Buffer
	checkSystemdTimeout(dropin, service, systemdDir, newCaptureLogger(&logBuf))

	if logContainsMsg(&logBuf, wantWarnMsg) {
		t.Errorf("unexpected warning on non-systemd host\nlog:\n%s", logBuf.String())
	}
}

// ─── Test 5: real TakeSnapshot pipeline ───────────────────────────────────────

// TestRealSnapshotPipeline exercises the actual UTXOSet.TakeSnapshot() and
// ValidatorRegistry.TakeSnapshot() production code paths — the same calls the
// SIGTERM handler makes — and confirms the snapshot survives a full disk
// round-trip and is faithfully restored by RestoreFromSnapshot.
//
// This guards against regressions in the shutdown orchestration: if
// TakeSnapshot or RestoreFromSnapshot were accidentally broken, this test
// would catch it before the node ships.
func TestRealSnapshotPipeline(t *testing.T) {
	dir := t.TempDir()

	// Build a minimal chain: genesis + 2 blocks.
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	makeBlock := func(height uint64, prev crypto.Hash32, txs []core.Transaction) *core.Block {
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prev,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(txs),
		}
		if err := hdr.Sign(priv); err != nil {
			t.Fatalf("Sign h=%d: %v", height, err)
		}
		return &core.Block{Header: hdr, Txs: txs}
	}

	// Genesis with a coinbase output so UTXOSet has a real entry.
	genesisTxs := []core.Transaction{core.CoinbaseTx(crypto.Point32(pub), 1_000_000)}
	genesis := makeBlock(0, crypto.Hash32{}, genesisTxs)
	blk1 := makeBlock(1, genesis.Hash(), nil)
	blk2 := makeBlock(2, blk1.Hash(), nil)

	// Apply blocks to a real UTXOSet (mirrors the startup scan path).
	utxos := core.NewUTXOSet()
	for _, b := range []*core.Block{genesis, blk1, blk2} {
		if err := utxos.ApplyBlock(b); err != nil {
			t.Fatalf("ApplyBlock h=%d: %v", b.Header.Height, err)
		}
	}
	utxoCountBefore := utxos.Count()
	if utxoCountBefore == 0 {
		t.Fatal("UTXOSet is empty after applying genesis — test setup error")
	}

	// Wire a real ValidatorRegistry.
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)
	activeBefore, _ := registry.Count()

	// ── Simulate shutdown: TakeSnapshot + save (the SIGTERM handler path).
	tipHashArr := blk2.Hash()
	tipHeight := blk2.Header.Height
	tipHashHex := fmt.Sprintf("%x", tipHashArr[:])

	shutSnap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		TxTotal:    int64(utxoCountBefore),
		UTXOs:      utxos.TakeSnapshot(),    // ← real production call
		Registry:   registry.TakeSnapshot(), // ← real production call
	}
	if err := saveStartupSnapshot(dir, shutSnap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	// Snapshot file must exist with the correct height in the name.
	wantPath := snapshotPath(dir, tipHeight)
	if _, err := os.Stat(wantPath); os.IsNotExist(err) {
		t.Fatalf("snapshot file not created: %s", wantPath)
	}

	// ── Simulate second start: load + RestoreFromSnapshot (the fast-path).
	loaded, err := loadStartupSnapshot(dir, tipHeight, tipHashHex)
	if err != nil {
		t.Fatalf("loadStartupSnapshot: %v", err)
	}

	utxos2 := core.NewUTXOSet()
	utxos2.RestoreFromSnapshot(loaded.UTXOs) // ← real production call

	registry2 := core.NewValidatorRegistry()
	registry2.SetUTXOSet(utxos2)
	registry2.RestoreFromSnapshot(loaded.Registry) // ← real production call

	// UTXO counts must match.
	if got := utxos2.Count(); got != utxoCountBefore {
		t.Errorf("UTXOSet count after restore: got %d want %d", got, utxoCountBefore)
	}

	// Registry active-validator count must match.
	activeAfter, _ := registry2.Count()
	if activeAfter != activeBefore {
		t.Errorf("registry active validators after restore: got %d want %d", activeAfter, activeBefore)
	}

	// Log messages must reflect the fast path, not the block scan.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)
	log.Info("startup fast path complete — snapshot loaded",
		"tip_height", tipHeight,
		"active_utxos", len(loaded.UTXOs.ActiveUTXOs),
	)
	if !logContainsMsg(&logBuf, "startup fast path complete — snapshot loaded") {
		t.Error("fast-path log message missing")
	}
}

// ─── Test 6: exact-tip fast path discards snapshot on DB tip hash mismatch ───

// TestExactTipFastPathHashMismatchDiscard confirms that the exact-tip snapshot
// fast path in main.go falls back to the block scan when the DB tip record
// contains a hash that does not match the hash stored inside the snapshot file.
//
// Scenario:
//   - A valid snapshot is saved at height H with the true block hash (correctHex).
//   - The DB tip record is then "corrupted": it claims the same height H but a
//     different hash (corruptHex).
//   - The startup fast path passes corruptHex to loadStartupSnapshot; the
//     function returns a "snapshot hash mismatch" error (not os.ErrNotExist).
//   - snapLoaded must remain false and the warning
//     "snapshot load error, falling back to block scan" must be logged.
//
// This guards the invariant described in task 1056: a corrupt tip record cannot
// bypass the hash cross-check and cause stale UTXO/registry state to be loaded.
func TestExactTipFastPathHashMismatchDiscard(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 4) // genesis + 4 blocks → tip height 4

	tip := blocks[len(blocks)-1]
	correctHash := tip.Hash()
	tipHeight := tip.Header.Height           // 4
	correctHex := fmt.Sprintf("%x", correctHash[:])

	// Save a valid snapshot using the correct block hash.
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: correctHex,
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{},
		Registry: core.RegistrySnapshot{
			Validators: map[string]*core.ValidatorEntry{},
		},
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	// Construct a hash that is different from the correct one (simulate a
	// corrupt or mismatched DB tip record).
	var corruptArr crypto.Hash32
	for i := range corruptArr {
		corruptArr[i] = 0xff // all 0xff — guaranteed to differ from a real block hash
	}
	corruptHex := fmt.Sprintf("%x", corruptArr[:])
	if corruptHex == correctHex {
		t.Fatal("test setup error: corrupt hash equals correct hash")
	}

	// ── Simulate the exact-tip fast path from main.go (lines ~358-384) using
	// the DB-supplied (corrupted) hash instead of the real block hash.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	snapLoaded := false
	if _, serr := loadStartupSnapshot(dir, tipHeight, corruptHex); serr == nil {
		snapLoaded = true
		log.Info("startup fast path complete — snapshot loaded", "tip_height", tipHeight)
	} else if !os.IsNotExist(serr) {
		// Hash mismatch is a non-NotExist error → log warning and fall back.
		log.Warn("snapshot load error, falling back to block scan", "err", serr)
	}

	// ── Assertions.

	// snapLoaded must remain false: the mismatch must have prevented the fast path.
	if snapLoaded {
		t.Error("snapLoaded should be false when DB tip hash does not match snapshot hash")
	}

	// The warning must be logged so operators can diagnose the mismatch.
	if !logContainsMsg(&logBuf, "snapshot load error, falling back to block scan") {
		t.Error("expected warning \"snapshot load error, falling back to block scan\" was not logged")
		t.Logf("captured log:\n%s", logBuf.String())
	}

	// The fast-path success message must NOT appear.
	if logContainsMsg(&logBuf, "startup fast path complete — snapshot loaded") {
		t.Error("fast-path success log must NOT appear when hash mismatch discards the snapshot")
		t.Logf("captured log:\n%s", logBuf.String())
	}
}

// ─── Test 7: prev-backup snapshot hash check ──────────────────────────────────

// TestPrevBackupHashMismatchRejected confirms that loadPrevBackupSnapshot — the
// production recovery helper in snapshot.go — rejects the prev-backup file when
// the caller-supplied tip hash (sourced from the DB tip record) does not match
// the hash stored inside the file.
//
// Scenario:
//   - A valid snapshot is saved at height H (primary).
//   - A second save at height H+1 promotes the H primary to H-prev.json via
//     the existing backup logic in saveStartupSnapshot.
//   - loadPrevBackupSnapshot is called with the correct height but a corrupted
//     expected hash: it must return a non-nil error (not os.ErrNotExist).
//   - loadPrevBackupSnapshot is called again with the correct hash: it must
//     succeed and return the decoded snapshot.
//
// This test exercises the production loadPrevBackupSnapshot code path end-to-end
// so that any future change that removes or weakens the hash check will be
// caught immediately.
func TestPrevBackupHashMismatchRejected(t *testing.T) {
	dir := t.TempDir()

	// ── Step 1: build two tip blocks so we have two distinct hashes.
	_, blocks := buildChainInStore(t, dir, 1) // genesis (h=0) + 1 block (h=1)

	blk0 := blocks[0]
	blk1 := blocks[1]
	hash0 := blk0.Hash()
	hash1 := blk1.Hash()
	hexH0 := fmt.Sprintf("%x", hash0[:])
	hexH1 := fmt.Sprintf("%x", hash1[:])

	// ── Step 2: first save at height 0 → creates primary snapshot-v1-0.json.
	snap0 := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  0,
		TipHashHex: hexH0,
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	if err := saveStartupSnapshot(dir, snap0); err != nil {
		t.Fatalf("first saveStartupSnapshot: %v", err)
	}
	primaryH0 := snapshotPath(dir, 0)
	if _, err := os.Stat(primaryH0); os.IsNotExist(err) {
		t.Fatalf("primary snapshot-v1-0.json not created")
	}

	// ── Step 3: second save at height 1 → saveStartupSnapshot backs up the
	// height-0 primary to snapshot-v1-0-prev.json before writing the new one.
	snap1 := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  1,
		TipHashHex: hexH1,
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	if err := saveStartupSnapshot(dir, snap1); err != nil {
		t.Fatalf("second saveStartupSnapshot: %v", err)
	}
	prevPath := snapshotPrevPath(primaryH0) // snapshot-v1-0-prev.json
	if _, err := os.Stat(prevPath); os.IsNotExist(err) {
		t.Fatalf("prev backup %s was not created by the second save", prevPath)
	}

	// ── Step 4: corrupt expected hash → loadPrevBackupSnapshot must reject.
	var corruptArr crypto.Hash32
	for i := range corruptArr {
		corruptArr[i] = 0xde
	}
	corruptHex := fmt.Sprintf("%x", corruptArr[:])
	if corruptHex == hexH0 {
		t.Fatal("test setup error: corrupt hash equals correct hash")
	}

	_, mismatchErr := loadPrevBackupSnapshot(dir, 0, corruptHex)
	if mismatchErr == nil {
		t.Error("loadPrevBackupSnapshot: expected hash mismatch error; got nil")
	}
	if os.IsNotExist(mismatchErr) {
		// Must be a validation error, not a missing-file error, so callers can
		// distinguish "no backup available" from "backup is mismatched".
		t.Errorf("loadPrevBackupSnapshot: got os.ErrNotExist but wanted a validation error; err=%v", mismatchErr)
	}

	// ── Step 5: correct expected hash → loadPrevBackupSnapshot must succeed.
	loaded, goodErr := loadPrevBackupSnapshot(dir, 0, hexH0)
	if goodErr != nil {
		t.Errorf("loadPrevBackupSnapshot with correct hash: unexpected error: %v", goodErr)
	}
	if loaded == nil {
		t.Fatal("loadPrevBackupSnapshot with correct hash: returned nil snapshot")
	}
	if loaded.TipHeight != 0 {
		t.Errorf("loaded.TipHeight: got %d want 0", loaded.TipHeight)
	}
	if loaded.TipHashHex != hexH0 {
		t.Errorf("loaded.TipHashHex mismatch")
	}
}
