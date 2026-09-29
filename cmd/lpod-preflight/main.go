// lpod-preflight inspects an isolated copy of the chain database without writes.
// Its numbers are not a historical issuance proof or a migration witness.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

func run() error {
	path := flag.String("copied-db", "", "path to an isolated LevelDB copy")
	sampleBlocks := flag.Uint64("sample-blocks", 128, "number of most recent canonical bodies to verify (max 50000)")
	sampleEnd := flag.Uint64("sample-end-height", 0, "end of sample; zero means copied database tip")
	searchNonReward := flag.Uint64("search-non-reward", 0, "scan this many older blocks backwards for a non-reward transaction (max 2500000)")
	flag.Parse()
	if *path == "" {
		return fmt.Errorf("usage: lpod-preflight --copied-db PATH")
	}
	if *sampleBlocks == 0 || *sampleBlocks > 50_000 {
		return fmt.Errorf("sample-blocks must be between 1 and 50000")
	}
	if *searchNonReward > 2_500_000 {
		return fmt.Errorf("search-non-reward exceeds 2500000 blocks")
	}
	db, err := store.OpenLPoDAuditReadOnly(*path)
	if err != nil {
		return err
	}
	defer db.Close()

	tip, height, err := db.GetTip()
	if err != nil {
		return fmt.Errorf("read tip: %w", err)
	}
	end := height
	if *sampleEnd != 0 {
		if *sampleEnd > height {
			return fmt.Errorf("sample-end-height exceeds copied database tip")
		}
		end = *sampleEnd
	}
	indexedGenesis, found, err := db.GetCanonicalHash(0)
	if err != nil {
		return fmt.Errorf("read genesis index: %w", err)
	}
	if !found {
		return fmt.Errorf("genesis index missing")
	}
	raw, err := db.GetRawBlock(indexedGenesis)
	if err != nil {
		return fmt.Errorf("read genesis body: %w", err)
	}
	if raw == nil {
		return fmt.Errorf("genesis body missing")
	}
	var genesis core.Block
	if err := json.Unmarshal(raw, &genesis); err != nil {
		return fmt.Errorf("decode genesis body: %w", err)
	}
	if genesis.Header.Height != 0 {
		return fmt.Errorf("genesis index contains height %d", genesis.Header.Height)
	}
	pool, poolFound, err := db.LoadStakingPoolRemaining()
	if err != nil {
		return fmt.Errorf("read persisted validator pool: %w", err)
	}
	// Sample full recent bodies from the isolated copy. A disagreement between
	// indexed hashes, current header hashing or the current transaction Merkle
	// codec rules out using this candidate binary to extend that live chain.
	start := uint64(0)
	if end+1 > *sampleBlocks {
		start = end + 1 - *sampleBlocks
	}
	var sampled, missing, hashMismatch, merkleMismatch, nonRewardTxs uint64
	var firstHashMismatch, firstMerkleMismatch *uint64
	for h := start; h <= end; h++ {
		indexed, found, err := db.GetCanonicalHash(h)
		if err != nil {
			return fmt.Errorf("read canonical index %d: %w", h, err)
		}
		if !found {
			missing++
			continue
		}
		body, err := db.GetRawBlock(indexed)
		if err != nil {
			return fmt.Errorf("read full body %d: %w", h, err)
		}
		if body == nil {
			missing++
			continue
		}
		var block core.Block
		if err := json.Unmarshal(body, &block); err != nil {
			return fmt.Errorf("decode full body %d: %w", h, err)
		}
		sampled++
		for _, tx := range block.Txs {
			if !tx.IsCoinbase() {
				nonRewardTxs++
			}
		}
		if block.Header.Height != h || block.Hash() != indexed {
			hashMismatch++
			if firstHashMismatch == nil {
				firstHashMismatch = &h
			}
		}
		if core.MerkleRoot(block.Txs) != block.Header.MerkleRoot {
			merkleMismatch++
			if firstMerkleMismatch == nil {
				firstMerkleMismatch = &h
			}
		}
	}
	var historicalTxHeight *uint64
	var historicalTxVersion *core.TxVersion
	var historicalMerkleMatches *bool
	for n, h := uint64(0), height; n < *searchNonReward; n++ {
		indexed, found, err := db.GetCanonicalHash(h)
		if err != nil {
			return fmt.Errorf("search canonical index %d: %w", h, err)
		}
		if found {
			body, err := db.GetRawBlock(indexed)
			if err != nil {
				return fmt.Errorf("search canonical body %d: %w", h, err)
			}
			if body != nil {
				var block core.Block
				if err := json.Unmarshal(body, &block); err != nil {
					return fmt.Errorf("decode searched body %d: %w", h, err)
				}
				for _, tx := range block.Txs {
					if !tx.IsCoinbase() {
						version := tx.Version
						matches := core.MerkleRoot(block.Txs) == block.Header.MerkleRoot
						historicalTxHeight, historicalTxVersion, historicalMerkleMatches = &h, &version, &matches
						break
					}
				}
				if historicalTxHeight != nil {
					break
				}
			}
		}
		if h == 0 {
			break
		}
		h--
	}
	result := struct {
		TipHeight                     uint64          `json:"tip_height"`
		SampleStartHeight             uint64          `json:"sample_start_height"`
		SampleEndHeight               uint64          `json:"sample_end_height"`
		TipHash                       string          `json:"tip_hash"`
		IndexedGenesisHash            string          `json:"indexed_genesis_hash"`
		DecodedGenesisHash            string          `json:"decoded_genesis_hash"`
		PersistedValidatorPoolFound   bool            `json:"persisted_validator_pool_found"`
		PersistedValidatorPoolNAPRO   string          `json:"persisted_validator_pool_napro,omitempty"`
		HistoricalIssuanceProven      bool            `json:"historical_issuance_proven"`
		AvailableSaleAllocationProven bool            `json:"available_sale_allocation_proven"`
		RecentFullBodiesSampled       uint64          `json:"recent_full_bodies_sampled"`
		RecentFullBodiesMissing       uint64          `json:"recent_full_bodies_missing"`
		RecentNonRewardTxs            uint64          `json:"recent_non_reward_txs"`
		RecentHeaderHashMismatches    uint64          `json:"recent_header_hash_mismatches"`
		RecentMerkleMismatches        uint64          `json:"recent_merkle_mismatches"`
		FirstHashMismatchHeight       *uint64         `json:"first_hash_mismatch_height,omitempty"`
		FirstMerkleMismatchHeight     *uint64         `json:"first_merkle_mismatch_height,omitempty"`
		HistoricalNonRewardHeight     *uint64         `json:"historical_non_reward_height,omitempty"`
		HistoricalNonRewardVersion    *core.TxVersion `json:"historical_non_reward_version,omitempty"`
		HistoricalMerkleMatches       *bool           `json:"historical_merkle_matches,omitempty"`
	}{
		TipHeight: height, SampleStartHeight: start, SampleEndHeight: end, TipHash: fmt.Sprintf("%x", tip),
		IndexedGenesisHash:          fmt.Sprintf("%x", indexedGenesis),
		DecodedGenesisHash:          fmt.Sprintf("%x", genesis.Hash()),
		PersistedValidatorPoolFound: poolFound,
		PersistedValidatorPoolNAPRO: func() string {
			if !poolFound {
				return ""
			}
			return fmt.Sprintf("%d", pool)
		}(),
		HistoricalIssuanceProven: false, AvailableSaleAllocationProven: false,
		RecentFullBodiesSampled: sampled, RecentFullBodiesMissing: missing,
		RecentNonRewardTxs:         nonRewardTxs,
		RecentHeaderHashMismatches: hashMismatch, RecentMerkleMismatches: merkleMismatch,
		FirstHashMismatchHeight: firstHashMismatch, FirstMerkleMismatchHeight: firstMerkleMismatch,
		HistoricalNonRewardHeight: historicalTxHeight, HistoricalNonRewardVersion: historicalTxVersion,
		HistoricalMerkleMatches: historicalMerkleMatches,
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	if missing != 0 || hashMismatch != 0 || merkleMismatch != 0 ||
		(*searchNonReward > 0 && (historicalTxHeight == nil || !*historicalMerkleMatches)) {
		return fmt.Errorf("lpod preflight: incomplete or incompatible canonical-body sample")
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
