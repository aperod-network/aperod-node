package api

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

var errDiskTxIndexMismatch = errors.New("disk tx index hash mismatch")

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
// IMPORTANT: blocks are stored via PutRawBlock as marshaled core.Block JSON,
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

	canonicalHash, canonical, err := s.blockStore.GetCanonicalHash(entry.Height)
	if err != nil {
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("disk canonical block hash at height %d: %w", entry.Height, err)
	}
	if !canonical {
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("%w: no canonical block hash at height=%d", errDiskTxIndexMismatch, entry.Height)
	}

	// 2. Load the raw block bytes (stored as marshaled core.Block JSON by
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
	if b.Header.Height != entry.Height || b.Hash() != canonicalHash {
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("%w: block at height=%d does not match its canonical index",
				errDiskTxIndexMismatch, entry.Height)
	}
	if entry.TxIdx < 0 || entry.TxIdx >= len(b.Txs) {
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("disk tx index out of range: height=%d txIdx=%d txCount=%d",
				entry.Height, entry.TxIdx, len(b.Txs))
	}
	tx := b.Txs[entry.TxIdx]
	if !matchesDiskTransactionHash(&tx, hash) {
		actualHash := tx.Hash()
		return core.Transaction{}, core.TxLocation{}, false,
			fmt.Errorf("%w: height=%d txIdx=%d requested=%x actual=%x",
				errDiskTxIndexMismatch,
				entry.Height, entry.TxIdx, hash, actualHash)
	}

	// 4. Construct TxLocation using the full deserialized block so that
	//    loc.Block.Hash() returns the correct block hash in the REST response.
	//    A synthetic block with only Header.Height set would produce the wrong
	//    hash because Block.Hash() hashes the entire serialized block.
	loc := core.TxLocation{
		Block:   &b,
		TxIndex: entry.TxIdx,
	}
	return tx, loc, true, nil
}

// matchesDiskTransactionHash supports transaction indexes written by the
// pre-CLSAG transaction-ID rule. This is a read-only compatibility check:
// consensus validation, signing, block construction, and new transaction IDs
// continue to use Transaction.Hash().
func matchesDiskTransactionHash(tx *core.Transaction, requested crypto.Hash32) bool {
	if tx == nil {
		return false
	}
	if tx.Hash() == requested {
		return true
	}
	if tx.Version != core.TxVersionCLSAG || tx.AVM != nil {
		return false
	}
	return preCLSAGTransactionHash(tx) == requested
}

// preCLSAGTransactionHash reproduces the transaction-ID fields used before
// CLSAG ring commitments and pseudo-outputs were included in the hash. It is
// intentionally limited to disk lookup of CLSAG transactions and must never
// be used to validate, sign, or create a transaction.
func preCLSAGTransactionHash(tx *core.Transaction) crypto.Hash32 {
	fee := make([]byte, 8)
	binary.LittleEndian.PutUint64(fee, tx.Fee)
	parts := [][]byte{{byte(tx.Version)}, fee, tx.FeeCommit[:], tx.Extra}
	for _, input := range tx.Inputs {
		parts = append(parts, input.KeyImage[:], input.AmountCommit[:])
		for _, pub := range input.Ring {
			parts = append(parts, pub[:])
		}
		if tx.Version == core.TxVersionCommitmentBinding {
			parts = append(parts, []byte{input.RealIndex})
		}
	}
	for _, output := range tx.Outputs {
		parts = append(parts, output.OneTimePub[:], output.AmountCommit[:],
			output.TxPubKey[:], output.EncAmount[:])
	}
	return crypto.HashBytes(parts...)
}
