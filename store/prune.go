package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// PruneBlocksOlderThan strips the full transaction data (TxData) from all
// blocks whose height is strictly less than pruneBelow, while keeping the
// block header metadata intact so the height index and chain integrity are
// preserved.
//
// The function is incremental: it reads a "prune_cursor" from metadata so that
// repeated calls only scan the newly pruneable window.  A batch of up to
// maxPruneBatch blocks is processed per call to bound latency.
//
// Returns the number of blocks whose TxData was erased in this call.
func (d *DB) PruneBlocksOlderThan(pruneBelow uint64) (int, error) {
	const maxPruneBatch = 1000

	// Read cursor from metadata (little-endian uint64, matches PutTip convention).
	cursorBytes, err := d.GetMeta("prune_cursor")
	if err != nil {
		return 0, fmt.Errorf("read prune cursor: %w", err)
	}
	cursor := uint64(0)
	if len(cursorBytes) == 8 {
		cursor = binary.LittleEndian.Uint64(cursorBytes)
	}

	// Resolve the activation height once per call, rather than probing all
	// version keys for every block in the pruning window.
	var fundingHeight uint64
	var fundingHash crypto.Hash32
	hasFunding := false
	for _, key := range []string{
		"lpod/activation_height/v1",
		"lpod/activation_height/v2",
		"lpod/activation_height/v3",
	} {
		activation, err := d.GetMeta(key)
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", key, err)
		}
		if activation == nil {
			continue
		}
		if len(activation) != 8 {
			return 0, fmt.Errorf("corrupt %s: expected 8-byte activation height", key)
		}
		height := binary.LittleEndian.Uint64(activation)
		if hasFunding && height != fundingHeight {
			return 0, fmt.Errorf("conflicting lpod activation heights")
		}
		fundingHeight = height
		hasFunding = true
	}

	if hasFunding {
		canonicalHash, err := d.get(heightKey(fundingHeight))
		if err != nil {
			return 0, fmt.Errorf("read canonical funding block at height %d: %w", fundingHeight, err)
		}
		if len(canonicalHash) != len(fundingHash) {
			return 0, fmt.Errorf("missing canonical funding block at height %d", fundingHeight)
		}
		copy(fundingHash[:], canonicalHash)
		fundingBody, err := d.GetRawBlock(fundingHash)
		if err != nil {
			return 0, fmt.Errorf("read funding block at height %d: %w", fundingHeight, err)
		}
		if fundingBody == nil {
			return 0, fmt.Errorf("missing funding block at height %d", fundingHeight)
		}
	}

	protectedVestingHeights := map[uint64]bool{}
	checkpoint, err := d.LoadLPoDCheckpoint()
	if err != nil {
		return 0, fmt.Errorf("load reserve checkpoint before pruning: %w", err)
	}
	if checkpoint != nil && checkpoint.GenesisVesting != nil {
		protectedVestingHeights[checkpoint.GenesisVesting.FundingHeight] = true
		for _, h := range checkpoint.GenesisVesting.PaidHeights {
			if h > 0 {
				protectedVestingHeights[h] = true
			}
		}
	}
	if err := d.validateSaleRetentionIndex(); err != nil {
		return 0, fmt.Errorf("validate sale retention before pruning: %w", err)
	}
	if cursor >= pruneBelow {
		return 0, nil // nothing new to prune
	}

	end := pruneBelow
	if end > cursor+maxPruneBatch {
		end = cursor + maxPruneBatch
	}

	pruned := 0
	for h := cursor + 1; h <= end; h++ {
		raw, err := d.GetRawBlockByHeight(h)
		if err != nil {
			return pruned, fmt.Errorf("get block at height %d: %w", h, err)
		}
		retainedValue, err := d.get(saleRetainedBlockKey(h))
		if err != nil {
			return pruned, fmt.Errorf("read sale retention marker at height %d: %w", h, err)
		}
		if flags, retainedHash, retained, err := saleRetentionMarker(retainedValue, h); err != nil {
			return pruned, err
		} else if retained {
			canonicalHash, found, err := d.GetCanonicalHash(h)
			if err != nil || !found || canonicalHash != retainedHash {
				return pruned, fmt.Errorf("sale retention marker is not bound to canonical height %d: %v", h, err)
			}
			if raw == nil {
				return pruned, fmt.Errorf("retained sale block body missing at height %d", h)
			}
			if flags&saleRetainedDelivery != 0 {
				err = validateSaleRetentionBody(raw, h, retainedHash)
			} else {
				err = validateSaleRetentionHeader(raw, h, retainedHash)
			}
			if err != nil {
				return pruned, err
			}
			continue
		}
		if hasFunding && h == fundingHeight {
			canonicalHash, err := d.get(heightKey(h))
			if err != nil {
				return pruned, fmt.Errorf("read canonical funding block at height %d: %w", h, err)
			}
			if !bytes.Equal(canonicalHash, fundingHash[:]) {
				return pruned, fmt.Errorf("canonical funding block changed at height %d", h)
			}
			if raw == nil {
				return pruned, fmt.Errorf("missing funding block at height %d", h)
			}
			// Keep the full canonical funding block body for checkpoint replay.
			continue
		}
		if raw == nil {
			// Gap in chain — skip, advance cursor past it.
			continue
		}
		// Retain the at-most eleven reserve/payment bodies for permanent canonical
		// payout evidence, even when ordinary old block bodies are pruned.
		if protectedVestingHeights[h] {
			continue
		}

		stripped, err := d.stripCanonicalBlockTransactions(h, raw)
		if err != nil {
			return pruned, err
		}
		if stripped {
			pruned++
		}
	}

	// Advance cursor.
	var cb [8]byte
	binary.LittleEndian.PutUint64(cb[:], end)
	if err := d.PutMeta("prune_cursor", cb[:]); err != nil {
		return pruned, fmt.Errorf("update prune cursor: %w", err)
	}
	return pruned, nil
}

// stripCanonicalBlockTransactions supports the current core.Block encoding and
// the older flattened StoredBlock encoding. Either representation must match
// the canonical height index before it can be rewritten.
func (d *DB) stripCanonicalBlockTransactions(height uint64, raw []byte) (bool, error) {
	hash, found, err := d.GetCanonicalHash(height)
	if err != nil || !found {
		return false, fmt.Errorf("missing canonical hash while pruning height %d: %v", height, err)
	}

	var block core.Block
	if err := json.Unmarshal(raw, &block); err == nil &&
		block.Header.Height == height && block.Hash() == hash {
		if len(block.Txs) == 0 {
			// Empty blocks and blocks already pruned by an interrupted prior run
			// need no rewrite. Sale delivery bodies were validated above.
			return false, nil
		}
		if block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
			return false, fmt.Errorf("block body merkle root mismatch at height %d", height)
		}
		block.Txs = nil
		encoded, err := json.Marshal(&block)
		if err != nil {
			return false, fmt.Errorf("marshal pruned block at height %d: %w", height, err)
		}
		if err := d.PutRawBlock(hash, height, encoded); err != nil {
			return false, fmt.Errorf("rewrite pruned block at height %d: %w", height, err)
		}
		return true, nil
	}

	var stored StoredBlock
	if err := json.Unmarshal(raw, &stored); err != nil ||
		stored.Height != height || stored.Hash != hash {
		return false, fmt.Errorf("block at height %d is not bound to its canonical hash", height)
	}
	if len(stored.TxData) == 0 {
		return false, nil
	}
	stored.TxData = nil
	if err := d.PutBlock(hash, &stored); err != nil {
		return false, fmt.Errorf("rewrite legacy pruned block at height %d: %w", height, err)
	}
	return true, nil
}
