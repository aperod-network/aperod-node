package core

// utxo_rollback_test.go — regression test for the rollback-journal fix.
//
// Verifies that RollbackBlock restores a spent UTXO to both Get() and
// GetByPubKey() even when spentPubKeys was already at capacity when the
// block was applied — so the UTXO was never added to the decoy pool and
// rollback state must come exclusively from the separate rollback journal.

import (
	"errors"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
)

func TestRollbackBlockPropagatesPersistentIndexFailure(t *testing.T) {
	utxos := NewUTXOSet()
	var txHash crypto.Hash32
	txHash[0] = 0xA5
	var pub crypto.Point32
	pub[0] = 0x02
	u := &UTXO{TxHash: txHash, OutputIndex: 3, OneTimePub: pub}
	utxos.rollbackJournal[7] = []rollbackEntry{{ringMember: pub, utxo: u}}

	wantErr := errors.New("durable delete failed")
	utxos.OnUTXORestored = func(crypto.Hash32, uint32) error { return wantErr }
	err := utxos.RollbackBlock(&Block{Header: BlockHeader{Height: 7}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RollbackBlock error = %v, want wrapped persistence error", err)
	}
	// The callback executes outside the UTXO lock; a lookup must not deadlock.
	if got := utxos.Get(txHash, 3); got == nil {
		t.Fatal("in-memory UTXO was not restored")
	}
}

// TestRollbackBlock_FullDecoyPool checks the scenario the reviewer flagged:
//  1. Fill spentPubKeys to cap (temporarily lowered to 3 so we do not need
//     to pre-fill 50 000 entries).
//  2. Apply a block that spends one more UTXO — the decoy pool is full, so
//     the UTXO is NOT added to spentPubKeys, only to the rollback journal.
//  3. Roll the block back.
//  4. Assert both Get() and GetByPubKey() recover the UTXO — proving the
//     rollback journal (not the capped decoy pool) was used.
func TestRollbackBlock_FullDecoyPool(t *testing.T) {
	// ── Temporarily lower the decoy-pool cap ─────────────────────────────────
	// This lets us saturate the pool with 3 dummy entries rather than 50 000.
	origMax := maxSpentDecoys
	maxSpentDecoys = 3
	defer func() { maxSpentDecoys = origMax }()

	// ── Validator + Alice wallet ──────────────────────────────────────────────
	valPriv, valPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	alice, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys (alice): %v", err)
	}

	// ── Block 0: coinbase mints a UTXO to Alice ───────────────────────────────
	cb0 := CoinbaseTx(alice.Spend.Public, 100_000_000)
	txs0 := []Transaction{cb0}
	hdr0 := BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: valPub,
		MerkleRoot:   MerkleRoot(txs0),
	}
	if err := hdr0.Sign(valPriv); err != nil {
		t.Fatalf("Sign block 0: %v", err)
	}
	b0 := &Block{Header: hdr0, Txs: txs0}

	utxos := NewUTXOSet()
	if err := utxos.ApplyBlock(b0); err != nil {
		t.Fatalf("ApplyBlock(0): %v", err)
	}

	// Retrieve Alice's UTXO — we need its AmountCommit to build a matching
	// RingInput, and its OneTimePub to verify restoration after rollback.
	cb0Hash := cb0.Hash()
	aliceUTXO := utxos.Get(cb0Hash, 0)
	if aliceUTXO == nil {
		t.Fatal("Alice's coinbase UTXO not found after block 0")
	}

	// ── Saturate the decoy pool with 3 dummy entries ──────────────────────────
	// Write directly to spentPubKeys — internal test has package-level access.
	for i := 0; i < 3; i++ {
		dummy, dErr := crypto.GenerateWalletKeys()
		if dErr != nil {
			t.Fatalf("GenerateWalletKeys (dummy %d): %v", i, dErr)
		}
		utxos.mu.Lock()
		utxos.spentPubKeys[dummy.Spend.Public] = &UTXO{OneTimePub: dummy.Spend.Public}
		utxos.mu.Unlock()
	}
	if got := utxos.SpentDecoyCount(); got != 3 {
		t.Fatalf("expected 3 decoys in pool before spend, got %d", got)
	}

	// ── Build block 1: a ring tx that spends Alice's UTXO ────────────────────
	// Ring member 0 is Alice's real pub key; others are random valid keys.
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = crypto.RingMember(alice.Spend.Public)
	for i := 1; i < crypto.RingSize; i++ {
		d, dErr := crypto.GenerateWalletKeys()
		if dErr != nil {
			t.Fatalf("GenerateWalletKeys (ring %d): %v", i, dErr)
		}
		ring[i] = crypto.RingMember(d.Spend.Public)
	}
	var msg crypto.Hash32
	msg[0] = 0xAB
	sig, sigErr := crypto.MLSAGSign(msg, ring, 0, alice.Spend.Private)
	if sigErr != nil {
		t.Fatalf("MLSAGSign: %v", sigErr)
	}

	// Recipient bob gets the output of the spend.
	bob, bErr := crypto.GenerateWalletKeys()
	if bErr != nil {
		t.Fatalf("GenerateWalletKeys (bob): %v", bErr)
	}
	spendTx := Transaction{
		Inputs: []RingInput{
			{
				KeyImage:     sig.KeyImage,
				Ring:         ring,
				AmountCommit: aliceUTXO.AmountCommit,
			},
		},
		Outputs: []Output{
			{
				OneTimePub:   bob.Spend.Public,
				AmountCommit: aliceUTXO.AmountCommit,
			},
		},
		Signatures: []*crypto.MLSAGSignature{sig},
	}
	txs1 := []Transaction{spendTx}
	hdr1 := BlockHeader{
		Height:       1,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: valPub,
		MerkleRoot:   MerkleRoot(txs1),
	}
	if err := hdr1.Sign(valPriv); err != nil {
		t.Fatalf("Sign block 1: %v", err)
	}
	b1 := &Block{Header: hdr1, Txs: txs1}

	if err := utxos.ApplyBlock(b1); err != nil {
		t.Fatalf("ApplyBlock(1): %v", err)
	}

	// ── Assert state after spend ──────────────────────────────────────────────
	if utxos.Get(cb0Hash, 0) != nil {
		t.Error("Alice's UTXO should be absent from Get() after spend")
	}
	if utxos.GetByPubKey(aliceUTXO.OneTimePub) != nil {
		t.Error("Alice's UTXO should be absent from GetByPubKey() after spend")
	}
	// Decoy pool must remain at 3 — Alice's UTXO was NOT added (pool was full).
	if got := utxos.SpentDecoyCount(); got != 3 {
		t.Errorf("expected decoy pool to remain at 3 (full), got %d", got)
	}
	// Rollback journal must have one entry at height 1.
	utxos.mu.RLock()
	journalLen := len(utxos.rollbackJournal[1])
	utxos.mu.RUnlock()
	if journalLen == 0 {
		t.Fatal("rollback journal has no entry for height 1 — fix to ApplyBlock is missing")
	}

	// ── Roll back block 1 ─────────────────────────────────────────────────────
	var restored []UTXOKey
	utxos.OnUTXORestored = func(txHash crypto.Hash32, outIdx uint32) error {
		restored = append(restored, UTXOKey{TxHash: txHash, OutputIndex: outIdx})
		return nil
	}
	if err := utxos.RollbackBlock(b1); err != nil {
		t.Fatalf("RollbackBlock(1): %v", err)
	}

	// ── Assert restoration ────────────────────────────────────────────────────
	// Both Get() and GetByPubKey() must recover Alice's UTXO via the journal.
	if got := utxos.Get(cb0Hash, 0); got == nil {
		t.Error("RollbackBlock: Alice's UTXO missing from Get() — journal not used for restoration")
	}
	if got := utxos.GetByPubKey(aliceUTXO.OneTimePub); got == nil {
		t.Error("RollbackBlock: Alice's UTXO missing from GetByPubKey() — journal not used for restoration")
	}
	if len(restored) != 1 || restored[0] != (UTXOKey{TxHash: cb0Hash, OutputIndex: 0}) {
		t.Fatalf("RollbackBlock: persistent-index callback got %v, want restored Alice UTXO", restored)
	}

	// Bob's output (created by block 1) must be absent after rollback.
	spendHash := spendTx.Hash()
	if utxos.Get(spendHash, 0) != nil {
		t.Error("RollbackBlock: block-1 output should have been removed")
	}

	// Journal entry for height 1 must be cleared after rollback.
	utxos.mu.RLock()
	journalAfter := len(utxos.rollbackJournal[1])
	utxos.mu.RUnlock()
	if journalAfter != 0 {
		t.Errorf("rollback journal for height 1 should be empty after rollback, got %d entries", journalAfter)
	}

	t.Log("TestRollbackBlock_FullDecoyPool: journal correctly restores UTXO absent from capped decoy pool ✓")
}

