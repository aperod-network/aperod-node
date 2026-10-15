package store

import (
	"encoding/json"
	"testing"

	"github.com/syndtr/goleveldb/leveldb/opt"
)

func TestRingSamplingVerifyDetectsUnfencedLegacyWriter(t *testing.T) {
	db := samplingDB(t)
	for i := byte(1); i <= 4; i++ {
		putSamplingOutput(t, db, sparseRingOutput(i))
	}
	before, err := db.VerifyRingSamplingIndex()
	if err != nil || before.Canonical != 4 || before.Eligible != 4 {
		t.Fatal(before, err)
	}
	// Emulate an older binary: mutate only canonical rm/, leaving a valid
	// count/slot namespace. A marker-only check incorrectly accepts this.
	newMember := sparseRingOutput(7)
	raw, err := json.Marshal(newMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Put(ringMemberKey(newMember.OneTimePub), raw, &opt.WriteOptions{Sync: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.VerifyRingSamplingIndex(); err == nil {
		t.Fatal("stale ready marker passed full audit")
	}
	if err := db.InvalidateRingSamplingIndex(); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureRingMemberIndex(); err != nil {
		t.Fatal(err)
	}
	after, err := db.VerifyRingSamplingIndex()
	if err != nil || after.Canonical != 5 || after.Eligible != 5 || after.CanonicalSHA256 == before.CanonicalSHA256 {
		t.Fatal(after, err)
	}
}

func TestRingSamplingVerifyCorruptionAndClosedBackend(t *testing.T) {
	for _, mutate := range []func(*DB) error{
		func(d *DB) error { return d.InvalidateRingSamplingIndex() },
		func(d *DB) error { return d.db.Put(ringSamplingSlotKey(1), make([]byte, 32), nil) },
		func(d *DB) error { return d.db.Put(ringSamplingSlotKey(4), sparseRingOutput(1).OneTimePub[:], nil) },
		func(d *DB) error {
			return d.db.Put(append(append([]byte{}, ringSamplingReversePrefix...), make([]byte, 32)...), ringSamplingUint(0), nil)
		},
		func(d *DB) error {
			return d.db.Put(append(append([]byte{}, ringSamplingSlotPrefix...), 1), []byte{1}, nil)
		},
		func(d *DB) error { return d.Close() },
	} {
		d := samplingDB(t)
		for i := byte(1); i <= 4; i++ {
			putSamplingOutput(t, d, sparseRingOutput(i))
		}
		if err := mutate(d); err != nil {
			t.Fatal(err)
		}
		if _, err := d.VerifyRingSamplingIndex(); err == nil {
			t.Fatal("invalid or closed backend accepted")
		}
	}
}
