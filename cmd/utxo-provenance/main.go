// utxo-provenance compares a recorded UTXO with the indexed full block body.
// It never opens the live node database or writes to the supplied copy.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type outputMatch struct {
	TxIndex          int    `json:"tx_index"`
	OutputIndex      int    `json:"output_index"`
	TxHash           string `json:"tx_hash"`
	LegacyTxHash     string `json:"legacy_tx_hash"`
	IsCoinbase       bool   `json:"is_coinbase"`
	Inputs           int    `json:"inputs"`
	CommitmentEquals bool   `json:"commitment_equals"`
	FullOutputEquals bool   `json:"full_output_equals"`
}

type report struct {
	Height                  uint64        `json:"height"`
	IndexedBlockHash        string        `json:"indexed_block_hash"`
	DecodedBlockHash        string        `json:"decoded_block_hash"`
	PreviousBlockHash       string        `json:"previous_block_hash"`
	PreOracleHeaderHash     string        `json:"pre_oracle_header_hash"`
	PreBaseFeeHeaderHash    string        `json:"pre_base_fee_header_hash"`
	PreOracleSignatureOK    bool          `json:"pre_oracle_signature_ok"`
	HeaderOraclePrice       uint64        `json:"header_oracle_price"`
	HeaderBaseFee           uint64        `json:"header_base_fee"`
	BlockHashMatchesIndex   bool          `json:"block_hash_matches_index"`
	MerkleRootMatchesHeader bool          `json:"merkle_root_matches_header"`
	ClaimedTxHash           string        `json:"claimed_tx_hash"`
	ClaimedOutputIndex      uint32        `json:"claimed_output_index"`
	StoredUTXOExists        bool          `json:"stored_utxo_exists"`
	StoredUTXOHeight        uint64        `json:"stored_utxo_height,omitempty"`
	ClaimedTxIndexHeight    uint64        `json:"claimed_tx_index_height,omitempty"`
	ClaimedTxIndexPosition  int           `json:"claimed_tx_index_position,omitempty"`
	ClaimedTxIndexExists    bool          `json:"claimed_tx_index_exists"`
	Matches                 []outputMatch `json:"matches"`
}

func run() error {
	path := flag.String("copied-db", "", "existing isolated LevelDB copy (read-only)")
	height := flag.Uint64("height", 0, "claimed block height")
	hashHex := flag.String("tx-hash", "", "recorded transaction hash (64 hex characters)")
	index := flag.Uint("output-index", 0, "recorded output index")
	flag.Parse()
	if *path == "" || len(*hashHex) != 64 || *index > uint(^uint32(0)) {
		return fmt.Errorf("usage: utxo-provenance --copied-db PATH --height N --tx-hash HEX64 --output-index N")
	}
	hashBytes, err := hex.DecodeString(*hashHex)
	if err != nil {
		return fmt.Errorf("invalid transaction hash: %w", err)
	}
	var claimedHash crypto.Hash32
	copy(claimedHash[:], hashBytes)

	db, err := store.OpenLPoDAuditReadOnly(*path)
	if err != nil {
		return err
	}
	defer db.Close()

	indexedHash, found, err := db.GetCanonicalHash(*height)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no height-index entry at %d", *height)
	}
	raw, err := db.GetRawBlock(indexedHash)
	if err != nil {
		return err
	}
	if raw == nil {
		return fmt.Errorf("full body missing or pruned at height %d", *height)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		return fmt.Errorf("decode block at %d: %w", *height, err)
	}
	if block.Header.Height != *height {
		return fmt.Errorf("height-index entry %d contains header height %d", *height, block.Header.Height)
	}

	r := report{
		Height:                  *height,
		IndexedBlockHash:        fmt.Sprintf("%x", indexedHash),
		DecodedBlockHash:        fmt.Sprintf("%x", block.Hash()),
		PreviousBlockHash:       fmt.Sprintf("%x", block.Header.PrevHash),
		BlockHashMatchesIndex:   indexedHash == block.Hash(),
		MerkleRootMatchesHeader: core.MerkleRoot(block.Txs) == block.Header.MerkleRoot,
		ClaimedTxHash:           *hashHex,
		ClaimedOutputIndex:      uint32(*index),
		Matches:                 []outputMatch{},
	}
	preOracle, preBaseFee := historicalHeaderHashes(&block.Header)
	r.PreOracleHeaderHash = fmt.Sprintf("%x", preOracle)
	r.PreBaseFeeHeaderHash = fmt.Sprintf("%x", preBaseFee)
	r.PreOracleSignatureOK = block.Header.ValidatorPub.Verify(preOracle, block.Header.Signature)
	r.HeaderOraclePrice = block.Header.OraclePrice
	r.HeaderBaseFee = block.Header.BaseFee
	record, err := db.GetUTXO(claimedHash, uint32(*index))
	if err != nil {
		return err
	}
	if record != nil {
		r.StoredUTXOExists = true
		r.StoredUTXOHeight = record.BlockHeight
		if record.TxHash != claimedHash || record.OutputIndex != uint32(*index) {
			return fmt.Errorf("stored UTXO key disagrees with its embedded identity")
		}
	}
	loc, err := db.LookupTxIdx(claimedHash)
	if err != nil {
		return err
	}
	if loc != nil {
		r.ClaimedTxIndexExists = true
		r.ClaimedTxIndexHeight = loc.Height
		r.ClaimedTxIndexPosition = loc.TxIdx
	}
	for ti, tx := range block.Txs {
		for oi, out := range tx.Outputs {
			if record == nil {
				continue
			}
			commitmentEquals := out.AmountCommit == record.AmountCommit
			fullEquals := commitmentEquals &&
				out.OneTimePub == record.OneTimePub &&
				out.TxPubKey == record.TxPubKey &&
				out.EncAmount == record.EncAmount
			if !commitmentEquals {
				continue
			}
			r.Matches = append(r.Matches, outputMatch{
				TxIndex:          ti,
				OutputIndex:      oi,
				TxHash:           fmt.Sprintf("%x", tx.Hash()),
				LegacyTxHash:     fmt.Sprintf("%x", legacyTxHash(&tx)),
				IsCoinbase:       tx.IsCoinbase(),
				Inputs:           len(tx.Inputs),
				CommitmentEquals: commitmentEquals,
				FullOutputEquals: fullEquals,
			})
		}
	}
	return json.NewEncoder(os.Stdout).Encode(r)
}

// legacyTxHash reproduces the hash inputs from the production source before
// CLSAG ring commitments and pseudo-outputs were added to transaction identity.
// It is diagnostic only; never use it for signing or validating new blocks.
func legacyTxHash(tx *core.Transaction) crypto.Hash32 {
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

func historicalHeaderHashes(h *core.BlockHeader) (crypto.Hash32, crypto.Hash32) {
	height, timestamp, round, oracle := make([]byte, 8), make([]byte, 8), make([]byte, 4), make([]byte, 8)
	binary.LittleEndian.PutUint64(height, h.Height)
	binary.LittleEndian.PutUint64(timestamp, uint64(h.Timestamp))
	binary.LittleEndian.PutUint32(round, h.Round)
	binary.LittleEndian.PutUint64(oracle, h.OraclePrice)
	parts := [][]byte{height, h.PrevHash[:], h.MerkleRoot[:], timestamp, round, h.ValidatorPub}
	preOracle := crypto.HashBytes(parts...)
	return preOracle, crypto.HashBytes(append(parts, oracle)...)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
