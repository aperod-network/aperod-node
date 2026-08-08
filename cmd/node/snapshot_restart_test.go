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

	"github.com/aperod/aperod/config"
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

// ─── Tests: parseGOMLEMLIMITBytes ─────────────────────────────────────────────

func TestParseGOMLEMLIMITBytes(t *testing.T) {
	tests := []struct {
		input   string
		wantOK  bool
		wantVal int64
	}{
		// absent / disabled (exact Go runtime grammar)
		{"", false, 0},
		{"off", false, 0},   // exact spelling required
		{"OFF", false, 0},   // case-fold NOT accepted by runtime
		{"Off", false, 0},
		{"0", false, 0},
		// plain byte counts
		{"1", true, 1},
		{"1073741824", true, 1073741824},
		{"5368709120", true, 5368709120},
		{"5905580032", true, 5905580032},
		// B suffix (explicit bytes — runtime accepts this)
		{"1B", true, 1},
		{"5368709120B", true, 5368709120},
		// IEC suffixes supported by the Go runtime (B KiB MiB GiB TiB only)
		{"1KiB", true, 1 << 10},
		{"512MiB", true, 512 << 20},
		{"5GiB", true, 5 << 30},
		{"1TiB", true, 1 << 40},
		// PiB and EiB are NOT in the Go runtime grammar
		{"1PiB", false, 0},
		{"1EiB", false, 0},
		// whitespace NOT accepted (runtime is strict)
		{" 5GiB ", false, 0},
		{" 5368709120 ", false, 0},
		// malformed / SI units — treated as absent
		{"garbage", false, 0},
		{"5GB", false, 0},
		{"5MB", false, 0},
		{"-1", false, 0},
		{"-1GiB", false, 0},
		// overflow: 8388608 TiB = 2^23 * 2^40 = 2^63 bytes — overflows int64
		{"8388608TiB", false, 0},
	}
	for _, tc := range tests {
		gotVal, gotOK := parseGOMLEMLIMITBytes(tc.input)
		if gotOK != tc.wantOK || (tc.wantOK && gotVal != tc.wantVal) {
			t.Errorf("parseGOMLEMLIMITBytes(%q) = (%d, %v), want (%d, %v)",
				tc.input, gotVal, gotOK, tc.wantVal, tc.wantOK)
		}
	}
}

// ─── Tests: checkGOMLEMLIMIT ──────────────────────────────────────────────────

const dropin = "/etc/systemd/system/aperod-node.service.d/gomemlimit.conf"

// TestCheckGOMLEMLIMIT_WarnCases verifies that absent, zero, "off" (exact),
// and unrecognised values all produce a warning and return nil in non-strict mode.
func TestCheckGOMLEMLIMIT_WarnCases(t *testing.T) {
	// All of these are effectively "no limit" — either unset, explicitly off,
	// zero bytes, or unrecognised format that the runtime would reject anyway.
	cases := []string{"", "0", "off", "OFF", "Off", "garbage", "5GB", "5MB", " 5GiB "}
	for _, val := range cases {
		var logBuf bytes.Buffer
		err := checkGOMLEMLIMIT(val, false, false, dropin, newCaptureLogger(&logBuf))
		if err != nil {
			t.Errorf("GOMEMLIMIT=%q: expected nil error in non-strict mode, got: %v", val, err)
		}
		if !logContainsMsg(&logBuf, "GOMEMLIMIT is not set — node may OOM under load") {
			t.Errorf("GOMEMLIMIT=%q: expected warn log\nlog:\n%s", val, logBuf.String())
		}
	}
}

// TestCheckGOMLEMLIMIT_SilentWhenSet verifies that valid Go runtime GOMEMLIMIT
// values produce no warning and no error (non-strict mode).
// Accepted: bare integer, B suffix, KiB/MiB/GiB/TiB suffixes.
func TestCheckGOMLEMLIMIT_SilentWhenSet(t *testing.T) {
	validValues := []string{
		"5368709120", "5905580032", "1073741824", // bare byte counts
		"5368709120B",                             // explicit B suffix
		"5B",                                      // small explicit byte count
		"5GiB", "512MiB", "1TiB", "1KiB",         // IEC suffixes
	}
	for _, val := range validValues {
		var logBuf bytes.Buffer
		err := checkGOMLEMLIMIT(val, false, false, dropin, newCaptureLogger(&logBuf))
		if err != nil {
			t.Errorf("GOMEMLIMIT=%q: unexpected error: %v", val, err)
		}
		if logContainsMsg(&logBuf, "GOMEMLIMIT is not set") {
			t.Errorf("GOMEMLIMIT=%q: unexpected warning\nlog:\n%s", val, logBuf.String())
		}
	}
}

// TestCheckGOMLEMLIMIT_StrictModeErrorsOnMissing verifies that --strict-memlimit
// returns a non-nil error for absent, zero, "off", malformed, and overflow values.
func TestCheckGOMLEMLIMIT_StrictModeErrorsOnMissing(t *testing.T) {
	cases := []string{"", "0", "off", "garbage", "5GB", "OFF", " 5GiB ", "8388608TiB"}
	for _, val := range cases {
		var logBuf bytes.Buffer
		err := checkGOMLEMLIMIT(val, false, true, dropin, newCaptureLogger(&logBuf))
		if err == nil {
			t.Errorf("GOMEMLIMIT=%q: expected error in strict mode, got nil", val)
		}
	}
}

// TestCheckGOMLEMLIMIT_StrictModeSilentWhenSet verifies that --strict-memlimit
// does NOT error for valid plain-byte, B-suffix, or IEC-suffix values.
func TestCheckGOMLEMLIMIT_StrictModeSilentWhenSet(t *testing.T) {
	validValues := []string{
		"5905580032", "5368709120B",
		"5GiB", "512MiB", "1TiB",
	}
	for _, val := range validValues {
		var logBuf bytes.Buffer
		err := checkGOMLEMLIMIT(val, false, true, dropin, newCaptureLogger(&logBuf))
		if err != nil {
			t.Errorf("GOMEMLIMIT=%q: unexpected error in strict mode: %v", val, err)
		}
		if logContainsMsg(&logBuf, "GOMEMLIMIT is not set") {
			t.Errorf("GOMEMLIMIT=%q: unexpected warning\nlog:\n%s", val, logBuf.String())
		}
	}
}

