package mainnetbootstrap

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

const historicalCommitteeHeight uint64 = 4_024_017
const historicalCommitteeHash = "63c5b24951137717f1636c4081ea77f2f93e8a5119030e9776ce2e27316e47de"
const historicalCommitteeGenesis = "c8fde86c1764a5603eabd6d9c56c9e47b4cf06d88d299a55665694473e2c512f"
const historicalCommitteeThresholdPPM uint32 = 667_000

// HistoricalCommitteeDraft grants no authority to approve a different block
// or committee. This is an off-chain, operator-attested recovery statement.
func HistoricalCommitteeDraft() store.FinalityCommitteeMigration {
	decode := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			panic(err) // static, checked pins above
		}
		return b
	}
	operator := crypto.ValidatorPubKey(decode(HistoricalCommitteeOperatorPublicKeyHex))
	block := crypto.Hash32(decode(historicalCommitteeHash))
	return store.FinalityCommitteeMigration{
		Version:              store.FinalityCommitteeOperatorVersion,
		Genesis:              crypto.Hash32(decode(historicalCommitteeGenesis)),
		CertificateHeight:    historicalCommitteeHeight,
		CertificateBlockHash: block,
		ApprovalHeight:       historicalCommitteeHeight,
		ApprovalBlockHash:    block,
		BFTThresholdPPM:      historicalCommitteeThresholdPPM,
		HistoricalCommittee:  []crypto.ValidatorPubKey{operator},
		Approvals:            []store.FinalityMigrationApproval{{Validator: operator, Signature: make([]byte, 64)}},
	}
}

func VerifyHistoricalCommitteeOperator(m store.FinalityCommitteeMigration, pinned []crypto.ValidatorPubKey) error {
	draft := HistoricalCommitteeDraft()
	expected, err := draft.SigningMessage()
	if err != nil {
		return err
	}
	actual, err := m.SigningMessage()
	if err != nil {
		return err
	}
	pub := draft.Approvals[0].Validator
	if actual != expected || len(pinned) != 1 || !pub.Equals(pinned[0]) ||
		len(m.Approvals) != 1 || !pub.Equals(m.Approvals[0].Validator) ||
		!pub.Verify(actual, m.Approvals[0].Signature) {
		return fmt.Errorf("finality migration: invalid pinned Mainnet operator attestation")
	}
	return nil
}

// ReadReleaseHistoricalCommittee authenticates the optional statement at its
// fixed bundle path. Callers must still bind it to the closed chain snapshot.
func ReadReleaseHistoricalCommittee(root string) (store.FinalityCommitteeMigration, error) {
	var statement store.FinalityCommitteeMigration
	path := filepath.Join(root, filepath.FromSlash(releaseHistoricalCommitteePath))
	info, err := os.Lstat(path)
	if err != nil {
		return statement, fmt.Errorf("inspect historical committee statement: %w", err)
	}
	const maxStatement = 16 << 10
	if !info.Mode().IsRegular() || info.Size() > maxStatement {
		return statement, fmt.Errorf("historical committee statement must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return statement, err
	}
	defer file.Close()
	dec := json.NewDecoder(io.LimitReader(file, maxStatement+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&statement); err != nil {
		return statement, fmt.Errorf("decode historical committee statement: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return statement, fmt.Errorf("historical committee statement contains trailing data")
	}
	pin := HistoricalCommitteeDraft().Approvals[0].Validator
	return statement, VerifyHistoricalCommitteeOperator(statement, []crypto.ValidatorPubKey{pin})
}
