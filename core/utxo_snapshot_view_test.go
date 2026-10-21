package core

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestUTXOSnapshotViewStaysPointInTimeAcrossRecordRepair(t *testing.T) {
	set := NewUTXOSet()
	var txHash crypto.Hash32
	var pub crypto.Point32
	var oldCommit, repairedCommit crypto.Commitment
	txHash[0], pub[0] = 1, 2
	oldCommit[0], repairedCommit[0] = 3, 4
	input := &UTXO{TxHash: txHash, OneTimePub: pub, AmountCommit: oldCommit}
	set.Add(input)

	view := set.TakeSnapshotView()
	if len(view.ActiveUTXOs) != 1 {
		t.Fatalf("view has %d active outputs, want 1", len(view.ActiveUTXOs))
	}

	// Add owns its input and readers receive copies, so mutations outside the
	// UTXOSet cannot alter a view or the live state.
	input.AmountCommit = repairedCommit
	read := set.Get(txHash, 0)
	read.AmountCommit = repairedCommit

	set.PatchAmountCommit(pub, repairedCommit)
	if got := view.ActiveUTXOs[0].AmountCommit; got != oldCommit {
		t.Fatalf("captured view changed after repair: got %x, want %x", got, oldCommit)
	}
	if got := set.Get(txHash, 0).AmountCommit; got != repairedCommit {
		t.Fatalf("live UTXO commitment = %x, want repaired %x", got, repairedCommit)
	}
}

func TestUTXOSnapshotViewStaysPointInTimeAcrossReconcile(t *testing.T) {
	set := NewUTXOSet()
	var txHash crypto.Hash32
	var pub crypto.Point32
	var oldCommit, diskCommit crypto.Commitment
	txHash[0], pub[0] = 9, 10
	oldCommit[0], diskCommit[0] = 11, 12
	set.Add(&UTXO{TxHash: txHash, OneTimePub: pub, AmountCommit: oldCommit})
	view := set.TakeSnapshotView()

	checked, fixed, mismatched := set.ReconcileWithStore(
		func(gotHash crypto.Hash32, outIdx uint32) (*UTXO, bool) {
			return &UTXO{
				TxHash:       gotHash,
				OutputIndex:  outIdx,
				OneTimePub:   pub,
				AmountCommit: diskCommit,
			}, true
		},
		nil,
	)
	if checked != 1 || fixed != 1 || mismatched != 0 {
		t.Fatalf("reconcile = (%d, %d, %d), want (1, 1, 0)", checked, fixed, mismatched)
	}
	if got := view.ActiveUTXOs[0].AmountCommit; got != oldCommit {
		t.Fatalf("captured view changed during reconcile: got %x, want %x", got, oldCommit)
	}
	if got := set.Get(txHash, 0).AmountCommit; got != diskCommit {
		t.Fatalf("live UTXO commitment = %x, want disk %x", got, diskCommit)
	}
}

func TestUTXOSnapshotViewSerializesWhileRepairsReplaceRecords(t *testing.T) {
	set := NewUTXOSet()
	const count = 2048
	pubs := make([]crypto.Point32, count)
	for i := range pubs {
		var txHash crypto.Hash32
		pubs[i][0] = byte(i)
		pubs[i][1] = byte(i >> 8)
		txHash[0] = byte(i)
		txHash[1] = byte(i >> 8)
		set.Add(&UTXO{TxHash: txHash, OneTimePub: pubs[i]})
	}
	view := set.TakeSnapshotView()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			if _, err := json.Marshal(view); err != nil {
				t.Errorf("marshal immutable snapshot view: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		var corrected crypto.Commitment
		corrected[0] = 1
		for _, pub := range pubs {
			set.PatchAmountCommit(pub, corrected)
		}
	}()
	wg.Wait()
}