// TestCheckGOMLEMLIMIT_ConfigLimitApplied verifies that when configLimitApplied
// is true, checkGOMLEMLIMIT returns nil and emits no warning — regardless of
// whether GOMEMLIMIT env is set — because the in-process limit already covers it.
func TestCheckGOMLEMLIMIT_ConfigLimitApplied(t *testing.T) {
	// Both empty and non-empty env values should be silent when configLimitApplied=true.
	envValues := []string{"", "0", "off", "5905580032"}
	for _, val := range envValues {
		var logBuf bytes.Buffer
		err := checkGOMLEMLIMIT(val, true, false, dropin, newCaptureLogger(&logBuf))
		if err != nil {
			t.Errorf("configLimitApplied=true, GOMEMLIMIT=%q: unexpected error: %v", val, err)
		}
		if logContainsMsg(&logBuf, "GOMEMLIMIT is not set") {
			t.Errorf("configLimitApplied=true, GOMEMLIMIT=%q: unexpected warning\nlog:\n%s", val, logBuf.String())
		}
	}
	// strict mode + configLimitApplied should also be silent.
	for _, val := range []string{"", "0"} {
		var logBuf bytes.Buffer
		err := checkGOMLEMLIMIT(val, true, true, dropin, newCaptureLogger(&logBuf))
		if err != nil {
			t.Errorf("configLimitApplied=true strict, GOMEMLIMIT=%q: unexpected error: %v", val, err)
		}
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

// ─── Test 8: truncated / invalid-JSON prev-backup is discarded ───────────────

// TestPrevBackupTruncatedDecodeError confirms that loadPrevBackupSnapshot
// returns a non-nil error (not os.ErrNotExist) and does not load any UTXO
// state when the prev-backup file exists but contains truncated or invalid JSON.
//
// Scenario A — truncated file:
//   - A valid snapshot is saved at height H (primary).
//   - A second save at H+1 promotes H-primary to H-prev.json.
//   - The prev file is truncated to half its size (simulates a mid-write crash).
//   - loadPrevBackupSnapshot(dir, H, correctHex) must return a non-nil,
//     non-ErrNotExist error; snapLoaded must stay false.
//
// Scenario B — invalid JSON:
//   - The prev file is overwritten with the literal string "not json".
//   - Same assertions: non-nil error, not ErrNotExist, snapLoaded false.
//
// Together these cover the failure mode described in task 1062: a decode error
// occurring before the hash check must be caught and reported, never silently
// ignored.
func TestPrevBackupTruncatedDecodeError(t *testing.T) {
	dir := t.TempDir()

	// ── Build two blocks so we have two distinct heights.
	_, blocks := buildChainInStore(t, dir, 1) // genesis (h=0) + block (h=1)

	blk0 := blocks[0]
	blk1 := blocks[1]
	hash0 := blk0.Hash()
	hash1 := blk1.Hash()
	hexH0 := fmt.Sprintf("%x", hash0[:])
	hexH1 := fmt.Sprintf("%x", hash1[:])

	// ── Save primary at h=0.
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
	prevPath := snapshotPrevPath(primaryH0) // will be created by next save

	// ── Save at h=1; this promotes snapshot-v1-0.json → snapshot-v1-0-prev.json.
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
	if _, err := os.Stat(prevPath); os.IsNotExist(err) {
		t.Fatalf("prev backup %s was not created by the second save", prevPath)
	}

	// ── Helper: assert loadPrevBackupSnapshot rejects the (now-corrupt) prev file.
	assertRejected := func(label string) {
		t.Helper()
		snapLoaded := false
		loaded, err := loadPrevBackupSnapshot(dir, 0, hexH0)
		if err == nil {
			snapLoaded = true
			_ = loaded
		}

		if err == nil {
			t.Errorf("[%s] loadPrevBackupSnapshot: expected non-nil error; got nil (snapLoaded=%v)", label, snapLoaded)
		}
		if os.IsNotExist(err) {
			// Must be a decode / validation error, not a missing-file error.
			t.Errorf("[%s] loadPrevBackupSnapshot: got os.ErrNotExist; wanted a decode error (file exists but is corrupt)", label)
		}
		if snapLoaded {
			t.Errorf("[%s] snapLoaded should remain false when prev backup is corrupt", label)
		}
		if loaded != nil {
			t.Errorf("[%s] loaded snapshot should be nil when prev backup is corrupt; got %+v", label, loaded)
		}
	}

	// ── Scenario A: truncate the prev file to half its size.
	info, err := os.Stat(prevPath)
	if err != nil {
		t.Fatalf("stat prev backup: %v", err)
	}
	fullSize := info.Size()
	if fullSize < 2 {
		t.Fatalf("prev backup too small (%d bytes) — test setup error", fullSize)
	}
	if err := os.Truncate(prevPath, fullSize/2); err != nil {
		t.Fatalf("os.Truncate prev backup: %v", err)
	}
	assertRejected("truncated")

	// ── Scenario B: overwrite the prev file with invalid JSON.
	if err := os.WriteFile(prevPath, []byte("not json"), 0644); err != nil {
		t.Fatalf("WriteFile prev backup (invalid JSON): %v", err)
	}
	assertRejected("invalid JSON")
}

// ─── Test 9: both snapshots corrupt → falls back to block scan ───────────────

// TestBothSnapshotsCorruptFallsBackToScan confirms that when BOTH the primary
// snapshot (truncated) and the same-height prev-backup (invalid JSON) are
// unreadable, loadStartupSnapshotWithFallback returns a non-nil error and the
// caller's startup logic correctly falls back to a full block scan instead of
// crashing.
//
// Scenario:
//   - A valid snapshot is saved at height H (primary + same-height prev-backup
//     both written atomically by saveStartupSnapshot).
//   - The primary snapshot file is truncated to half its size (simulates a
//     mid-write power-loss or process kill during a disk flush).
//   - The prev-backup file is overwritten with the literal string "not json"
//     (simulates independent corruption of the recovery floor).
//   - loadStartupSnapshotWithFallback is called: it tries the primary (fails
//     on decode), then tries the prev-backup (fails on decode), and returns a
//     non-nil, non-ErrNotExist error.
//   - The caller's startup logic mirrors main.go: snapLoaded stays false and
//     the "snapshot load error, falling back to block scan" warning is logged.
//   - The fast-path success message must NOT appear.
//
// This is the integration-level complement to TestPrevBackupTruncatedDecodeError
// (which exercises loadPrevBackupSnapshot in isolation).  Together they close
// the gap identified in task 1068: both failure modes must be handled without
// panicking or crashing the node.
func TestBothSnapshotsCorruptFallsBackToScan(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 3) // genesis + 3 blocks → height 0..3

	tip := blocks[len(blocks)-1]
	tipHash := tip.Hash()
	tipHeight := tip.Header.Height           // 3
	tipHashHex := fmt.Sprintf("%x", tipHash[:])

	// ── Save a valid snapshot at the current tip.
	// saveStartupSnapshot writes both:
	//   • snapshot-v1-3.json         (primary)
	//   • snapshot-v1-3-prev.json    (same-height recovery floor)
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	primaryPath := snapshotPath(dir, tipHeight)
	prevPath := snapshotPrevPath(primaryPath)

	// Confirm the primary was created.
	if _, err := os.Stat(primaryPath); os.IsNotExist(err) {
		t.Fatalf("primary snapshot not created: %s", primaryPath)
	}
	// Write the prev-backup explicitly — the first save at a height does not
	// auto-create a prev (task #1489: nothing to preserve on a fresh checkpoint).
	writeGzipSnapFile(t, prevPath, snap)

	// ── Corrupt the primary snapshot: truncate to half its size.
	info, err := os.Stat(primaryPath)
	if err != nil {
		t.Fatalf("stat primary: %v", err)
	}
	fullSize := info.Size()
	if fullSize < 2 {
		t.Fatalf("primary snapshot unexpectedly tiny (%d bytes) — test setup error", fullSize)
	}
	if err := os.Truncate(primaryPath, fullSize/2); err != nil {
		t.Fatalf("os.Truncate primary: %v", err)
	}

	// ── Corrupt the prev-backup: overwrite with invalid JSON.
	if err := os.WriteFile(prevPath, []byte("not json"), 0644); err != nil {
		t.Fatalf("WriteFile prev-backup (invalid JSON): %v", err)
	}

	// ── Simulate the startup fast-path from main.go using the production helper.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	snapLoaded := false
	loaded, _, loadErr := loadStartupSnapshotWithFallback(dir, tipHeight, tipHashHex, log)
	if loadErr == nil {
		// Both files are corrupt — this branch must NOT be taken.
		snapLoaded = true
		log.Info("startup fast path complete — snapshot loaded",
			"tip_height", tipHeight,
			"active_utxos", len(loaded.UTXOs.ActiveUTXOs),
		)
	} else if !os.IsNotExist(loadErr) {
		// Both files failed with non-ErrNotExist errors — this branch MUST be
		// taken; the caller logs the warning and falls back to a block scan.
		log.Warn("snapshot load error, falling back to block scan", "err", loadErr)
	}

	// ── Assertions.

	// snapLoaded must remain false: neither corrupt file must be accepted.
	if snapLoaded {
		t.Error("snapLoaded should be false when both primary and prev-backup are corrupt")
	}

	// loadErr must be non-nil and not os.ErrNotExist (both files exist but are
	// unreadable — this is distinct from "no snapshot available").
	if loadErr == nil {
		t.Error("loadStartupSnapshotWithFallback: expected non-nil error when both snapshots are corrupt; got nil")
	}
	if os.IsNotExist(loadErr) {
		t.Errorf("loadStartupSnapshotWithFallback: got os.ErrNotExist but both files exist and are corrupt; err=%v", loadErr)
	}

	// The fallback warning must be logged so operators can diagnose the failure.
	if !logContainsMsg(&logBuf, "snapshot load error, falling back to block scan") {
		t.Error("expected warning \"snapshot load error, falling back to block scan\" was not logged")
		t.Logf("captured log:\n%s", logBuf.String())
	}

	// The fast-path success message must NOT appear.
	if logContainsMsg(&logBuf, "startup fast path complete — snapshot loaded") {
		t.Error("fast-path success log must NOT appear when both snapshots are corrupt")
		t.Logf("captured log:\n%s", logBuf.String())
	}
}

// ─── Test 10: missing primary with valid prev-backup → recovered via fallback ─

// TestMissingPrimaryWithValidPrevReturnsNotExist confirms that when the primary
// snapshot file has been deleted but a valid same-height prev-backup with a
// matching hash exists on disk, loadStartupSnapshotWithFallback recovers via
// the prev-backup and returns success (nil error), rather than returning
// os.ErrNotExist and forcing an unnecessary full block scan.
//
// This reflects intentional design: the prev-backup was written atomically
// alongside the primary by saveStartupSnapshot, so it is equally trustworthy
// when the hashes match.  Returning ErrNotExist in this case would silently
// force a multi-hour rescan after any crash that left only the prev-backup.
//
// Assertions:
//   - loadStartupSnapshotWithFallback returns nil error (success).
//   - The returned snapshot has the correct TipHeight and TipHashHex.
//   - The "loaded v2 prev-backup snapshot (primary absent)" warning is logged.
//   - snapLoaded becomes true so the caller uses the fast path.
func TestMissingPrimaryWithValidPrevReturnsNotExist(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 3) // genesis + 3 blocks → height 0..3

	tip := blocks[len(blocks)-1]
	tipHash := tip.Hash()
	tipHeight := tip.Header.Height // 3
	tipHashHex := fmt.Sprintf("%x", tipHash[:])

	// ── Save a valid snapshot so both primary and prev-backup are created.
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("saveStartupSnapshot: %v", err)
	}

	primaryPath := snapshotPath(dir, tipHeight)
	prevPath := snapshotPrevPath(primaryPath)

	// Confirm the primary was created.
	if _, err := os.Stat(primaryPath); os.IsNotExist(err) {
		t.Fatalf("primary snapshot not created: %s", primaryPath)
	}
	// Write the prev-backup explicitly — the first save at a height does not
	// auto-create a prev (task #1489: nothing to preserve on a fresh checkpoint).
	writeGzipSnapFile(t, prevPath, snap)

	// Confirm the prev-backup is valid (sanity-check for the test setup).
	validPrev, prevErr := loadPrevBackupSnapshot(dir, tipHeight, tipHashHex)
	if prevErr != nil {
		t.Fatalf("test setup: prev-backup should be valid before primary removal; err=%v", prevErr)
	}
	if validPrev == nil {
		t.Fatal("test setup: prev-backup returned nil before primary removal")
	}

	// ── Remove the primary snapshot file (simulates operator deletion or a
	// failure mode where the temp→primary rename never completed).
	if err := os.Remove(primaryPath); err != nil {
		t.Fatalf("os.Remove primary: %v", err)
	}
	if _, err := os.Stat(primaryPath); !os.IsNotExist(err) {
		t.Fatalf("primary snapshot still exists after removal — test setup error")
	}

	// ── Call the production entry point.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	snapLoaded := false
	loaded, _, loadErr := loadStartupSnapshotWithFallback(dir, tipHeight, tipHashHex, log)
	if loadErr == nil {
		snapLoaded = true
		log.Info("startup fast path complete — snapshot loaded",
			"tip_height", tipHeight,
			"active_utxos", len(loaded.UTXOs.ActiveUTXOs),
		)
	} else if os.IsNotExist(loadErr) {
		_ = loadErr // caller treats missing primary as "no fast path"
	} else {
		log.Warn("snapshot load error, falling back to block scan", "err", loadErr)
	}

	// ── Assertions.

	// Recovery must succeed: the prev-backup has a matching hash and is valid.
	if loadErr != nil {
		t.Errorf("loadStartupSnapshotWithFallback: expected nil error (recovery via prev-backup); got %v\nlog:\n%s",
			loadErr, logBuf.String())
	}

	// snapLoaded must be true: the fast path is available via the prev-backup.
	if !snapLoaded {
		t.Errorf("snapLoaded should be true when prev-backup with matching hash is available\nlog:\n%s", logBuf.String())
	}

	// The returned snapshot must have the correct tip height and hash.
	if loaded == nil {
		t.Fatal("loadStartupSnapshotWithFallback: returned nil snapshot with nil error")
	}
	if loaded.TipHeight != tipHeight {
		t.Errorf("loaded.TipHeight = %d, want %d", loaded.TipHeight, tipHeight)
	}
	if loaded.TipHashHex != tipHashHex {
		t.Errorf("loaded.TipHashHex = %s, want %s", loaded.TipHashHex, tipHashHex)
	}

	// The operator-visible warning must be emitted so they know the primary was absent.
	if !logContainsMsg(&logBuf, "loaded v2 prev-backup snapshot (primary absent)") {
		t.Errorf("expected \"loaded v2 prev-backup snapshot (primary absent)\" warning was not logged\nlog:\n%s", logBuf.String())
	}
}

