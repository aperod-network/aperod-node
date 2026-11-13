package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb/util"
)

var finalityCertificateKey = []byte("consensus/finality_certificate/v1")
var finalityCertificateHeightPrefix = []byte("consensus/finality_certificate/v2/")
var localFinalityVoteKey = []byte("consensus/local_finality_vote/v1")
var finalityVotePrefix = []byte("consensus/finality_vote/v1/")
var voteEquivocationPrefix = []byte("consensus/vote_equivocation/v1/")
var finalityCommitteePrefix = []byte("consensus/finality_committee/v1/")

type FinalityVote struct {
	Validator   crypto.ValidatorPubKey `json:"validator"`
	Signature   []byte                 `json:"signature"`
	VoteVersion uint8                  `json:"vote_version,omitempty"`
}

// FinalityCertificate stores votes for one canonical block. Consensus checks
// their signatures and independently authorized membership before accepting a
// quorum. Loading this record alone does not authenticate a historical committee.
type FinalityCertificate struct {
	Version   uint8          `json:"version"`
	Height    uint64         `json:"height"`
	BlockHash crypto.Hash32  `json:"block_hash"`
	Votes     []FinalityVote `json:"votes"`
}

// LocalFinalityVote is the latest vote signed by this node. It is persisted
// independently of quorum certificates so the exact signature can be relayed
// after reconnects and restarts without signing again.
type LocalFinalityVote struct {
	Version     uint8                  `json:"version"`
	Height      uint64                 `json:"height"`
	BlockHash   crypto.Hash32          `json:"block_hash"`
	Validator   crypto.ValidatorPubKey `json:"validator"`
	Signature   []byte                 `json:"signature"`
	VoteVersion uint8                  `json:"vote_version,omitempty"`
}

// PersistedFinalityVote is one authenticated vote retained so partial quorums
// and anti-equivocation state survive process restarts.
type PersistedFinalityVote struct {
	Version     uint8                  `json:"version"`
	Height      uint64                 `json:"height"`
	BlockHash   crypto.Hash32          `json:"block_hash"`
	Validator   crypto.ValidatorPubKey `json:"validator"`
	Signature   []byte                 `json:"signature"`
	VoteVersion uint8                  `json:"vote_version"`
}

// FinalityCommitteeSnapshot pins the committee and quorum threshold used to
// validate votes in one epoch, so a restart can safely complete an older
// partial quorum after the live registry has advanced.
type FinalityCommitteeSnapshot struct {
	Version         uint8                    `json:"version"`
	Epoch           uint64                   `json:"epoch"`
	Validators      []crypto.ValidatorPubKey `json:"validators"`
	BFTThresholdPPM uint32                   `json:"bft_threshold_ppm"`
}

// VoteEquivocationEvidence contains two height-bound signatures by one
// validator for different blocks at the same height.
type VoteEquivocationEvidence struct {
	Version      uint8                  `json:"version"`
	Height       uint64                 `json:"height"`
	Validator    crypto.ValidatorPubKey `json:"validator"`
	BlockHash1   crypto.Hash32          `json:"block_hash_1"`
	Signature1   []byte                 `json:"signature_1"`
	VoteVersion1 uint8                  `json:"vote_version_1"`
	BlockHash2   crypto.Hash32          `json:"block_hash_2"`
	Signature2   []byte                 `json:"signature_2"`
	VoteVersion2 uint8                  `json:"vote_version_2"`
}

func finalityVoteKey(height uint64, validator crypto.ValidatorPubKey) []byte {
	key := make([]byte, len(finalityVotePrefix)+8+len(validator))
	copy(key, finalityVotePrefix)
	binary.BigEndian.PutUint64(key[len(finalityVotePrefix):], height)
	copy(key[len(finalityVotePrefix)+8:], validator)
	return key
}

func finalityCommitteeKey(epoch uint64) []byte {
	key := make([]byte, len(finalityCommitteePrefix)+8)
	copy(key, finalityCommitteePrefix)
	binary.BigEndian.PutUint64(key[len(finalityCommitteePrefix):], epoch)
	return key
}

