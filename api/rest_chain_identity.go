package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

type identityHeaderEvidence struct {
	Height                  uint64 `json:"height"`
	HeaderHash              string `json:"header_hash"`
	CanonicalIndexHash      string `json:"canonical_index_hash"`
	PrevHash                string `json:"prev_hash"`
	Timestamp               int64  `json:"timestamp_ns"`
	ValidatorPub            string `json:"validator_pub"`
	SignatureAuthority      string `json:"signature_authority"`
	Signature               string `json:"signature"`
	SignatureValidForHeader bool   `json:"signature_valid_for_header_hash"`
	SignatureValidForIndex  bool   `json:"signature_valid_for_canonical_index_hash"`
	HeaderHashMatchesIndex  bool   `json:"header_hash_matches_canonical_index"`
}

type identityFinalityVote struct {
	Validator   string `json:"validator"`
	Signature   string `json:"signature"`
	VoteVersion uint8  `json:"vote_version"`
}

type identityFinalityEvidence struct {
	Version            uint8                  `json:"version"`
	Height             uint64                 `json:"height"`
	BlockHash          string                 `json:"block_hash"`
	Epoch              uint64                 `json:"epoch"`
	ThresholdPPM       uint32                 `json:"threshold_ppm"`
	RequiredVotes      int                    `json:"required_votes"`
	ValidVotes         int                    `json:"valid_votes"`
	Verified           bool                   `json:"verified"`
	VerificationStatus string                 `json:"verification_status,omitempty"`
	Committee          []string               `json:"committee"`
	Votes              []identityFinalityVote `json:"votes"`
}

type chainIdentityEvidenceResponse struct {
	SchemaVersion        string                            `json:"schema_version"`
	TrustModel           string                            `json:"trust_model"`
	Verification         string                            `json:"verification"`
	Genesis              identityHeaderEvidence            `json:"genesis"`
	Checkpoint           identityHeaderEvidence            `json:"checkpoint"`
	Finality             identityFinalityEvidence          `json:"finality"`
	BootstrapAttestation *mainnetbootstrap.SignedStatement `json:"bootstrap_attestation,omitempty"`
	Limitations          []string                          `json:"limitations"`
}