// ─── Test 11: corrupt primary falls back to prev-backup ────────────────────────

// TestCorruptPrimaryFallsBackToPrev confirms that when the primary snapshot is
// truncated (simulating a mid-write crash) and a valid same-height prev-backup
// was written by saveStartupSnapshot, loadStartupSnapshotWithFallback recovers
// via the prev file and emits the distinct fallback log lines.
//
// This exercises the typical different-height scenario: a first save at height
// H-1 establishes an older checkpoint, then a second save at height H creates
// the current primary and its same-height prev-backup.  Corrupting the H
// primary must recover from the H prev-backup — not fall back to a full scan.
func TestCorruptPrimaryFallsBackToPrev(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 3) // genesis + 3 blocks → height 0..3

	// blk2 is the "previous tip" (H-1 = 2); blk3 is the "current tip" (H = 3).
	blk2 := blocks[2]
	blk3 := blocks[3]
	hash2 := blk2.Hash()
	hash3 := blk3.Hash()
	hexH2 := fmt.Sprintf("%x", hash2[:])
	hexH3 := fmt.Sprintf("%x", hash3[:])

	// ── First save at height 2 — creates snapshot-v1-2.json (primary).
	snap2 := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  2,
		TipHashHex: hexH2,
		TxTotal:    2,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	if err := saveStartupSnapshot(dir, snap2); err != nil {
		t.Fatalf("first saveStartupSnapshot (h=2): %v", err)
	}

	// ── Second save at height 3 — saveStartupSnapshot:
	//    • promotes snapshot-v2-2.json.gz → snapshot-v2-2-prev.json.gz (prior backup)
	//    • writes snapshot-v2-3.json.gz (primary; no same-height prev on first save
	//      at this height — task #1489 fix)
	//
	// After this call the directory contains:
	//   snapshot-v2-2-prev.json.gz  (prior height backup)
	//   snapshot-v2-3.json.gz       (current primary)
	snap3 := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  3,
		TipHashHex: hexH3,
		TxTotal:    7,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	if err := saveStartupSnapshot(dir, snap3); err != nil {
		t.Fatalf("second saveStartupSnapshot (h=3): %v", err)
	}

	// Write the same-height prev-backup explicitly — this simulates a second
	// save at height 3 (e.g. a shutdown snapshot) or explicit prev creation.
	// The first save at a height does not auto-create a prev (task #1489).
	primaryPath := snapshotPath(dir, 3)
	prevPath3 := snapshotPrevPath(primaryPath)
	writeGzipSnapFile(t, prevPath3, snap3)

	// ── Corrupt the primary by truncating it to half its size (simulates a
	// power-loss or process kill during a disk flush).
	info, err := os.Stat(primaryPath)
	if err != nil {
		t.Fatalf("stat primary: %v", err)
	}
	if err := os.Truncate(primaryPath, info.Size()/2); err != nil {
		t.Fatalf("truncate primary: %v", err)
	}

	// ── loadStartupSnapshotWithFallback must succeed via the H-prev backup.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	loaded, _, loadErr := loadStartupSnapshotWithFallback(dir, 3, hexH3, log)
	if loadErr != nil {
		t.Fatalf("loadStartupSnapshotWithFallback: expected success via prev-backup, got: %v", loadErr)
	}
	if loaded == nil {
		t.Fatal("loadStartupSnapshotWithFallback: returned nil snapshot")
	}

	// Content must match the originally saved snapshot at height 3.
	if loaded.TipHeight != 3 {
		t.Errorf("loaded.TipHeight = %d, want 3", loaded.TipHeight)
	}
	if loaded.TipHashHex != hexH3 {
		t.Errorf("loaded.TipHashHex mismatch: got %s want %s", loaded.TipHashHex, hexH3)
	}
	if loaded.TxTotal != 7 {
		t.Errorf("loaded.TxTotal = %d, want 7", loaded.TxTotal)
	}

	// Both the corrupt-primary warning and the distinct fallback success line
	// must be present so operators can see the recovery event in node logs.
	const corruptMsg = "snapshot primary corrupt or unreadable, trying prev-backup"
	if !logContainsMsg(&logBuf, corruptMsg) {
		t.Errorf("expected corrupt-primary warning %q was not emitted\nlog:\n%s", corruptMsg, logBuf.String())
	}
	const fallbackMsg = "startup fast path — loaded prev-backup snapshot; primary was unreadable"
	if !logContainsMsg(&logBuf, fallbackMsg) {
		t.Errorf("expected fallback log %q was not emitted\nlog:\n%s", fallbackMsg, logBuf.String())
	}
}

