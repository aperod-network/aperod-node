package store

import (
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/crypto"
)

var finalityCertificateKey = []byte("consensus/finality_certificate/v1")
var localFinalityVoteKey = []byte("consensus/local_finality_vote/v1")

type FinalityVote struct {
	Validator crypto.ValidatorPubKey `json:"validator"`
	Signature []byte                 `json:"signature"`
}

// FinalityCertificate is an authentic quorum proof for one canonical block.
// It is never inferred from a tip, snapshot, or checkpoint.
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
	Version   uint8                  `json:"version"`
	Height    uint64                 `json:"height"`
	BlockHash crypto.Hash32          `json:"block_hash"`
	Validator crypto.ValidatorPubKey `json:"validator"`
	Signature []byte                 `json:"signature"`
}

func (d *DB) SaveLocalFinalityVote(v LocalFinalityVote) error {
	if v.Version != 1 || v.BlockHash == (crypto.Hash32{}) || len(v.Signature) == 0 {
		return fmt.Errorf("finality: invalid local vote")
	}
	if prior, err := d.LoadLocalFinalityVote(); err != nil {
		return err
	} else if prior != nil {
		if v.Height < prior.Height {
			return fmt.Errorf("finality: refusing local vote height regression")
		}
		if v.Height == prior.Height && (v.BlockHash != prior.BlockHash || !v.Validator.Equals(prior.Validator)) {
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
	if prior, err := d.LoadFinalityCertificate(); err != nil {
		return err
	} else if prior != nil {
		if c.Height < prior.Height {
			return fmt.Errorf("finality: refusing certificate height regression")
		}
		if c.Height == prior.Height && c.BlockHash != prior.BlockHash {
			return fmt.Errorf("finality: conflicting certificate at height %d", c.Height)
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return d.putSync(finalityCertificateKey, raw)
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