// restChainIdentityEvidence exports the canonical genesis and checkpoint
// evidence. When the server has a verified Mainnet bootstrap statement, the
// explicit operator_1_of_1 policy is sufficient for that anchor; the handler
// reports finality quorum as unverified rather than silently requiring it.
func (s *Server) restChainIdentityEvidence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.blockStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "canonical chain store unavailable")
		return
	}

	genesis, genesisIndexHash, err := s.identityHeaderAt(0)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("genesis evidence unavailable: %v", err))
		return
	}
	genesisHeaderHash := genesis.Hash()
	genesisAuthority := s.identitySignatureAuthority(genesis.Header.ValidatorPub, genesis.Header.Height)
	genesisHeaderSignatureValid := s.verifyAuditHeader(&genesis.Header)
	genesisIndexSignatureValid := genesisAuthority.Verify(genesisIndexHash, genesis.Header.Signature)
	if !genesisHeaderSignatureValid && !genesisIndexSignatureValid {
		writeJSONError(w, http.StatusServiceUnavailable, "genesis signature does not verify against the header or canonical index hash")
		return
	}

	if s.mainnetIdentity != nil {
		s.restMainnetOperatorIdentityEvidence(
			w,
			genesis,
			genesisIndexHash,
			genesisHeaderHash,
			genesisAuthority,
			genesisHeaderSignatureValid,
			genesisIndexSignatureValid,
		)
		return
	}

	certificate, err := s.blockStore.LoadFinalityCertificate()
	if err != nil || certificate == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "persisted finality certificate unavailable")
		return
	}
	if certificate.Version != 1 || certificate.Height == 0 || certificate.BlockHash == (crypto.Hash32{}) {
		writeJSONError(w, http.StatusServiceUnavailable, "persisted finality certificate is malformed")
		return
	}

	checkpoint, checkpointIndexHash, err := s.identityHeaderAt(certificate.Height)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("checkpoint evidence unavailable: %v", err))
		return
	}
	if checkpointIndexHash != certificate.BlockHash || checkpoint.Hash() != certificate.BlockHash {
		writeJSONError(w, http.StatusServiceUnavailable, "finality certificate does not match the canonical checkpoint body and index")
		return
	}
	if !s.verifyAuditHeader(&checkpoint.Header) {
		writeJSONError(w, http.StatusServiceUnavailable, "checkpoint block signature does not verify")
		return
	}
	if checkpoint.Header.MerkleRoot != core.MerkleRoot(checkpoint.Txs) {
		writeJSONError(w, http.StatusServiceUnavailable, "checkpoint transaction body does not match its Merkle root")
		return
	}
	checkpointAuthority := s.identitySignatureAuthority(checkpoint.Header.ValidatorPub, checkpoint.Header.Height)

	epoch := certificate.Height / core.EpochLength
	committeeSnapshot, err := s.blockStore.LoadFinalityCommitteeSnapshot(epoch)
	if err != nil || committeeSnapshot == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "checkpoint committee snapshot unavailable")
		return
	}
	finality, err := verifyIdentityFinalityEvidence(*certificate, *committeeSnapshot, s.validatorAuthorityFork)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("finality evidence invalid: %v", err))
		return
	}
	// Committee and vote records are keyed by the collateral identity. The
	// block signature is verified separately with checkpointAuthority, which
	// may be a height-scoped replacement signer after an authority rotation.
	if err := verifyOperatorOneOfOne(finality, checkpoint.Header.ValidatorPub); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "operator 1-of-1 identity evidence invalid: "+err.Error())
		return
	}
	finality.Verified = true
	finality.VerificationStatus = "verified_operator_1_of_1"

	response := chainIdentityEvidenceResponse{
		SchemaVersion: "aperod-chain-identity-evidence-v3",
		TrustModel:    "operator_1_of_1",
		Verification:  "genesis_signature_and_checkpoint_operator_1_of_1_verified_by_serving_node",
		Genesis: identityHeaderEvidence{
			Height:                  genesis.Header.Height,
			HeaderHash:              hashHex(genesisHeaderHash),
			CanonicalIndexHash:      hashHex(genesisIndexHash),
			PrevHash:                hashHex(genesis.Header.PrevHash),
			Timestamp:               genesis.Header.Timestamp,
			ValidatorPub:            genesis.Header.ValidatorPub.Hex(),
			SignatureAuthority:      genesisAuthority.Hex(),
			Signature:               hex.EncodeToString(genesis.Header.Signature),
			SignatureValidForHeader: genesisHeaderSignatureValid,
			SignatureValidForIndex:  genesisIndexSignatureValid,
			HeaderHashMatchesIndex:  genesisHeaderHash == genesisIndexHash,
		},
		Checkpoint: identityHeaderEvidence{
			Height:                  checkpoint.Header.Height,
			HeaderHash:              hashHex(checkpoint.Hash()),
			CanonicalIndexHash:      hashHex(checkpointIndexHash),
			PrevHash:                hashHex(checkpoint.Header.PrevHash),
			Timestamp:               checkpoint.Header.Timestamp,
			ValidatorPub:            checkpoint.Header.ValidatorPub.Hex(),
			SignatureAuthority:      checkpointAuthority.Hex(),
			Signature:               hex.EncodeToString(checkpoint.Header.Signature),
			SignatureValidForHeader: true,
			SignatureValidForIndex:  checkpointAuthority.Verify(checkpointIndexHash, checkpoint.Header.Signature),
			HeaderHashMatchesIndex:  checkpoint.Hash() == checkpointIndexHash,
		},
		Finality: finality,
		Limitations: []string{
			"No network name or token-supply claim is made by this evidence endpoint.",
			"Trust is explicitly operator 1-of-1; it is not an independent validator quorum.",
			"The checkpoint is bound to this node's canonical index; this response does not export or replay the intervening block history.",
			"Clients should independently verify the genesis and checkpoint signatures, the single operator key, and the parent-linked chain history.",
		},
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) restMainnetOperatorIdentityEvidence(
	w http.ResponseWriter,
	genesis *core.Block,
	genesisIndexHash, genesisHeaderHash crypto.Hash32,
	genesisAuthority crypto.ValidatorPubKey,
	genesisHeaderSignatureValid, genesisIndexSignatureValid bool,
) {
	identity := s.mainnetIdentity
	attestation := identity.signedBootstrap
	statement := attestation.Statement
	if statement.TrustModel != mainnetbootstrap.TrustModel ||
		statement.GenesisHeaderHash != hashHex(identity.genesisHeaderHash) ||
		statement.GenesisIndexHash != hashHex(identity.genesisIndexHash) ||
		statement.CheckpointHeight != identity.checkpointHeight ||
		statement.CheckpointHeaderHash != hashHex(identity.checkpointHeaderHash) ||
		statement.CheckpointIndexHash != hashHex(identity.checkpointIndexHash) ||
		genesisHeaderHash != identity.genesisHeaderHash ||
		genesisIndexHash != identity.genesisIndexHash {
		writeJSONError(w, http.StatusServiceUnavailable, "verified Mainnet bootstrap identity is inconsistent")
		return
	}

	checkpoint, checkpointIndexHash, err := s.identityHeaderAt(identity.checkpointHeight)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("signed Mainnet checkpoint evidence unavailable: %v", err))
		return
	}
	if checkpointIndexHash != identity.checkpointIndexHash ||
		checkpoint.Hash() != identity.checkpointHeaderHash ||
		checkpoint.Header.ValidatorPub.Hex() != statement.CheckpointValidatorPublicKeyHex ||
		len(checkpoint.Header.Signature) != statement.CheckpointSignatureLength {
		writeJSONError(w, http.StatusServiceUnavailable, "checkpoint body does not match the signed Mainnet bootstrap statement")
		return
	}
	if !s.verifyAuditHeader(&checkpoint.Header) {
		writeJSONError(w, http.StatusServiceUnavailable, "signed Mainnet checkpoint block signature does not verify")
		return
	}
	if checkpoint.Header.MerkleRoot != core.MerkleRoot(checkpoint.Txs) {
		writeJSONError(w, http.StatusServiceUnavailable, "signed Mainnet checkpoint transaction body does not match its Merkle root")
		return
	}

	checkpointAuthority := s.identitySignatureAuthority(checkpoint.Header.ValidatorPub, checkpoint.Header.Height)
	response := chainIdentityEvidenceResponse{
		SchemaVersion: "aperod-chain-identity-evidence-v3",
		TrustModel:    mainnetbootstrap.TrustModel,
		Verification:  "operator_signed_bootstrap_genesis_and_checkpoint_signatures_verified_by_serving_node",
		Genesis: identityHeaderEvidence{
			Height:                  genesis.Header.Height,
			HeaderHash:              hashHex(genesisHeaderHash),
			CanonicalIndexHash:      hashHex(genesisIndexHash),
			PrevHash:                hashHex(genesis.Header.PrevHash),
			Timestamp:               genesis.Header.Timestamp,
			ValidatorPub:            genesis.Header.ValidatorPub.Hex(),
			SignatureAuthority:      genesisAuthority.Hex(),
			Signature:               hex.EncodeToString(genesis.Header.Signature),
			SignatureValidForHeader: genesisHeaderSignatureValid,
			SignatureValidForIndex:  genesisIndexSignatureValid,
			HeaderHashMatchesIndex:  genesisHeaderHash == genesisIndexHash,
		},
		Checkpoint: identityHeaderEvidence{
			Height:                  checkpoint.Header.Height,
			HeaderHash:              hashHex(checkpoint.Hash()),
			CanonicalIndexHash:      hashHex(checkpointIndexHash),
			PrevHash:                hashHex(checkpoint.Header.PrevHash),
			Timestamp:               checkpoint.Header.Timestamp,
			ValidatorPub:            checkpoint.Header.ValidatorPub.Hex(),
			SignatureAuthority:      checkpointAuthority.Hex(),
			Signature:               hex.EncodeToString(checkpoint.Header.Signature),
			SignatureValidForHeader: true,
			SignatureValidForIndex:  checkpointAuthority.Verify(checkpointIndexHash, checkpoint.Header.Signature),
			HeaderHashMatchesIndex:  checkpoint.Hash() == checkpointIndexHash,
		},
		Finality: identityFinalityEvidence{
			Height:             identity.checkpointHeight,
			BlockHash:          hashHex(identity.checkpointIndexHash),
			Verified:           false,
			VerificationStatus: "not_verified_by_operator_bootstrap_attestation",
		},
		BootstrapAttestation: &attestation,
		Limitations: []string{
			"Trust is operator 1-of-1; it is not independent validator-committee quorum evidence.",
			"The signed bootstrap attestation binds only its listed genesis and checkpoint; intervening history is not replayed by this endpoint.",
			"The snapshot payload is not verified by this endpoint.",
			"The finality certificate and historical committee are not verified by this operator-bootstrap response.",
		},
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) identitySignatureAuthority(identity crypto.ValidatorPubKey, height uint64) crypto.ValidatorPubKey {
	if s.validatorAuthorityFork != nil {
		return s.validatorAuthorityFork.Authority(identity, height)
	}
	return identity
}

