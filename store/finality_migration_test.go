package store

import (
	"bytes"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestFinalityCommitteeMigrationSigningMessageBindsCanonicalCommittee(t *testing.T) {
	_, first, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	committee := []crypto.ValidatorPubKey{first, second}
	if bytes.Compare(committee[0], committee[1]) > 0 {
		committee[0], committee[1] = committee[1], committee[0]
	}
	migration := FinalityCommitteeMigration{
		Version:              FinalityCommitteeMigrationVersion,
		Genesis:              crypto.Hash32{1},
		CertificateHeight:    10,
		CertificateBlockHash: crypto.Hash32{2},
		ApprovalHeight:       11,
		ApprovalBlockHash:    crypto.Hash32{3},
		BFTThresholdPPM:      667_000,
		HistoricalCommittee:  committee,
		Approvals: []FinalityMigrationApproval{
			{Validator: first, Signature: make([]byte, 64)},
		},
	}

	message, err := migration.SigningMessage()
	if err != nil {
		t.Fatal(err)
	}
	changed := migration
	changed.ApprovalBlockHash = crypto.Hash32{4}
	changedMessage, err := changed.SigningMessage()
	if err != nil {
		t.Fatal(err)
	}
	if changedMessage == message {
		t.Fatal("approval anchor was not bound into the signed migration message")
	}

	unsorted := migration
	unsorted.HistoricalCommittee = []crypto.ValidatorPubKey{committee[1], committee[0]}
	if _, err := unsorted.SigningMessage(); err == nil {
		t.Fatal("non-canonical historical committee order was accepted")
	}
}
