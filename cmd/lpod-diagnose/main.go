// lpod-diagnose compares historical header hash candidates against a copied
// database's canonical index. It is an offline, read-only diagnostic.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type hashCandidate struct {
	Hash              string `json:"hash"`
	MatchesIndex      bool   `json:"matches_index"`
	SignatureVerifies bool   `json:"signature_verifies"`
}

type diagnosis struct {
	Height      uint64                   `json:"height"`
	Status      string                   `json:"status"`
	IndexHash   string                   `json:"index_hash,omitempty"`
	BlockHash   string                   `json:"block_hash,omitempty"`
	MerkleMatch *bool                    `json:"merkle_match,omitempty"`
	Pruned      bool                     `json:"pruned,omitempty"`
	Candidates  map[string]hashCandidate `json:"candidates,omitempty"`
	Error       string                   `json:"error,omitempty"`
}

func main() {
	copiedDB := flag.String("copied-db", "", "path to a separate copied LevelDB database (required)")
	heightText := flag.String("height", "", "canonical block height to diagnose (required)")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "lpod-diagnose compares historical block-header hash candidates from an offline database copy.")
		fmt.Fprintln(flag.CommandLine.Output(), "The supplied database must be a separate cold copy, never the live node database.")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *copiedDB == "" || *heightText == "" {
		fmt.Fprintln(os.Stderr, "usage: lpod-diagnose --copied-db PATH --height HEIGHT")
		os.Exit(2)
	}
	height, err := strconv.ParseUint(*heightText, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --height %q: %v\n", *heightText, err)
		os.Exit(2)
	}
	result := diagnose(*copiedDB, height)
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "encode diagnostic report: %v\n", err)
		os.Exit(1)
	}
	if result.Status == "error" {
		os.Exit(1)
	}
}

func diagnose(path string, height uint64) diagnosis {
	result := diagnosis{Height: height}
	db, err := store.OpenLPoDAuditReadOnly(path)
	if err != nil {
		result.Status, result.Error = "error", err.Error()
		return result
	}
	defer db.Close()

	indexHash, found, err := db.GetCanonicalHash(height)
	if err != nil {
		result.Status, result.Error = "error", fmt.Sprintf("read canonical height index: %v", err)
		return result
	}
	if !found {
		result.Status = "missing_index"
		return result
	}
	result.IndexHash = hashString(indexHash)
	raw, err := db.GetRawBlock(indexHash)
	if err != nil {
		result.Status, result.Error = "error", fmt.Sprintf("read indexed block: %v", err)
		return result
	}
	if raw == nil {
		result.Status = "missing_or_pruned"
		return result
	}
	if isPrunedBlock(raw) {
		result.Status, result.Pruned = "pruned", true
		return result
	}

	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		result.Status, result.Error = "error", fmt.Sprintf("decode full block: %v", err)
		return result
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		result.Status, result.Error = "error", fmt.Sprintf("decode block fields: %v", err)
		return result
	}
	if _, ok := fields["Txs"]; !ok {
		result.Status, result.Error = "error", "stored block does not contain a full Txs field"
		return result
	}
	if block.Header.Height != height {
		result.Status, result.Error = "error", fmt.Sprintf("block header height is %d", block.Header.Height)
		return result
	}

	currentHash := block.Hash()
	result.BlockHash = hashString(currentHash)
	merkleMatch := block.Header.MerkleRoot == core.MerkleRoot(block.Txs)
	result.MerkleMatch = &merkleMatch
	result.Candidates = make(map[string]hashCandidate, 3)
	for _, item := range []struct {
		name            string
		oracle, baseFee bool
	}{
		{name: "before_oracle_price_and_base_fee"},
		{name: "oracle_price_only", oracle: true},
		{name: "current_with_oracle_price_and_base_fee", oracle: true, baseFee: true},
	} {
		hash := historicalHeaderHash(block.Header, item.oracle, item.baseFee)
		result.Candidates[item.name] = hashCandidate{
			Hash:              hashString(hash),
			MatchesIndex:      hash == indexHash,
			SignatureVerifies: block.Header.ValidatorPub.Verify(hash, block.Header.Signature),
		}
	}
	result.Status = "ok"
	return result
}

// historicalHeaderHash preserves the byte-for-byte field order and
// little-endian integer encoding from core/block.go before the OraclePrice and
// BaseFee additions. HashBytes in crypto is SHA-256 over these concatenated
// fields, with no separators or length prefixes.
func historicalHeaderHash(header core.BlockHeader, includeOraclePrice, includeBaseFee bool) crypto.Hash32 {
	var encoded bytes.Buffer
	var u64 [8]byte
	binary.LittleEndian.PutUint64(u64[:], header.Height)
	encoded.Write(u64[:])
	encoded.Write(header.PrevHash[:])
	encoded.Write(header.MerkleRoot[:])
	binary.LittleEndian.PutUint64(u64[:], uint64(header.Timestamp))
	encoded.Write(u64[:])
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], header.Round)
	encoded.Write(u32[:])
	encoded.Write(header.ValidatorPub)
	if includeOraclePrice {
		binary.LittleEndian.PutUint64(u64[:], header.OraclePrice)
		encoded.Write(u64[:])
	}
	if includeBaseFee {
		binary.LittleEndian.PutUint64(u64[:], header.BaseFee)
		encoded.Write(u64[:])
	}
	sum := sha256.Sum256(encoded.Bytes())
	return crypto.Hash32(sum)
}

func hashString(hash crypto.Hash32) string {
	return fmt.Sprintf("%x", hash[:])
}

func isPrunedBlock(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	const firstPrunedField = `{"height"`
	if !bytes.HasPrefix(raw, []byte(firstPrunedField)) || !bytes.Contains(raw, []byte(`"tx_count"`)) {
		return false
	}
	for i := len(firstPrunedField); i < len(raw); i++ {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case ':':
			return true
		default:
			return false
		}
	}
	return false
}
