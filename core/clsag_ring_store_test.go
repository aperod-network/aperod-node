package core

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

type testRingStore struct {
	members map[crypto.Point32]*UTXO
	sampled []DecoyUTXO
}

func (*testRingStore) IsKeyImageSpent(crypto.KeyImage) (bool, error) { return false, nil }
func (*testRingStore) MarkKeyImageSpent(crypto.KeyImage) error      { return nil }
func (*testRingStore) DeleteKeyImage(crypto.KeyImage) error         { return nil }
func (s *testRingStore) LookupRingMember(pub crypto.Point32) (*UTXO, error) {
	return s.members[pub], nil
}
func (s *testRingStore) SampleRingMembers(count int, _ map[crypto.Point32]bool) ([]DecoyUTXO, error) {
	if count > len(s.sampled) {
		count = len(s.sampled)
	}
	return s.sampled[:count], nil
}

func TestCLSAGOutputCacheIsBoundedAndFallsBackToPersistentIndex(t *testing.T) {
	oldLimit := maxCLSAGRecentOutputs
	maxCLSAGRecentOutputs = 2
	t.Cleanup(func() { maxCLSAGRecentOutputs = oldLimit })

	backend := &testRingStore{members: make(map[crypto.Point32]*UTXO)}
	set := NewUTXOSetWithDB(backend)
	set.SetCLSAGActivationHeight(10)

	var first *UTXO
	for i := byte(1); i <= 4; i++ {
		var hash crypto.Hash32
		var pub crypto.Point32
		hash[0], pub[0] = i, i
		u := &UTXO{TxHash: hash, OneTimePub: pub, BlockHeight: 10}
		backend.members[pub] = u
		set.Add(u)
		if i == 1 {
			first = u
		}
	}

	if got := set.Get(first.TxHash, 0); got != nil {
		t.Fatal("old v5 output remained in bounded in-memory cache")
	}
	if got := set.GetRingMember(first.OneTimePub); got != first {
		t.Fatal("persistent ring-member fallback did not resolve evicted output")
	}
}