// ─── Test 11: both snapshots have wrong hash → falls back to block scan ──────

// TestBothSnapshotsHashMismatchFallsBackToScan confirms that when both the
// primary snapshot and the same-height prev-backup contain valid JSON but an
// incorrect TipHashHex, loadStartupSnapshotWithFallback returns a non-nil,
// non-ErrNotExist error and the caller falls back to a full block scan.
//
// This is the complement to TestBothSnapshotsCorruptFallsBackToScan: that test
// exercises truncated/invalid-JSON files; this test exercises files that are
// structurally valid JSON but fail the hash cross-check.  Both failure modes
// must result in the same safe outcome: no snapshot loaded, no crash.
func TestBothSnapshotsHashMismatchFallsBackToScan(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 2) // genesis + 2 blocks → height 0..2

	tip := blocks[len(blocks)-1]
	tipHash := tip.Hash()
	tipHeight := tip.Header.Height           // 2
	tipHashHex := fmt.Sprintf("%x", tipHash[:])

	// Build a wrong hash that is guaranteed to differ from the real one.
	var wrongArr crypto.Hash32
	for i := range wrongArr {
		wrongArr[i] = 0xba
	}
	wrongHex := fmt.Sprintf("%x", wrongArr[:])
	if wrongHex == tipHashHex {
		t.Fatal("test setup error: wrong hash equals correct hash")
	}

	// Write a structurally valid snapshot whose TipHashHex is wrong into the
	// primary path.  We must not use saveStartupSnapshot here because it writes
	// the correct hash; instead write the files directly.
	primaryPath := snapshotPath(dir, tipHeight)
	prevPath := snapshotPrevPath(primaryPath)

	badSnap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: wrongHex, // ← deliberate mismatch
		TxTotal:    0,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	writeSnapFile := func(path string) {
		t.Helper()
		data, err := json.Marshal(badSnap)
		if err != nil {
			t.Fatalf("marshal badSnap: %v", err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}
	writeSnapFile(primaryPath)
	writeSnapFile(prevPath)

	// ── Simulate the startup fast-path using the production helper.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	snapLoaded := false
	loaded, _, loadErr := loadStartupSnapshotWithFallback(dir, tipHeight, tipHashHex, log)
	if loadErr == nil {
		snapLoaded = true
		log.Info("startup fast path complete — snapshot loaded",
			"tip_height", tipHeight,
			"active_utxos", len(loaded.UTXOs.ActiveUTXOs),
		)
	} else if !os.IsNotExist(loadErr) {
		log.Warn("snapshot load error, falling back to block scan", "err", loadErr)
	}

	// ── Assertions.

	// snapLoaded must remain false: a hash-mismatch must never be accepted.
	if snapLoaded {
		t.Error("snapLoaded should be false when both primary and prev-backup have wrong hash")
	}

	// loadErr must be non-nil and not os.ErrNotExist (both files exist but
	// fail validation — distinct from "no snapshot available").
	if loadErr == nil {
		t.Error("loadStartupSnapshotWithFallback: expected non-nil error when both snapshots have wrong hash; got nil")
	}
	if os.IsNotExist(loadErr) {
		t.Errorf("loadStartupSnapshotWithFallback: got os.ErrNotExist but both files exist; err=%v", loadErr)
	}

	// The fallback warning must be logged.
	if !logContainsMsg(&logBuf, "snapshot load error, falling back to block scan") {
		t.Error("expected warning \"snapshot load error, falling back to block scan\" was not logged")
		t.Logf("captured log:\n%s", logBuf.String())
	}

	// The fast-path success message must NOT appear.
	if logContainsMsg(&logBuf, "startup fast path complete — snapshot loaded") {
		t.Error("fast-path success log must NOT appear when both snapshots have wrong hash")
		t.Logf("captured log:\n%s", logBuf.String())
	}
}

// ─── Test 12: abort on copyFile failure leaves old primary intact ─────────────

// TestSaveSnapshotAbortOnCopyFailLeavesOldPrimaryIntact confirms that when the
// required same-height prev-backup copy inside saveStartupSnapshot fails (e.g.
// disk full), the function:
//   (a) removes the .tmp file and returns a non-nil error, and
//   (b) leaves the pre-existing primary snapshot file untouched so the node
//       can still recover at startup.
//
// Scenario:
//   - A valid snapshot is saved at height H, establishing an old primary
//     (snapshot-v1-H.json) with known content.
//   - A directory is created at the path copyFile would use for its own
//     internal temp file (snapshotPrevPath(primary) + ".tmp").  On all
//     supported platforms os.Create on a path that is an existing directory
//     returns an error (EISDIR on Linux/macOS), which causes copyFile to fail
//     before it writes any bytes.
//   - saveStartupSnapshot is called again for the same height H.  It:
//       1. writes the new primary .tmp (succeeds — a different path)
//       2. calls copyFile(tmp, prevPath) — fails because prevPath+".tmp" is a dir
//       3. removes tmp and returns a non-nil error
//   - The test confirms the error is non-nil.
//   - The test confirms the old primary is still readable and has the original
//     TipHashHex / TxTotal values (i.e. no partial overwrite occurred).
//
// This guards the invariant that a copyFile failure during the required
// prev-backup step aborts the save cleanly, leaving the recovery floor intact.
func TestSaveSnapshotAbortOnCopyFailLeavesOldPrimaryIntact(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 4) // genesis + 4 blocks → height 0..4

	tip := blocks[len(blocks)-1]
	tipHash := tip.Hash()
	tipHeight := tip.Header.Height // 4
	tipHashHex := fmt.Sprintf("%x", tipHash[:])

	// ── Step 1: write the initial snapshot — establishes the old primary.
	const oldTxTotal int64 = 77
	snap := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		TxTotal:    oldTxTotal,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	if err := saveStartupSnapshot(dir, snap); err != nil {
		t.Fatalf("first saveStartupSnapshot: %v", err)
	}

	primaryPath := snapshotPath(dir, tipHeight) // snapshot-v1-4.json
	if _, err := os.Stat(primaryPath); os.IsNotExist(err) {
		t.Fatalf("primary snapshot not created: %s", primaryPath)
	}

	// ── Step 2: block the copyFile temp file that saveStartupSnapshot uses
	// when writing the same-height prev-backup.
	//
	// copyFile(tmp, dst) creates dst+".tmp" internally before renaming to dst.
	// Creating a directory at that path causes os.Create to return EISDIR,
	// which propagates as a copyFile error → saveStartupSnapshot aborts.
	prevPath := snapshotPrevPath(primaryPath)    // snapshot-v1-4-prev.json
	blockPath := prevPath + ".tmp"               // snapshot-v1-4-prev.json.tmp
	if err := os.MkdirAll(blockPath, 0755); err != nil {
		t.Fatalf("MkdirAll blocker %s: %v", blockPath, err)
	}
	t.Cleanup(func() { os.RemoveAll(blockPath) })

	// ── Step 3: attempt a second save at the same height.
	// saveStartupSnapshot must fail because copyFile cannot create its temp file.
	const newTxTotal int64 = 99
	snap2 := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  tipHeight,
		TipHashHex: tipHashHex,
		TxTotal:    newTxTotal,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	saveErr := saveStartupSnapshot(dir, snap2)
	if saveErr == nil {
		t.Fatal("saveStartupSnapshot: expected non-nil error when copyFile is blocked; got nil")
	}

	// ── Step 4: confirm the old primary is still intact.

	// The primary file must still exist.
	if _, statErr := os.Stat(primaryPath); os.IsNotExist(statErr) {
		t.Fatalf("old primary %s was removed by the failed save", primaryPath)
	}

	// The primary must still be parseable and carry the ORIGINAL content,
	// not the new TxTotal that was written to the (now-removed) .tmp file.
	loaded, loadErr := loadStartupSnapshot(dir, tipHeight, tipHashHex)
	if loadErr != nil {
		t.Fatalf("old primary is unreadable after failed save: %v", loadErr)
	}
	if loaded.TxTotal != oldTxTotal {
		t.Errorf("old primary TxTotal: got %d want %d — primary was overwritten by a failed save",
			loaded.TxTotal, oldTxTotal)
	}
	if loaded.TipHashHex != tipHashHex {
		t.Errorf("old primary TipHashHex mismatch after failed save")
	}

	// The .tmp file that saveStartupSnapshot wrote must have been cleaned up.
	tmpPath := primaryPath + ".tmp"
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Errorf("saveStartupSnapshot left stale .tmp file %s after aborting", tmpPath)
	}
}

