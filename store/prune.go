package store

import (
	"bytes"
	"encoding/binary"
	"fmt"

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

		// Strip TxData: replace the block bytes with a header-only version.
		// We encode the pruned marker as the single JSON string `null` appended
		// to the height key only; the block hash key is left pointing to a
		// stripped StoredBlock with TxData omitted.
		sb, err := d.GetBlockByHeight(h)
		if err != nil {
			return pruned, fmt.Errorf("unmarshal block at height %d: %w", h, err)
		}
		if sb == nil {
			continue
		}
		if len(sb.TxData) == 0 {
			// Already pruned or empty block — just advance cursor.
			continue
		}
		sb.TxData = nil // drop transaction payloads
		if err := d.PutBlock(sb.Hash, sb); err != nil {
			return pruned, fmt.Errorf("rewrite pruned block at height %d: %w", h, err)
		}
		pruned++
	}

	// Advance cursor.
	var cb [8]byte
	binary.LittleEndian.PutUint64(cb[:], end)
	if err := d.PutMeta("prune_cursor", cb[:]); err != nil {
		return pruned, fmt.Errorf("update prune cursor: %w", err)
	}
	return pruned, nil
}
