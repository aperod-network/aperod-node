package core

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

func guardianAnchorForTest() crypto.Hash32 {
	return crypto.HashBytes([]byte("guardian-test-anchor"))
}

func TestGuardianFundBuilderAndCanonicality(t *testing.T) {
	anchor := guardianAnchorForTest()
	a, err := BuildGuardianFundTx(anchor, 42)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildGuardianFundTx(anchor, 42)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash() != b.Hash() || !GuardianFundTxAt(&a, anchor, 42) {
		t.Fatal("builder must be deterministic and canonical")
	}
	if a.Outputs[0].AmountCommit == (crypto.Commitment{}) || a.Outputs[0].OneTimePub == (crypto.Point32{}) {
		t.Fatal("guardian output must carry a public commitment and key")
	}
	for _, mutate := range []func(*Transaction){
		func(tx *Transaction) { tx.Outputs[0].AmountCommit[0] ^= 1 },
		func(tx *Transaction) { tx.Outputs[0].OneTimePub[0] ^= 1 },
		func(tx *Transaction) { tx.Extra[len(tx.Extra)-1] ^= 1 },
		func(tx *Transaction) { tx.Outputs = append(tx.Outputs, Output{}) },
		func(tx *Transaction) { tx.RangeProofs = []*crypto.RangeProof{nil} },
		func(tx *Transaction) { tx.Signatures = []*crypto.MLSAGSignature{nil} },
		func(tx *Transaction) { tx.CLSAGSignatures = []*crypto.CLSAGSignature{nil} },
		func(tx *Transaction) { tx.AVM = &AVMPayload{} },
	} {
		bad := a
		bad.Outputs = append([]Output(nil), a.Outputs...)
		bad.Extra = append([]byte(nil), a.Extra...)
		mutate(&bad)
		if GuardianFundTxAt(&bad, anchor, 42) {
			t.Fatal("mutated guardian transaction accepted")
		}
	}
}

func TestGuardianProtocolLockSurvivesSnapshotRestore(t *testing.T) {
	tx, err := BuildGuardianFundTx(guardianAnchorForTest(), 9)
	if err != nil {
		t.Fatal(err)
	}
	set := NewUTXOSet()
	if err := set.ApplyBlock(&Block{Header: BlockHeader{Height: 9}, Txs: []Transaction{tx}}); err != nil {
		t.Fatal(err)
	}
	snap := set.TakeSnapshot()
	restored := NewUTXOSet()
	restored.RestoreFromSnapshot(snap)
	hash := tx.Hash()
	u := restored.Get(hash, 0)
	if u == nil || !u.ProtocolLocked {
		t.Fatalf("protocol lock lost across snapshot: %#v", u)
	}
	if restored.GetByPubKey(tx.Outputs[0].OneTimePub) != nil ||
		restored.GetRingMember(tx.Outputs[0].OneTimePub) != nil {
		t.Fatal("snapshot restore exposed protocol-locked output")
	}
}

func TestGuardianProtocolLockSurvivesKnownSpendReplay(t *testing.T) {
	tx, err := BuildGuardianFundTx(guardianAnchorForTest(), 11)
	if err != nil {
		t.Fatal(err)
	}
	set := NewUTXOSet()
	set.ReplayBlockKnownSpends(&Block{
		Header: BlockHeader{Height: 11},
		Txs:    []Transaction{tx},
	})
	u := set.Get(tx.Hash(), 0)
	if u == nil || !u.ProtocolLocked {
		t.Fatalf("replay lost protocol lock: %#v", u)
	}
	if set.GetByPubKey(tx.Outputs[0].OneTimePub) != nil ||
		len(set.SampleCLSAGDecoys(1, nil)) != 0 {
		t.Fatal("replay exposed protocol-locked output")
	}
}

func TestGuardianFundNeverMempoolOrSpendable(t *testing.T) {
	tx, err := BuildGuardianFundTx(guardianAnchorForTest(), 7)
	if err != nil {
		t.Fatal(err)
	}
	mp := NewMempool(DefaultMempoolConfig())
	if err := mp.Add(tx); err == nil {
		t.Fatal("ordinary mempool admission accepted guardian tx")
	}
	if err := mp.AddPrivileged(tx); err == nil {
		t.Fatal("privileged mempool admission accepted guardian tx")
	}
	set := NewUTXOSet()
	block := &Block{Header: BlockHeader{Height: 7}, Txs: []Transaction{tx}}
	if err := set.ApplyBlock(block); err != nil {
		t.Fatal(err)
	}
	h := tx.Hash()
	if set.Get(h, 0) == nil {
		t.Fatal("guardian record was not materialized")
	}
	pub := tx.Outputs[0].OneTimePub
	if set.GetByPubKey(pub) != nil || set.GetRingMember(pub) != nil {
		t.Fatal("guardian output was exposed to spending/ring membership")
	}
	if got := set.SampleCLSAGDecoys(1, nil); len(got) != 0 {
		t.Fatal("guardian output selected as decoy")
	}
	if err := set.RollbackBlock(block); err != nil {
		t.Fatal(err)
	}
	if set.Get(h, 0) != nil {
		t.Fatal("guardian output survives rollback")
	}
}
