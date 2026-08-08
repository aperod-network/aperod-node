package api

import (
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// getTransactionFromDisk is the LevelDB disk fallback for Chain.GetTransaction.
//
// Chain keeps only the last MaxInMemoryBlocks transactions in its in-memory
// txIndex map.  Any transaction in an older block — including admin-minted
// UTXOs that were confirmed long ago — is evicted from that window and causes
// Chain.GetTransaction to return !ok, even though the data is still fully
// present on disk.
//
// This method reads the persisted tx-location index (prefixTxIdx) to find the
// block height and tx position, then loads the block from LevelDB and
// deserialises the transaction.
//
// IMPORTANT: blocks are stored via PutRawBlock as marshalled core.Block JSON,
// NOT as StoredBlock JSON.  GetBlockByHeight returns a StoredBlock and always
// yields nil TxData when given a core.Block payload.  We must use
// GetRawBlockByHeight + json.Unmarshal into core.Block to recover the full
// transaction list.
//
// Returns:
//   - (tx, loc, true,  nil) on success
//   - (zero, zero, false, nil) when the tx hash is not in the index
//   - (zero, zero, false, err) on a storage I/O or deserialisation error
func (s *Server) getTransactionFromDisk(hash crypto.Hash32) (core.Transaction, core.TxLocation, bool, error) {
	if s.blockStore == nil {
		return core.Transaction{}, core.TxLocation{}, false, nil
	}

	// 1. Look up the block location from the persisted tx-hash index.
	entry, err := s.blockStore.LookupTxIdx(hash)
	if err != nil {
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("disk tx index lookup: %w", err)
	}
	if entry == nil {
		return core.Transaction{}, core.TxLocation{}, false, nil
	}

	// 2. Load the raw block bytes (stored as marshalled core.Block JSON by
	//    storeBlock/PutRawBlock — not as StoredBlock JSON).
	raw, err := s.blockStore.GetRawBlockByHeight(entry.Height)
	if err != nil {
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("disk load raw block at height %d: %w", entry.Height, err)
	}
	if raw == nil {
		return core.Transaction{}, core.TxLocation{}, false, nil
	}

	// 3. Deserialise the block to access its transaction list.
	var b core.Block
	if err := json.Unmarshal(raw, &b); err != nil {
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("disk unmarshal block at height %d: %w", entry.Height, err)
	}
	if entry.TxIdx >= len(b.Txs) {
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("disk tx index out of range: height=%d txIdx=%d txCount=%d",
				entry.Height, entry.TxIdx, len(b.Txs))
	}

	// 4. Construct a synthetic TxLocation.  Only Header.Height is required by
	//    the downstream mint-detection code (loc.Block.Header.Height).
	loc := core.TxLocation{
		Block:   &core.Block{Header: core.BlockHeader{Height: entry.Height}},
		TxIndex: entry.TxIdx,
	}
	return b.Txs[entry.TxIdx], loc, true, nil
}
