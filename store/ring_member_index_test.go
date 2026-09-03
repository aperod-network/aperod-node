package store_test

import (
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func ringPoint(seed byte) crypto.Point32 {
	var p crypto.Point32
	for i := range p {
		p[i] = seed + byte(i)
	}
	return p
}

func TestRingMemberIndexPersistsSamplesAndDeletesCanonicalOutputs(t *testing.T) {
	db := openTestDB(t)
	const outputCount = 64

	for i := 0; i < outputCount; i++ {
		hash := randHash(t, byte(i+1))
		pub := ringPoint(byte(i + 17))
		var commit crypto.Commitment
		commit[0] = byte(i + 1)
		err := db.PutUTXO(hash, 0, &store.StoredUTXO{
			TxHash: hash, OneTimePub: pub, AmountCommit: commit, BlockHeight: uint64(i + 1),
		})
		if err != nil {
			t.Fatalf("PutUTXO[%d]: %v", i, err)
		}
	}

	target := ringPoint(22)
	got, err := db.LookupRingMember(target)
	if err != nil {
		t.Fatalf("LookupRingMember: %v", err)
	}
	if got == nil || got.OneTimePub != target || got.AmountCommit[0] != 6 {
		t.Fatalf("lookup returned wrong output: %#v", got)
	}

	excluded := map[crypto.Point32]bool{target: true}
	sampled, err := db.SampleRingMembers(15, excluded)
	if err != nil {
		t.Fatalf("SampleRingMembers: %v", err)
	}
	if len(sampled) != 15 {
		t.Fatalf("sample size = %d, want 15", len(sampled))
	}
	seen := make(map[crypto.Point32]bool)
	for _, decoy := range sampled {
		if decoy.OneTimePub == target {
			t.Fatal("excluded output was sampled")
		}
		if seen[decoy.OneTimePub] {
			t.Fatal("duplicate output was sampled")
		}
		seen[decoy.OneTimePub] = true
	}

	targetHash := randHash(t, 6)
	if err := db.DeleteUTXO(targetHash, 0); err != nil {
		t.Fatalf("DeleteUTXO: %v", err)
	}
	got, err = db.LookupRingMember(target)
	if err != nil {
		t.Fatalf("LookupRingMember after delete: %v", err)
	}
	if got != nil {
		t.Fatal("rolled-back output remains in ring-member index")
	}
}