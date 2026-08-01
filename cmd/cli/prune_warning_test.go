package main

// CLI integration tests for the prune-window guard in the `validator stake`
// command (step 3 of the stake flow).
//
// The command now has two distinct behaviours depending on blocks_until_pruned:
//
//  1. blocks_until_pruned < core.PartialUnbondingBlocks
//     → hard rejection: RunE returns a non-nil error containing the block count.
//
//  2. blocks_until_pruned >= core.PartialUnbondingBlocks (field present)
//     → ⚠️ WARNING is printed to stdout but the stake proceeds.
//
//  3. blocks_until_pruned absent
//     → no warning, no error related to pruning.
//
// Tests spin up an httptest.Server serving a minimal UTXO response (with a
// valid Pedersen commitment so the pre-flight check passes), capture os.Stdout
// via os.Pipe, call the cobra command directly (no subprocess), and assert
// on stdout content and/or the returned error.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// captureOutput redirects os.Stdout to a pipe for the duration of fn,
// then returns everything written to stdout as a string.
func captureOutput(fn func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck
	return buf.String()
}

// buildStakeServer starts an httptest.Server that handles:
//
//   - GET /api/v1/utxo/{hash}/0  → returns utxoBody JSON
//   - POST /api/v1/stake          → returns {"status":"ok"} with 201
func buildPruneTestServer(t *testing.T, utxoBody []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/utxo/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(utxoBody) //nolint:errcheck
	})
	mux.HandleFunc("/api/v1/stake", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"status":"ok","tx_hash":"aabb"}`)) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// buildUTXOResponse constructs the JSON body that the mock UTXO server returns.
// If blocksUntilPruned > 0, the field is included; otherwise it is omitted.
func buildUTXOResponse(t *testing.T, commitHex string, blocksUntilPruned uint64) []byte {
	t.Helper()
	m := map[string]interface{}{
		"tx_hash":           strings.Repeat("ab", 32),
		"out_idx":           0,
		"amount_commit_hex": commitHex,
		"exists":            true,
	}
	if blocksUntilPruned > 0 {
		m["blocks_until_pruned"] = blocksUntilPruned
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal UTXO response: %v", err)
	}
	return raw
}

// stakeFixture holds a consistent keypair + amount + deterministic commitment
// for tests that need the CLI's Pedersen pre-flight check to pass.
type stakeFixture struct {
	privKeyHex string
	amountAPR  float64
	amountNAPR uint64
	commitHex  string
	txHashHex  string
}

func newStakeFixture(t *testing.T) stakeFixture {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}

	const amountAPR float64 = 100000
	const amountNAPR uint64 = 10_000_000_000_000 // 100 000 APRO in nAPRO

	var spendPub crypto.Point32
	copy(spendPub[:], []byte(pub))

	blind, err := crypto.DeterministicMintBlind(spendPub, amountNAPR)
	if err != nil {
		t.Fatalf("DeterministicMintBlind: %v", err)
	}
	commit, err := crypto.Commit(amountNAPR, blind)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	privBytes := priv.Bytes()

	var txHash crypto.Hash32
	for i := range txHash {
		txHash[i] = 0xAB
	}

	return stakeFixture{
		privKeyHex: hex.EncodeToString(privBytes),
		amountAPR:  amountAPR,
		amountNAPR: amountNAPR,
		commitHex:  fmt.Sprintf("%x", commit[:]),
		txHashHex:  fmt.Sprintf("%x", txHash[:]),
	}
}

// resetStakeCmd resets the cobra command flags so each test starts clean.
// Cobra stores flag state on the command; we need to re-register for each run.
func resetStakeCmd() {
	f := validatorStakeCmd.Flags()
	// Look up each flag and reset it to its default.
	if n := f.Lookup("node"); n != nil {
		n.Value.Set(n.DefValue) //nolint:errcheck
	}
	if n := f.Lookup("priv-key"); n != nil {
		n.Value.Set(n.DefValue) //nolint:errcheck
	}
	if n := f.Lookup("utxo-txhash"); n != nil {
		n.Value.Set(n.DefValue) //nolint:errcheck
	}
	if n := f.Lookup("utxo-idx"); n != nil {
		n.Value.Set(n.DefValue) //nolint:errcheck
	}
	if n := f.Lookup("amount"); n != nil {
		n.Value.Set(n.DefValue) //nolint:errcheck
	}
}

// runStakeCmd sets the given flag values on validatorStakeCmd and calls RunE.
// Returns the captured stdout and any error from RunE.
func runStakeCmd(t *testing.T, nodeURL, privKey, txHashHex string, amount float64) (string, error) {
	t.Helper()

	f := validatorStakeCmd.Flags()
	f.Set("node", nodeURL)                              //nolint:errcheck
	f.Set("priv-key", privKey)                          //nolint:errcheck
	f.Set("utxo-txhash", txHashHex)                     //nolint:errcheck
	f.Set("utxo-idx", "0")                              //nolint:errcheck
	f.Set("amount", fmt.Sprintf("%g", amount)) //nolint:errcheck

	var runErr error
	out := captureOutput(func() {
		runErr = validatorStakeCmd.RunE(validatorStakeCmd, nil)
	})
	return out, runErr
}

// ── Rejection tests (blocks_until_pruned < PartialUnbondingBlocks) ────────────

// TestCLIPruneGuard_RejectsWhenBelowUnbondingPeriod verifies that the command
// returns an error (and does NOT broadcast) when blocks_until_pruned is well
// below PartialUnbondingBlocks.
func TestCLIPruneGuard_RejectsWhenBelowUnbondingPeriod(t *testing.T) {
	fix := newStakeFixture(t)

	// 50 blocks is far below PartialUnbondingBlocks (43 200).
	utxoBody := buildUTXOResponse(t, fix.commitHex, 50)
	srv := buildPruneTestServer(t, utxoBody)

	resetStakeCmd()
	_, err := runStakeCmd(t, srv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)

	if err == nil {
		t.Fatal("expected an error when blocks_until_pruned < PartialUnbondingBlocks, got nil")
	}
	if !strings.Contains(err.Error(), "50") {
		t.Errorf("error should mention the block count (50); got: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", core.PartialUnbondingBlocks)) {
		t.Errorf("error should mention PartialUnbondingBlocks (%d); got: %v",
			core.PartialUnbondingBlocks, err)
	}
}

// TestCLIPruneGuard_RejectsAtBoundaryMinusOne verifies rejection exactly one
// block before the unbonding period boundary.
func TestCLIPruneGuard_RejectsAtBoundaryMinusOne(t *testing.T) {
	fix := newStakeFixture(t)

	boundary := core.PartialUnbondingBlocks - 1
	utxoBody := buildUTXOResponse(t, fix.commitHex, boundary)
	srv := buildPruneTestServer(t, utxoBody)

	resetStakeCmd()
	_, err := runStakeCmd(t, srv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)

	if err == nil {
		t.Fatalf("expected error for blocks_until_pruned=%d (boundary-1), got nil", boundary)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", boundary)) {
		t.Errorf("error should mention block count %d; got: %v", boundary, err)
	}
}

// ── Warning tests (blocks_until_pruned >= PartialUnbondingBlocks) ─────────────

// TestCLIPruneGuard_WarningAtBoundary verifies that a UTXO with
// blocks_until_pruned == PartialUnbondingBlocks prints a warning but succeeds.
func TestCLIPruneGuard_WarningAtBoundary(t *testing.T) {
	fix := newStakeFixture(t)

	boundary := core.PartialUnbondingBlocks
	utxoBody := buildUTXOResponse(t, fix.commitHex, boundary)
	srv := buildPruneTestServer(t, utxoBody)

	resetStakeCmd()
	out, err := runStakeCmd(t, srv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)

	if err != nil {
		t.Fatalf("expected success for blocks_until_pruned=%d (boundary), got error: %v",
			boundary, err)
	}
	if !strings.Contains(out, "⚠️") || !strings.Contains(out, "WARNING") {
		t.Errorf("expected ⚠️ WARNING in stdout for blocks_until_pruned=%d\ngot:\n%s",
			boundary, out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d", boundary)) {
		t.Errorf("expected block count %d in warning; got:\n%s", boundary, out)
	}
}

// TestCLIPruneGuard_WarningAboveBoundary verifies that a UTXO well above the
// unbonding period prints a warning and still succeeds.
func TestCLIPruneGuard_WarningAboveBoundary(t *testing.T) {
	fix := newStakeFixture(t)

	// Well above threshold — close enough to print the warning, safe enough to proceed.
	safeCount := core.PartialUnbondingBlocks + 500
	utxoBody := buildUTXOResponse(t, fix.commitHex, safeCount)
	srv := buildPruneTestServer(t, utxoBody)

	resetStakeCmd()
	out, err := runStakeCmd(t, srv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)

	if err != nil {
		t.Fatalf("expected success for blocks_until_pruned=%d, got error: %v", safeCount, err)
	}
	if !strings.Contains(out, "⚠️") || !strings.Contains(out, "WARNING") {
		t.Errorf("expected ⚠️ WARNING in stdout for blocks_until_pruned=%d\ngot:\n%s",
			safeCount, out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d", safeCount)) {
		t.Errorf("expected block count %d in warning; got:\n%s", safeCount, out)
	}
}

// ── No-warning tests (blocks_until_pruned absent) ─────────────────────────────

// TestCLIPruneGuard_WarningAbsentWhenFieldMissing verifies that no ⚠️ warning
// is printed when the UTXO endpoint does NOT include blocks_until_pruned
// (archive mode or UTXO safely far from pruning).
func TestCLIPruneGuard_WarningAbsentWhenFieldMissing(t *testing.T) {
	fix := newStakeFixture(t)

	// blocksUntilPruned = 0 → field omitted from response.
	utxoBody := buildUTXOResponse(t, fix.commitHex, 0)
	srv := buildPruneTestServer(t, utxoBody)

	resetStakeCmd()
	out, err := runStakeCmd(t, srv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)

	if err != nil {
		t.Fatalf("expected success when blocks_until_pruned is absent, got error: %v", err)
	}
	if strings.Contains(out, "⚠️") || strings.Contains(out, "WARNING") {
		t.Errorf("unexpected ⚠️ WARNING in stdout when blocks_until_pruned is absent\n"+
			"got stdout:\n%s", out)
	}
}

// Ensure the core package is imported (suppress unused-import error if crypto
// helpers are defined in fixture helpers only).
var _ = core.StakeDeposit