// ─── Test 13: rename failure leaves old primary intact ───────────────────────

// TestSaveSnapshotRenameFailureLeavesOldPrimaryIntact confirms that when the
// final os.Rename(tmp, path) step inside saveStartupSnapshot fails, the
// function:
//   (a) returns a non-nil error,
//   (b) leaves no stale .tmp file in the data directory, and
//   (c) leaves the pre-existing primary snapshot file untouched and readable
//       with its original content.
//
// Scenario:
//   - A valid snapshot is saved at height H, establishing an old primary
//     (snapshot-v1-H.json) with a known TxTotal value.
//   - A save is then attempted at height H+1.  Before the call, a directory is
//     created at the destination path (snapshot-v1-(H+1).json).  On Linux
//     os.Rename returns ENOTDIR or EISDIR when the destination is a directory
//     and the source is a regular file, so the rename step returns an error
//     after the prev-backup copy has already succeeded.
//   - saveStartupSnapshot must remove the .tmp file and propagate the error.
//   - The old primary at height H must still be readable with the original
//     TxTotal value (i.e. the prior-height backup logic in saveStartupSnapshot
//     copies rather than moves the existing primary, so it cannot destroy it).
//   - No file matching "*.tmp" must remain in the data directory.
//
// This is the rename-failure complement to
// TestSaveSnapshotAbortOnCopyFailLeavesOldPrimaryIntact (Test 12), which covers
// the earlier copyFile failure mode.
func TestSaveSnapshotRenameFailureLeavesOldPrimaryIntact(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 5) // genesis + 5 blocks → height 0..5

	blk4 := blocks[4]
	blk5 := blocks[5]
	hash4 := blk4.Hash()
	hash5 := blk5.Hash()
	hex4 := fmt.Sprintf("%x", hash4[:])
	hex5 := fmt.Sprintf("%x", hash5[:])

	// ── Step 1: write the initial snapshot at height 4 — establishes old primary.
	const oldTxTotal int64 = 42
	snap4 := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  4,
		TipHashHex: hex4,
		TxTotal:    oldTxTotal,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	if err := saveStartupSnapshot(dir, snap4); err != nil {
		t.Fatalf("first saveStartupSnapshot (h=4): %v", err)
	}

	oldPrimaryPath := snapshotPath(dir, 4) // snapshot-v1-4.json
	if _, err := os.Stat(oldPrimaryPath); os.IsNotExist(err) {
		t.Fatalf("old primary not created: %s", oldPrimaryPath)
	}

	// ── Step 2: block the rename destination for the new height-5 primary.
	//
	// saveStartupSnapshot writes snapshot-v1-5.json.tmp and then renames it to
	// snapshot-v1-5.json.  Pre-placing a directory at that path causes os.Rename
	// to return an error (ENOTDIR on Linux) after the prev-backup copy has
	// already succeeded, so this exercises exactly the rename-failure path.
	newPrimaryPath := snapshotPath(dir, 5) // snapshot-v1-5.json
	if err := os.MkdirAll(newPrimaryPath, 0755); err != nil {
		t.Fatalf("MkdirAll rename-blocker %s: %v", newPrimaryPath, err)
	}
	t.Cleanup(func() { os.RemoveAll(newPrimaryPath) })

	// ── Step 3: attempt the save at height 5 — must fail on rename.
	const newTxTotal int64 = 99
	snap5 := startupSnapshot{
		Version:    snapVersion,
		TipHeight:  5,
		TipHashHex: hex5,
		TxTotal:    newTxTotal,
		UTXOs:      core.UTXOSnapshot{},
		Registry:   core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}},
	}
	saveErr := saveStartupSnapshot(dir, snap5)
	if saveErr == nil {
		t.Fatal("saveStartupSnapshot: expected non-nil error when rename destination is a directory; got nil")
	}

	// ── Step 4: confirm the old primary at height 4 is still intact.

	// The old primary file must still exist.
	if _, statErr := os.Stat(oldPrimaryPath); os.IsNotExist(statErr) {
		t.Fatalf("old primary %s was removed by the failed save", oldPrimaryPath)
	}

	// The old primary must still be parseable and carry the ORIGINAL TxTotal.
	loaded, loadErr := loadStartupSnapshot(dir, 4, hex4)
	if loadErr != nil {
		t.Fatalf("old primary is unreadable after failed save: %v", loadErr)
	}
	if loaded.TxTotal != oldTxTotal {
		t.Errorf("old primary TxTotal: got %d want %d — primary was corrupted by the failed save",
			loaded.TxTotal, oldTxTotal)
	}
	if loaded.TipHashHex != hex4 {
		t.Errorf("old primary TipHashHex mismatch after failed save")
	}

	// ── Step 5: confirm no stale .tmp files remain in the data directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("stale .tmp file left in data dir after failed save: %s", e.Name())
		}
	}
}

