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

	// 2. Load the stored block (contains TxData as []json.RawMessage).
	sb, err := s.blockStore.GetBlockByHeight(entry.Height)
	if err != nil {
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("disk load block at height %d: %w", entry.Height, err)
	}
	if sb == nil || entry.TxIdx >= len(sb.TxData) {
		return core.Transaction{}, core.TxLocation{}, false, nil
	}

	// 3. Deserialise the raw JSON back into a core.Transaction.
	var tx core.Transaction
	if err := json.Unmarshal(sb.TxData[entry.TxIdx], &tx); err != nil {
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("disk unmarshal tx (height=%d txIdx=%d): %w",
				entry.Height, entry.TxIdx, err)
	}

	// 4. Construct a synthetic TxLocation.  Only Header.Height is required by
	//    the downstream mint-detection code (loc.Block.Header.Height); the full
	//    block is not needed so we avoid loading it a second time.
	loc := core.TxLocation{
		Block:   &core.Block{Header: core.BlockHeader{Height: entry.Height}},
		TxIndex: entry.TxIdx,
	}
	return tx, loc, true, nil
}