// TestRollbackBlock_AfterSnapshotRestore verifies that a snapshot round-trip
// preserves the rollback journal so that RollbackBlock works correctly when:
//  1. The decoy pool is full after restore (from SpentDecoys in the snapshot).
//  2. A new block is applied and then rolled back.
//
// This guards the specific failure mode the code-review raised: a node that
// restores from a periodic snapshot, finds its decoy pool full, accepts a
// block that would normally put the new spent UTXO in the journal (not the
// pool), then needs to roll back — and must be able to restore the UTXO.
func TestRollbackBlock_AfterSnapshotRestore(t *testing.T) {
	// Temporarily lower the decoy cap so the pool saturates cheaply.
	origMax := maxSpentDecoys
	maxSpentDecoys = 3
	defer func() { maxSpentDecoys = origMax }()

	// ── Validator + Alice ─────────────────────────────────────────────────────
	valPriv, valPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	alice, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys (alice): %v", err)
	}

	// ── Block 0: coinbase to Alice ────────────────────────────────────────────
	cb0 := CoinbaseTx(alice.Spend.Public, 100_000_000)
	txs0 := []Transaction{cb0}
	hdr0 := BlockHeader{
		Height:       0,
		Timestamp:    1_000_000_000,
		ValidatorPub: valPub,
		MerkleRoot:   MerkleRoot(txs0),
	}
	if err := hdr0.Sign(valPriv); err != nil {
		t.Fatalf("Sign block 0: %v", err)
	}
	b0 := &Block{Header: hdr0, Txs: txs0}

	utxos := NewUTXOSet()
	if err := utxos.ApplyBlock(b0); err != nil {
		t.Fatalf("ApplyBlock(0): %v", err)
	}
	cb0Hash := cb0.Hash()
	aliceUTXO := utxos.Get(cb0Hash, 0)
	if aliceUTXO == nil {
		t.Fatal("Alice's coinbase UTXO not found after block 0")
	}

	// ── Saturate decoy pool with 3 dummy entries, then snapshot ───────────────
	for i := 0; i < 3; i++ {
		dummy, dErr := crypto.GenerateWalletKeys()
		if dErr != nil {
			t.Fatalf("GenerateWalletKeys (dummy %d): %v", i, dErr)
		}
		utxos.mu.Lock()
		utxos.spentPubKeys[dummy.Spend.Public] = &UTXO{OneTimePub: dummy.Spend.Public}
		utxos.mu.Unlock()
	}

	// Take a snapshot and restore into a fresh UTXOSet — simulating a node
	// restart that loads the periodic snapshot from disk.
	snap := utxos.TakeSnapshot()
	restored := NewUTXOSet()
	restored.RestoreFromSnapshot(snap)

	// Verify Alice's UTXO survived the snapshot round-trip.
	if restored.Get(cb0Hash, 0) == nil {
		t.Fatal("Alice's UTXO missing from restored UTXOSet")
	}
	if restored.SpentDecoyCount() != 3 {
		t.Fatalf("expected 3 decoys after restore, got %d", restored.SpentDecoyCount())
	}

	// ── Apply block 1 on the RESTORED set ────────────────────────────────────
	// Decoy pool is already full (3/3) — Alice's spent UTXO must go to the
	// journal, not the pool.
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = crypto.RingMember(alice.Spend.Public)
	for i := 1; i < crypto.RingSize; i++ {
		d, dErr := crypto.GenerateWalletKeys()
		if dErr != nil {
			t.Fatalf("GenerateWalletKeys (ring %d): %v", i, dErr)
		}
		ring[i] = crypto.RingMember(d.Spend.Public)
	}
	var msg crypto.Hash32
	msg[0] = 0xCD
	sig, sigErr := crypto.MLSAGSign(msg, ring, 0, alice.Spend.Private)
	if sigErr != nil {
		t.Fatalf("MLSAGSign: %v", sigErr)
	}
	bob, bErr := crypto.GenerateWalletKeys()
	if bErr != nil {
		t.Fatalf("GenerateWalletKeys (bob): %v", bErr)
	}

	// Get aliceUTXO from the restored set (commit may differ due to round-trip).
	restoredAlice := restored.Get(cb0Hash, 0)
	spendTx := Transaction{
		Inputs: []RingInput{
			{
				KeyImage:     sig.KeyImage,
				Ring:         ring,
				AmountCommit: restoredAlice.AmountCommit,
			},
		},
		Outputs: []Output{
			{
				OneTimePub:   bob.Spend.Public,
				AmountCommit: restoredAlice.AmountCommit,
			},
		},
		Signatures: []*crypto.MLSAGSignature{sig},
	}
	txs1 := []Transaction{spendTx}
	hdr1 := BlockHeader{
		Height:       1,
		Timestamp:    1_000_000_001,
		ValidatorPub: valPub,
		MerkleRoot:   MerkleRoot(txs1),
	}
	if err := hdr1.Sign(valPriv); err != nil {
		t.Fatalf("Sign block 1: %v", err)
	}
	b1 := &Block{Header: hdr1, Txs: txs1}

	if err := restored.ApplyBlock(b1); err != nil {
		t.Fatalf("ApplyBlock(1) on restored set: %v", err)
	}

	// Alice's UTXO must be absent after spend.
	if restored.Get(cb0Hash, 0) != nil {
		t.Error("Alice's UTXO should be absent after spend on restored set")
	}
	// Decoy pool stays at 3 — pool was full.
	if restored.SpentDecoyCount() != 3 {
		t.Errorf("expected decoy pool to stay at 3, got %d", restored.SpentDecoyCount())
	}
	// Journal must have an entry for height 1.
	restored.mu.RLock()
	jlen := len(restored.rollbackJournal[1])
	restored.mu.RUnlock()
	if jlen == 0 {
		t.Fatal("no rollback journal entry for height 1 on restored set")
	}

	// ── Roll back block 1 ─────────────────────────────────────────────────────
	if err := restored.RollbackBlock(b1); err != nil {
		t.Fatalf("RollbackBlock(1) on restored set: %v", err)
	}

	// Both Get() and GetByPubKey() must recover Alice's UTXO.
	if restored.Get(cb0Hash, 0) == nil {
		t.Error("Get(): Alice's UTXO not restored after rollback on snapshot-restored set")
	}
	if restored.GetByPubKey(restoredAlice.OneTimePub) == nil {
		t.Error("GetByPubKey(): Alice's UTXO not restored after rollback on snapshot-restored set")
	}

	t.Log("TestRollbackBlock_AfterSnapshotRestore: snapshot round-trip preserves rollback capability ✓")
}