func (s *Server) identityHeaderAt(height uint64) (*core.Block, crypto.Hash32, error) {
	indexHash, found, err := s.blockStore.GetCanonicalHash(height)
	if err != nil {
		return nil, crypto.Hash32{}, err
	}
	if !found {
		return nil, crypto.Hash32{}, fmt.Errorf("canonical height %d is missing", height)
	}
	raw, err := s.blockStore.GetRawBlock(indexHash)
	if err != nil {
		return nil, crypto.Hash32{}, err
	}
	if len(raw) == 0 {
		return nil, crypto.Hash32{}, fmt.Errorf("block body at height %d is missing", height)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, crypto.Hash32{}, err
	}
	if block.Header.Height != height {
		return nil, crypto.Hash32{}, fmt.Errorf("block body height %d does not match index height %d", block.Header.Height, height)
	}
	if height == 0 && !block.IsGenesis() {
		return nil, crypto.Hash32{}, fmt.Errorf("height zero is not a genesis block")
	}
	return &block, indexHash, nil
}

func verifyIdentityFinalityEvidence(
	certificate store.FinalityCertificate,
	committee store.FinalityCommitteeSnapshot,
	authorityFork *core.ValidatorAuthorityFork,
) (identityFinalityEvidence, error) {
	if committee.Version != 1 ||
		committee.Epoch != certificate.Height/core.EpochLength ||
		committee.BFTThresholdPPM == 0 ||
		committee.BFTThresholdPPM > 1_000_000 ||
		len(committee.Validators) == 0 {
		return identityFinalityEvidence{}, fmt.Errorf("committee snapshot is malformed")
	}

	members := make(map[string]crypto.ValidatorPubKey, len(committee.Validators))
	committeeKeys := make([]string, 0, len(committee.Validators))
	for _, validator := range committee.Validators {
		if len(validator) != 32 {
			return identityFinalityEvidence{}, fmt.Errorf("committee contains a malformed public key")
		}
		key := validator.Hex()
		if _, duplicate := members[key]; duplicate {
			return identityFinalityEvidence{}, fmt.Errorf("committee contains a duplicate public key")
		}
		members[key] = validator
		committeeKeys = append(committeeKeys, key)
	}

	required, err := core.RequiredFinalityVotes(len(committee.Validators), committee.BFTThresholdPPM)
	if err != nil {
		return identityFinalityEvidence{}, err
	}
	if len(certificate.Votes) > len(committee.Validators) {
		return identityFinalityEvidence{}, fmt.Errorf("certificate has more votes than committee members")
	}
	votes := make([]identityFinalityVote, 0, len(certificate.Votes))
	seen := make(map[string]bool, len(certificate.Votes))
	for _, vote := range certificate.Votes {
		key := vote.Validator.Hex()
		if len(vote.Validator) != 32 || len(vote.Signature) != 64 || seen[key] {
			return identityFinalityEvidence{}, fmt.Errorf("certificate contains malformed or duplicate vote")
		}
		if _, member := members[key]; !member {
			return identityFinalityEvidence{}, fmt.Errorf("certificate includes a signer outside the checkpoint committee")
		}
		version := vote.VoteVersion
		if version == 0 {
			version = crypto.FinalityVoteVersionLegacy
		}
		if version != crypto.FinalityVoteVersionLegacy && version != crypto.FinalityVoteVersionHeightBound {
			return identityFinalityEvidence{}, fmt.Errorf("certificate uses unsupported vote version %d", version)
		}
		message := crypto.FinalityVoteMessage(version, certificate.Height, certificate.BlockHash)
		authority := vote.Validator
		if authorityFork != nil {
			authority = authorityFork.Authority(vote.Validator, certificate.Height)
		}
		if !authority.Verify(message, vote.Signature) {
			return identityFinalityEvidence{}, fmt.Errorf("certificate contains an invalid vote signature")
		}
		seen[key] = true
		votes = append(votes, identityFinalityVote{
			Validator:   key,
			Signature:   hex.EncodeToString(vote.Signature),
			VoteVersion: version,
		})
	}
	if len(seen) < required {
		return identityFinalityEvidence{}, fmt.Errorf("certificate has %d valid votes; %d required", len(seen), required)
	}

	return identityFinalityEvidence{
		Version:       certificate.Version,
		Height:        certificate.Height,
		BlockHash:     hashHex(certificate.BlockHash),
		Epoch:         committee.Epoch,
		ThresholdPPM:  committee.BFTThresholdPPM,
		RequiredVotes: required,
		ValidVotes:    len(seen),
		Committee:     committeeKeys,
		Votes:         votes,
	}, nil
}

func verifyOperatorOneOfOne(finality identityFinalityEvidence, checkpointIdentity crypto.ValidatorPubKey) error {
	if len(checkpointIdentity) != 32 {
		return fmt.Errorf("checkpoint operator key is malformed")
	}
	if len(finality.Committee) != 1 || len(finality.Votes) != 1 ||
		finality.RequiredVotes != 1 || finality.ValidVotes != 1 {
		return fmt.Errorf("exactly one committee member and one valid signature are required")
	}
	operator := checkpointIdentity.Hex()
	if finality.Committee[0] != operator || finality.Votes[0].Validator != operator {
		return fmt.Errorf("checkpoint collateral identity is not the sole finality member")
	}
	return nil
}

func hashHex(hash crypto.Hash32) string {
	return hex.EncodeToString(hash[:])
}
