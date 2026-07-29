package core

import (
	"fmt"
	"time"
)

// BlockVerifierConfig holds parameters for block validation.
type BlockVerifierConfig struct {
	// MaxBlockTime is the maximum allowed future timestamp for a block.
	MaxBlockTime time.Duration
	// MaxPastDrift is the maximum allowed age of a block timestamp relative to
	// the node's wall clock. Blocks older than this are rejected to prevent
	// Timejacking — replaying blocks with stale timestamps to skew chain time.
	// Set to 0 to disable the past-drift check (e.g. during fast sync).
	MaxPastDrift time.Duration
	// MaxTxsPerBlock is the hard limit on transactions per block.
	MaxTxsPerBlock int
	// MaxBlockSize in bytes.
	MaxBlockSize int
}

// DefaultBlockVerifierConfig returns safe production defaults.
func DefaultBlockVerifierConfig() BlockVerifierConfig {
	return BlockVerifierConfig{
		MaxBlockTime:   2 * time.Second,  // reject blocks > 2s in the future
		MaxPastDrift:   30 * time.Second, // reject blocks > 30s in the past (anti-timejacking)
		MaxTxsPerBlock: 2000,
		MaxBlockSize:   4 * 1024 * 1024, // 4 MB
	}
}

// BlockVerifier validates blocks against chain and consensus rules.
// Cryptographic tx verification is delegated to TxVerifier.
type BlockVerifier struct {
	cfg    BlockVerifierConfig
	chain  *Chain
	txVerifier *TxVerifier
}

// NewBlockVerifier creates a block verifier.
func NewBlockVerifier(cfg BlockVerifierConfig, chain *Chain, txV *TxVerifier) *BlockVerifier {
	return &BlockVerifier{cfg: cfg, chain: chain, txVerifier: txV}
}

// VerifyBlock performs full block validation:
//  1. Structural validity (block.Validate — header, sig, merkle, tx structure)
//  2. Chain continuity (extends tip, prev hash)
//  3. Timestamp is not too far in the future, not before parent
//  4. Tx count and size limits
//  5. Cryptographic tx verification (ring sigs, range proofs, balance)
func (v *BlockVerifier) VerifyBlock(block *Block) error {
	// 1. Structural validity
	if err := block.Validate(); err != nil {
		return fmt.Errorf("structural: %w", err)
	}

	// 2. Chain continuity
	if !block.IsGenesis() {
		parent := v.chain.GetByHash(block.Header.PrevHash)
		if parent == nil {
			return fmt.Errorf("block %d: parent %x not found",
				block.Header.Height, block.Header.PrevHash[:8])
		}
		if parent.Header.Height+1 != block.Header.Height {
			return fmt.Errorf("block %d: height discontinuity (parent=%d)",
				block.Header.Height, parent.Header.Height)
		}

		// 3. Timestamp
		parentTime := time.Unix(0, parent.Header.Timestamp)
		blockTime := time.Unix(0, block.Header.Timestamp)

		if !blockTime.After(parentTime) {
			return fmt.Errorf("block %d: timestamp %v not after parent %v",
				block.Header.Height, blockTime, parentTime)
		}
		now := time.Now().UTC()
		if blockTime.After(now.Add(v.cfg.MaxBlockTime)) {
			return fmt.Errorf("block %d: timestamp %v too far in the future (now=%v)",
				block.Header.Height, blockTime, now)
		}
		// Anti-timejacking: reject blocks with stale timestamps so an attacker
		// cannot replay old headers to skew the chain's perceived wall time.
		if v.cfg.MaxPastDrift > 0 && blockTime.Before(now.Add(-v.cfg.MaxPastDrift)) {
			return fmt.Errorf("block %d: timestamp %v too far in the past (now=%v, maxDrift=%v)",
				block.Header.Height, blockTime, now, v.cfg.MaxPastDrift)
		}
	}

	// 4. Tx limits
	if len(block.Txs) > v.cfg.MaxTxsPerBlock {
		return fmt.Errorf("block %d: too many txs: %d > %d",
			block.Header.Height, len(block.Txs), v.cfg.MaxTxsPerBlock)
	}
	if size := block.Size(); size > v.cfg.MaxBlockSize {
		return fmt.Errorf("block %d: too large: %d bytes > %d",
			block.Header.Height, size, v.cfg.MaxBlockSize)
	}

	// 5. Cryptographic tx verification
	if v.txVerifier != nil {
		if err := v.txVerifier.VerifyBlock(block); err != nil {
			return fmt.Errorf("tx crypto: %w", err)
		}
	}

	return nil
}
