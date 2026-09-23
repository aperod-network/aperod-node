package store

import (
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/crypto"
)

var finalityCertificateKey = []byte("consensus/finality_certificate/v1")

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