func validateFinalityCommitteeSnapshot(snapshot FinalityCommitteeSnapshot, epoch uint64) error {
	if snapshot.Version != 1 || snapshot.Epoch != epoch || len(snapshot.Validators) == 0 ||
		len(snapshot.Validators) > 512 || snapshot.BFTThresholdPPM == 0 ||
		snapshot.BFTThresholdPPM > 1_000_000 {
		return fmt.Errorf("finality: invalid committee snapshot for epoch %d", epoch)
	}
	for i, validator := range snapshot.Validators {
		if len(validator) != 32 || (i > 0 && bytes.Compare(snapshot.Validators[i-1], validator) >= 0) {
			return fmt.Errorf("finality: committee members are malformed or not canonical at epoch %d", epoch)
		}
	}
	return nil
}

func (d *DB) SaveFinalityCommitteeSnapshot(snapshot FinalityCommitteeSnapshot) error {
	if err := validateFinalityCommitteeSnapshot(snapshot, snapshot.Epoch); err != nil {
		return err
	}
	key := finalityCommitteeKey(snapshot.Epoch)
	if priorRaw, err := d.get(key); err != nil {
		return err
	} else if priorRaw != nil {
		var prior FinalityCommitteeSnapshot
		if err := json.Unmarshal(priorRaw, &prior); err != nil {
			return fmt.Errorf("finality: corrupt committee snapshot: %w", err)
		}
		if err := validateFinalityCommitteeSnapshot(prior, snapshot.Epoch); err != nil {
			return err
		}
		if prior.BFTThresholdPPM != snapshot.BFTThresholdPPM ||
			len(prior.Validators) != len(snapshot.Validators) {
			return fmt.Errorf("finality: committee snapshot changed during epoch %d", snapshot.Epoch)
		}
		for i := range prior.Validators {
			if !prior.Validators[i].Equals(snapshot.Validators[i]) {
				return fmt.Errorf("finality: committee snapshot changed during epoch %d", snapshot.Epoch)
			}
		}
		return nil
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return d.putSync(key, raw)
}

func (d *DB) LoadFinalityCommitteeSnapshot(epoch uint64) (*FinalityCommitteeSnapshot, error) {
	raw, err := d.get(finalityCommitteeKey(epoch))
	if err != nil || raw == nil {
		return nil, err
	}
	var snapshot FinalityCommitteeSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, fmt.Errorf("finality: corrupt committee snapshot at epoch %d: %w", epoch, err)
	}
	if err := validateFinalityCommitteeSnapshot(snapshot, epoch); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func voteEquivocationKey(height uint64, validator crypto.ValidatorPubKey) []byte {
	key := make([]byte, len(voteEquivocationPrefix)+8+len(validator))
	copy(key, voteEquivocationPrefix)
	binary.BigEndian.PutUint64(key[len(voteEquivocationPrefix):], height)
	copy(key[len(voteEquivocationPrefix)+8:], validator)
	return key
}

func finalityCertificateHeightKey(height uint64) []byte {
	key := make([]byte, len(finalityCertificateHeightPrefix)+8)
	copy(key, finalityCertificateHeightPrefix)
	binary.BigEndian.PutUint64(key[len(finalityCertificateHeightPrefix):], height)
	return key
}

func (d *DB) SaveFinalityVote(v PersistedFinalityVote) error {
	if v.Version != 1 || v.BlockHash == (crypto.Hash32{}) ||
		len(v.Validator) != 32 || len(v.Signature) != 64 ||
		(v.VoteVersion != crypto.FinalityVoteVersionLegacy &&
			v.VoteVersion != crypto.FinalityVoteVersionHeightBound) {
		return fmt.Errorf("finality: invalid persisted vote")
	}
	key := finalityVoteKey(v.Height, v.Validator)
	if priorRaw, err := d.get(key); err != nil {
		return err
	} else if priorRaw != nil {
		var prior PersistedFinalityVote
		if err := json.Unmarshal(priorRaw, &prior); err != nil {
			return fmt.Errorf("finality: corrupt persisted vote: %w", err)
		}
		if prior.Height != v.Height || !prior.Validator.Equals(v.Validator) ||
			prior.BlockHash != v.BlockHash || prior.VoteVersion != v.VoteVersion ||
			!bytes.Equal(prior.Signature, v.Signature) {
			return fmt.Errorf("finality: conflicting persisted vote at height %d", v.Height)
		}
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return d.putSync(key, raw)
}

func (d *DB) LoadFinalityVote(height uint64, validator crypto.ValidatorPubKey) (*PersistedFinalityVote, error) {
	if len(validator) != 32 {
		return nil, fmt.Errorf("finality: invalid validator key")
	}
	raw, err := d.get(finalityVoteKey(height, validator))
	if err != nil || raw == nil {
		return nil, err
	}
	var vote PersistedFinalityVote
	if err := json.Unmarshal(raw, &vote); err != nil {
		return nil, fmt.Errorf("finality: corrupt persisted vote at height %d: %w", height, err)
	}
	if vote.Version != 1 || vote.Height != height || !vote.Validator.Equals(validator) ||
		vote.BlockHash == (crypto.Hash32{}) || len(vote.Signature) != 64 ||
		(vote.VoteVersion != crypto.FinalityVoteVersionLegacy &&
			vote.VoteVersion != crypto.FinalityVoteVersionHeightBound) {
		return nil, fmt.Errorf("finality: persisted vote key/value mismatch at height %d", height)
	}
	return &vote, nil
}

func (d *DB) LoadFinalityVotes(minHeight, maxHeight uint64) ([]PersistedFinalityVote, error) {
	if maxHeight < minHeight {
		return nil, nil
	}
	iter := d.db.NewIterator(util.BytesPrefix(finalityVotePrefix), nil)
	defer iter.Release()
	votes := make([]PersistedFinalityVote, 0)
	for iter.Next() {
		key := iter.Key()
		if len(key) != len(finalityVotePrefix)+8+32 {
			return nil, fmt.Errorf("finality: malformed persisted vote key")
		}
		height := binary.BigEndian.Uint64(key[len(finalityVotePrefix):])
		if height < minHeight || height > maxHeight {
			continue
		}
		var vote PersistedFinalityVote
		if err := json.Unmarshal(iter.Value(), &vote); err != nil {
			return nil, fmt.Errorf("finality: corrupt persisted vote at height %d: %w", height, err)
		}
		if vote.Version != 1 || vote.Height != height ||
			!bytes.Equal(vote.Validator, key[len(finalityVotePrefix)+8:]) ||
			vote.BlockHash == (crypto.Hash32{}) || len(vote.Signature) != 64 ||
			(vote.VoteVersion != crypto.FinalityVoteVersionLegacy &&
				vote.VoteVersion != crypto.FinalityVoteVersionHeightBound) {
			return nil, fmt.Errorf("finality: persisted vote key/value mismatch at height %d", height)
		}
		votes = append(votes, vote)
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}
	return votes, nil
}

func (d *DB) SaveVoteEquivocationEvidence(e VoteEquivocationEvidence) error {
	if e.Version != 1 || e.BlockHash1 == (crypto.Hash32{}) ||
		e.BlockHash2 == (crypto.Hash32{}) || e.BlockHash1 == e.BlockHash2 ||
		len(e.Validator) != 32 || len(e.Signature1) != 64 || len(e.Signature2) != 64 ||
		e.VoteVersion1 != crypto.FinalityVoteVersionHeightBound ||
		e.VoteVersion2 != crypto.FinalityVoteVersionHeightBound {
		return fmt.Errorf("finality: invalid vote equivocation evidence")
	}
	key := voteEquivocationKey(e.Height, e.Validator)
	if exists, err := d.has(key); err != nil {
		return err
	} else if exists {
		if _, err := d.LoadVoteEquivocationEvidence(e.Height, e.Validator); err != nil {
			return err
		}
		return nil // retain the first complete, independently verifiable proof
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return d.putSync(key, raw)
}

func (d *DB) LoadVoteEquivocationEvidence(height uint64, validator crypto.ValidatorPubKey) (*VoteEquivocationEvidence, error) {
	raw, err := d.get(voteEquivocationKey(height, validator))
	if err != nil || raw == nil {
		return nil, err
	}
	var evidence VoteEquivocationEvidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return nil, fmt.Errorf("finality: corrupt vote equivocation evidence: %w", err)
	}
	if evidence.Version != 1 || evidence.Height != height || !evidence.Validator.Equals(validator) ||
		len(evidence.Signature1) != 64 || len(evidence.Signature2) != 64 ||
		evidence.BlockHash1 == (crypto.Hash32{}) || evidence.BlockHash2 == (crypto.Hash32{}) ||
		evidence.BlockHash1 == evidence.BlockHash2 ||
		evidence.VoteVersion1 != crypto.FinalityVoteVersionHeightBound ||
		evidence.VoteVersion2 != crypto.FinalityVoteVersionHeightBound {
		return nil, fmt.Errorf("finality: vote equivocation evidence key/value mismatch")
	}
	return &evidence, nil
}

func (d *DB) SaveLocalFinalityVote(v LocalFinalityVote) error {
	if v.Version != 1 || v.BlockHash == (crypto.Hash32{}) || len(v.Signature) == 0 {
		return fmt.Errorf("finality: invalid local vote")
	}
	if v.VoteVersion != 0 && v.VoteVersion != crypto.FinalityVoteVersionLegacy &&
		v.VoteVersion != crypto.FinalityVoteVersionHeightBound {
		return fmt.Errorf("finality: unsupported local vote signing version %d", v.VoteVersion)
	}
	if prior, err := d.LoadLocalFinalityVote(); err != nil {
		return err
	} else if prior != nil {
		if v.Height < prior.Height {
			return fmt.Errorf("finality: refusing local vote height regression")
		}
		if v.Height == prior.Height && (v.BlockHash != prior.BlockHash ||
			!v.Validator.Equals(prior.Validator) || v.VoteVersion != prior.VoteVersion ||
			!bytes.Equal(v.Signature, prior.Signature)) {
			return fmt.Errorf("finality: conflicting local vote at height %d", v.Height)
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return d.putSync(localFinalityVoteKey, raw)
}

func (d *DB) LoadLocalFinalityVote() (*LocalFinalityVote, error) {
	raw, err := d.get(localFinalityVoteKey)
	if err != nil || raw == nil {
		return nil, err
	}
	var v LocalFinalityVote
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("finality: corrupt local vote: %w", err)
	}
	if v.Version != 1 {
		return nil, fmt.Errorf("finality: unsupported local vote version %d", v.Version)
	}
	return &v, nil
}

// SaveFinalityCertificate fsyncs a monotonic certificate. Consensus verifies
// signatures, committee membership and canonical chain binding before calling.
func (d *DB) SaveFinalityCertificate(c FinalityCertificate) error {
	if c.Version != 1 || c.BlockHash == (crypto.Hash32{}) || len(c.Votes) == 0 {
		return fmt.Errorf("finality: invalid certificate")
	}
	if prior, err := d.LoadFinalityCertificateAt(c.Height); err != nil {
		return err
	} else if prior != nil {
		if c.BlockHash != prior.BlockHash {
			return fmt.Errorf("finality: conflicting certificate at height %d", c.Height)
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	latest, err := d.LoadFinalityCertificate()
	if err != nil {
		return err
	}
	if latest != nil && c.Height < latest.Height {
		// Preserve certificates for a late-completed finality gap without
		// duplicating every normal, monotonically advancing certificate.
		return d.putSync(finalityCertificateHeightKey(c.Height), raw)
	}
	if latest == nil || c.Height >= latest.Height {
		return d.putSync(finalityCertificateKey, raw)
	}
	return nil
}

func (d *DB) LoadFinalityCertificate() (*FinalityCertificate, error) {
	raw, err := d.get(finalityCertificateKey)
	if err != nil || raw == nil {
		return nil, err
	}
	var c FinalityCertificate
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("finality: corrupt certificate: %w", err)
	}
	if c.Version != 1 {
		return nil, fmt.Errorf("finality: unsupported certificate version %d", c.Version)
	}
	return &c, nil
}

func (d *DB) LoadFinalityCertificateAt(height uint64) (*FinalityCertificate, error) {
	raw, err := d.get(finalityCertificateHeightKey(height))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		latest, err := d.LoadFinalityCertificate()
		if err != nil || latest == nil || latest.Height != height {
			return nil, err
		}
		return latest, nil
	}
	var c FinalityCertificate
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("finality: corrupt certificate at height %d: %w", height, err)
	}
	if c.Version != 1 || c.Height != height {
		return nil, fmt.Errorf("finality: certificate key/value mismatch at height %d", height)
	}
	return &c, nil
}
