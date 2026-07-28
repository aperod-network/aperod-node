package store

import (
	"encoding/binary"
	"fmt"
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
