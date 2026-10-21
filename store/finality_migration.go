package store

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/aperod/aperod/crypto"
)

const FinalityCommitteeMigrationVersion uint8 = 1

const finalityCommitteeMigrationDomain = "aperod/finality-committee-migration/v1\x00"

type FinalityMigrationApproval struct {
	Validator crypto.ValidatorPubKey `json:"validator"`
	Signature []byte                 `json:"signature"`
}

// FinalityCommitteeMigration is a one-certificate, validator-authorized
// historical committee attestation. It does not add votes to, rewrite, or
// weaken the quorum rules of the original certificate.
type FinalityCommitteeMigration struct {
	Version                uint8                         `json:"version"`
	Genesis                crypto.Hash32                 `json:"genesis"`
	CertificateHeight      uint64                        `json:"certificate_height"`
	CertificateBlockHash   crypto.Hash32                 `json:"certificate_block_hash"`
	ApprovalHeight         uint64                        `json:"approval_height"`
	ApprovalBlockHash      crypto.Hash32                 `json:"approval_block_hash"`
	BFTThresholdPPM        uint32                        `json:"bft_threshold_ppm"`
	HistoricalCommittee    []crypto.ValidatorPubKey      `json:"historical_committee"`
	Approvals              []FinalityMigrationApproval   `json:"approvals"`
}

// SigningMessage returns the domain-separated digest approved by migration
// authorities. Committee and approval order must be canonical to avoid
// multiple encodings of the same migration.
func (m FinalityCommitteeMigration) SigningMessage() (crypto.Hash32, error) {
	if m.Version != FinalityCommitteeMigrationVersion ||
		m.Genesis == (crypto.Hash32{}) ||
		m.CertificateHeight == 0 ||
		m.CertificateBlockHash == (crypto.Hash32{}) ||
		m.ApprovalHeight < m.CertificateHeight ||
		m.ApprovalBlockHash == (crypto.Hash32{}) ||
		m.BFTThresholdPPM == 0 ||
		m.BFTThresholdPPM >= 1_000_000 ||
		len(m.HistoricalCommittee) == 0 ||
		len(m.HistoricalCommittee) > 256 {
		return crypto.Hash32{}, fmt.Errorf("finality migration: invalid header or committee")
	}

	var payload bytes.Buffer
	payload.WriteString(finalityCommitteeMigrationDomain)
	payload.WriteByte(m.Version)
	payload.Write(m.Genesis[:])
	if err := binary.Write(&payload, binary.BigEndian, m.CertificateHeight); err != nil {
		return crypto.Hash32{}, err
	}
	payload.Write(m.CertificateBlockHash[:])
	if err := binary.Write(&payload, binary.BigEndian, m.ApprovalHeight); err != nil {
		return crypto.Hash32{}, err
	}
	payload.Write(m.ApprovalBlockHash[:])
	if err := binary.Write(&payload, binary.BigEndian, m.BFTThresholdPPM); err != nil {
		return crypto.Hash32{}, err
	}
	if err := binary.Write(&payload, binary.BigEndian, uint32(len(m.HistoricalCommittee))); err != nil {
		return crypto.Hash32{}, err
	}
	for i, validator := range m.HistoricalCommittee {
		if len(validator) != 32 || isZeroFinalityKey(validator) {
			return crypto.Hash32{}, fmt.Errorf("finality migration: invalid historical validator key")
		}
		if i > 0 && bytes.Compare(m.HistoricalCommittee[i-1], validator) >= 0 {
			return crypto.Hash32{}, fmt.Errorf("finality migration: historical committee must be sorted and unique")
		}
		payload.Write(validator)
	}
	for i, approval := range m.Approvals {
		if len(approval.Validator) != 32 || isZeroFinalityKey(approval.Validator) ||
			len(approval.Signature) != 64 {
			return crypto.Hash32{}, fmt.Errorf("finality migration: malformed approval")
		}
		if i > 0 && bytes.Compare(m.Approvals[i-1].Validator, approval.Validator) >= 0 {
			return crypto.Hash32{}, fmt.Errorf("finality migration: approvals must be sorted and unique")
		}
	}
	return crypto.HashBytes(payload.Bytes()), nil
}

func isZeroFinalityKey(key crypto.ValidatorPubKey) bool {
	if len(key) != 32 {
		return false
	}
	for _, b := range key {
		if b != 0 {
			return false
		}
	}
	return true
}