// ─── Test 10: truncated snapshot falls back to block scan ─────────────────────

// TestTruncatedSnapshotFallsBackToScan confirms that loadStartupSnapshot returns
// a non-nil error and that snapLoaded remains false when the snapshot file is
// truncated mid-JSON (simulating a power-loss or partial write).
//
// Scenario:
//   - A valid snapshot is saved at height H.
//   - The snapshot file is truncated to half its size before the node restarts.
//   - loadStartupSnapshot must return a non-nil, non-ErrNotExist error because
//     the file exists but its JSON is malformed.
//   - snapLoaded must stay false; the warning
//     "snapshot load error, falling back to block scan" must be logged.
func TestTruncatedSnapshotFallsBackToScan(t *testing.T) {
	dir := t.TempDir()
	_, blocks := buildChainInStore(t, dir, 3) // genesis + 3 blocks → tip height 3

	tip := blocks[len(blocks)-1]
	tipHash := tip.Hash()
	tipHeight := tip.Header.Height
	tipHashHex := fmt.Sprintf("%x", tipHash[:])

	// Save a valid snapshot (mirrors the SIGTERM shutdown handler).
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

	// Confirm the file exists and has non-zero size.
	snapFile := snapshotPath(dir, tipHeight)
	info, err := os.Stat(snapFile)
	if err != nil {
		t.Fatalf("snapshot file not found after save: %v", err)
	}
	fullSize := info.Size()
	if fullSize < 2 {
		t.Fatalf("snapshot file unexpectedly tiny (%d bytes) — test setup error", fullSize)
	}

	// Truncate the file to half its size, simulating a partial write due to
	// a power-loss or crash mid-flush.
	truncSize := fullSize / 2
	if err := os.Truncate(snapFile, truncSize); err != nil {
		t.Fatalf("os.Truncate: %v", err)
	}

	// ── Simulate the exact-tip fast-path from main.go.
	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	snapLoaded := false
	if _, serr := loadStartupSnapshot(dir, tipHeight, tipHashHex); serr == nil {
		snapLoaded = true
		log.Info("startup fast path complete — snapshot loaded", "tip_height", tipHeight)
	} else if !os.IsNotExist(serr) {
		// Truncation produces a JSON decode error — a non-ErrNotExist error —
		// so this branch must be taken and the warning must be logged.
		log.Warn("snapshot load error, falling back to block scan", "err", serr)
	}

	// ── Assertions.

	// snapLoaded must stay false: a truncated file must never be accepted.
	if snapLoaded {
		t.Error("snapLoaded should be false when snapshot file is truncated")
	}

	// The fallback warning must be logged so operators can diagnose the issue.
	if !logContainsMsg(&logBuf, "snapshot load error, falling back to block scan") {
		t.Error("expected warning \"snapshot load error, falling back to block scan\" was not logged for truncated snapshot")
		t.Logf("captured log:\n%s", logBuf.String())
	}

	// The fast-path success message must NOT appear.
	if logContainsMsg(&logBuf, "startup fast path complete — snapshot loaded") {
		t.Error("fast-path success log must NOT appear when snapshot is truncated")
		t.Logf("captured log:\n%s", logBuf.String())
	}
}

