package main

import (
	"fmt"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

const archiveConsistencyTrust = "internal consistency only; no historical committee anchor or quorum proof"

// No committee-authority input exists in this checker. Counting valid votes
// cannot authenticate a self-selected committee, even if every key signs.
type endpointEvidence struct {
	SignatureValidVoteKeys map[string]bool
	CommitteeAuthenticated bool
	QuorumAuthenticated    bool
	ProductionReady        bool
	Trust                  string
}

func endpointCertificateEvidence(cert *store.FinalityCertificate) (endpointEvidence, error) {
	result := endpointEvidence{Trust: archiveConsistencyTrust, SignatureValidVoteKeys: map[string]bool{}}
	if cert == nil || cert.Version != 1 || cert.BlockHash == (crypto.Hash32{}) || len(cert.Votes) == 0 {
		return result, fmt.Errorf("invalid or empty certificate")
	}
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), cert.BlockHash.Bytes())
	for _, vote := range cert.Votes {
		key := vote.Validator.Hex()
		if len(vote.Validator) != 32 || result.SignatureValidVoteKeys[key] || !vote.Validator.Verify(message, vote.Signature) {
			return result, fmt.Errorf("invalid or duplicate certificate vote")
		}
		result.SignatureValidVoteKeys[key] = true
	}
	return result, nil
}
