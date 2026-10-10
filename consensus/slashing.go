package consensus

import (
	"fmt"
	"sync"

	"github.com/aperod/aperod/crypto"
)

// DoubleSignEvidence records two conflicting block signatures from the same
// validator at the same height. Both rounds are retained because signing
// competing rounds at one height is slashable too.
type DoubleSignEvidence struct {
	Height    uint64
	Round1    uint32
	Validator crypto.ValidatorPubKey
	Hash1     crypto.Hash32
	Sig1      []byte
	Round2    uint32
	Hash2     crypto.Hash32
	Sig2      []byte
}

// evidenceKey uniquely identifies a validator's proposal at a height.
type evidenceKey struct {
	height uint64
	pubHex string
}

// slashingDetector detects double-signing by a validator at a height.
type slashingDetector struct {
	mu   sync.Mutex
	seen map[evidenceKey]signedSlot // first block seen per height and validator
}

type signedSlot struct {
	hash  crypto.Hash32
	sig   []byte
	round uint32
}

func newSlashingDetector() *slashingDetector {
	return &slashingDetector{seen: make(map[evidenceKey]signedSlot)}
}

// CheckBlock records a signed block header and returns DoubleSignEvidence
// if this validator has already signed a different block at the same height.
// Returns nil if no double-sign is detected.
func (sd *slashingDetector) CheckBlock(height uint64, round uint32, pub crypto.ValidatorPubKey, hash crypto.Hash32, sig []byte) *DoubleSignEvidence {
	key := evidenceKey{height: height, pubHex: pub.ID()}
	sd.mu.Lock()
	defer sd.mu.Unlock()

	prev, exists := sd.seen[key]
	if !exists {
		sd.seen[key] = signedSlot{hash: hash, sig: sig, round: round}
		// Opportunistically prune entries for heights far below this one to
		// prevent the seen map from growing unboundedly.  Keep the last 2048
		// heights of evidence so any delayed double-sign is still caught.
		if height > 2048 {
			pruneBelow := height - 2048
			for k := range sd.seen {
				if k.height < pruneBelow {
					delete(sd.seen, k)
				}
			}
		}
		return nil
	}
	if prev.hash == hash {
		return nil // same block, not a double-sign
	}
	return &DoubleSignEvidence{
		Height:    height,
		Round1:    prev.round,
		Validator: pub,
		Hash1:     prev.hash,
		Sig1:      prev.sig,
		Round2:    round,
		Hash2:     hash,
		Sig2:      sig,
	}
}

// PruneBelow removes all slashing evidence records for heights strictly below
// the given height.  Call this periodically (e.g. once per epoch) to bound
// memory usage when the seen map is not pruned by CheckBlock alone.
func (sd *slashingDetector) PruneBelow(height uint64) {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	for k := range sd.seen {
		if k.height < height {
			delete(sd.seen, k)
		}
	}
}

// String returns a human-readable description of the evidence.
func (e *DoubleSignEvidence) String() string {
	return fmt.Sprintf(
		"DOUBLE-SIGN validator=%s height=%d rounds=%d,%d hash1=%x hash2=%x",
		e.Validator.ID(), e.Height, e.Round1, e.Round2, e.Hash1[:6], e.Hash2[:6],
	)
}
