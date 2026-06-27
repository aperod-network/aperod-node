package consensus

import (
	"fmt"
	"sync"

	"github.com/aperod/aperod/crypto"
)

// DoubleSignEvidence records two conflicting block signatures from the same
// validator in the same slot (same height + round).
type DoubleSignEvidence struct {
	Height    uint64
	Round     uint32
	Validator crypto.ValidatorPubKey
	Hash1     crypto.Hash32
	Sig1      []byte
	Hash2     crypto.Hash32
	Sig2      []byte
}

// slotKey uniquely identifies a proposer slot.
type slotKey struct {
	height uint64
	round  uint32
	pubHex string
}

// slashingDetector detects double-signing within the same validator slot.
type slashingDetector struct {
	mu   sync.Mutex
	seen map[slotKey]signedSlot // first block seen per slot
}

type signedSlot struct {
	hash crypto.Hash32
	sig  []byte
}

func newSlashingDetector() *slashingDetector {
	return &slashingDetector{seen: make(map[slotKey]signedSlot)}
}

// CheckBlock records a signed block header and returns DoubleSignEvidence
// if this validator has already signed a different block in the same slot.
// Returns nil if no double-sign is detected.
func (sd *slashingDetector) CheckBlock(height uint64, round uint32, pub crypto.ValidatorPubKey, hash crypto.Hash32, sig []byte) *DoubleSignEvidence {
	key := slotKey{height: height, round: round, pubHex: pub.ID()}
	sd.mu.Lock()
	defer sd.mu.Unlock()

	prev, exists := sd.seen[key]
	if !exists {
		sd.seen[key] = signedSlot{hash: hash, sig: sig}
		return nil
	}
	if prev.hash == hash {
		return nil // same block, not a double-sign
	}
	return &DoubleSignEvidence{
		Height:    height,
		Round:     round,
		Validator: pub,
		Hash1:     prev.hash,
		Sig1:      prev.sig,
		Hash2:     hash,
		Sig2:      sig,
	}
}

// String returns a human-readable description of the evidence.
func (e *DoubleSignEvidence) String() string {
	return fmt.Sprintf(
		"DOUBLE-SIGN validator=%s height=%d round=%d hash1=%x hash2=%x",
		e.Validator.ID(), e.Height, e.Round, e.Hash1[:6], e.Hash2[:6],
	)
}
