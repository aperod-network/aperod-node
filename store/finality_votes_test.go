package store

import (
	"bytes"
	"sort"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestFinalityCommitteeSnapshotIsDurableAndImmutable(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var validators []crypto.ValidatorPubKey
	for range 3 {
		_, pub, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
		validators = append(validators, pub)
	}
	sort.Slice(validators, func(i, j int) bool { return bytes.Compare(validators[i], validators[j]) < 0 })
	snapshot := FinalityCommitteeSnapshot{
		Version: 1, Epoch: 7, Validators: validators, BFTThresholdPPM: 667_000,
	}
	if err := db.SaveFinalityCommitteeSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.LoadFinalityCommitteeSnapshot(snapshot.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Version != snapshot.Version || got.Epoch != snapshot.Epoch ||
		got.BFTThresholdPPM != snapshot.BFTThresholdPPM || len(got.Validators) != len(validators) {
		t.Fatalf("restored committee snapshot = %#v", got)
	}
	for i := range validators {
		if !got.Validators[i].Equals(validators[i]) {
			t.Fatalf("restored validator %d differs from the snapshot", i)
		}
	}
	changed := snapshot
	changed.Validators = append([]crypto.ValidatorPubKey(nil), validators[:2]...)
	if err := db.SaveFinalityCommitteeSnapshot(changed); err == nil {
		t.Fatal("committee snapshot was overwritten for an existing epoch")
	}
}

func TestFinalityVoteJournalAndEquivocationEvidencePersist(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	validator := crypto.ValidatorPubKey(make([]byte, 32))
	validator[31] = 1
	hash1 := crypto.HashStr("canonical-finality-vote")
	vote := PersistedFinalityVote{
		Version: 1, Height: 5, BlockHash: hash1, Validator: validator,
		Signature: bytes.Repeat([]byte{1}, 64), VoteVersion: crypto.FinalityVoteVersionHeightBound,
	}
	if err := db.SaveFinalityVote(vote); err != nil {
		t.Fatal(err)
	}
	evidence := VoteEquivocationEvidence{
		Version: 1, Height: 5, Validator: validator,
		BlockHash1: hash1, Signature1: bytes.Repeat([]byte{1}, 64),
		VoteVersion1: crypto.FinalityVoteVersionHeightBound,
		BlockHash2:   crypto.HashStr("conflicting-finality-vote"),
		Signature2:   bytes.Repeat([]byte{2}, 64),
		VoteVersion2: crypto.FinalityVoteVersionHeightBound,
	}
	if err := db.SaveVoteEquivocationEvidence(evidence); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	votes, err := db.LoadFinalityVotes(5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(votes) != 1 || votes[0].BlockHash != hash1 || votes[0].VoteVersion != crypto.FinalityVoteVersionHeightBound {
		t.Fatalf("reopened finality vote journal = %#v", votes)
	}
	prior, err := db.LoadFinalityVote(5, validator)
	if err != nil || prior == nil || prior.BlockHash != hash1 {
		t.Fatalf("height-specific finality vote = %#v, err=%v", prior, err)
	}
	gotEvidence, err := db.LoadVoteEquivocationEvidence(5, validator)
	if err != nil {
		t.Fatal(err)
	}
	if gotEvidence == nil || gotEvidence.BlockHash1 != evidence.BlockHash1 ||
		gotEvidence.BlockHash2 != evidence.BlockHash2 ||
		!bytes.Equal(gotEvidence.Signature2, evidence.Signature2) {
		t.Fatalf("reopened equivocation evidence = %#v", gotEvidence)
	}
	conflict := vote
	conflict.BlockHash = evidence.BlockHash2
	if err := db.SaveFinalityVote(conflict); err == nil {
		t.Fatal("vote journal accepted two hashes for one validator and height")
	}
}

func TestFinalityCertificatesAreAddressableByHeight(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	validator := crypto.ValidatorPubKey(make([]byte, 32))
	validator[31] = 1
	certificate := func(height uint64) FinalityCertificate {
		return FinalityCertificate{
			Version: 1, Height: height, BlockHash: crypto.HashStr(string(rune(height + 1))),
			Votes: []FinalityVote{{Validator: validator, Signature: bytes.Repeat([]byte{byte(height)}, 64)}},
		}
	}
	latest := certificate(12)
	older := certificate(10)
	if err := db.SaveFinalityCertificate(latest); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveFinalityCertificate(older); err != nil {
		t.Fatal(err)
	}
	gotLatest, err := db.LoadFinalityCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if gotLatest == nil || gotLatest.Height != latest.Height {
		t.Fatalf("latest certificate regressed: %#v", gotLatest)
	}
	gotOlder, err := db.LoadFinalityCertificateAt(older.Height)
	if err != nil {
		t.Fatal(err)
	}
	if gotOlder == nil || gotOlder.BlockHash != older.BlockHash {
		t.Fatalf("height-addressed certificate = %#v", gotOlder)
	}
}