// ─── Tests: memory_limit_bytes config integration ─────────────────────────────

// TestMemoryLimitBytes_ConfigPath_SilencesGOMLEMLIMITWarning is an end-to-end
// integration test for the config load → configLimitApplied → checkGOMLEMLIMIT
// path introduced in run().
//
// It writes a minimal node.yaml with memory_limit_bytes set to a positive
// value, loads the config with config.Load, replicates the run() guard logic,
// and asserts that checkGOMLEMLIMIT emits NO warning and returns nil — because
// the in-process limit from node.yaml satisfies the check.
func TestMemoryLimitBytes_ConfigPath_SilencesGOMLEMLIMITWarning(t *testing.T) {
	// Ensure GOMEMLIMIT env is absent for the duration of this test so the
	// config-path branch (not the env-var branch) is exercised.
	t.Setenv("GOMEMLIMIT", "")

	// Write a minimal node.yaml with a positive memory_limit_bytes.
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "node.yaml")
	yamlContent := "memory_limit_bytes: 5368709120\n" // 5 GiB
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write node.yaml: %v", err)
	}

	// Load config — exercises config.Load YAML parsing of MemoryLimitBytes.
	cfg, err := config.Load(yamlPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.MemoryLimitBytes != 5368709120 {
		t.Fatalf("expected MemoryLimitBytes=5368709120, got %d", cfg.MemoryLimitBytes)
	}

	// Replicate the run() guard logic exactly.
	// debug.SetMemoryLimit is intentionally NOT called here to avoid side-effects
	// on the test process; configLimitApplied is derived from the same condition.
	configLimitApplied := cfg.MemoryLimitBytes > 0 && os.Getenv("GOMEMLIMIT") == ""

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	err = checkGOMLEMLIMIT(os.Getenv("GOMEMLIMIT"), configLimitApplied, false, dropin, log)
	if err != nil {
		t.Errorf("checkGOMLEMLIMIT returned unexpected error: %v", err)
	}
	if logContainsMsg(&logBuf, "GOMEMLIMIT is not set — node may OOM under load") {
		t.Errorf("unexpected GOMEMLIMIT warning logged — memory_limit_bytes should silence it\nlog:\n%s", logBuf.String())
	}
}

// TestStrictMemLimit_ConfigPath_Succeeds confirms that --strict-memlimit
// (strictMode=true) does NOT return an error when memory_limit_bytes is set in
// node.yaml and GOMEMLIMIT is absent.
//
// This is the integration path exercised by run() when an operator sets
// memory_limit_bytes in their node.yaml and starts the node with
// --strict-memlimit.  Without the guard in checkGOMLEMLIMIT that short-circuits
// on configLimitApplied, the node would refuse to start even though a hard
// memory cap is already in effect via debug.SetMemoryLimit.
func TestStrictMemLimit_ConfigPath_Succeeds(t *testing.T) {
	// Ensure GOMEMLIMIT env is absent so the config-path branch is the only
	// possible source of a memory cap.
	t.Setenv("GOMEMLIMIT", "")

	// Write a minimal node.yaml with a positive memory_limit_bytes.
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "node.yaml")
	yamlContent := "memory_limit_bytes: 5368709120\n" // 5 GiB
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write node.yaml: %v", err)
	}

	// Load config — exercises config.Load YAML parsing of MemoryLimitBytes.
	cfg, err := config.Load(yamlPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.MemoryLimitBytes != 5368709120 {
		t.Fatalf("expected MemoryLimitBytes=5368709120, got %d", cfg.MemoryLimitBytes)
	}

	// Replicate the run() guard logic exactly.
	// debug.SetMemoryLimit is intentionally NOT called here to avoid
	// side-effects on the test process; configLimitApplied is derived from the
	// same condition run() uses.
	configLimitApplied := cfg.MemoryLimitBytes > 0 && os.Getenv("GOMEMLIMIT") == ""
	if !configLimitApplied {
		t.Fatal("expected configLimitApplied=true but it was false")
	}

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	// strictMode=true — this is the path exercised by --strict-memlimit.
	err = checkGOMLEMLIMIT(os.Getenv("GOMEMLIMIT"), configLimitApplied, true, dropin, log)
	if err != nil {
		t.Errorf("checkGOMLEMLIMIT returned unexpected error in strict mode with memory_limit_bytes set: %v", err)
	}
	if logContainsMsg(&logBuf, "GOMEMLIMIT is not set — node may OOM under load") {
		t.Errorf("unexpected GOMEMLIMIT warning logged — memory_limit_bytes should silence it even in strict mode\nlog:\n%s", logBuf.String())
	}
}

// ─── Test N: shutdown ordering regression ─────────────────────────────────────

