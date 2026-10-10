package main

// startup_integrity_test.go — integration coverage for the startup integrity
// self-heal, exercising the REAL production code path (checkStartupIntegrity,
// called verbatim from run()).
//
// The behaviour under test: when the canonical height index (h/<tipHeight>) is
// missing or zeroed but the tip pointer is intact, the node repairs the index
// from the tip pointer and logs the self-heal at INFO — NOT WARN or ERROR.
// Operators filter alerts by log level, so an accidental revert of INFO → WARN
// would spuriously page on every relay-node restart after an rsync bootstrap.
// This test locks that log level in.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// captureHandler is a minimal slog.Handler that records every emitted record so
// tests can assert on message text and level.  It is safe for concurrent use.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// find returns the first captured record whose message contains substr.
func (h *captureHandler) find(substr string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if strings.Contains(r.Message, substr) {
			return r, true
		}
	}
	return slog.Record{}, false
}

// TestCheckStartupIntegrity_MissingIndex_LogsINFO drives the production
// checkStartupIntegrity helper against a store whose height index for the tip
// height is zeroed (h/<tip> → all-zero hash, so GetBlockByHeight returns nil)
// while the tip pointer still records the real tip hash — the exact state left
// by an rsync bootstrap or an OOM-kill that damaged the SST.
//
// It asserts:
//   - checkStartupIntegrity performs the self-heal without error and signals
//     the caller to continue (done==false), i.e. the node keeps starting.
//   - the repair message is emitted at level INFO (not WARN / ERROR).
//   - the height index was actually repaired (GetBlockByHeight now resolves).
func TestCheckStartupIntegrity_MissingIndex_LogsINFO(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	const tipHeight = uint64(973102)
	var tipHash crypto.Hash32
	tipHash[0] = 0xAB
	tipHash[31] = 0xCD

	// Store the block body + a valid tip pointer, then zero the height index so
	// GetBlockByHeight(tip) returns nil — the trigger for the repair branch.
	sb := &store.StoredBlock{Height: tipHeight, Hash: tipHash, Timestamp: 1_700_000_000}
	if err := db.PutBlock(tipHash, sb); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	if err := db.PutTip(tipHash, tipHeight); err != nil {
		t.Fatalf("PutTip: %v", err)
	}
	// Zero the height-index entry (simulates a missing/zeroed h/ pointer).
	if err := db.RepairHeightIndex(tipHeight, crypto.Hash32{}); err != nil {
		t.Fatalf("zero height index: %v", err)
	}
	if got, err := db.GetBlockByHeight(tipHeight); err != nil {
		t.Fatalf("precondition GetBlockByHeight err: %v", err)
	} else if got != nil {
		t.Fatalf("precondition: GetBlockByHeight = %v, want nil (index zeroed)", got)
	}

	capH := &captureHandler{}
	log := slog.New(capH)

	// Exercise the REAL production path. nonValidator=true so a repair failure
	// (not expected here) would warn-and-continue rather than hard-fail; a
	// successful repair logs INFO regardless of node role.
	done, err := checkStartupIntegrity(
		db, tipHeight, tipHash,
		true,  // nonValidator
		false, // resetTip
		false, // repairDB
		log,
	)
	if err != nil {
		t.Fatalf("checkStartupIntegrity returned error: %v", err)
	}
	if done {
		t.Fatalf("checkStartupIntegrity done=true, want false (node must continue starting)")
	}

	// The self-heal message must be present and at INFO level.
	rec, ok := capH.find("height index was missing/zeroed")
	if !ok {
		t.Fatalf("expected a 'height index was missing/zeroed' repair log; got none")
	}
	if rec.Level != slog.LevelInfo {
		t.Fatalf("repair log level = %v, want INFO (operators filter alerts by level; WARN/ERROR would spuriously page)", rec.Level)
	}

	// No WARN/ERROR should have been emitted for a successful self-heal.
	for _, badMsg := range []string{"repair failed"} {
		if r, found := capH.find(badMsg); found {
			t.Fatalf("unexpected failure log %q at level %v on a successful repair", r.Message, r.Level)
		}
	}

	// The repair actually fixed the index: GetBlockByHeight now resolves the
	// block body, so a subsequent restart skips the repair branch entirely.
	got, err := db.GetBlockByHeight(tipHeight)
	if err != nil {
		t.Fatalf("post-repair GetBlockByHeight err: %v", err)
	}
	if got == nil || got.Hash != tipHash {
		t.Fatalf("post-repair GetBlockByHeight = %v, want block with hash %x", got, tipHash[:8])
	}
}

// TestCheckStartupIntegrity_Consistent_NoRepairLog confirms that when the
// height index already agrees with the tip pointer, the node reports the
// integrity check passed (INFO) and never emits a repair/self-heal message.
func TestCheckStartupIntegrity_Consistent_NoRepairLog(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	const tipHeight = uint64(5)
	var tipHash crypto.Hash32
	tipHash[0] = 0x11

	sb := &store.StoredBlock{Height: tipHeight, Hash: tipHash, Timestamp: 42}
	if err := db.PutBlock(tipHash, sb); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	if err := db.PutTip(tipHash, tipHeight); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	capH := &captureHandler{}
	log := slog.New(capH)

	done, err := checkStartupIntegrity(db, tipHeight, tipHash, true, false, false, log)
	if err != nil {
		t.Fatalf("checkStartupIntegrity: %v", err)
	}
	if done {
		t.Fatalf("done=true, want false")
	}

	if _, found := capH.find("missing/zeroed"); found {
		t.Fatalf("unexpected repair log on a consistent index")
	}
	rec, ok := capH.find("startup integrity check passed")
	if !ok {
		t.Fatalf("expected 'startup integrity check passed' log")
	}
	if rec.Level != slog.LevelInfo {
		t.Fatalf("pass log level = %v, want INFO", rec.Level)
	}
}
