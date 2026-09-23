package store_test

import (
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestProtocolLockedUTXONeverPersistentRingMember(t *testing.T) {
	db := openTestDB(t)
	hash := randHash(t, 201)
	pub := ringPoint(201)
	var commit crypto.Commitment
	commit[0] = 1
	if err := db.PutUTXO(hash, 0, &store.StoredUTXO{
		TxHash: hash, OneTimePub: pub, AmountCommit: commit, ProtocolLocked: true,
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.GetUTXO(hash, 0)
	if err != nil || loaded == nil || !loaded.ProtocolLocked {
		t.Fatalf("protocol lock was not preserved: %#v %v", loaded, err)
	}
	ringMember, err := db.LookupRingMember(pub)
	if err != nil || ringMember != nil {
		t.Fatalf("protocol-locked output exposed by durable ring lookup: %#v %v", ringMember, err)
	}
	decoys, err := db.SampleRingMembers(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoys) != 0 {
		t.Fatalf("locked output selected as persistent decoy: %#v", decoys)
	}
}

func TestGuardianFundDurableBindingRejectsRestartMutation(t *testing.T) {
	db := openTestDB(t)
	genesis := randHash(t, 202)
	if err := db.BindGuardianFundConfig(100, genesis); err != nil {
		t.Fatal(err)
	}
	if err := db.BindGuardianFundConfig(100, genesis); err != nil {
		t.Fatalf("idempotent restart rejected: %v", err)
	}
	if err := db.BindGuardianFundConfig(101, genesis); err == nil {
		t.Fatal("activation-height mutation accepted")
	}
	if err := db.BindGuardianFundConfig(0, genesis); err == nil {
		t.Fatal("disabling a bound activation accepted")
	}
	if err := db.BindGuardianFundConfig(100, randHash(t, 203)); err == nil {
		t.Fatal("genesis mutation accepted")
	}
}