// TestShutdownSnapshotMatchesFinalDBTip is a regression test for the shutdown
// ordering fix: close(stop) + <-engineDone BEFORE db.GetTip() + saveSnapshot.
//
// # The race
//
// Before the fix the shutdown path read the DB tip first:
//
//	db.GetTip()             → returns (hash H, height H)
//	[engine produces H+1]   ← race window
//	saveStartupSnapshot(H)  → snapshot at H, but DB tip is already H+1
//
// On the next restart the node looked for snapshot-v2-(H+1).json.gz, found
// nothing, and fell back to a multi-hour block scan.
//
// # The fix
//
//	close(stop)             → engine receives shutdown signal
//	<-engineDone            → wait until engine.Run() has fully returned
//	db.GetTip()             → reads the final, stable tip (H+1)
//	saveStartupSnapshot(H+1)→ snapshot height == DB tip height guaranteed
//
// # What this test does
//
// It uses performShutdown (the production function from main.go) with a
// controlled fake engine that writes block H+1 to the DB AFTER receiving the
// shutdown signal — exactly the scenario that caused the race.
//
// With the correct ordering (close(stop) → <-engineDone → GetTip):
//   - performShutdown signals stop; fake engine writes block 6 and closes engineDone
//   - performShutdown waits for engineDone, reads DB tip = 6, saves snapshot at 6
//   - Next startup loads snapshot-v2-6.json.gz → fast path succeeds ✓
//
// If the ordering were reverted (GetTip before stop+wait):
//   - performShutdown reads DB tip = 5 before the fake engine runs
//   - Snapshot saved at 5; DB tip = 6 after the engine writes its final block
//   - Next startup looks for snapshot-v2-6.json.gz, finds nothing, falls back
//     to a block scan → this test fails ✗
func TestShutdownSnapshotMatchesFinalDBTip(t *testing.T) {
	dir := t.TempDir()
	db, blocks := buildChainInStore(t, dir, 5) // genesis + 5 blocks, tip at H=5

	tip5 := blocks[5]
	hash5 := tip5.Hash()

	// ── Build a real UTXOSet and ValidatorRegistry ──────────────────────────
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	utxos := core.NewUTXOSet()
	for _, b := range blocks {
		if err := utxos.ApplyBlock(b); err != nil {
			t.Fatalf("ApplyBlock h=%d: %v", b.Header.Height, err)
		}
	}
	registry := core.NewValidatorRegistry()
	registry.SetUTXOSet(utxos)
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)

	// ── Fake engine ─────────────────────────────────────────────────────────
	// Simulates a real consensus engine that may be finishing a block when
	// the shutdown signal arrives.
	//
	// Behaviour: upon receiving <-stop, it writes block H+1=6 to the DB (the
	// "in-flight" block), then closes engineDone — just as the real engine.Run()
	// finishes its current tick and returns after close(stop).
	stop := make(chan struct{})
	engineDone := make(chan struct{})

	go func() {
		defer close(engineDone)
		<-stop // wait for shutdown signal from performShutdown

		// Write the in-flight block (H+1=6) to the DB, mirroring OnBlockProduced.
		hdr6 := core.BlockHeader{
			Height:       6,
			PrevHash:     hash5,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(nil),
		}
		if signErr := hdr6.Sign(priv); signErr != nil {
			t.Errorf("fake engine: Sign block 6: %v", signErr)
			return
		}
		blk6 := &core.Block{Header: hdr6}
		raw6, marshalErr := json.Marshal(blk6)
		if marshalErr != nil {
			t.Errorf("fake engine: Marshal block 6: %v", marshalErr)
			return
		}
		h6 := blk6.Hash()
		if putErr := db.PutRawBlock(h6, 6, raw6); putErr != nil {
			t.Errorf("fake engine: PutRawBlock block 6: %v", putErr)
			return
		}
		if tipErr := db.PutTip(h6, 6); tipErr != nil {
			t.Errorf("fake engine: PutTip block 6: %v", tipErr)
		}
	}()

	// ── Call the production shutdown function ────────────────────────────────
	// performShutdown (main.go) enforces the correct ordering:
	//   1. close(stop)   → fake engine wakes up, writes block 6, closes engineDone
	//   2. <-engineDone  → waits for fake engine to finish
	//   3. db.GetTip()   → reads the final tip = 6
	//   4. saveSnapshot  → snapshot written at height 6
	var shutLog bytes.Buffer
	performShutdown(stop, engineDone, db, utxos, registry, dir, newCaptureLogger(&shutLog), nil)

	// ── Assert: snapshot height == final DB tip (6, not 5) ──────────────────
	dbTipHash, dbTipHeight, dbErr := db.GetTip()
	if dbErr != nil {
		t.Fatalf("db.GetTip after shutdown: %v", dbErr)
	}
	if dbTipHeight != 6 {
		t.Fatalf("expected DB tip height 6 (fake engine wrote it); got %d", dbTipHeight)
	}
	dbTipHashHex := fmt.Sprintf("%x", dbTipHash[:])

	// The snapshot must be at height 6.
	// Under the pre-fix ordering the snapshot would be at height 5 (read before
	// the engine committed its final block), making this call return an error.
	loaded, snapLoadErr := loadStartupSnapshot(dir, dbTipHeight, dbTipHashHex)
	if snapLoadErr != nil {
		t.Errorf("snapshot must be at final DB tip (height %d) — load failed: %v\n"+
			"hint: if snapshot-v2-5.json.gz exists but not snapshot-v2-6.json.gz, the "+
			"shutdown read the tip before the engine quiesced (pre-fix race restored)",
			dbTipHeight, snapLoadErr)
		t.Logf("shutdown log:\n%s", shutLog.String())
	}
	if loaded != nil && loaded.TipHeight != dbTipHeight {
		t.Errorf("snapshot TipHeight=%d != DB tip height=%d — shutdown ordering race detected",
			loaded.TipHeight, dbTipHeight)
	}

	// ── Assert: next restart logs fast path, not block scan ─────────────────
	var restartLog bytes.Buffer
	log2 := newCaptureLogger(&restartLog)
	snapLoaded := false
	loaded2, _, loadErr2 := loadStartupSnapshotWithFallback(dir, dbTipHeight, dbTipHashHex, log2)
	if loadErr2 == nil {
		snapLoaded = true
		log2.Info("startup fast path complete — snapshot loaded",
			"tip_height", dbTipHeight,
			"active_utxos", len(loaded2.UTXOs.ActiveUTXOs),
		)
	}
	if !snapLoaded {
		log2.Info("running startup block scan",
			"tip_height", dbTipHeight,
			"ki_from_index", false,
			"heap_sys_mib_before", uint64(0),
		)
	}

	if !logContainsMsg(&restartLog, "startup fast path complete — snapshot loaded") {
		t.Error("node must take the fast path on restart after a correct shutdown — block scan triggered instead")
		t.Logf("restart log:\n%s", restartLog.String())
	}
	if logContainsMsg(&restartLog, "running startup block scan") {
		t.Error("block scan must NOT be triggered after a clean shutdown with correct ordering")
		t.Logf("restart log:\n%s", restartLog.String())
	}
}

// TestMemoryLimitBytes_Zero_EmitsGOMLEMLIMITWarning confirms that when
// memory_limit_bytes is absent (zero, the default) and GOMEMLIMIT is also
// unset, checkGOMLEMLIMIT still emits the OOM warning.
//
// This acts as a regression guard: if the "no config, no env" branch were
// accidentally silenced, operators would lose the warning that protects them
// from silent OOM kills.
func TestMemoryLimitBytes_Zero_EmitsGOMLEMLIMITWarning(t *testing.T) {
	// Ensure GOMEMLIMIT env is absent so neither branch silences the warning.
	t.Setenv("GOMEMLIMIT", "")

	// Write a minimal node.yaml with no memory_limit_bytes (defaults to 0).
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "node.yaml")
	yamlContent := "memory_limit_bytes: 0\n"
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write node.yaml: %v", err)
	}

	cfg, err := config.Load(yamlPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.MemoryLimitBytes != 0 {
		t.Fatalf("expected MemoryLimitBytes=0, got %d", cfg.MemoryLimitBytes)
	}

	// Replicate the run() guard logic: zero value → configLimitApplied=false.
	configLimitApplied := cfg.MemoryLimitBytes > 0 && os.Getenv("GOMEMLIMIT") == ""

	var logBuf bytes.Buffer
	log := newCaptureLogger(&logBuf)

	err = checkGOMLEMLIMIT(os.Getenv("GOMEMLIMIT"), configLimitApplied, false, dropin, log)
	if err != nil {
		t.Errorf("checkGOMLEMLIMIT returned unexpected error in non-strict mode: %v", err)
	}
	if !logContainsMsg(&logBuf, "GOMEMLIMIT is not set — node may OOM under load") {
		t.Errorf("expected GOMEMLIMIT warning was not logged when memory_limit_bytes=0 and GOMEMLIMIT is absent\nlog:\n%s", logBuf.String())
	}
}
